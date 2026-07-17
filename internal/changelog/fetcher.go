package changelog

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maximumFetchedBody = 10 << 20

type Fetched struct {
	FinalURL                      *url.URL
	MediaType, ETag, LastModified string
	Body                          []byte
	FetchedAt                     time.Time
}

type IPResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type Fetcher struct {
	Resolver    IPResolver
	DialContext func(context.Context, string, string) (net.Conn, error)
	Timeout     time.Duration
	Now         func() time.Time
	TLSConfig   *tls.Config

	mu    sync.Mutex
	hosts map[string]*hostGate
}

type hostGate struct {
	sem  chan struct{}
	refs int
}

func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Fetched, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return Fetched{}, fmt.Errorf("unsafe changelog URL %q", rawURL)
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && unsafeIP(ip) {
		return Fetched{}, fmt.Errorf("private or unsafe address %s", ip)
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resolver := f.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dial := f.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := resolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("resolve %s: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("resolve %s: no addresses", host)
			}
			for _, ip := range ips {
				if unsafeIP(ip) {
					return nil, fmt.Errorf("private or unsafe address %s for %s", ip, host)
				}
			}
			return dial(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		TLSClientConfig:       f.TLSConfig,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: hostLimitedTransport{fetcher: f, next: transport}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("redirect limit exceeded")
		}
		if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.User != nil {
			return errors.New("unsafe redirect scheme")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Fetched{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Fetched{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Fetched{}, fmt.Errorf("GET %s: %s", resp.Request.URL, resp.Status)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return Fetched{}, fmt.Errorf("invalid media type: %w", err)
	}
	mediaType = strings.ToLower(mediaType)
	if mediaType != "text/html" && mediaType != "application/xhtml+xml" && mediaType != "text/plain" && mediaType != "text/markdown" {
		return Fetched{}, fmt.Errorf("unsupported media type %q", mediaType)
	}
	if resp.ContentLength > maximumFetchedBody {
		return Fetched{}, fmt.Errorf("response is too large: limit is %d bytes", maximumFetchedBody)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maximumFetchedBody+1))
	if err != nil {
		return Fetched{}, err
	}
	if len(body) > maximumFetchedBody {
		return Fetched{}, fmt.Errorf("response is too large: limit is %d bytes", maximumFetchedBody)
	}
	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now()
	}
	return Fetched{FinalURL: resp.Request.URL, MediaType: mediaType, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"), Body: body, FetchedAt: now}, nil
}

type hostLimitedTransport struct {
	fetcher *Fetcher
	next    http.RoundTripper
}

func (t hostLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	release, err := t.fetcher.acquire(req.Context(), strings.ToLower(req.URL.Hostname()))
	if err != nil {
		return nil, err
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &releaseBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Close() error { err := b.ReadCloser.Close(); b.once.Do(b.release); return err }

func unsafeIP(ip netip.Addr) bool {
	return !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func (f *Fetcher) acquire(ctx context.Context, host string) (func(), error) {
	f.mu.Lock()
	if f.hosts == nil {
		f.hosts = make(map[string]*hostGate)
	}
	gate := f.hosts[host]
	if gate == nil {
		gate = &hostGate{sem: make(chan struct{}, 2)}
		f.hosts[host] = gate
	}
	gate.refs++
	f.mu.Unlock()
	select {
	case gate.sem <- struct{}{}:
		return func() {
			<-gate.sem
			f.releaseGate(host, gate)
		}, nil
	case <-ctx.Done():
		f.releaseGate(host, gate)
		return nil, ctx.Err()
	}
}

func (f *Fetcher) releaseGate(host string, gate *hostGate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate.refs--
	if gate.refs == 0 && f.hosts[host] == gate {
		delete(f.hosts, host)
	}
}

package changelog

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if values, ok := r[host]; ok {
		return values, nil
	}
	return nil, errors.New("unknown host")
}

func publicTestFetcher(server *httptest.Server) *Fetcher {
	addr := server.Listener.Addr().String()
	f := &Fetcher{
		Resolver: staticResolver{"public.test": {netip.MustParseAddr("192.0.2.1")}},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	if transport, ok := server.Client().Transport.(*http.Transport); ok && transport.TLSClientConfig != nil {
		f.TLSConfig = transport.TLSClientConfig.Clone()
		f.TLSConfig.ServerName = "example.com"
	}
	return f
}

func TestFetcherAcceptsHTTPAndHTTPS(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	servers := []*httptest.Server{httptest.NewServer(handler), httptest.NewTLSServer(handler)}
	for _, server := range servers {
		t.Run(server.URL, func(t *testing.T) {
			defer server.Close()
			raw := strings.Replace(server.URL, "127.0.0.1", "public.test", 1)
			got, err := publicTestFetcher(server).Fetch(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Body) != "ok" {
				t.Fatalf("body=%q", got.Body)
			}
		})
	}
}

func TestFetcherReturnsBodyValidatorsAndFinalURL(t *testing.T) {
	now := time.Unix(123, 0).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Last-Modified", "yesterday")
		_, _ = w.Write([]byte("<h1>Changes</h1>"))
	}))
	defer server.Close()
	f := publicTestFetcher(server)
	f.Now = func() time.Time { return now }
	got, err := f.Fetch(context.Background(), "http://public.test/start")
	if err != nil {
		t.Fatal(err)
	}
	if got.FinalURL.String() != "http://public.test/start" || got.MediaType != "text/html" || got.ETag != `"abc"` || got.LastModified != "yesterday" || string(got.Body) != "<h1>Changes</h1>" || !got.FetchedAt.Equal(now) {
		t.Fatalf("fetched = %#v", got)
	}
}

func TestFetcherRejectsUnsafeInputsBeforeDial(t *testing.T) {
	for _, raw := range []string{"ftp://public.test/a", "http://127.0.0.1/a", "http://[::1]/a", "http://private.test/a"} {
		t.Run(raw, func(t *testing.T) {
			called := false
			f := &Fetcher{Resolver: staticResolver{"private.test": {netip.MustParseAddr("10.0.0.1")}}, DialContext: func(context.Context, string, string) (net.Conn, error) {
				called = true
				return nil, errors.New("dialed")
			}}
			if _, err := f.Fetch(context.Background(), raw); err == nil {
				t.Fatal("unsafe URL accepted")
			}
			if called {
				t.Fatal("unsafe address was dialed")
			}
		})
	}
}

func TestFetcherRevalidatesEveryRedirectAndLimitsRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://private.test/next", http.StatusFound)
	}))
	defer server.Close()
	f := publicTestFetcher(server)
	f.Resolver = staticResolver{"public.test": {netip.MustParseAddr("192.0.2.1")}, "private.test": {netip.MustParseAddr("127.0.0.1")}}
	if _, err := f.Fetch(context.Background(), "http://public.test/start"); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("redirect error = %v", err)
	}

	hops := 0
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/"+strings.Repeat("x", hops), http.StatusFound)
	}))
	defer server2.Close()
	f = publicTestFetcher(server2)
	if _, err := f.Fetch(context.Background(), "http://public.test/"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect limit error = %v", err)
	}
	if hops != 4 {
		t.Fatalf("requests = %d, want 4", hops)
	}
}

func TestFetcherAcceptsExactlyThreeRedirectsAndRejectsFour(t *testing.T) {
	for _, tc := range []struct {
		redirects int
		wantErr   bool
	}{{3, false}, {4, true}} {
		t.Run(fmt.Sprintf("redirects_%d", tc.redirects), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
				if n < tc.redirects {
					http.Redirect(w, r, fmt.Sprintf("/%d", n+1), http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("ok"))
			}))
			defer server.Close()
			_, err := publicTestFetcher(server).Fetch(context.Background(), "http://public.test/0")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
			wantRequests := tc.redirects + 1
			if tc.wantErr {
				wantRequests = tc.redirects
			}
			if requests != wantRequests {
				t.Fatalf("requests=%d want=%d", requests, wantRequests)
			}
		})
	}
}

func TestFetcherLimitsConcurrencyAtRedirectDestinationHost(t *testing.T) {
	var active, maximum atomic.Int32
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "origin") {
			http.Redirect(w, r, "http://target.test/end", http.StatusFound)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	f := publicTestFetcher(server)
	f.Resolver = staticResolver{"origin1.test": {netip.MustParseAddr("192.0.2.1")}, "origin2.test": {netip.MustParseAddr("192.0.2.2")}, "origin3.test": {netip.MustParseAddr("192.0.2.3")}, "target.test": {netip.MustParseAddr("192.0.2.4")}}
	var wg sync.WaitGroup
	wg.Add(3)
	for i := 1; i <= 3; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = f.Fetch(context.Background(), fmt.Sprintf("http://origin%d.test/start", i))
		}(i)
	}
	<-entered
	<-entered
	thirdEntered := false
	select {
	case <-entered:
		thirdEntered = true
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if thirdEntered {
		t.Fatal("third redirected request entered")
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum=%d", maximum.Load())
	}
	f.mu.Lock()
	retainedHosts := len(f.hosts)
	f.mu.Unlock()
	if retainedHosts != 0 {
		t.Fatalf("retained host gates=%d", retainedHosts)
	}
}

func TestFetcherRejectsMediaTypeAndOversize(t *testing.T) {
	for _, tc := range []struct {
		name, media string
		body        []byte
	}{
		{"media", "image/png", []byte("png")}, {"size", "text/html", make([]byte, (10<<20)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.media)
				_, _ = w.Write(tc.body)
			}))
			defer server.Close()
			if _, err := publicTestFetcher(server).Fetch(context.Background(), "http://public.test/"); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestFetcherAppliesRequestTimeout(t *testing.T) {
	f := &Fetcher{Resolver: staticResolver{"public.test": {netip.MustParseAddr("192.0.2.1")}}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }, Timeout: 20 * time.Millisecond}
	start := time.Now()
	_, err := f.Fetch(context.Background(), "https://public.test/")
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("timeout error=%v elapsed=%v", err, time.Since(start))
	}
}

func TestFetcherLimitsEachHostToTwoConcurrentRequests(t *testing.T) {
	var active, maximum atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	f := publicTestFetcher(server)
	var wg sync.WaitGroup
	wg.Add(3)
	for range 3 {
		go func() { defer wg.Done(); _, _ = f.Fetch(context.Background(), "http://public.test/") }()
	}
	<-entered
	<-entered
	select {
	case <-entered:
		t.Fatal("third request entered concurrently")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency=%d", maximum.Load())
	}
}

func TestDiscoverLinksScoresNormalizesDeduplicatesAndCaps(t *testing.T) {
	base, _ := url.Parse("https://example.test/docs/index.html")
	html := []byte(`<a href="/noise">home</a><a href="changes">Changes</a><a href="/releases">Release notes</a><a href="/releases#top">release duplicate</a><a href="/v1">Version 1.2.3</a><a href="/changelog">Changelog</a><a href="/history">History</a><a href="mailto:x@y">mail</a>`)
	got := DiscoverLinks(base, html)
	if len(got) != 5 {
		t.Fatalf("candidates=%d: %#v", len(got), got)
	}
	if got[0].Label != "Changelog" || got[0].URL.Fragment != "" {
		t.Fatalf("first=%#v", got[0])
	}
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c.URL.String()] {
			t.Fatalf("duplicate %s", c.URL)
		}
		seen[c.URL.String()] = true
	}
}

func TestDiscoverLinksPrioritizesSelectedVersion(t *testing.T) {
	base, _ := url.Parse("https://example.test/")
	got := DiscoverLinks(base, []byte(`<a href="/changelog">Changelog</a><a href="/releases/8.2.1">Version 8.2.1 notes</a>`), "8.2.1")
	if len(got) != 2 || got[0].URL.Path != "/releases/8.2.1" {
		t.Fatalf("candidates=%#v", got)
	}
}

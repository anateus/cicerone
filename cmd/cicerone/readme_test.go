package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/changelog"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/download"
	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/gitrepo"
	"github.com/anateus/cicerone/internal/store"
	"github.com/anateus/cicerone/internal/testutil"
	"github.com/anateus/cicerone/internal/upstream"
)

type readmeDNS struct{}

func (readmeDNS) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
}

func newREADMELoader(t *testing.T, homepage, source string, handler http.HandlerFunc) *packageDetailLoader {
	t.Helper()
	ctx := context.Background()
	cache, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	repo := testutil.NewGitRepo(t)
	commit := repo.Commit("Formula/w/widget.rb", fmt.Sprintf(`class Widget < Formula
  homepage %q
  url %q
  version "2.0"
end`, homepage, source), "widget 2.0", time.Now())
	event := domain.UpdateEvent{ID: "event", PackageID: "widget", Name: "widget", Type: domain.PackageFormula, Kind: domain.EventVersion, NewVersion: "2.0", Repository: "homebrew-core", DefinitionPath: "Formula/w/widget.rb", Commit: commit, Time: time.Now()}
	if err := cache.UpsertEvents(ctx, []domain.UpdateEvent{event}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	fetcher := &changelog.Fetcher{
		Resolver: readmeDNS{},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
		TLSConfig: server.Client().Transport.(*http.Transport).TLSClientConfig.Clone(),
	}
	fetcher.TLSConfig.ServerName = "example.com"
	queue := download.NewQueue(download.Options{Context: ctx, Workers: 2, HostInterval: -1})
	t.Cleanup(queue.Close)
	loader := &packageDetailLoader{store: cache, queue: queue, fetcher: fetcher}
	loader.changelogs = changelogLoader{cache: cache, repository: func(context.Context, string) (gitrepo.Repository, error) {
		return gitrepo.New(gitrepo.Source{Name: "homebrew-core", Path: repo.Path}, execx.NewRunner()), nil
	}, locator: &upstream.Locator{Store: cache, Fetch: func(ctx context.Context, rawURL string) (upstream.FetchedPage, error) {
		fetched, err := fetcher.Fetch(ctx, rawURL)
		return upstream.FetchedPage{FinalURL: fetched.FinalURL, MediaType: fetched.MediaType, Body: fetched.Body}, err
	}}}
	return loader
}

func TestREADMEFallsBackToHomepageAndCachesReadableContent(t *testing.T) {
	const source = `<html><head><title>Widget</title></head><body><h1>Widget</h1><p>A <strong>small</strong> tool.</p><h2>Install</h2><pre><code>brew install widget</code></pre><a href="guide">User guide</a><script>tracking()</script></body></html>`
	loader := newREADMELoader(t, "https://widget.test/", "https://downloads.test/widget.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "widget.test" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, source)
	})
	document, err := loader.LoadREADME(context.Background(), "widget", "event")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Widget", "**small**", "## Install", "brew install widget", "https://widget.test/guide"} {
		if !strings.Contains(string(document.Extracted), want) {
			t.Errorf("missing %q in %s", want, document.Extracted)
		}
	}
	if document.SourceURL != "https://widget.test/" || string(document.Raw) != source || strings.Contains(string(document.Extracted), "tracking()") {
		t.Fatalf("homepage document = %#v", document)
	}
	cached, found, err := loader.LoadCachedREADME(context.Background(), "widget")
	if err != nil || !found || string(cached.Extracted) != string(document.Extracted) {
		t.Fatalf("cached homepage = %#v, %v, %v", cached, found, err)
	}
}

func TestREADMEPrefersRepositoryAndFallsBackAfterMissingFiles(t *testing.T) {
	for _, readmeExists := range []bool{true, false} {
		t.Run(fmt.Sprintf("repository_readme_%v", readmeExists), func(t *testing.T) {
			var homepageRequests atomic.Int32
			loader := newREADMELoader(t, "https://widget.test/", "https://github.com/acme/widget/archive/v2.tar.gz", func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "raw.githubusercontent.com" && readmeExists && strings.HasSuffix(r.URL.Path, "/README.md") {
					w.Header().Set("Content-Type", "text/plain")
					fmt.Fprint(w, "# Repository README\n\nKeep the original Markdown.")
					return
				}
				if r.Host == "widget.test" {
					homepageRequests.Add(1)
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, "<h1>Project homepage</h1><p>Install widget.</p>")
					return
				}
				http.NotFound(w, r)
			})
			doc, err := loader.LoadREADME(context.Background(), "widget", "event")
			if err != nil {
				t.Fatal(err)
			}
			if readmeExists {
				if string(doc.Extracted) != "# Repository README\n\nKeep the original Markdown." || homepageRequests.Load() != 0 || doc.ExtractionStatus != "ok" {
					t.Fatalf("repository README = %#v; homepage requests = %d", doc, homepageRequests.Load())
				}
			} else if !strings.Contains(string(doc.Extracted), "# Project homepage") || doc.ExtractionStatus != "homepage" {
				t.Fatalf("homepage fallback = %#v", doc)
			}
		})
	}
}

func TestHomepageRedirectRevalidationAndOfflineCache(t *testing.T) {
	var offline atomic.Bool
	var revalidated atomic.Int32
	loader := newREADMELoader(t, "https://widget.test/", "https://downloads.test/widget.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		if r.Host != "widget.test" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/docs/", http.StatusFound)
			return
		}
		if r.Header.Get("If-None-Match") == `"homepage-v1"` {
			revalidated.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/xhtml+xml")
		w.Header().Set("ETag", `"homepage-v1"`)
		fmt.Fprint(w, `<h1>Widget</h1><p>Offline documentation.</p><a href="guide">Guide</a>`)
	})
	ctx := context.Background()
	first, err := loader.LoadREADME(ctx, "widget", "event")
	if err != nil {
		t.Fatal(err)
	}
	if first.URL != "https://widget.test/" || first.SourceURL != "https://widget.test/docs/" || !strings.Contains(string(first.Extracted), "https://widget.test/docs/guide") {
		t.Fatalf("redirected document = %#v", first)
	}
	second, err := loader.refreshREADMEWithCached(ctx, "widget", "event", first)
	if err != nil || revalidated.Load() != 1 || second.ID != first.ID || string(second.Extracted) != string(first.Extracted) {
		t.Fatalf("revalidation = %#v, %v; conditional requests = %d", second, err, revalidated.Load())
	}
	offline.Store(true)
	if _, err := loader.refreshREADMEWithCached(ctx, "widget", "event", second); err == nil {
		t.Fatal("expected refresh failure while offline")
	}
	cached, found, err := loader.LoadCachedREADME(ctx, "widget")
	if err != nil || !found || cached.ID != first.ID || string(cached.Extracted) != string(first.Extracted) {
		t.Fatalf("offline cache = %#v, %v, %v", cached, found, err)
	}
}

func TestHomepageRejectsUnsupportedContentAndCancels(t *testing.T) {
	loader := newREADMELoader(t, "https://widget.test/", "https://downloads.test/widget.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "binary data")
	})
	if _, err := loader.LoadREADME(context.Background(), "widget", "event"); err == nil || !strings.Contains(err.Error(), "unsupported media type") {
		t.Fatalf("unsupported homepage error = %v", err)
	}
	if _, found, err := loader.LoadCachedREADME(context.Background(), "widget"); err != nil || found {
		t.Fatalf("invalid response was cached: found=%v, err=%v", found, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loader.LoadREADME(ctx, "widget", "event"); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}

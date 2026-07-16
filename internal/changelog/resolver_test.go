package changelog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cicerone/internal/domain"
	"cicerone/internal/store"
)

func TestResolverUsesRepositoryFilenameOrderAndCachesResult(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("unauthenticated authorization = %q", got)
		}
		if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("API version = %q", r.Header.Get("X-GitHub-Api-Version"))
		}
		switch r.URL.Path {
		case "/repos/acme/widget/contents":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "NEWS.md", "download_url": serverURL(r) + "/news"}, {"name": "changelog.MD", "download_url": serverURL(r) + "/changelog"}})
		case "/changelog":
			w.Write([]byte("## v1.2.3\n\nChosen first.\n"))
		case "/news":
			w.Write([]byte("## v1.2.3\n\nWrong order.\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	r := NewResolver(cache, server.Client())
	r.APIBaseURL = server.URL
	r.Now = func() time.Time { return time.Unix(1000, 0).UTC() }
	pkg := PackageRef{Name: "widget", FullName: "widget", RepositoryURL: "https://github.com/acme/widget", Type: domain.PackageFormula}
	section, err := r.Resolve(ctx, pkg, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(section.Body, "Chosen first") {
		t.Fatalf("body = %q", section.Body)
	}
	before := requests
	section, err = r.Resolve(ctx, pkg, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if requests != before {
		t.Fatalf("cached resolve made %d requests", requests-before)
	}
}

func TestResolverBacksOffAfterFailedRefresh(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	now := time.Unix(3000, 0).UTC()
	r := NewResolver(cache, server.Client())
	r.APIBaseURL = server.URL
	r.Now = func() time.Time { return now }
	pkg := PackageRef{Name: "widget", RepositoryURL: "https://github.com/acme/widget", Type: domain.PackageFormula}
	if _, err := r.Resolve(ctx, pkg, "3.0.0"); err == nil {
		t.Fatal("failed refresh returned nil error")
	}
	before := requests
	if _, err := r.Resolve(ctx, pkg, "3.0.0"); err == nil || !strings.Contains(err.Error(), "backed off") {
		t.Fatalf("second error = %v, want backoff", err)
	}
	if requests != before {
		t.Fatalf("backoff made %d requests", requests-before)
	}
}

func TestResolverTriesNormalizedGitHubReleaseTagsAndOptionalToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret")
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if strings.HasSuffix(r.URL.Path, "/releases/tags/v2.0.0") {
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v2.0.0", "body": "Release body", "html_url": "https://github.com/acme/widget/releases/tag/v2.0.0"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	r := NewResolver(cache, server.Client())
	r.APIBaseURL = server.URL
	section, err := r.Resolve(ctx, PackageRef{Name: "widget", RepositoryURL: "https://github.com/acme/widget", Type: domain.PackageFormula}, "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if section.Confidence != 1 || !strings.Contains(section.Body, "Release body") {
		t.Fatalf("section = %#v", section)
	}
	if len(paths) < 2 || !strings.Contains(strings.Join(paths, "\n"), "/releases/tags/2.0.0") || !strings.Contains(strings.Join(paths, "\n"), "/releases/tags/v2.0.0") {
		t.Fatalf("paths = %v", paths)
	}
	before := len(paths)
	if _, err := r.Resolve(ctx, PackageRef{Name: "widget", RepositoryURL: "https://github.com/acme/widget", Type: domain.PackageFormula}, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if len(paths) != before {
		t.Fatalf("cached release made %d HTTP requests", len(paths)-before)
	}
}

func serverURL(r *http.Request) string { return "http://" + r.Host }

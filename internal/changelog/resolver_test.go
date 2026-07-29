package changelog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cicerone/internal/domain"
	"cicerone/internal/execx"
	"cicerone/internal/store"
	"github.com/google/go-cmp/cmp"
)

type runnerCall struct {
	name string
	args []string
}

type recordingRunner struct {
	result execx.Result
	err    error
	calls  []runnerCall
}

func TestRepositoryMetadataTagsCombinesTopicsAndLanguages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"topics":   []string{"terminal", "go"},
				"language": "Go",
			})
		case "/repos/acme/widget/languages":
			_ = json.NewEncoder(w).Encode(map[string]int64{
				"Go": 900, "Shell": 100,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resolver := NewResolver(nil, server.Client())
	resolver.APIBaseURL = server.URL
	got, err := resolver.RepositoryMetadataTags(context.Background(), "https://github.com/acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"go", "terminal", "Shell"}, got); diff != "" {
		t.Fatalf("repository metadata tags (-want +got):\n%s", diff)
	}
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	r.calls = append(r.calls, runnerCall{name: name, args: append([]string(nil), args...)})
	return r.result, r.err
}

func (r *recordingRunner) Stream(context.Context, string, ...string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected Stream call")
}

func TestResolverGitHubTokenPrefersEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", " environment-token ")
	runner := &recordingRunner{result: execx.Result{Stdout: []byte("cli-token\n")}}
	r := NewResolver(nil, nil, WithGitHubTokenRunner(runner))
	if r.githubToken != "environment-token" {
		t.Fatalf("token=%q", r.githubToken)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("calls=%v", runner.calls)
	}
}

func TestResolverGitHubTokenFallsBackToCLI(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	runner := &recordingRunner{result: execx.Result{Stdout: []byte("cli-token\n")}}
	r := NewResolver(nil, nil, WithGitHubTokenRunner(runner))
	if r.githubToken != "cli-token" {
		t.Fatalf("token=%q", r.githubToken)
	}
	if diff := cmp.Diff([]runnerCall{{name: "gh", args: []string{"auth", "token"}}}, runner.calls, cmp.AllowUnexported(runnerCall{})); diff != "" {
		t.Fatal(diff)
	}
}

func TestResolverGitHubTokenUnavailable(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	tests := []struct {
		name   string
		result execx.Result
		err    error
	}{
		{name: "runner error", err: errors.New("gh unavailable")},
		{name: "whitespace stdout", result: execx.Result{Stdout: []byte(" \n\t")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingRunner{result: tt.result, err: tt.err}
			r := NewResolver(nil, nil, WithGitHubTokenRunner(runner))
			if r.githubToken != "" {
				t.Fatalf("token=%q", r.githubToken)
			}
		})
	}
}

func TestResolverGitHubTokenResolvedOnceAndUsedForRequests(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	runner := &recordingRunner{result: execx.Result{Stdout: []byte("cli-token\n")}}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer cli-token" {
			t.Errorf("authorization=%q", got)
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	r := NewResolver(nil, server.Client(), WithGitHubTokenRunner(runner))
	r.APIBaseURL = server.URL
	for range 2 {
		resp, err := r.request(context.Background(), server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%v", runner.calls)
	}
}

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

func TestResolverUsesCodebergRepositoryChangelog(t *testing.T) {
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Codeberg request leaked authorization header %q", got)
		}
		switch r.URL.Path {
		case "/api/v1/repos/pter/pter/contents":
			_ = json.NewEncoder(w).Encode([]map[string]string{{
				"name": "CHANGELOG.md", "download_url": serverURL(r) + "/raw/changelog",
			}})
		case "/raw/changelog":
			_, _ = w.Write([]byte("## 3.23.1\n\nCodeberg changes.\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	r := NewResolver(cache, server.Client())
	r.CodebergAPIBaseURL = server.URL + "/api/v1"
	r.githubToken = "github-secret"
	section, err := r.Resolve(ctx, PackageRef{
		Name: "pter", FullName: "pter", RepositoryURL: "https://codeberg.org/pter/pter", Type: domain.PackageFormula,
	}, "3.23.1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(section.Body, "Codeberg changes") {
		t.Fatalf("section body = %q", section.Body)
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

func TestResolverUsesLowConfidenceCacheOnlyAsFallback(t *testing.T) {
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	if err := cache.UpsertChangelogPackage(ctx, "widget", "widget", "formula"); err != nil {
		t.Fatal(err)
	}
	_, err = cache.SaveChangelogArtifact(ctx, "widget", store.ChangelogArtifact{URL: "https://example.test/weak", Hash: "weak", Raw: []byte{}, Extracted: []byte("Prose mentions 4.0.0 without a heading.")})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/releases/tags/4.0.0") {
			json.NewEncoder(w).Encode(map[string]string{"tag_name": "4.0.0", "body": "Strong release body", "html_url": "https://example.test/release"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	r := NewResolver(cache, server.Client())
	r.APIBaseURL = server.URL
	section, err := r.Resolve(ctx, PackageRef{Name: "widget", RepositoryURL: "https://github.com/acme/widget", Type: domain.PackageFormula}, "4.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if section.Confidence != 1 || !strings.Contains(section.Body, "Strong") {
		t.Fatalf("section=%#v,want strong discovery", section)
	}
}

func TestResolverTriesGitHubReleaseWhileLinkedFallbackIsBackedOff(t *testing.T) {
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	now := time.Unix(4000, 0).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/tags/v3.0.0") {
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v3.0.0", "body": "Release fallback", "html_url": "https://github.com/acme/widget/releases/tag/v3.0.0"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	backoffKey := server.URL + "/repos/acme/widget"
	if err := cache.RecordChangelogFailure(ctx, backoffKey, now, errors.New("linked discovery failed")); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(cache, server.Client())
	r.APIBaseURL = server.URL
	r.Now = func() time.Time { return now.Add(time.Minute) }
	section, err := r.Resolve(ctx, PackageRef{Name: "widget", RepositoryURL: "https://github.com/acme/widget", Type: domain.PackageFormula}, "3.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(section.Body, "Release fallback") {
		t.Fatalf("section = %#v", section)
	}
}

func TestResolverRejectsOversizeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, (8<<20)+1)) }))
	defer server.Close()
	r := NewResolver(nil, server.Client())
	if _, _, _, _, err := r.getBytes(context.Background(), server.URL); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversize error=%v", err)
	}
}

func TestResolverRejectsOversizeJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("[]")); w.Write(make([]byte, 4<<20)) }))
	defer server.Close()
	r := NewResolver(nil, server.Client())
	var value []any
	if err := r.getJSON(context.Background(), server.URL, &value); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversize JSON error=%v", err)
	}
}

func TestResolverFollowsRankedHomepageChangelogAndPreservesProvenance(t *testing.T) {
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	pageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(`<a href="/noise">Home</a><a href="/changes">Changelog</a>`))
			return
		}
		if r.URL.Path == "/changes" {
			_, _ = w.Write([]byte(`<main><h1>Changes</h1><h2>5.0.0</h2><ul><li>Linked fix.</li></ul></main>`))
			return
		}
		http.NotFound(w, r)
	}))
	defer pageServer.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer api.Close()
	fetcher := publicTestFetcher(pageServer)
	r := NewResolver(cache, api.Client())
	r.APIBaseURL = api.URL
	r.Fetcher = fetcher
	r.Extractor = ReadabilityExtractor{}
	section, err := r.Resolve(ctx, PackageRef{Name: "widget", RepositoryURL: "https://github.com/acme/widget", Homepage: "http://public.test/", Type: domain.PackageFormula}, "5.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if section.SourceURL != "http://public.test/changes" || !strings.Contains(section.Body, "- Linked fix.") {
		t.Fatalf("section=%#v", section)
	}
	artifacts, err := cache.ChangelogArtifacts(ctx, "widget")
	if err != nil {
		t.Fatal(err)
	}
	var linked bool
	for _, a := range artifacts {
		if a.URL == "http://public.test/changes" && a.ParentID != nil {
			linked = true
		}
	}
	if !linked {
		t.Fatalf("linked artifact provenance missing: %#v", artifacts)
	}
}

func TestResolverUsesHomepageWithoutGitHubRepository(t *testing.T) {
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(`<a href="/changes">Changelog</a>`))
			return
		}
		_, _ = w.Write([]byte(`<article><h1>6.0.0</h1><ul><li>Generic source.</li></ul></article>`))
	}))
	defer page.Close()
	r := NewResolver(cache, nil)
	r.Fetcher = publicTestFetcher(page)
	r.Extractor = ReadabilityExtractor{}
	section, err := r.Resolve(ctx, PackageRef{Name: "widget", Homepage: "http://public.test/", Type: domain.PackageFormula}, "6.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(section.Body, "Generic source") {
		t.Fatalf("section=%#v", section)
	}
}

func TestResolverPrefersLabeledPageOverExactSeedAndCapsDiscovery(t *testing.T) {
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := cache.UpsertChangelogPackage(ctx, "widget", "widget", "formula"); err != nil {
		t.Fatal(err)
	}
	requests := map[string]int{}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path]++
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<main><h1>7.0.0</h1><p>Seed full document.</p><a href="/changelog">Changelog</a><a href="/release">Release</a><a href="/changes">Changes</a><a href="/history">History</a><a href="/v7">7.0.0 notes</a><a href="/sixth">Changelog archive</a></main>`))
		case "/changelog":
			_, _ = w.Write([]byte(`<article><h1>7.0.0</h1><p>Labeled winner.</p><a href="/depth2">Changelog detail</a></article>`))
		default:
			_, _ = w.Write([]byte(`<article><p>no selected version</p></article>`))
		}
	}))
	defer page.Close()
	r := NewResolver(cache, nil)
	r.Fetcher = publicTestFetcher(page)
	r.Extractor = ReadabilityExtractor{}
	section, err := r.resolveLinked(ctx, "widget", "http://public.test/", "7.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(section.Body, "Labeled winner") {
		t.Fatalf("section=%#v", section)
	}
	total := 0
	for _, n := range requests {
		total += n
	}
	if total > 6 {
		t.Fatalf("requests=%d want seed + at most five candidates: %#v", total, requests)
	}
	if requests["/depth2"] != 0 {
		t.Fatalf("returned winner should stop before depth two: %#v", requests)
	}
}

func TestResolverRecordsFailureAndBacksOffLinkedURL(t *testing.T) {
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	calls := 0
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "fail", http.StatusServiceUnavailable)
	}))
	defer page.Close()
	r := NewResolver(cache, nil)
	r.Fetcher = publicTestFetcher(page)
	now := time.Unix(9000, 0).UTC()
	r.Now = func() time.Time { return now }
	if _, err := r.resolveLinked(ctx, "widget", "http://public.test/", "1.0.0"); err == nil {
		t.Fatal("first resolve succeeded")
	}
	before := calls
	if _, err := r.resolveLinked(ctx, "widget", "http://public.test/", "1.0.0"); err == nil || !strings.Contains(err.Error(), "backed off") {
		t.Fatalf("second error=%v", err)
	}
	if calls != before {
		t.Fatalf("backoff made %d requests", calls-before)
	}
}

func TestResolverLimitsDiscoveryToTwoHopsAndFiveCandidates(t *testing.T) {
	t.Run("two hops", func(t *testing.T) {
		ctx := context.Background()
		cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer cache.Close()
		if err := cache.UpsertChangelogPackage(ctx, "widget", "widget", "formula"); err != nil {
			t.Fatal(err)
		}
		requests := map[string]int{}
		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests[r.URL.Path]++
			w.Header().Set("Content-Type", "text/html")
			switch r.URL.Path {
			case "/":
				_, _ = w.Write([]byte(`<a href="/d1">Changelog</a>`))
			case "/d1":
				_, _ = w.Write([]byte(`<a href="/d2">Changelog detail</a>`))
			case "/d2":
				_, _ = w.Write([]byte(`<a href="/d3">Changelog deeper</a>`))
			default:
				_, _ = w.Write([]byte(`<h1>9.0.0</h1>`))
			}
		}))
		defer page.Close()
		r := NewResolver(cache, nil)
		r.Fetcher = publicTestFetcher(page)
		r.Extractor = ReadabilityExtractor{}
		if _, err := r.resolveLinked(ctx, "widget", "http://public.test/", "9.0.0"); err == nil {
			t.Fatal("unexpected match")
		}
		if requests["/d2"] != 1 || requests["/d3"] != 0 {
			t.Fatalf("requests=%#v", requests)
		}
	})
	t.Run("five candidates", func(t *testing.T) {
		ctx := context.Background()
		cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer cache.Close()
		if err := cache.UpsertChangelogPackage(ctx, "widget", "widget", "formula"); err != nil {
			t.Fatal(err)
		}
		requests := map[string]int{}
		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests[r.URL.Path]++
			w.Header().Set("Content-Type", "text/html")
			if r.URL.Path == "/" {
				_, _ = w.Write([]byte(`<a href="/1">Changelog 1</a><a href="/2">Changelog 2</a><a href="/3">Changelog 3</a><a href="/4">Changelog 4</a><a href="/5">Changelog 5</a><a href="/6">Changelog 6</a>`))
				return
			}
			_, _ = w.Write([]byte(`<p>not selected</p>`))
		}))
		defer page.Close()
		r := NewResolver(cache, nil)
		r.Fetcher = publicTestFetcher(page)
		r.Extractor = ReadabilityExtractor{}
		_, _ = r.resolveLinked(ctx, "widget", "http://public.test/", "9.0.0")
		total := 0
		for _, n := range requests {
			total += n
		}
		if total != 6 || requests["/6"] != 0 {
			t.Fatalf("requests=%#v total=%d", requests, total)
		}
	})
}

func TestGitHubRepositoryInfersGitHubPagesProject(t *testing.T) {
	owner, repo, ok := githubRepository("https://acme.github.io/widget/docs/")
	if !ok || owner != "acme" || repo != "widget" {
		t.Fatalf("githubRepository = %q, %q, %t", owner, repo, ok)
	}
}

func TestChangelogFileRank(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{{"CHANGELOG", 0}, {"changelog.md", 0}, {"CHANGES.rst", 1}, {"news.txt", 2}, {"HISTORY", 3}, {"history.md", 4}, {"History.txt", 5}, {"RELEASES.md", 6}, {"PROJECT_WHATSNEW.HTML", 7}, {"README.md", 100}, {"HISTORY.rst", 100}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := changelogFileRank(tt.name); got != tt.want {
				t.Fatalf("rank=%d,want %d", got, tt.want)
			}
		})
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
	r := NewResolver(cache, server.Client(), WithGitHubTokenRunner(nil))
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

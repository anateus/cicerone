package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/changelog"
	"cicerone/internal/domain"
	"cicerone/internal/execx"
	"cicerone/internal/gitrepo"
	"cicerone/internal/history"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
	"cicerone/internal/syncer"
	"cicerone/internal/testutil"
	"cicerone/internal/tui"
)

type fixtureRunner struct {
	installed []byte
	calls     int
}

type publicResolver struct{}

func (publicResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

func (r *fixtureRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	r.calls++
	if name == "brew" && reflect.DeepEqual(args, []string{"info", "--json=v2", "--installed"}) {
		return execx.Result{Stdout: r.installed}, nil
	}
	return execx.Result{}, os.ErrNotExist
}

func (r *fixtureRunner) Stream(context.Context, string, ...string) (io.ReadCloser, error) {
	r.calls++
	return nil, os.ErrNotExist
}

func TestCachedRestartNeedsNoHTTPOrGitAndReturnsSameFeed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	dbPath := filepath.Join(t.TempDir(), "cicerone.db")
	destination, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	installedJSON := mustFixture(t, "homebrew-installed.json")
	brewRunner := &fixtureRunner{installed: installedJSON}
	installed, err := homebrew.NewClient(brewRunner).Installed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.SetInstalled(ctx, installed); err != nil {
		t.Fatal(err)
	}
	brewBin := filepath.Join(t.TempDir(), "brew")
	marker := filepath.Join(t.TempDir(), "upgrade.args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$CICERONE_UPGRADE_MARKER\"\necho upgraded\n"
	if err := os.WriteFile(brewBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(brewBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CICERONE_UPGRADE_MARKER", marker)
	var actionOutput bytes.Buffer
	if err := homebrew.NewClient(brewRunner).RunAction(ctx, homebrew.Action{Kind: homebrew.Upgrade, Package: "fixture", Type: domain.PackageFormula}, &actionOutput); err != nil {
		t.Fatal(err)
	}
	if args := strings.TrimSpace(string(mustRead(t, marker))); args != "upgrade --formula fixture" {
		t.Fatalf("fake upgrade args = %q", args)
	}

	for fixtureIndex, fixture := range []struct {
		name, kind, path, oldFile, newFile string
	}{
		{"homebrew-core", "formula", "Formula/f/fixture.rb", "formula-v1.rb", "formula-v2.rb"},
		{"homebrew-cask", "cask", "Casks/f/fixture-app.rb", "cask-v1.rb", "cask-v2.rb"},
	} {
		repo := testutil.NewGitRepo(t)
		base := now.Add(-time.Duration(4-fixtureIndex*2) * time.Hour)
		repo.Commit(fixture.path, string(mustFixture(t, fixture.oldFile)), "initial", base)
		repo.Commit(fixture.path, string(mustFixture(t, fixture.newFile)), "upgrade", base.Add(time.Hour))
		source := gitrepo.Source{Name: fixture.name, Kind: fixture.kind, Path: repo.Path}
		adapter := gitrepo.New(source, execx.NewRunner())
		if _, err := history.NewIndexer(adapter, destination).Index(ctx, source, history.Request{Since: now.Add(-24 * time.Hour)}); err != nil {
			t.Fatalf("index %s: %v", fixture.name, err)
		}
	}

	releaseJSON := mustFixture(t, "github-release.json")
	linkedHTML := mustFixture(t, "changelog.html")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/linked":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<a href="/changelog">Release notes</a>`)
			return
		case "/changelog":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(linkedHTML)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/contents") {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(releaseJSON)
	}))
	resolver := changelog.NewResolver(destination, server.Client())
	resolver.APIBaseURL = server.URL
	section, err := resolver.Resolve(ctx, changelog.PackageRef{Name: "fixture", FullName: "fixture", Type: domain.PackageFormula, RepositoryURL: "https://github.com/example/fixture"}, "2.0")
	if err != nil {
		t.Fatal(err)
	}
	if section.SourceURL == "" || !strings.Contains(section.Body, "Fixture release notes") {
		t.Fatalf("resolved section = %#v", section)
	}
	serverAddress := server.Listener.Addr().String()
	resolver.Fetcher = &changelog.Fetcher{
		Resolver: publicResolver{},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, serverAddress)
		},
	}
	linkedSection, err := resolver.Resolve(ctx, changelog.PackageRef{Name: "fixture-app", FullName: "fixture-app", Type: domain.PackageCask, Homepage: "http://fixture.test" + server.URL[strings.LastIndex(server.URL, ":"):] + "/linked"}, "2.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(linkedSection.SourceURL, "/changelog") || !strings.Contains(linkedSection.Body, "Linked fixture release notes") {
		t.Fatalf("linked section lacks provenance/content: %#v", linkedSection)
	}

	filter := domain.FeedFilter{Now: now, Horizon: 30 * 24 * time.Hour, Kinds: map[domain.EventKind]bool{domain.EventVersion: true}, Types: map[domain.PackageType]bool{domain.PackageFormula: true, domain.PackageCask: true}, RollUp: true}
	wantFeed, err := destination.QueryFeed(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(wantFeed) != 2 {
		t.Fatalf("initial feed groups = %d, want 2", len(wantFeed))
	}
	formulaOnly := filter
	formulaOnly.Types = map[domain.PackageType]bool{domain.PackageFormula: true}
	if groups, err := destination.QueryFeed(ctx, formulaOnly); err != nil || len(groups) != 1 {
		t.Fatalf("formula filter groups = %d, err %v", len(groups), err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()

	restarted, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	gotFeed, err := restarted.QueryFeed(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotFeed, wantFeed) {
		t.Fatalf("offline feed changed:\ngot  %#v\nwant %#v", gotFeed, wantFeed)
	}
	var gitDiscoveryCalls atomic.Int32
	coordinator := syncer.New(syncer.Dependencies{LoadSources: func(context.Context) ([]syncer.Source, error) {
		gitDiscoveryCalls.Add(1)
		return nil, nil
	}})
	defer coordinator.Close()
	model := tui.NewModel(tui.Dependencies{Context: ctx, OnReady: func() tea.Msg {
		coordinator.Start(ctx)
		return nil
	}})
	updated, readyCmd := model.Update(tui.FeedLoaded{RequestID: 1, Groups: gotFeed})
	if !strings.Contains(updated.View().Content, "fixture") {
		t.Fatalf("cached feed was not rendered before background start:\n%s", updated.View().Content)
	}
	if calls := gitDiscoveryCalls.Load(); calls != 0 {
		t.Fatalf("cached restart made %d Git discovery calls before rendering", calls)
	}
	if batch, ok := readyCmd().(tea.BatchMsg); ok {
		for _, cmd := range batch {
			go cmd()
		}
	}
	deadline := time.Now().Add(time.Second)
	for gitDiscoveryCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if gitDiscoveryCalls.Load() == 0 {
		t.Fatal("background coordinator did not start after cached feed render")
	}
	offlineResolver := changelog.NewResolver(restarted, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("offline restart made an HTTP request")
		return nil, os.ErrNotExist
	})})
	gotSection, err := offlineResolver.Resolve(ctx, changelog.PackageRef{Name: "fixture", FullName: "fixture", Type: domain.PackageFormula, RepositoryURL: "https://github.com/example/fixture"}, "2.0")
	if err != nil {
		t.Fatal(err)
	}
	if gotSection.SourceURL != section.SourceURL || gotSection.Body != section.Body {
		t.Fatalf("cached section changed: got %#v, want %#v", gotSection, section)
	}
	gotLinked, err := offlineResolver.Resolve(ctx, changelog.PackageRef{Name: "fixture-app", FullName: "fixture-app", Type: domain.PackageCask, Homepage: "https://offline.invalid/linked"}, "2.0")
	if err != nil {
		t.Fatal(err)
	}
	if gotLinked.SourceURL != linkedSection.SourceURL || gotLinked.Body != linkedSection.Body {
		t.Fatalf("cached linked section changed: got %#v, want %#v", gotLinked, linkedSection)
	}
	if requests == 0 {
		t.Fatal("initial changelog resolution made no fixture HTTP request")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

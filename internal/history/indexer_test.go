package history

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cicerone/internal/domain"
	"cicerone/internal/execx"
	"cicerone/internal/gitrepo"
	"cicerone/internal/store"
	"cicerone/internal/testutil"
)

func TestIndexerPersistsRangeAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formula("1"), "add foo", now.Add(-40*24*time.Hour))
	repo.Commit("Formula/foo.rb", formula("2"), "update foo", now.Add(-10*24*time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Kind: "formula", Path: repo.Path}
	runner := &countingRunner{Runner: execx.NewRunner()}
	indexer := NewIndexer(gitrepo.New(source, runner), s)
	req := Request{Since: now.Add(-30 * 24 * time.Hour), Kinds: map[domain.EventKind]bool{domain.EventVersion: true}}
	first, err := indexer.Index(ctx, source, req)
	if err != nil {
		t.Fatal(err)
	}
	runner.shows = 0
	second, err := indexer.Index(ctx, source, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Events != 1 || second.Events != 0 {
		t.Fatalf("results first=%#v second=%#v", first, second)
	}
	if runner.shows != 0 {
		t.Fatalf("idempotent rerun reparsed %d blobs", runner.shows)
	}
	newHead := repo.Commit("Formula/foo.rb", formula("3"), "new commit", now)
	third, err := indexer.Index(ctx, source, req)
	if err != nil {
		t.Fatal(err)
	}
	if third.Events != 1 || third.Head != newHead {
		t.Fatalf("new commit result=%#v want head %s", third, newHead)
	}
	state, ok, err := s.HistoryState(ctx, "core")
	if err != nil || !ok || state.Head != newHead || !state.Since.Equal(req.Since) {
		t.Fatalf("state=%#v ok=%v err=%v", state, ok, err)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{Now: now, Horizon: 365 * 24 * time.Hour})
	if err != nil || len(groups) != 2 {
		t.Fatalf("groups=%#v err=%v", groups, err)
	}
}

func TestIndexerExtendsRangeBackward(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formula("1"), "add", now.Add(-40*24*time.Hour))
	repo.Commit("Formula/foo.rb", formula("2"), "bump", now.Add(-10*24*time.Hour))
	s, _ := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Kind: "formula", Path: repo.Path}
	idx := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	if _, err := idx.Index(ctx, source, Request{Since: now.Add(-30 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	result, err := idx.Index(ctx, source, Request{Since: now.Add(-50 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 {
		t.Fatalf("result=%#v", result)
	}
}

func TestIndexerPersistsDiagnosticsWhenClassificationIsFiltered(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Casks/foo.rb", "cask \"foo\" do\n  version :unknown\nend\n", "unsupported", now.Add(-time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "cask", Kind: "cask", Path: repo.Path}
	idx := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	result, err := idx.Index(ctx, source, Request{Since: now.Add(-24 * time.Hour), Kinds: map[domain.EventKind]bool{domain.EventVersion: true}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 0 || result.Diagnostics == 0 {
		t.Fatalf("result=%#v", result)
	}
	diagnostics, err := s.HistoryDiagnostics(ctx, "cask")
	if err != nil || len(diagnostics) == 0 {
		t.Fatalf("diagnostics=%#v err=%v", diagnostics, err)
	}
}

func TestIndexerAddsOneOlderFallbackForEachInstalledKind(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formulaWith("1", "0", "https://one.test"), "version", now.Add(-90*24*time.Hour))
	repo.Commit("Formula/foo.rb", formulaWith("1", "1", "https://one.test"), "revision", now.Add(-80*24*time.Hour))
	repo.Commit("Formula/foo.rb", formulaWith("1", "1", "https://two.test"), "metadata", now.Add(-70*24*time.Hour))
	repo.Commit("Formula/bar.rb", strings.Replace(formula("1"), "class Foo", "class Bar", 1), "recent", now.Add(-time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	idx := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	result, err := idx.Index(ctx, source, Request{Since: now.Add(-30 * 24 * time.Hour), Installed: []domain.PackageID{"foo"}})
	if err != nil {
		t.Fatal(err)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{Now: now, Horizon: 365 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 4 {
		t.Fatalf("events=%d groups=%#v", result.Events, groups)
	}
	kinds := map[domain.EventKind]bool{}
	for _, g := range groups {
		for _, e := range g.Events {
			if e.PackageID == "foo" {
				kinds[e.Kind] = true
			}
		}
	}
	for _, kind := range []domain.EventKind{domain.EventVersion, domain.EventRevision, domain.EventMetadata} {
		if !kinds[kind] {
			t.Fatalf("missing %s fallback", kind)
		}
	}
}

func TestIndexerPersistsRenameAlias(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/old.rb", formula("1"), "add", now.Add(-time.Hour))
	if err := os.Rename(filepath.Join(repo.Path, "Formula/old.rb"), filepath.Join(repo.Path, "Formula/new.rb")); err != nil {
		t.Fatal(err)
	}
	repo.Run("-C", repo.Path, "add", "-A")
	repo.Run("-C", repo.Path, "commit", "-m", "rename")
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	idx := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	if _, err := idx.Index(ctx, source, Request{Since: now.Add(-24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveHistoryPackageID(ctx, "core", "old")
	if err != nil || got != "new" {
		t.Fatalf("alias resolved=%q err=%v", got, err)
	}
}

func formula(version string) string {
	return "class Foo < Formula\n  homepage \"https://example.test\"\n  url \"https://example.test/foo-" + version + ".tgz\"\n  version \"" + version + "\"\nend\n"
}
func formulaWith(version, revision, homepage string) string {
	return "class Foo < Formula\n  homepage \"" + homepage + "\"\n  url \"https://example.test/foo-" + version + ".tgz\"\n  version \"" + version + "\"\n  revision " + revision + "\nend\n"
}

type countingRunner struct {
	execx.Runner
	shows int
}

func (r *countingRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	for _, arg := range args {
		if arg == "show" {
			r.shows++
		}
	}
	return r.Runner.Run(ctx, name, args...)
}
func (r *countingRunner) Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	return r.Runner.Stream(ctx, name, args...)
}

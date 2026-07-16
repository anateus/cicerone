package history

import (
	"context"
	"path/filepath"
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
	indexer := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	req := Request{Since: now.Add(-30 * 24 * time.Hour), Kinds: map[domain.EventKind]bool{domain.EventVersion: true}}
	first, err := indexer.Index(ctx, source, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := indexer.Index(ctx, source, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Events != 1 || second.Events != 0 {
		t.Fatalf("results first=%#v second=%#v", first, second)
	}
	state, ok, err := s.HistoryState(ctx, "core")
	if err != nil || !ok || state.Head == "" || !state.Since.Equal(req.Since) {
		t.Fatalf("state=%#v ok=%v err=%v", state, ok, err)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{Now: now, Horizon: 365 * 24 * time.Hour})
	if err != nil || len(groups) != 1 {
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

func formula(version string) string {
	return "class Foo < Formula\n  homepage \"https://example.test\"\n  url \"https://example.test/foo-" + version + ".tgz\"\n  version \"" + version + "\"\nend\n"
}

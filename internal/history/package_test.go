package history

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/gitrepo"
	"github.com/anateus/cicerone/internal/store"
	"github.com/anateus/cicerone/internal/testutil"
)

func packageIndexer(t *testing.T, source gitrepo.Source) (*Indexer, *store.Store) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewIndexer(gitrepo.New(source, execx.NewRunner()), s), s
}

func TestIndexPackageNewestRealUpdateWithoutCursorOrCoverage(t *testing.T) {
	ctx := context.Background()
	repo := testutil.NewGitRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	repo.Commit("Formula/f/foo.rb", formula("1"), "initial", now.Add(-4*time.Hour))
	version := repo.Commit("Formula/f/foo.rb", formula("2"), "release", now.Add(-3*time.Hour))
	repo.Commit("Formula/f/foo.rb", formulaWith("2", "", "https://changed.test"), "metadata", now.Add(-2*time.Hour))
	repo.Commit("Formula/b/bar.rb", "version \"99\"\n", "unrelated", now.Add(-time.Hour))
	source := gitrepo.Source{Kind: "formula", Name: "homebrew-core", Path: repo.Path}
	indexer, s := packageIndexer(t, source)
	for iteration := 0; iteration < 2; iteration++ {
		found, err := indexer.IndexPackage(ctx, source, "homebrew/core/foo", "Formula/f/foo.rb")
		if err != nil || !found {
			t.Fatalf("lookup %d: found=%v err=%v", iteration, found, err)
		}
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{Now: now, Horizon: 24 * time.Hour})
	if err != nil || countEvents(groups) != 1 {
		t.Fatalf("feed=%#v err=%v", groups, err)
	}
	event := groups[0].Events[0]
	if event.Commit != version || event.PackageID != "homebrew/core/foo" || event.OldVersion != "1" || event.NewVersion != "2" || event.Kind != domain.EventVersion {
		t.Fatalf("indexed event=%#v", event)
	}
	if _, exists, err := s.HistoryState(ctx, source.Name); err != nil || exists {
		t.Fatalf("history state exists=%v err=%v", exists, err)
	}
	if covered, err := s.HasHistoryFallbackCoverage(ctx, source.Name, "homebrew/core/foo", domain.EventVersion); err != nil || covered {
		t.Fatalf("fallback coverage=%v err=%v", covered, err)
	}
}

func TestIndexPackageCaskRevisionAndAmbiguousHead(t *testing.T) {
	ctx := context.Background()
	repo := testutil.NewGitRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	base := "cask \"foo\" do\n  version computed_version\nend\n"
	repo.Commit("Casks/f/foo.rb", base, "initial", now.Add(-3*time.Hour))
	repo.Commit("Casks/f/foo.rb", "cask \"foo\" do\n  version \"1\"\nend\n", "literal version", now.Add(-150*time.Minute))
	revision := repo.Commit("Casks/f/foo.rb", "cask \"foo\" do\n  version \"1\"\n  revision 1\nend\n", "revision", now.Add(-2*time.Hour))
	repo.Commit("Casks/f/foo.rb", "cask \"foo\" do\n  version computed_version\nend\n", "computed", now.Add(-time.Hour))
	source := gitrepo.Source{Kind: "cask", Name: "homebrew-cask", Path: repo.Path}
	indexer, s := packageIndexer(t, source)
	found, err := indexer.IndexPackage(ctx, source, "foo", "Casks/f/foo.rb")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{Now: now, Horizon: 24 * time.Hour})
	if err != nil || countEvents(groups) != 1 || groups[0].Events[0].Commit != revision || groups[0].Events[0].Kind != domain.EventRevision {
		t.Fatalf("feed=%#v err=%v", groups, err)
	}
}

func TestIndexPackageFindsVersionBehindNewerRevision(t *testing.T) {
	ctx := context.Background()
	repo := testutil.NewGitRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	repo.Commit("Formula/foo.rb", formula("1"), "initial", now.Add(-4*time.Hour))
	version := repo.Commit("Formula/foo.rb", formula("2"), "release", now.Add(-3*time.Hour))
	repo.Commit("Formula/foo.rb", formulaWith("2", "1", "https://example.test"), "revision", now.Add(-2*time.Hour))
	source := gitrepo.Source{Kind: "formula", Name: "core", Path: repo.Path}
	indexer, s := packageIndexer(t, source)
	if found, err := indexer.IndexPackage(ctx, source, "foo", "Formula/foo.rb"); err != nil || !found {
		t.Fatalf("version lookup found=%v err=%v", found, err)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{Now: now, Horizon: 24 * time.Hour, Kinds: map[domain.EventKind]bool{domain.EventVersion: true}})
	if err != nil || countEvents(groups) != 1 || groups[0].Events[0].Commit != version || groups[0].Events[0].NewVersion != "2" {
		t.Fatalf("version-only feed=%#v err=%v", groups, err)
	}
}

func TestIndexPackageInitialAdditionAndMissingPath(t *testing.T) {
	ctx := context.Background()
	repo := testutil.NewGitRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	head := repo.Commit("Formula/foo.rb", formula("1"), "initial", now.Add(-time.Hour))
	source := gitrepo.Source{Kind: "formula", Name: "core", Path: repo.Path}
	indexer, s := packageIndexer(t, source)
	if found, err := indexer.IndexPackage(ctx, source, "other", "Formula/other.rb"); err != nil || found {
		t.Fatalf("missing path found=%v err=%v", found, err)
	}
	if found, err := indexer.IndexPackage(ctx, source, "foo", "Formula/foo.rb"); err != nil || !found {
		t.Fatalf("initial path found=%v err=%v", found, err)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{Now: now, Horizon: 24 * time.Hour})
	if err != nil || countEvents(groups) != 1 || groups[0].Events[0].Commit != head || groups[0].Events[0].OldVersion != "" {
		t.Fatalf("initial event=%#v err=%v", groups, err)
	}
}

func TestIndexPackageRejectsUnsafeOrMismatchedInputs(t *testing.T) {
	ctx := context.Background()
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formula("1"), "initial", time.Now().Add(-time.Hour))
	source := gitrepo.Source{Kind: "formula", Name: "core", Path: repo.Path}
	indexer, s := packageIndexer(t, source)
	for _, tc := range []struct {
		id     domain.PackageID
		path   string
		source gitrepo.Source
	}{
		{"bar", "Formula/foo.rb", source},
		{"foo", "Formula/../foo.rb", source},
		{"foo", "/Formula/foo.rb", source},
		{"foo", "Formula/:foo.rb", source},
		{"foo", "Casks/foo.rb", source},
		{"homebrew/cask/foo", "Formula/foo.rb", source},
		{"foo", "Formula/foo.rb", gitrepo.Source{Kind: "cask", Name: "core", Path: repo.Path}},
		{"foo", "Formula/foo.rb", gitrepo.Source{Kind: "formula", Name: "core", Path: t.TempDir()}},
	} {
		if found, err := indexer.IndexPackage(ctx, tc.source, tc.id, tc.path); found || err == nil {
			t.Errorf("id=%q path=%q source=%#v: found=%v err=%v", tc.id, tc.path, tc.source, found, err)
		}
	}
	if err := s.UpsertEvents(ctx, []domain.UpdateEvent{{ID: "existing", PackageID: "foo", Name: "foo", Type: domain.PackageCask, Kind: domain.EventVersion}}); err != nil {
		t.Fatal(err)
	}
	if found, err := indexer.IndexPackage(ctx, source, "foo", "Formula/foo.rb"); found || err == nil {
		t.Fatalf("stored type mismatch: found=%v err=%v", found, err)
	}
}

func TestIndexPackageSkipsRenameAndDoesNotBorrowOldIdentity(t *testing.T) {
	ctx := context.Background()
	repo := testutil.NewGitRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	repo.Commit("Formula/old.rb", formula("1"), "old package", now.Add(-2*time.Hour))
	if err := os.Rename(filepath.Join(repo.Path, "Formula/old.rb"), filepath.Join(repo.Path, "Formula/new.rb")); err != nil {
		t.Fatal(err)
	}
	repo.Run("-C", repo.Path, "add", "-A")
	repo.Run("-C", repo.Path, "commit", "-m", "rename")
	source := gitrepo.Source{Kind: "formula", Name: "core", Path: repo.Path}
	indexer, s := packageIndexer(t, source)
	found, err := indexer.IndexPackage(ctx, source, "new", "Formula/new.rb")
	if err != nil || found {
		t.Fatalf("rename found=%v err=%v", found, err)
	}
	if has, err := s.HasHistoryEvent(ctx, source.Name, "new", domain.EventVersion); err != nil || has {
		t.Fatalf("unexpected rename event: has=%v err=%v", has, err)
	}
}

func TestIndexPackageBoundedHistory(t *testing.T) {
	ctx := context.Background()
	repo := testutil.NewGitRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	repo.Commit("Formula/foo.rb", formula("1"), "initial", now.Add(-102*time.Minute))
	for n := 0; n < 100; n++ {
		repo.Commit("Formula/foo.rb", formula("1")+strings.Repeat("#", n+1)+"\n", "metadata", now.Add(time.Duration(n-100)*time.Minute))
	}
	source := gitrepo.Source{Kind: "formula", Name: "core", Path: repo.Path}
	indexer, _ := packageIndexer(t, source)
	found, err := indexer.IndexPackage(ctx, source, "foo", "Formula/foo.rb")
	if err != nil || found {
		t.Fatalf("bounded lookup found=%v err=%v", found, err)
	}
}

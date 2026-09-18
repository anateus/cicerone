package history

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func TestIndexerTreatsSyncBookkeepingRowAsUnindexed(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	head := repo.Commit("Formula/foo.rb", formula("1"), "add", now.Add(-time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SyncStarted(ctx, "core", now); err != nil {
		t.Fatal(err)
	}
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	result, err := NewIndexer(gitrepo.New(source, execx.NewRunner()), s).Index(ctx, source, Request{Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Head != head || result.Events != 1 {
		t.Fatalf("result=%#v", result)
	}
}

func TestIndexerPublishesBatchesBeforeCompletion(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	for version := 0; version < 101; version++ {
		repo.Commit("Formula/foo.rb", formula(fmt.Sprintf("%d", version)), fmt.Sprintf("version %d", version), now.Add(time.Duration(version-101)*time.Minute))
	}
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	firstBatch := make(chan Progress, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		firstBatchPublished := false
		_, indexErr := NewIndexer(gitrepo.New(source, execx.NewRunner()), s).Index(ctx, source, Request{Since: now.Add(-24 * time.Hour), Progress: func(progress Progress) {
			if progress.Batches == 1 && !firstBatchPublished {
				firstBatchPublished = true
				firstBatch <- progress
				<-release
			}
		}})
		done <- indexErr
	}()
	progress := <-firstBatch
	if progress.Commits != 10 || progress.Events != 10 {
		t.Fatalf("first progress=%#v", progress)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil || countEvents(groups) != 10 {
		t.Fatalf("visible events=%d err=%v", countEvents(groups), err)
	}
	if state, ok, err := s.HistoryState(ctx, "core"); err != nil || ok {
		t.Fatalf("partial state=%#v ok=%v err=%v", state, ok, err)
	}
	select {
	case err := <-done:
		t.Fatalf("index completed before release: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	groups, err = s.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil || countEvents(groups) != 101 {
		t.Fatalf("final events=%d err=%v", countEvents(groups), err)
	}
	if state, ok, err := s.HistoryState(ctx, "core"); err != nil || !ok || state.Head == "" {
		t.Fatalf("final state=%#v ok=%v err=%v", state, ok, err)
	}
}

func TestIndexerCancellationRetriesWithoutDuplicates(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	var commits []string
	for version := 0; version < 101; version++ {
		commits = append(commits, repo.Commit("Formula/foo.rb", formula(fmt.Sprintf("%d", version)), fmt.Sprintf("version %d", version), now.Add(time.Duration(version-101)*time.Minute)))
	}
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	runner := &countingRunner{Runner: execx.NewRunner()}
	indexer := NewIndexer(gitrepo.New(source, runner), s)
	ctx, cancel := context.WithCancel(context.Background())
	_, err = indexer.Index(ctx, source, Request{Since: now.Add(-24 * time.Hour), Progress: func(progress Progress) {
		if progress.Batches == 1 {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled index error=%v", err)
	}
	if state, ok, err := s.HistoryState(context.Background(), "core"); err != nil || ok {
		t.Fatalf("cancelled state=%#v ok=%v err=%v", state, ok, err)
	}
	groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{})
	if err != nil || countEvents(groups) != 10 {
		t.Fatalf("partial events=%d err=%v", countEvents(groups), err)
	}
	checkpointed := append([]string(nil), commits[len(commits)-10:]...)
	repo.Commit("Formula/foo.rb", formula("101"), "version 101", now)
	runner.revisions = nil
	var resumedProgress []Progress
	if _, err := indexer.Index(context.Background(), source, Request{Since: now.Add(-23 * time.Hour), Progress: func(progress Progress) {
		resumedProgress = append(resumedProgress, progress)
	}}); err != nil {
		t.Fatal(err)
	}
	foundStartupBatch := false
	for _, progress := range resumedProgress {
		if progress.Commits == len(checkpointed)+historyInitialBatchCommits && progress.Batches == 1 {
			foundStartupBatch = true
		}
	}
	if !foundStartupBatch {
		t.Fatalf("resume progress = %#v, want the next 10 commits durable as the startup batch", resumedProgress)
	}
	processed := map[string]bool{}
	for _, commit := range checkpointed {
		processed[commit] = true
	}
	for _, revision := range runner.revisions {
		commit := strings.TrimSuffix(strings.SplitN(revision, ":", 2)[0], "^")
		if processed[commit] {
			t.Fatalf("retry reopened blob from checkpointed commit %s", revision)
		}
	}
	groups, err = s.QueryFeed(context.Background(), domain.FeedFilter{})
	if err != nil || countEvents(groups) != 102 {
		t.Fatalf("retried events=%d err=%v", countEvents(groups), err)
	}
	if state, ok, err := s.HistoryState(context.Background(), "core"); err != nil || !ok || state.Head == "" {
		t.Fatalf("retried state=%#v ok=%v err=%v", state, ok, err)
	}
}

func TestIndexerCancellationCheckpointsPartialBatch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	for version := 0; version < 25; version++ {
		repo.Commit("Formula/foo.rb", formula(fmt.Sprintf("%d", version)), fmt.Sprintf("version %d", version), now.Add(time.Duration(version-25)*time.Minute))
	}
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	ctx, cancel := context.WithCancel(context.Background())
	request := Request{Since: now.Add(-time.Hour), Progress: func(progress Progress) {
		if progress.Commits == 20 {
			cancel()
		}
	}}
	_, err = NewIndexer(gitrepo.New(source, execx.NewRunner()), s).Index(ctx, source, request)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Index error=%v, want context cancellation", err)
	}
	progress, err := s.HistoryScanProgress(context.Background(), "core", historyScanKey("initial", time.Time{}, time.Time{}, request, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 20 {
		t.Fatalf("checkpointed commits=%d, want first batch plus 10-commit partial batch", len(progress))
	}
}

func TestIndexerResumeDropsCheckpointedCommitsRewrittenWhileInterrupted(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	var commits []string
	for version := 0; version < 12; version++ {
		commits = append(commits, repo.Commit("Formula/foo.rb", formula(fmt.Sprintf("%d", version)), fmt.Sprintf("version %d", version), now.Add(time.Duration(version-12)*time.Minute)))
	}
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	indexer := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	ctx, cancel := context.WithCancel(context.Background())
	_, err = indexer.Index(ctx, source, Request{Since: now.Add(-time.Hour), Progress: func(progress Progress) {
		if progress.Batches == 1 {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled index error=%v", err)
	}

	repo.Run("-C", repo.Path, "reset", "--hard", commits[0])
	replacement := repo.Commit("Formula/foo.rb", formula("replacement"), "replacement", now)
	result, err := indexer.Index(context.Background(), source, Request{Since: now.Add(-59 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Head != replacement {
		t.Fatalf("resumed head=%s, want %s", result.Head, replacement)
	}
	groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{})
	if err != nil || countEvents(groups) != 2 {
		t.Fatalf("events after interrupted rewrite=%d err=%v", countEvents(groups), err)
	}
}

func countEvents(groups []domain.FeedGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.Events)
	}
	return total
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

func TestIndexerDoesNotRepeatExhaustedInstalledFallback(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formula("1"), "add", now.Add(-40*24*time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	runner := &countingRunner{Runner: execx.NewRunner()}
	idx := NewIndexer(gitrepo.New(source, runner), s)

	request := Request{Since: now.Add(-30 * 24 * time.Hour), Installed: []domain.PackageID{"never-existed"}}
	if _, err := idx.Index(ctx, source, request); err != nil {
		t.Fatal(err)
	}
	if runner.shows == 0 {
		t.Fatal("first fallback did not inspect repository history")
	}
	runner.shows = 0
	if _, err := idx.Index(ctx, source, request); err != nil {
		t.Fatal(err)
	}
	if runner.shows != 0 {
		t.Fatalf("second fallback reopened %d historical blobs after exhaustive coverage", runner.shows)
	}
}

func TestIndexerSkipsCustomTapPackagesDuringCoreFallback(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formula("1"), "add", now.Add(-40*24*time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	runner := &countingRunner{Runner: execx.NewRunner()}
	idx := NewIndexer(gitrepo.New(source, runner), s)

	if _, err := idx.Index(ctx, source, Request{Since: now.Add(-30 * 24 * time.Hour), Installed: []domain.PackageID{"owner/tap/private"}}); err != nil {
		t.Fatal(err)
	}
	if runner.shows != 0 {
		t.Fatalf("custom-tap fallback inspected %d core repository blobs", runner.shows)
	}
}

func TestIndexerSkipsPackagesFromAnotherRepositoryType(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formula("1"), "add", now.Add(-40*24*time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetInstalled(ctx, []domain.InstalledPackage{{PackageID: "cask-only", Name: "cask-only", Type: domain.PackageCask}}); err != nil {
		t.Fatal(err)
	}
	source := gitrepo.Source{Name: "core", Kind: string(domain.PackageFormula), Path: repo.Path}
	runner := &countingRunner{Runner: execx.NewRunner()}
	idx := NewIndexer(gitrepo.New(source, runner), s)

	if _, err := idx.Index(ctx, source, Request{Since: now.Add(-30 * 24 * time.Hour), Installed: []domain.PackageID{"cask-only"}}); err != nil {
		t.Fatal(err)
	}
	if runner.shows != 0 {
		t.Fatalf("cask fallback inspected %d core repository blobs", runner.shows)
	}
}

func TestIndexerPersistsRecentCursorBeforeInstalledFallbackCompletes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	old := repo.Commit("Formula/foo.rb", formula("1"), "old installed formula", now.Add(-90*24*time.Hour))
	head := repo.Commit("Formula/bar.rb", strings.Replace(formula("1"), "class Foo", "class Bar", 1), "recent formula", now.Add(-time.Hour))
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	fallbackKey := historyScanKey("fallback", time.Time{}, time.Time{}, Request{Installed: []domain.PackageID{"foo"}}, true)
	if err := s.ApplyHistoryBatch(context.Background(), store.HistoryBatch{
		Repository: "core", ScanKey: fallbackKey,
		Processed: []store.HistoryProgress{{Commit: "previously-checkpointed"}},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &cancelOnCommitRunner{Runner: execx.NewRunner(), commit: old, cancel: cancel}
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	_, err = NewIndexer(gitrepo.New(source, runner), s).Index(ctx, source, Request{
		Since: now.Add(-30 * 24 * time.Hour), Installed: []domain.PackageID{"foo"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Index error = %v, want cancellation during installed fallback", err)
	}
	state, ok, err := s.HistoryState(context.Background(), "core")
	if err != nil || !ok || state.Head != head {
		t.Fatalf("recent state=%#v ok=%v err=%v, want head %s", state, ok, err, head)
	}
	groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{Now: now, Horizon: 30 * 24 * time.Hour})
	if err != nil || countEvents(groups) != 1 {
		t.Fatalf("recent events=%d err=%v, want quick update persisted", countEvents(groups), err)
	}
	progress, err := s.HistoryScanProgress(context.Background(), "core", fallbackKey)
	if err != nil || len(progress) != 1 || progress[0].Commit != "previously-checkpointed" {
		t.Fatalf("fallback progress=%#v err=%v, want earlier checkpoint preserved", progress, err)
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

func TestIndexerReconcilesRewrittenHistoryWithoutTouchingUnaffectedEvents(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	base := repo.Commit("Formula/foo.rb", formula("1"), "base", now.Add(-3*time.Hour))
	abandoned := repo.Commit("Formula/foo.rb", formula("2"), "abandoned", now.Add(-2*time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	idx := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	req := Request{Since: now.Add(-24 * time.Hour)}
	if _, err := idx.Index(ctx, source, req); err != nil {
		t.Fatal(err)
	}
	other := domain.UpdateEvent{ID: "other:e", PackageID: "other", Name: "other", Type: domain.PackageFormula, Kind: domain.EventVersion, Repository: "other", Commit: "same-name", Time: now.Add(-time.Hour)}
	if err := s.ApplyHistory(ctx, store.HistoryBatch{Repository: "other", Head: "same-name", Events: []domain.UpdateEvent{other}}); err != nil {
		t.Fatal(err)
	}
	repo.Run("-C", repo.Path, "reset", "--hard", base)
	replacement := repo.Commit("Formula/foo.rb", formula("3"), "replacement", now.Add(-time.Hour))
	result, err := idx.Index(ctx, source, req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 || result.Head != replacement {
		t.Fatalf("result=%#v", result)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{Now: now, Horizon: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	commits := map[string]bool{}
	for _, g := range groups {
		for _, event := range g.Events {
			commits[event.Repository+":"+event.Commit] = true
		}
	}
	if !commits["core:"+base] || !commits["core:"+replacement] || commits["core:"+abandoned] || !commits["other:same-name"] {
		t.Fatalf("reconciled commits=%v", commits)
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
	shows     int
	revisions []string
}

type cancelOnCommitRunner struct {
	execx.Runner
	commit string
	cancel context.CancelFunc
}

func (r *cancelOnCommitRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	for _, arg := range args {
		if strings.HasPrefix(arg, r.commit) {
			r.cancel()
			break
		}
	}
	return r.Runner.Run(ctx, name, args...)
}

func (r *countingRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	for index, arg := range args {
		if arg == "show" {
			r.shows++
			if index+1 < len(args) {
				r.revisions = append(r.revisions, args[index+1])
			}
		}
	}
	return r.Runner.Run(ctx, name, args...)
}
func (r *countingRunner) Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	return r.Runner.Stream(ctx, name, args...)
}

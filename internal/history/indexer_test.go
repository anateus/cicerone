package history

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	if progress.Commits != historyInitialBatchCommits || progress.Events != historyInitialBatchCommits {
		t.Fatalf("first progress=%#v", progress)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil || countEvents(groups) != historyInitialBatchCommits {
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
	if err != nil || countEvents(groups) != historyInitialBatchCommits {
		t.Fatalf("partial events=%d err=%v", countEvents(groups), err)
	}
	checkpointed := append([]string(nil), commits[len(commits)-historyInitialBatchCommits:]...)
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
		t.Fatalf("resume progress = %#v, want the next %d commits durable as the startup batch", resumedProgress, historyInitialBatchCommits)
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

func TestIndexerResumesFromFirstParentFrontier(t *testing.T) {
	for _, advanceHead := range []bool{false, true} {
		t.Run(fmt.Sprintf("advance-head=%v", advanceHead), func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			repo := testutil.NewGitRepo(t)
			var commits []string
			for version := range 30 {
				commits = append(commits, repo.Commit("Formula/foo.rb", formula(fmt.Sprintf("%d", version)), fmt.Sprintf("version %d", version), now.Add(time.Duration(version-30)*time.Minute)))
			}
			s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			source := gitrepo.Source{Name: "core", Path: repo.Path}
			runner := &countingRunner{Runner: execx.NewRunner()}
			indexer := NewIndexer(gitrepo.New(source, runner), s)
			since := now.Add(-24 * time.Hour)
			ctx, cancel := context.WithCancel(context.Background())
			_, err = indexer.Index(ctx, source, Request{Since: since, Progress: func(progress Progress) {
				if progress.Commits == 21 {
					cancel()
				}
			}})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("first index error=%v", err)
			}
			oldHead := commits[len(commits)-1]
			newHead := oldHead
			if advanceHead {
				newHead = repo.Commit("Formula/foo.rb", formula("30"), "version 30", now)
			}
			runner.logs = nil
			if _, err := indexer.Index(context.Background(), source, Request{Since: since}); err != nil {
				t.Fatal(err)
			}
			revisions := historyLogRevisions(runner.logs)
			want := []string{commits[len(commits)-22]}
			if advanceHead {
				want = append([]string{oldHead + ".." + newHead}, want...)
			}
			if fmt.Sprint(revisions) != fmt.Sprint(want) {
				t.Fatalf("history log revisions=%v, want only prefix and unprocessed tail %v", revisions, want)
			}
			groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{})
			wantEvents := len(commits)
			if advanceHead {
				wantEvents++
			}
			if err != nil || countEvents(groups) != wantEvents {
				t.Fatalf("events=%d err=%v, want %d", countEvents(groups), err, wantEvents)
			}
		})
	}
}

func TestIndexerRecoversWhenInterruptedScanHeadWasPruned(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	base := repo.Commit("Formula/foo.rb", formula("1"), "base", now.Add(-2*time.Minute))
	abandoned := repo.Commit("Formula/foo.rb", formula("2"), "abandoned", now.Add(-time.Minute))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	indexer := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	since := now.Add(-time.Hour)
	cancelCtx, cancel := context.WithCancel(ctx)
	request := Request{Since: since, Progress: func(progress Progress) {
		if progress.Batches == 1 {
			cancel()
		}
	}}
	if _, err := indexer.Index(cancelCtx, source, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("initial scan error=%v, want cancellation", err)
	}
	if state, ok, err := s.HistoryState(ctx, source.Name); err != nil || ok {
		t.Fatalf("interrupted state=%#v ok=%v err=%v", state, ok, err)
	}
	key := historyScanKey("initial", time.Time{}, time.Time{}, request, false)
	cursor, ok, err := s.HistoryScanCursor(ctx, source.Name, key)
	if err != nil || !ok || cursor.Head != abandoned || cursor.Frontier != abandoned {
		t.Fatalf("interrupted cursor=%#v ok=%v err=%v", cursor, ok, err)
	}

	repo.Run("-C", repo.Path, "reset", "--hard", base)
	replacement := repo.Commit("Formula/foo.rb", formula("3"), "replacement", now)
	lookupFailure := errors.New("git object database unavailable")
	failing := NewIndexer(gitrepo.New(source, &failingSavedHeadRunner{Runner: execx.NewRunner(), err: lookupFailure}), s)
	if _, err := failing.Index(ctx, source, Request{Since: since}); !errors.Is(err, lookupFailure) {
		t.Fatalf("saved-head lookup error=%v, want %v", err, lookupFailure)
	}
	cursor, ok, err = s.HistoryScanCursor(ctx, source.Name, key)
	if err != nil || !ok || cursor.Head != abandoned || cursor.Frontier != abandoned {
		t.Fatalf("cursor after lookup failure=%#v ok=%v err=%v", cursor, ok, err)
	}
	if progress, err := s.HistoryScanProgress(ctx, source.Name, key); err != nil || len(progress) != 1 || progress[0].Commit != abandoned {
		t.Fatalf("progress after lookup failure=%#v err=%v", progress, err)
	}
	repo.Run("-C", repo.Path, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all")
	repo.Run("-C", repo.Path, "prune", "--expire=now")
	result, lookupErr := execx.NewRunner().Run(ctx, "git", "-C", repo.Path, "cat-file", "-e", abandoned)
	var exitErr *exec.ExitError
	if !errors.As(lookupErr, &exitErr) || exitErr.ExitCode() != 1 || len(result.Stderr) != 0 {
		t.Fatalf("saved commit was not pruned: result=%#v err=%v", result, lookupErr)
	}
	if _, err := indexer.Index(ctx, source, Request{Since: since}); err != nil {
		t.Fatalf("resume after pruning: %v", err)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	commits := map[string]bool{}
	for _, group := range groups {
		for _, event := range group.Events {
			commits[event.Commit] = true
		}
	}
	if !commits[base] || !commits[replacement] || commits[abandoned] || len(commits) != 2 {
		t.Fatalf("events after pruned-head recovery=%v", commits)
	}
	if state, ok, err := s.HistoryState(ctx, source.Name); err != nil || !ok || state.Head != replacement {
		t.Fatalf("recovered state=%#v ok=%v err=%v", state, ok, err)
	}
	if progress, err := s.HistoryScanProgress(ctx, source.Name, key); err != nil || len(progress) != 0 {
		t.Fatalf("final progress=%#v err=%v", progress, err)
	}
}

func TestIndexerResumeHandlesRootFrontierAndChangedBounds(t *testing.T) {
	for _, changeBounds := range []bool{false, true} {
		t.Run(fmt.Sprintf("change-bounds=%v", changeBounds), func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			repo := testutil.NewGitRepo(t)
			root := repo.Commit("Formula/foo.rb", formula("1"), "root", now)
			s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			source := gitrepo.Source{Name: "core", Path: repo.Path}
			runner := &countingRunner{Runner: execx.NewRunner()}
			indexer := NewIndexer(gitrepo.New(source, runner), s)
			since := now.Add(-time.Hour)
			ctx, cancel := context.WithCancel(context.Background())
			if _, err := indexer.Index(ctx, source, Request{Since: since, Progress: func(progress Progress) {
				if progress.Batches == 1 {
					cancel()
				}
			}}); !errors.Is(err, context.Canceled) {
				t.Fatalf("first cancellation=%v", err)
			}
			if changeBounds {
				since = now.Add(time.Hour)
			}
			runner.logs = nil
			if _, err := indexer.Index(context.Background(), source, Request{Since: since}); err != nil {
				t.Fatal(err)
			}
			revisions := historyLogRevisions(runner.logs)
			if changeBounds && (len(revisions) != 1 || revisions[0] != root) {
				t.Fatalf("changed bounds did not rescan history: %v", revisions)
			}
			if !changeBounds && len(revisions) != 0 {
				t.Fatalf("root frontier unexpectedly rescanned history: %v", revisions)
			}
			groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{})
			want := 1
			if changeBounds {
				want = 0
			}
			if err != nil || countEvents(groups) != want {
				t.Fatalf("events=%d err=%v, want %d", countEvents(groups), err, want)
			}
		})
	}
}

func TestIndexerDoesNotSkipPastCutoffOnAdvancedHead(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formula("1"), "checkpoint", now.Add(-time.Minute))
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	indexer := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	since := now.Add(-time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := indexer.Index(ctx, source, Request{Since: since, Progress: func(progress Progress) {
		if progress.Batches == 1 {
			cancel()
		}
	}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("first cancellation=%v", err)
	}
	repo.Commit("Formula/foo.rb", formula("2"), "old-dated new head", now.Add(-2*time.Hour))
	if _, err := indexer.Index(context.Background(), source, Request{Since: since}); err != nil {
		t.Fatal(err)
	}
	groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{})
	if err != nil || countEvents(groups) != 0 {
		t.Fatalf("events after traversal cutoff=%d err=%v", countEvents(groups), err)
	}
}

func TestIndexerInterruptedAdvancedPrefixStillReconcilesRewrite(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	base := repo.Commit("Formula/foo.rb", formula("1"), "base", now.Add(-3*time.Minute))
	oldHead := repo.Commit("Formula/foo.rb", formula("2"), "old head", now.Add(-2*time.Minute))
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	indexer := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	since := now.Add(-time.Hour)
	cancelAfterFirstBatch := func() (context.Context, Request) {
		ctx, cancel := context.WithCancel(context.Background())
		return ctx, Request{Since: since, Progress: func(progress Progress) {
			if progress.Batches == 1 {
				cancel()
			}
		}}
	}
	ctx, request := cancelAfterFirstBatch()
	if _, err := indexer.Index(ctx, source, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("first cancellation=%v", err)
	}
	repo.Commit("Formula/foo.rb", formula("3"), "new prefix 1", now.Add(-time.Minute))
	abandoned := repo.Commit("Formula/foo.rb", formula("4"), "new prefix 2", now)
	ctx, request = cancelAfterFirstBatch()
	if _, err := indexer.Index(ctx, source, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("prefix cancellation=%v", err)
	}
	repo.Run("-C", repo.Path, "reset", "--hard", oldHead)
	replacement := repo.Commit("Formula/foo.rb", formula("5"), "replacement", now.Add(time.Minute))
	if _, err := indexer.Index(context.Background(), source, Request{Since: since}); err != nil {
		t.Fatal(err)
	}
	groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, group := range groups {
		for _, event := range group.Events {
			got[event.Commit] = true
		}
	}
	if !got[base] || !got[oldHead] || !got[replacement] || got[abandoned] || len(got) != 3 {
		t.Fatalf("commits after interrupted prefix rewrite=%v", got)
	}
}

func TestIndexerKeepsRewriteCheckpointsWhileFallbackIsInterrupted(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	base := repo.Commit("Formula/foo.rb", formula("1"), "old base", now.Add(-2*time.Hour))
	abandoned := repo.Commit("Formula/foo.rb", formula("2"), "abandoned", now.Add(-time.Minute))
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	since := now.Add(-time.Hour)
	kinds := map[domain.EventKind]bool{domain.EventVersion: true}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := NewIndexer(gitrepo.New(source, execx.NewRunner()), s).Index(ctx, source, Request{Since: since, Kinds: kinds, Progress: func(progress Progress) {
		if progress.Batches == 1 {
			cancel()
		}
	}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("initial cancellation=%v", err)
	}
	repo.Run("-C", repo.Path, "reset", "--hard", base)
	replacement := repo.Commit("Formula/foo.rb", formula("3"), "replacement", now)
	ctx, cancel = context.WithCancel(context.Background())
	runner := &cancelOnCommitRunner{Runner: execx.NewRunner(), commit: base, cancel: cancel}
	request := Request{Since: since, Kinds: kinds, Installed: []domain.PackageID{"never-existed"}}
	if _, err := NewIndexer(gitrepo.New(source, runner), s).Index(ctx, source, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("fallback cancellation=%v", err)
	}
	if state, ok, err := s.HistoryState(context.Background(), "core"); err != nil || ok {
		t.Fatalf("state before reconciliation=%#v ok=%v err=%v", state, ok, err)
	}
	if _, err := NewIndexer(gitrepo.New(source, execx.NewRunner()), s).Index(context.Background(), source, request); err != nil {
		t.Fatal(err)
	}
	groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, group := range groups {
		for _, event := range group.Events {
			got[event.Commit] = true
		}
	}
	if got[abandoned] || !got[replacement] || len(got) != 1 {
		t.Fatalf("events after interrupted rewrite reconciliation=%v", got)
	}
}

func TestIndexerDoesNotTreatSecondParentAsResumableMainline(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	base := repo.Commit("Formula/foo.rb", formula("1"), "base", now.Add(-2*time.Minute))
	abandoned := repo.Commit("Formula/foo.rb", formula("2"), "side branch", now.Add(-time.Minute))
	repo.Run("-C", repo.Path, "branch", "side")
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	indexer := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	since := now.Add(-time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := indexer.Index(ctx, source, Request{Since: since, Progress: func(progress Progress) {
		if progress.Batches == 1 {
			cancel()
		}
	}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("first cancellation=%v", err)
	}
	repo.Run("-C", repo.Path, "reset", "--hard", base)
	replacement := repo.Commit("Formula/foo.rb", formula("3"), "mainline", now)
	repo.Run("-C", repo.Path, "merge", "--no-ff", "-s", "ours", "side", "-m", "merge side")
	if _, err := indexer.Index(context.Background(), source, Request{Since: since}); err != nil {
		t.Fatal(err)
	}
	groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, group := range groups {
		for _, event := range group.Events {
			got[event.Commit] = true
		}
	}
	if !got[base] || !got[replacement] || got[abandoned] {
		t.Fatalf("mainline events=%v", got)
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
		if progress.Commits == 21 {
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
	if len(progress) != 21 {
		t.Fatalf("checkpointed commits=%d, want first batch plus a partial batch", len(progress))
	}
}

func TestIndexerCancellationDoesNotPublishPartialCommit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formula("1"), "base", now.Add(-2*time.Minute))
	if err := os.WriteFile(filepath.Join(repo.Path, "Formula/foo.rb"), []byte(formula("2")), 0o644); err != nil {
		t.Fatal(err)
	}
	bar := strings.Replace(formula("1"), "class Foo", "class Bar", 1)
	if err := os.WriteFile(filepath.Join(repo.Path, "Formula/bar.rb"), []byte(bar), 0o644); err != nil {
		t.Fatal(err)
	}
	repo.Run("-C", repo.Path, "add", "-A")
	repo.Run("-C", repo.Path, "commit", "-m", "two formulae")
	middle := strings.TrimSpace(repo.Run("-C", repo.Path, "rev-parse", "HEAD"))
	newest := repo.Commit("Formula/foo.rb", formula("3"), "newest", now.Add(time.Minute))
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &cancelOnCommitRunner{Runner: execx.NewRunner(), commit: middle, path: "Formula/foo.rb", cancel: cancel}
	since := now.Add(-time.Hour)
	_, err = NewIndexer(gitrepo.New(source, runner), s).Index(ctx, source, Request{Since: since})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Index error=%v, want cancellation", err)
	}
	groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{})
	if err != nil || countEvents(groups) != 1 || groups[0].Events[0].Commit != newest {
		t.Fatalf("events after partial commit=%#v err=%v", groups, err)
	}
	key := historyScanKey("initial", time.Time{}, time.Time{}, Request{Since: since}, false)
	checkpoint, err := s.HistoryScanProgress(context.Background(), "core", key)
	if err != nil || len(checkpoint) != 1 || checkpoint[0].Commit != newest {
		t.Fatalf("checkpoint=%#v err=%v", checkpoint, err)
	}
	if _, err := NewIndexer(gitrepo.New(source, execx.NewRunner()), s).Index(context.Background(), source, Request{Since: since}); err != nil {
		t.Fatal(err)
	}
	groups, err = s.QueryFeed(context.Background(), domain.FeedFilter{})
	if err != nil || countEvents(groups) != 4 {
		t.Fatalf("events after retry=%d err=%v, want 4", countEvents(groups), err)
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

func historyLogRevisions(logs [][]string) []string {
	var revisions []string
	for _, args := range logs {
		for _, arg := range args {
			if arg == "--name-status" && len(args) >= 2 {
				revisions = append(revisions, args[len(args)-2])
				break
			}
		}
	}
	return revisions
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

func TestIndexerSkipsFallbackSatisfiedByRecentScan(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formula("1"), "old", now.Add(-60*24*time.Hour))
	head := repo.Commit("Formula/foo.rb", formula("2"), "recent", now.Add(-time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	runner := &countingRunner{Runner: execx.NewRunner()}
	result, err := NewIndexer(gitrepo.New(source, runner), s).Index(ctx, source, Request{
		Since: now.Add(-30 * 24 * time.Hour), Installed: []domain.PackageID{"foo"},
		Kinds: map[domain.EventKind]bool{domain.EventVersion: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 1 {
		t.Fatalf("events=%d, want the recent version", result.Events)
	}
	if revisions := historyLogRevisions(runner.logs); len(revisions) != 1 || revisions[0] != head {
		t.Fatalf("history log revisions=%v, want only recent scan %s", revisions, head)
	}
}

func TestIndexerStillReconcilesSatisfiedFallbackCheckpoints(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	repo.Commit("Formula/foo.rb", formula("1"), "old", now.Add(-60*24*time.Hour))
	head := repo.Commit("Formula/foo.rb", formula("2"), "recent", now.Add(-time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	request := Request{Since: now.Add(-30 * 24 * time.Hour), Installed: []domain.PackageID{"foo"}, Kinds: map[domain.EventKind]bool{domain.EventVersion: true}}
	key := historyScanKey("fallback", time.Time{}, time.Time{}, request, true)
	stale := domain.UpdateEvent{ID: "stale", PackageID: "bar", Name: "bar", Type: domain.PackageFormula, Kind: domain.EventVersion, Repository: "core", Commit: "unreachable", Time: now}
	if err := s.ApplyHistoryBatch(ctx, store.HistoryBatch{Repository: "core", ScanKey: key, Events: []domain.UpdateEvent{stale}, Processed: []store.HistoryProgress{{Commit: stale.Commit, Events: 1}}}); err != nil {
		t.Fatal(err)
	}
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	runner := &countingRunner{Runner: execx.NewRunner()}
	if _, err := NewIndexer(gitrepo.New(source, runner), s).Index(ctx, source, request); err != nil {
		t.Fatal(err)
	}
	if revisions := historyLogRevisions(runner.logs); len(revisions) != 2 || revisions[0] != head || revisions[1] != head {
		t.Fatalf("history log revisions=%v, want recent and checkpoint reconciliation scans", revisions)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil || countEvents(groups) != 1 || groups[0].Events[0].Commit != head {
		t.Fatalf("events after checkpoint reconciliation=%#v err=%v", groups, err)
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

func TestIndexerResumesYieldedFallbackAfterNormalRefresh(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	var oldCommits []string
	for version := range 4 {
		oldCommits = append(oldCommits, repo.Commit("Formula/foo.rb", formula(fmt.Sprintf("%d", version)), fmt.Sprintf("old %d", version), now.Add(-60*24*time.Hour+time.Duration(version)*time.Minute)))
	}
	bar := strings.Replace(formula("1"), "class Foo", "class Bar", 1)
	oldHead := repo.Commit("Formula/bar.rb", bar, "recent", now.Add(-time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	runner := &countingRunner{Runner: execx.NewRunner()}
	indexer := NewIndexer(gitrepo.New(source, runner), s)
	if _, err := indexer.Index(ctx, source, Request{Since: now.Add(-30 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fallback := Request{Installed: []domain.PackageID{"never-existed"}}
	key := historyScanKey("fallback", time.Time{}, time.Time{}, fallback, true)
	fallbackCtx, cancel := context.WithCancel(ctx)
	fallback.Progress = func(progress Progress) {
		if progress.Batches == 1 {
			cancel()
		}
	}
	if _, err := indexer.Index(fallbackCtx, source, fallback); !errors.Is(err, context.Canceled) {
		t.Fatalf("yielded fallback error=%v, want cancellation", err)
	}
	before, ok, err := s.HistoryScanCursor(ctx, source.Name, key)
	if err != nil || !ok || before.Head != oldHead || before.Frontier != oldCommits[3] {
		t.Fatalf("yielded cursor=%#v ok=%v err=%v", before, ok, err)
	}
	progress, err := s.HistoryScanProgress(ctx, source.Name, key)
	if err != nil || len(progress) != 1 || progress[0].Commit != oldCommits[3] {
		t.Fatalf("yielded progress=%#v err=%v", progress, err)
	}

	newBar := strings.Replace(formula("2"), "class Foo", "class Bar", 1)
	newHead := repo.Commit("Formula/bar.rb", newBar, "refresh", now)
	if _, err := indexer.Index(ctx, source, Request{}); err != nil {
		t.Fatalf("normal refresh: %v", err)
	}
	after, ok, err := s.HistoryScanCursor(ctx, source.Name, key)
	if err != nil || !ok || after.Head != before.Head || after.Frontier != before.Frontier {
		t.Fatalf("fallback cursor after refresh=%#v ok=%v err=%v", after, ok, err)
	}
	progress, err = s.HistoryScanProgress(ctx, source.Name, key)
	if err != nil || len(progress) != 1 || progress[0].Commit != oldCommits[3] {
		t.Fatalf("fallback progress after refresh=%#v err=%v", progress, err)
	}
	fallback.Progress = nil
	runner.logs = nil
	if _, err := indexer.Index(ctx, source, fallback); err != nil {
		t.Fatalf("resumed fallback: %v", err)
	}
	revisions := historyLogRevisions(runner.logs)
	want := []string{oldHead + ".." + newHead, oldCommits[2]}
	if fmt.Sprint(revisions) != fmt.Sprint(want) {
		t.Fatalf("resumed fallback revisions=%v, want prefix and unprocessed tail %v", revisions, want)
	}
	if progress, err := s.HistoryScanProgress(ctx, source.Name, key); err != nil || len(progress) != 0 {
		t.Fatalf("completed fallback progress=%#v err=%v", progress, err)
	}
	if _, ok, err := s.HistoryScanCursor(ctx, source.Name, key); err != nil || ok {
		t.Fatalf("completed fallback cursor ok=%v err=%v", ok, err)
	}
	for _, kind := range []domain.EventKind{domain.EventVersion, domain.EventRevision, domain.EventMetadata} {
		if covered, err := s.HasHistoryFallbackCoverage(ctx, source.Name, "never-existed", kind); err != nil || !covered {
			t.Fatalf("fallback %s coverage=%v err=%v", kind, covered, err)
		}
	}
}

func TestIndexerReconcilesYieldedFallbackAcrossRewrite(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	base := repo.Commit("Formula/foo.rb", formula("1"), "base", now.Add(-60*24*time.Hour))
	abandoned := repo.Commit("Formula/foo.rb", formula("2"), "abandoned", now.Add(-59*24*time.Hour))
	bar := strings.Replace(formula("1"), "class Foo", "class Bar", 1)
	oldHead := repo.Commit("Formula/bar.rb", bar, "recent", now.Add(-time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	indexer := NewIndexer(gitrepo.New(source, execx.NewRunner()), s)
	if _, err := indexer.Index(ctx, source, Request{Since: now.Add(-30 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fallback := Request{Installed: []domain.PackageID{"foo"}, Kinds: map[domain.EventKind]bool{domain.EventVersion: true}}
	key := historyScanKey("fallback", time.Time{}, time.Time{}, fallback, true)
	fallbackCtx, cancel := context.WithCancel(ctx)
	fallback.Progress = func(progress Progress) {
		if progress.Batches == 1 {
			cancel()
		}
	}
	if _, err := indexer.Index(fallbackCtx, source, fallback); !errors.Is(err, context.Canceled) {
		t.Fatalf("yielded fallback error=%v", err)
	}
	if cursor, ok, err := s.HistoryScanCursor(ctx, source.Name, key); err != nil || !ok || cursor.Frontier != abandoned {
		t.Fatalf("yielded cursor=%#v ok=%v err=%v", cursor, ok, err)
	}
	repo.Run("-C", repo.Path, "reset", "--hard", base)
	newHead := repo.Commit("Formula/bar.rb", bar, "replacement", now)
	if _, err := indexer.Index(ctx, source, Request{}); err != nil {
		t.Fatalf("rewritten refresh: %v", err)
	}
	if _, ok, err := s.HistoryScanCursor(ctx, source.Name, key); err != nil || ok {
		t.Fatalf("rewritten fallback cursor survived: ok=%v err=%v", ok, err)
	}
	if progress, err := s.HistoryScanProgress(ctx, source.Name, key); err != nil || len(progress) != 0 {
		t.Fatalf("rewritten fallback progress=%#v err=%v", progress, err)
	}
	fallback.Progress = nil
	if _, err := indexer.Index(ctx, source, fallback); err != nil {
		t.Fatalf("fallback after rewrite: %v", err)
	}
	groups, err := s.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	commits := map[string]bool{}
	for _, group := range groups {
		for _, event := range group.Events {
			commits[event.Commit] = true
		}
	}
	if !commits[base] || !commits[newHead] || commits[oldHead] || commits[abandoned] || len(commits) != 2 {
		t.Fatalf("events after fallback rewrite=%v", commits)
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

type failingSavedHeadRunner struct {
	execx.Runner
	err error
}

func (r *failingSavedHeadRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	for index := range len(args) - 1 {
		if args[index] == "cat-file" && args[index+1] == "-e" {
			return execx.Result{}, r.err
		}
	}
	return r.Runner.Run(ctx, name, args...)
}

type countingRunner struct {
	execx.Runner
	shows     int
	revisions []string
	logs      [][]string
}

type cancelOnCommitRunner struct {
	execx.Runner
	commit string
	path   string
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

func (r *cancelOnCommitRunner) Start(ctx context.Context, name string, args ...string) (io.ReadWriteCloser, error) {
	stream, err := r.Runner.(execx.DuplexRunner).Start(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	return &observingDuplex{ReadWriteCloser: stream, observe: func(request []byte) {
		if strings.HasPrefix(string(request), r.commit+":") && (r.path == "" || strings.TrimSuffix(string(request), "\n") == r.commit+":"+r.path) {
			r.cancel()
		}
	}}, nil
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
func (r *countingRunner) Start(ctx context.Context, name string, args ...string) (io.ReadWriteCloser, error) {
	stream, err := r.Runner.(execx.DuplexRunner).Start(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	return &observingDuplex{ReadWriteCloser: stream, observe: func(request []byte) {
		r.shows++
		r.revisions = append(r.revisions, strings.TrimSuffix(string(request), "\n"))
	}}, nil
}

func (r *countingRunner) Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	for _, arg := range args {
		if arg == "log" {
			r.logs = append(r.logs, append([]string(nil), args...))
			break
		}
	}
	return r.Runner.Stream(ctx, name, args...)
}

type observingDuplex struct {
	io.ReadWriteCloser
	observe func([]byte)
}

func (s *observingDuplex) Write(p []byte) (int, error) {
	s.observe(p)
	return s.ReadWriteCloser.Write(p)
}

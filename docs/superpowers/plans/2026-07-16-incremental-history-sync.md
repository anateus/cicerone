# Incremental History Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stream Homebrew Git history, publish queryable 100-commit batches, and render live progress and newly discovered feed rows before first-run indexing completes.

**Architecture:** Git history becomes a callback stream. The indexer durably upserts bounded batches but advances repository cursors and performs destructive reconciliation only after full success. Coordinator progress independently drives TUI and plaintext refreshes.

**Tech Stack:** Go 1.26, SQLite, direct system Git execution, Bubble Tea v2, Bubbles v2.

## Global Constraints

- Yield streamed Git commits before EOF; never buffer the full `git log` output for first-run indexing.
- Atomically publish at most 100 scanned commits per batch.
- Emit progress only after its batch commits.
- Advance cursors, record complete ranges, and delete rewritten-history rows only after full success.
- Cancellation and retry are idempotent through deterministic IDs and conflict-safe upserts.
- TUI animation is decorative; numeric counts are authoritative.
- Plain output is newline-delimited, ANSI-free, and prints every event ID at most once.
- Use direct subprocess argument slices, never a shell.
- Tests never mutate Homebrew packages or user caches.
- Glow rendering is out of scope.

---

### Task 1: Stream Git Commit History

**Files:**
- Modify: `internal/gitrepo/history.go`
- Test: `internal/gitrepo/history_test.go`

**Interfaces:**
- Produces: `Repository.WalkCommits(context.Context, Range, func(Commit) error) error`.
- Preserves: `Repository.Commits` as a collecting wrapper.

- [ ] **Step 1: Write the failing pre-EOF test**

Use an `io.Pipe` fake runner. Start `WalkCommits`, write one complete commit plus the next header, and assert the first exact `Commit` arrives before closing the writer. Finish the second commit, including a rename change, close, and assert clean completion.

```go
func TestWalkCommitsYieldsBeforeEOF(t *testing.T) {
	reader, writer := io.Pipe()
	repo := New(Source{Path: "/repo"}, &streamRunner{reader: reader})
	yielded, done := make(chan Commit, 2), make(chan error, 1)
	go func() { done <- repo.WalkCommits(context.Background(), Range{Revision: "HEAD"}, func(c Commit) error { yielded <- c; return nil }) }()
	_, _ = writer.Write(firstCommitAndNextHeader())
	select {
	case got := <-yielded:
		if diff := cmp.Diff(wantFirstCommit(), got); diff != "" { t.Fatal(diff) }
	case <-time.After(time.Second):
		t.Fatal("commit not yielded before EOF")
	}
	_, _ = writer.Write(secondCommitRemainder())
	_ = writer.Close()
	if err := <-done; err != nil { t.Fatal(err) }
}
```

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/gitrepo -run TestWalkCommitsYieldsBeforeEOF -count=1 -v`

Expected: FAIL because `WalkCommits` is missing.

- [ ] **Step 3: Implement the streaming parser**

Reuse the existing exact Git arguments through `Runner.Stream`. Parse NUL-delimited tokens with `bufio.Reader`, retaining only the current commit. A new object-ID header causes the prior commit callback immediately. Emit the last commit at EOF. Join parse/callback errors with process-close errors. Preserve all malformed protocol checks.

`Commits` calls `WalkCommits` and appends results for compatibility.

- [ ] **Step 4: Verify and commit**

Run: `go test -race ./internal/gitrepo -count=1`

```bash
git add internal/gitrepo/history.go internal/gitrepo/history_test.go
git commit -m "refactor: stream repository commit history"
```

### Task 2: Separate Batch Persistence from Finalization

**Files:**
- Modify: `internal/store/history.go`
- Test: `internal/store/history_test.go`

**Interfaces:**
- Produces: `ApplyHistoryBatch(context.Context, HistoryBatch) error`.
- Produces: `FinalizeHistory(context.Context, HistoryBatch) error`.
- Preserves: atomic `ApplyHistory` compatibility.

- [ ] **Step 1: Write failing visibility and safety tests**

Apply a batch containing an event, alias, diagnostic, and nonblank head. Assert feed, alias, and diagnostic visibility while `HistoryState` remains unindexed. Finalize and assert the cursor/range appears.

Seed an old event, then batch a replacement with `RemoveCommits: []string{"old"}`. Assert both coexist before finalization and the abandoned event/alias/diagnostic disappear after finalization.

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/store -run 'TestApplyHistoryBatch|TestFinalizeHistory' -count=1 -v`

Expected: FAIL because the APIs are missing.

- [ ] **Step 3: Extract transaction helpers**

```go
func applyHistoryRows(ctx context.Context, tx *sql.Tx, batch HistoryBatch) error
func finalizeHistory(ctx context.Context, tx *sql.Tx, batch HistoryBatch) error
```

The first helper only upserts packages/events/aliases/diagnostics. The second only deletes abandoned commits and updates repository/range state. Each new exported method runs one helper in `Store.Write`; existing `ApplyHistory` runs both inside one transaction.

- [ ] **Step 4: Verify and commit**

Run: `go test -race ./internal/store -count=1`

```bash
git add internal/store/history.go internal/store/history_test.go
git commit -m "refactor: separate history batches from finalization"
```

### Task 3: Publish 100-Commit Index Batches

**Files:**
- Modify: `internal/history/indexer.go`
- Test: `internal/history/indexer_test.go`

**Interfaces:**
- Produces: `Progress{Commits, Events, Diagnostics, Batches int}`.
- Extends: `Request.Progress func(Progress)`.
- Uses: `WalkCommits`, `ApplyHistoryBatch`, `FinalizeHistory`.

- [ ] **Step 1: Write a failing real-repository incremental test**

Create 101 version-changing commits. A runner wrapping the real runner blocks the blob read for scanned commit 101. Start `Index`; after its first progress callback, assert 100 commits' events are queryable, indexing has not returned, and `HistoryState` is unindexed. Release and assert all 101 events and the final cursor.

- [ ] **Step 2: Add failing cancellation/retry/rewrite tests**

Cancel after batch one and assert valid partial rows plus no cursor. Retry and assert unique deterministic event IDs and final cursor. During a blocked rewritten scan, assert old and replacement rows coexist; after success, assert finalization removes abandoned rows.

- [ ] **Step 3: Verify RED**

Run: `go test ./internal/history -run 'TestIndexerPublishesBatchesBeforeCompletion|TestIndexerCancellationKeepsCursorUnfinished|TestIndexerRetryFinalizesWithoutDuplicates|TestIndexerDefersRewriteDeletion' -count=1 -v`

Expected: FAIL because indexing publishes once and has no progress.

- [ ] **Step 4: Implement batching**

```go
const historyBatchCommits = 100
type Progress struct { Commits, Events, Diagnostics, Batches int }
```

Walk every range incrementally. Flush event/alias/diagnostic slices after every 100 scanned commits and after the final partial batch. Call `ApplyHistoryBatch`, clear batch slices, increment batches, then synchronously report cumulative progress. Finally call `FinalizeHistory` exactly once. Preserve fallback coverage, filtering, aliases, diagnostics, rewrites, and idempotency.

- [ ] **Step 5: Verify and commit**

Run: `go test -race ./internal/history ./internal/store ./internal/gitrepo -count=1`

```bash
git add internal/history/indexer.go internal/history/indexer_test.go
git commit -m "feat: publish history in incremental batches"
```

### Task 4: Propagate Durable Progress Through the Coordinator

**Files:**
- Modify: `internal/syncer/coordinator.go`
- Test: `internal/syncer/coordinator_test.go`
- Modify: `cmd/cicerone/main.go`

**Interfaces:**
- Produces: `syncer.Progress{Commits, Events, Diagnostics, Batches int}`.
- Produces: `SyncProgress{Source string; At time.Time; Progress Progress}`.
- Extends: `syncer.Request.Progress func(Progress)`.

- [ ] **Step 1: Write the failing ordering test**

A fake source invokes two request progress callbacks only after exposing corresponding durable fake batches. Assert notification order: started, progress 1, dataset changed, progress 2, dataset changed, committed. Assert cancellation/failure fabricates no progress.

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/syncer -run TestCoordinatorPublishesDurableBatchProgress -count=1 -v`

Expected: FAIL because progress types are missing.

- [ ] **Step 3: Wire progress synchronously**

Before indexing, install a request callback that emits `SyncProgress` followed by `tui.DatasetChanged`. Translate `history.Progress` in `repositorySource.Index` without dropping an existing callback.

- [ ] **Step 4: Verify and commit**

Run: `go test -race ./internal/syncer ./cmd/cicerone -count=1`

```bash
git add internal/syncer/coordinator.go internal/syncer/coordinator_test.go cmd/cicerone/main.go
git commit -m "feat: report incremental synchronization progress"
```

### Task 5: Render Numeric Progress in the TUI

**Files:**
- Modify: `internal/tui/messages.go`
- Modify: `internal/tui/model.go`
- Modify: `internal/tui/styles.go`
- Test: `internal/tui/model_test.go`
- Test: `internal/tui/golden_test.go`
- Modify: `cmd/cicerone/runtime.go`

**Interfaces:**
- Consumes: a TUI-local translation of `syncer.SyncProgress`.
- Uses: existing `charm.land/bubbles/v2/spinner`.

- [ ] **Step 1: Write failing progress/render tests**

Send two progress messages and assert newer cumulative counts replace older ones. Assert exact source/commit/update/batch text and that a following feed refresh preserves selection/focus/viewport. Add a golden active-sync state. Assert spinner ticks only while active and numeric text is always present.

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/tui -run 'TestSyncProgress|TestProgress' -count=1 -v`

Expected: FAIL because progress state is absent.

- [ ] **Step 3: Implement the TUI model**

Track latest progress and active state by source. Initialize a Bubbles spinner and update it through normal Bubble Tea ticks only while active. Render:

```text
homebrew-core · 1,200 commits scanned · 84 updates · 12 batches
```

Clear active state on committed/failed while retaining final numeric summary. Translate runtime notifications without creating a package cycle.

- [ ] **Step 4: Verify and commit**

Run: `go test -race ./internal/tui ./cmd/cicerone -count=1`

```bash
git add internal/tui/messages.go internal/tui/model.go internal/tui/styles.go internal/tui/model_test.go internal/tui/golden_test.go cmd/cicerone/runtime.go
git commit -m "feat: render live synchronization progress"
```

### Task 6: Render Store Batches Live in Plain Mode

**Files:**
- Modify: `cmd/cicerone/plain.go`
- Test: `cmd/cicerone/plain_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes live progress/committed/failed notifications while `WaitSync` runs.
- Prints each newly visible `domain.EventID` once.

- [ ] **Step 1: Write a failing real-store rendering test**

Use a temporary real store behind a controlled runtime. Print cache, durably commit batch one, send progress, and block batch two. Assert batch-one numeric progress and its real queried feed row appear before release. Release and assert both rows once, final status, and exit. Assert no ANSI bytes. Preserve writer/source error tests.

- [ ] **Step 2: Verify RED**

Run: `go test ./cmd/cicerone -run 'TestRunPlainRendersRealStoreBatchesBeforeCompletion|TestRunPlainProgressHasNoANSI' -count=1 -v`

Expected: FAIL because plain mode waits before consuming notifications.

- [ ] **Step 3: Consume notifications while waiting**

Give production plain runtime a buffered notification channel. Run `WaitSync` in a goroutine, consume notifications until completion, then drain messages queued before completion. On progress, print stable counts, query preferences/feed, and print only IDs absent from a `seen map[domain.EventID]bool` seeded by cached output. Print final statuses and summary without duplicates. Never emit spinner frames, carriage returns, or ANSI.

- [ ] **Step 4: Document, verify, and commit**

Document bounded batches, live rows, and safe interrupted retry.

Run:

```bash
go test -race ./cmd/cicerone ./internal/syncer ./internal/history ./internal/gitrepo ./internal/store -count=1
go test -race ./... -count=1
test -z "$(gofmt -l .)"
go vet ./...
go build ./cmd/cicerone
git diff --check
```

```bash
git add cmd/cicerone/plain.go cmd/cicerone/plain_test.go README.md
git commit -m "feat: stream indexed feed rows in plain mode"
```

### Task 7: Verify the Complete Pipeline Independently

**Files:**
- Test: `internal/integration/app_test.go`

**Interfaces:**
- Verifies: streamed Git → history batch → SQLite query → coordinator progress → plaintext rendering.

- [ ] **Step 1: Add the deterministic integration test**

Use a 201-commit temporary repository, real store, real indexer/coordinator, and controlled runner blocking after batch one. Before release assert at least 100 commits reported, feed rows queryable, cursor incomplete, plain output contains numeric progress plus a real row, and sync still running. Release and assert final cursor, unique IDs, committed status, and clean exit.

- [ ] **Step 2: Prove the test detects all-at-once behavior**

Temporarily block before the first batch callback (or use an all-at-once test wrapper) and confirm the pre-release assertion fails for missing incremental publication. Restore production behavior before committing.

- [ ] **Step 3: Final verification and commit**

Run:

```bash
go test -race ./internal/integration -run TestIncrementalHistoryPipelinePublishesBeforeCompletion -count=1 -v
go test -race ./... -count=1
go vet ./...
test -z "$(gofmt -l .)"
git diff --check
```

```bash
git add internal/integration/app_test.go
git commit -m "test: verify incremental history pipeline"
```

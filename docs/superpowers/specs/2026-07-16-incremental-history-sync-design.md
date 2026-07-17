# Incremental History Sync Design

## Goal

Make first-run Homebrew history indexing observable and useful before the full repository scan completes. Cicerone must stream repository history, publish bounded batches into the real store, refresh rendered data after each committed batch, and retain safe retry and reconciliation semantics.

Glow-based Markdown inspector rendering is explicitly out of scope and remains a separate follow-up.

## Confirmed Problem

The current Git reader captures the complete `git log` output before parsing it. The history indexer then reads every relevant before/after blob, accumulates all events, aliases, and diagnostics in memory, and calls `ApplyHistory` once. The coordinator emits only start and final notifications, and plain mode waits for all sources before rendering refreshed data.

This is correct for small repositories but creates an all-or-nothing first run: no newly indexed feed data or meaningful progress becomes visible while Homebrew history is scanned.

## Streaming Repository History

`internal/gitrepo` will expose a streaming commit walk built on the existing direct subprocess runner's `Stream` method. It will parse the NUL-delimited `git log --name-status` protocol incrementally and invoke a callback for each complete commit without waiting for EOF.

The parser must yield a commit as soon as the next commit header proves the current record is complete. It must preserve rename/copy parsing, timestamps, subjects, and change lists. Cancellation closes and waits for the subprocess through the existing process reader. No shell is used.

The slice-returning `Commits` interface may remain as a compatibility wrapper that collects the streaming walk where callers genuinely need a complete list, but first-run indexing must use the streaming interface.

## Bounded Index Batches

The history indexer processes commits in source order and builds a batch of at most 100 scanned commits. Each batch atomically upserts its packages, immutable events, aliases, and diagnostics. It then clears batch-owned slices before continuing, so event payload memory is bounded by batch size rather than repository size.

After a batch transaction commits, the indexer reports cumulative progress:

- repository/source name;
- commits scanned;
- events discovered;
- diagnostics discovered;
- batches committed.

The final partial batch is committed and reported using the same path.

Deterministic event IDs and conflict-safe upserts make retry idempotent. Cancellation or failure may leave valid newly discovered rows visible, but it must not mark the repository scan complete.

Some scan-planning state remains necessarily cumulative: the set of already-seen commit IDs, installed-package fallback coverage, and rewritten commit IDs to remove. Large event bodies, aliases, diagnostics, and the `git log` byte stream must not accumulate for the whole repository.

## Cursor and Reconciliation Safety

Incremental batch commits do not update `repositories.head_commit` or `repository_ranges`, and do not delete events from rewritten history. Only a finalization transaction after the complete successful scan:

1. removes events, aliases, and diagnostics belonging to abandoned commits;
2. records the repository path and final head;
3. records the fully covered time range.

If the process stops early, the previous authoritative cursor remains unchanged. A retry scans from that cursor again, upserts deterministic rows without duplication, finishes reconciliation, and advances the cursor exactly once.

During an interrupted rewritten-history scan, old and replacement events may temporarily coexist. This favors immediately useful, recoverable data over destructive partial reconciliation; finalization restores the authoritative view.

## Coordinator Progress

The source/index boundary gains a progress callback. The coordinator converts committed-batch reports into a typed `SyncProgress` notification and a dataset-change notification. Progress is emitted only after the corresponding store transaction succeeds.

`SyncProgress` carries cumulative numeric counts; it is not an indefinite activity signal. Existing `SyncStarted`, `SyncCommitted`, and `SyncFailed` remain the lifecycle boundaries.

## TUI Rendering

The TUI re-queries the feed on each dataset-change notification while preserving selection, focus, expansion, filters, and viewport anchors through the existing stale-response protections.

The status view displays authoritative text such as:

```text
homebrew-core · 1,200 commits scanned · 84 updates · 12 batches
```

The already-installed Bubbles v2 spinner may appear beside an active source. It runs through the normal Bubble Tea update loop and is decoration only; numeric counts remain the source of truth. A determinate progress bar is not shown because the streaming scan has no reliable total without a separate full-history pass.

## Plain Rendering

Plain mode consumes coordinator notifications while synchronization is running instead of calling `Wait` before rendering them. It prints stable newline-delimited progress after every committed batch and re-queries the real store after each batch.

Newly visible feed events are printed once, deduplicated by `domain.EventID`. Cached events printed before synchronization seed the deduplication set. Plain output contains no cursor movement, ANSI escapes, spinner frames, or rewritten terminal lines, making it suitable for redirected output and automated inspection.

After all sources finish, plain mode prints final committed/failed statuses and a refreshed-feed summary without duplicating rows already emitted incrementally. Source failure details remain readable and cause a nonzero exit status.

## Independent Verification

Verification is split at component boundaries:

1. A blocking streaming-runner test proves the Git parser yields the first commit before EOF and preserves exact commit/change structures.
2. A real temporary Git repository and SQLite store prove the first 100-commit batch becomes queryable while a later commit/blob read is blocked.
3. Cancellation tests prove partial rows remain valid but the repository cursor does not advance.
4. Retry tests prove deterministic batches create no duplicate events and successful retry finalizes the cursor.
5. Rewritten-history tests prove abandoned rows are retained during partial work and removed only by successful finalization.
6. Coordinator tests prove progress is ordered after durable batch publication and before final committed status.
7. A real-store plain test proves committed batches flow through `QueryFeed` into deduplicated text before synchronization completes.
8. TUI tests prove progress counts render and batch dataset changes refresh rows without losing interaction state.

All tests use temporary repositories, temporary databases, fake subprocess streams, or model fakes. They never mutate Homebrew packages or user caches.

## Animation Research Decision

Use official Bubbles v2 components already present in the dependency graph. A Bubbles spinner is compatible with the existing Bubble Tea v2 model and adds no dependency. Direct Harmonica use and Huh's standalone spinner lifecycle add complexity without improving real progress reporting. Imperative stdout progress libraries are unsuitable because they would compete with Bubble Tea rendering.

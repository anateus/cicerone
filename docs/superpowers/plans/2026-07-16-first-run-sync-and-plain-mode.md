# First-Run Sync and Plain Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix first-run Homebrew history synchronization and add a production `--plain` one-shot mode for exercising Cicerone without an interactive terminal.

**Architecture:** Empty repository cursors created by sync bookkeeping are normalized to “unindexed” at the store boundary. Application dependencies move behind a shared runtime so Bubble Tea and plaintext execution use the same real store, Homebrew client, Git discovery, repositories, indexers, changelog resolver, and coordinator. Plain mode prints cached data, waits on coordinator notifications for one sync, prints refreshed data, and exits.

**Tech Stack:** Go 1.26, SQLite, system `brew` and `git`, Bubble Tea v2, Go standard library.

## Global Constraints

- The first-run application fix is the primary outcome; plaintext mode remains a small operational surface.
- `--plain` uses the normal user database, Homebrew metadata, and Cicerone-owned mirrors.
- Cached feed output must appear before Homebrew or Git synchronization starts.
- Plain mode must never install, upgrade, or uninstall packages.
- Subprocesses use direct argument slices and never invoke a shell.
- Source failures produce readable output and a nonzero exit status.
- Tests must not mutate real Homebrew packages or user caches.

---

### Task 1: Treat Empty History Cursors as Unindexed

**Files:**
- Modify: `internal/store/history.go`
- Test: `internal/history/indexer_test.go`

**Interfaces:**
- Consumes: `Store.SyncStarted(ctx, source, at)` and `Indexer.Index(ctx, source, req)`.
- Produces: `Store.HistoryState(ctx, repository)` returning `exists == false` when `repositories.head_commit` is empty.

- [ ] **Step 1: Write the failing production-sequence regression**

Add a test that creates a real temporary Git repository, opens a store, calls `SyncStarted`, and then indexes it:

```go
func TestIndexerTreatsSyncBookkeepingRowAsUnindexed(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	repo := testutil.NewGitRepo(t)
	head := repo.Commit("Formula/foo.rb", formula("1"), "add", now.Add(-time.Hour))
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SyncStarted(ctx, "core", now); err != nil { t.Fatal(err) }
	source := gitrepo.Source{Name: "core", Path: repo.Path}
	result, err := NewIndexer(gitrepo.New(source, execx.NewRunner()), s).Index(ctx, source, Request{Since: now.Add(-24 * time.Hour)})
	if err != nil { t.Fatal(err) }
	if result.Head != head || result.Events != 1 { t.Fatalf("result=%#v", result) }
}
```

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/history -run TestIndexerTreatsSyncBookkeepingRowAsUnindexed -count=1 -v`

Expected: FAIL with `merge base  <head>` because the empty cursor is treated as indexed.

- [ ] **Step 3: Normalize empty cursors in `HistoryState`**

After scanning the repository row, return an unindexed state when the cursor is blank:

```go
if strings.TrimSpace(result.Head) == "" {
	return HistoryState{}, false, nil
}
```

Keep sync-status rows intact; only history-state interpretation changes.

- [ ] **Step 4: Verify GREEN and regressions**

Run: `go test -race ./internal/history ./internal/store ./internal/syncer -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/history.go internal/history/indexer_test.go
git commit -m "fix: index repositories after first sync start"
```

### Task 2: Extract Shared Production Runtime

**Files:**
- Create: `cmd/cicerone/runtime.go`
- Modify: `cmd/cicerone/main.go`
- Test: `cmd/cicerone/main_test.go`

**Interfaces:**
- Produces: `runtimeServices` owning `context.Context`, store, Homebrew client, coordinator, source repository map, changelog loader, and ordered `Close() error`.
- Produces: `newRuntime(home string, notify func(tea.Msg)) (*runtimeServices, error)`.
- Preserves: TUI cached-first startup through `OnReady`.

- [ ] **Step 1: Write a failing lifecycle/wiring test**

Add a constructor seam test proving the runtime exposes non-nil store, coordinator, action runner, installed refresher, and changelog loader, and that `Close` is idempotent. Inject temporary paths and fake runner through package variables rather than touching user paths.

- [ ] **Step 2: Verify RED**

Run: `go test ./cmd/cicerone -run TestRuntimeServices -count=1 -v`

Expected: FAIL because `runtimeServices` and `newRuntime` do not exist.

- [ ] **Step 3: Move existing production construction into `runtime.go`**

Define the focused lifecycle surface:

```go
type runtimeServices struct {
	ctx context.Context
	store *store.Store
	brew *homebrew.Client
	coordinator *syncer.Coordinator
	changelogs changelogLoader
	cancel context.CancelFunc
	closeOnce sync.Once
	closeErr error
}

func (r *runtimeServices) Close() error {
	r.closeOnce.Do(func() {
		r.coordinator.Close()
		r.cancel()
		r.closeErr = r.store.Close()
	})
	return r.closeErr
}
```

Move source discovery, repository-map ownership, resolver construction, and coordinator notification wiring without changing their behavior. Keep `run()` responsible only for creating the runtime, constructing the TUI dependencies, starting Bubble Tea, and deferring runtime closure.

- [ ] **Step 4: Verify behavior preservation**

Run: `go test -race ./cmd/cicerone ./internal/syncer ./internal/tui -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/cicerone/main.go cmd/cicerone/runtime.go cmd/cicerone/main_test.go
git commit -m "refactor: share production application runtime"
```

### Task 3: Add One-Shot Plaintext Execution

**Files:**
- Create: `cmd/cicerone/plain.go`
- Modify: `cmd/cicerone/main.go`
- Test: `cmd/cicerone/plain_test.go`
- Modify: `README.md`

**Interfaces:**
- Produces: `runPlain(ctx context.Context, runtime plainRuntime, out io.Writer) error`.
- Extends: `execute` to accept `--plain`.

- [ ] **Step 1: Write failing ordering, completion, and failure tests**

Define a small test-facing runtime interface:

```go
type plainRuntime interface {
	QueryFeed(context.Context, domain.FeedFilter) ([]domain.FeedGroup, error)
	Preferences(context.Context) (domain.FeedFilter, error)
	StartSync(context.Context)
	WaitSync()
}
```

Inject a notification collector into the runtime and use a channel-controlled fake to assert cached rows are written before `StartSync`, `WaitSync` completes before the refreshed query, refreshed rows are printed, and any collected `SyncFailed` returns an error. Assert exact stable lines such as:

```text
Cached feed
foo 2.0 · version · 2026-07-16
Synchronizing homebrew-core…
homebrew-core synchronized · 1 updates
Refreshed feed
foo 3.0 · version · 2026-07-16
```

- [ ] **Step 2: Verify RED**

Run: `go test ./cmd/cicerone -run 'TestRunPlain|TestExecutePlain' -count=1 -v`

Expected: FAIL because `runPlain` and `--plain` routing do not exist.

- [ ] **Step 3: Implement deterministic plain rendering and one-shot waiting**

Render groups and children without ANSI escapes. Call `Coordinator.Start` followed by its condition-variable-backed `Wait`; collect `SyncStarted`, `SyncCommitted`, and `SyncFailed` notifications for output and return a joined error for failures. Query default preferences from the real store before each feed query. Do not add polling or sleeps.

Route the CLI explicitly:

```go
if len(args) == 1 && args[0] == "--plain" {
	return executePlain(stdout, stderr)
}
```

Document that `--plain` performs real read-only metadata synchronization and may update only Cicerone-owned caches.

- [ ] **Step 4: Verify focused and full tests**

Run:

```bash
go test -race ./cmd/cicerone ./internal/syncer ./internal/history -count=1
go test -race ./... -count=1
```

Expected: PASS with no real Homebrew mutation.

- [ ] **Step 5: Build and exercise the real command**

Run:

```bash
go build -o ./cicerone ./cmd/cicerone
./cicerone --plain
```

Expected: cached feed prints before synchronization; `homebrew-core` and `homebrew-cask` reach committed status; the process exits 0; output does not contain `merge base  `.

- [ ] **Step 6: Final verification and commit**

Run:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./... -count=1
go build ./cmd/cicerone
git diff --check
```

Expected: all commands exit 0.

```bash
git add cmd/cicerone/main.go cmd/cicerone/plain.go cmd/cicerone/plain_test.go README.md
git commit -m "feat: add plaintext one-shot synchronization"
```

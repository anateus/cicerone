# Cicerone Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a macOS-first Homebrew package feed that indexes update history, caches version-specific changelogs, and supports install/upgrade actions without disruptive UI refreshes.

**Architecture:** A single Go binary separates domain rules, SQLite persistence, external adapters, background services, and a Bubble Tea UI. SQLite is the atomic handoff between background ingestion and the UI; every refresh restores view state by stable identity.

**Tech Stack:** Go 1.25+, Bubble Tea v2.0.8, Bubbles v2.1.1, Lip Gloss v2.0.5, modernc SQLite v1.54.0, system `git` and `brew`, Go standard-library HTTP.

## Global Constraints

- Target macOS first; isolate OS paths and command execution for later Linuxbrew support.
- Use module path `cicerone`; changing the public module path is a release-time operation.
- Use SQLite WAL mode and serialize writes through one store-owned writer.
- At startup assert `sqlite_version() >= 3.51.3`; fail with recovery guidance if the bundled version is older.
- Render cached data before starting network, Git, or Homebrew refresh work.
- Never mutate a user-owned tap repository.
- Run subprocesses directly with argument slices; never invoke a shell.
- Preserve selection, viewport-relative row, filters, expansion, grouping, search text, and pane focus across refreshes.
- Default non-installed horizon: 30 days. Always include each installed package's newest matching event regardless of age.
- Default event filter: version events only. Default grouping: collapse adjacent same-package events after filtering and sorting.
- Automated tests must never install, upgrade, or uninstall real packages.

---

## File Map

- `cmd/cicerone/main.go`: process startup, dependency wiring, and terminal program lifecycle.
- `internal/domain/{package,event,feed,selection}.go`: dependency-free identities, update events, grouping, and stable selection.
- `internal/store/{store,migrations,feed,changelog}.go`: SQLite lifecycle, schema, writer serialization, and queries.
- `internal/execx/runner.go`: injectable, shell-free subprocess boundary.
- `internal/homebrew/{client,installed,actions}.go`: Homebrew metadata and package mutations.
- `internal/gitrepo/{repository,discovery,history}.go`: read-only local source selection and owned mirror operations.
- `internal/history/{parser,classifier,indexer}.go`: definition parsing, change classification, and incremental ingestion.
- `internal/changelog/{resolver,fetcher,extractor,matcher}.go`: bounded discovery, artifact caching, extraction, and version matching.
- `internal/syncer/coordinator.go`: bounded background orchestration and committed-change notifications.
- `internal/tui/{model,messages,feed,inspector,styles,actions}.go`: Bubble Tea state, Bubbles components, Lip Gloss rendering, and command modal.
- `internal/testutil/{git,http,runner}.go`: shared deterministic test fixtures.

---

### Task 1: Bootable Go Application and Configuration Paths

**Files:**
- Create: `go.mod`
- Create: `cmd/cicerone/main.go`
- Create: `internal/app/paths.go`
- Test: `internal/app/paths_test.go`
- Create: `Makefile`

**Interfaces:**
- Produces: `app.Paths{DataDir, CacheDir, DBPath string}` and `app.DefaultPaths(home string) Paths`.
- Produces: a bootable `cicerone` command whose initialization screen is expanded by Task 9.

- [ ] **Step 1: Write the failing paths test**

```go
func TestDefaultPaths(t *testing.T) {
	got := DefaultPaths("/Users/alice")
	want := Paths{
		DataDir:  "/Users/alice/Library/Application Support/cicerone",
		CacheDir: "/Users/alice/Library/Caches/cicerone",
		DBPath:   "/Users/alice/Library/Application Support/cicerone/cicerone.db",
	}
	if diff := cmp.Diff(want, got); diff != "" { t.Fatal(diff) }
}
```

- [ ] **Step 2: Initialize the module and verify the test fails**

Run: `go mod init cicerone && go get github.com/google/go-cmp/cmp@v0.7.0 && go test ./internal/app`

Expected: FAIL because `Paths` and `DefaultPaths` are undefined.

- [ ] **Step 3: Implement `Paths` and a minimal Bubble Tea entrypoint**

```go
type Paths struct { DataDir, CacheDir, DBPath string }

func DefaultPaths(home string) Paths {
	data := filepath.Join(home, "Library", "Application Support", "cicerone")
	return Paths{DataDir: data, CacheDir: filepath.Join(home, "Library", "Caches", "cicerone"), DBPath: filepath.Join(data, "cicerone.db")}
}
```

Add Bubble Tea v2.0.8, Bubbles v2.1.1, and Lip Gloss v2.0.5. `main.go` must resolve the home directory, create both directories with mode `0700`, and run a minimal model that displays `Cicerone is initializing…` and exits on `q`/Ctrl-C.

- [ ] **Step 4: Add standard developer commands**

```make
.PHONY: test test-race fmt vet build
test:
	go test ./...
test-race:
	go test -race ./...
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')
vet:
	go vet ./...
build:
	go build ./cmd/cicerone
```

- [ ] **Step 5: Verify and commit**

Run: `make fmt && make test && make vet && make build`

Expected: all commands exit 0 and `./cicerone` exists.

```bash
git add go.mod go.sum Makefile cmd/cicerone internal/app
git commit -m "feat: bootstrap Cicerone application"
```

### Task 2: Domain Events, Feed Filtering, and Roll-up

**Files:**
- Create: `internal/domain/package.go`
- Create: `internal/domain/event.go`
- Create: `internal/domain/feed.go`
- Create: `internal/domain/selection.go`
- Test: `internal/domain/feed_test.go`
- Test: `internal/domain/selection_test.go`

**Interfaces:**
- Produces: `PackageID string`, `EventID string`, `PackageType`, `EventKind`, `UpdateEvent`, `InstalledPackage{PackageID, Name, Version string; Type PackageType; Pinned, UpgradeAvailable bool}`, `FeedFilter`, and `FeedGroup`.
- Produces: `BuildFeed(events []UpdateEvent, installed map[PackageID]bool, f FeedFilter) []FeedGroup`.
- Produces: `RestoreSelection(old Anchor, groups []FeedGroup) Anchor`.

- [ ] **Step 1: Define failing feed examples**

Create table tests proving: 30-day exclusion for uninstalled packages; old installed packages remain; kind filters apply; deterministic newest-first order; and `Foo 1.3, Foo 1.2, Foo 1.1, Bar 3.2` becomes two groups while `Foo, Bar, Foo` remains three groups.

```go
type FeedFilter struct {
	Now time.Time
	Horizon time.Duration
	Kinds map[EventKind]bool
	Types map[PackageType]bool
	Query string
	RollUp bool
}
```

Run: `go test ./internal/domain -run TestBuildFeed -v`

Expected: FAIL because domain types are undefined.

- [ ] **Step 2: Implement immutable domain types and `BuildFeed`**

Use `EventID = repository + ":" + commit + ":" + packageID + ":" + kind`. Filtering must occur before sorting and roll-up. For each installed package/kind combination that has no event inside the horizon, retain its newest matching event. Break timestamp ties by event ID.

- [ ] **Step 3: Write failing stable-selection tests**

Cover an inserted row above selection, deletion of selection, expanded-child restoration, and empty results. `Anchor` contains `GroupID`, `ChildEventID`, `ViewportOffset`, and `FallbackIndex`.

- [ ] **Step 4: Implement `RestoreSelection`**

Exact priority: same child event, same group, clamped prior index, then no anchor for an empty feed. Preserve `ViewportOffset` when identity survives; otherwise set it to zero.

- [ ] **Step 5: Verify and commit**

Run: `go test ./internal/domain -count=1`

Expected: PASS.

```bash
git add internal/domain
git commit -m "feat: model and group package update feeds"
```

### Task 3: SQLite Schema, Serialized Writer, and Feed Queries

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/migrations.go`
- Create: `internal/store/schema/001_initial.sql`
- Create: `internal/store/feed.go`
- Test: `internal/store/store_test.go`
- Test: `internal/store/feed_test.go`

**Interfaces:**
- Consumes: domain types from Task 2.
- Produces: `Open(ctx context.Context, path string) (*Store, error)`, `Close() error`.
- Produces: `Write(ctx context.Context, func(*sql.Tx) error) error`.
- Produces: `UpsertEvents(ctx context.Context, []domain.UpdateEvent) error`, `SetInstalled(ctx context.Context, []domain.InstalledPackage) error`, and `QueryFeed(ctx context.Context, domain.FeedFilter) ([]domain.FeedGroup, error)`.
- Produces: `Preferences(ctx context.Context) (domain.FeedFilter, error)` and `SetPreferences(ctx context.Context, domain.FeedFilter) error`.

- [ ] **Step 1: Write failing migration and version tests**

Assert WAL mode, foreign keys, busy timeout, schema version 1, all design tables, FTS5 indexes for package names and changelog text, uniqueness of `(repository, commit_hash, package_id, kind)`, and `sqlite_version() >= 3.51.3`.

Run: `go get modernc.org/sqlite@v1.54.0 && go test ./internal/store -run TestOpen -v`

Expected: FAIL because `Open` is undefined.

- [ ] **Step 2: Implement `Open`, embedded migrations, and one writer goroutine**

Use `//go:embed schema/*.sql`. Configure `_pragma=journal_mode(WAL)`, `_pragma=foreign_keys(1)`, and `_pragma=busy_timeout(5000)`. Route every `Write` request through one channel owned by a goroutine; return the transaction result to the caller.

- [ ] **Step 3: Write failing idempotency and visibility tests**

Insert the same event twice and expect one row. Hold a reader while committing a writer and prove the next query sees the complete transaction, never a partial source sync.

- [ ] **Step 4: Implement event/installed upserts and SQL feed selection**

SQL selects candidate rows and installed fallback rows; call `domain.BuildFeed` for final deterministic grouping. Add indexes on `(event_time DESC, id)`, `(package_id, kind, event_time DESC)`, and installed package identity. Keep FTS5 external-content tables synchronized with triggers in the same write transaction. Store filters as explicit typed columns rather than an opaque serialized model.

- [ ] **Step 5: Verify and commit**

Run: `go test -race ./internal/store -count=1`

Expected: PASS without data races.

```bash
git add go.mod go.sum internal/store
git commit -m "feat: add transactional SQLite history store"
```

### Task 4: Safe Process Runner and Homebrew Installed State

**Files:**
- Create: `internal/execx/runner.go`
- Create: `internal/homebrew/client.go`
- Create: `internal/homebrew/installed.go`
- Test: `internal/homebrew/installed_test.go`
- Create: `internal/testutil/runner.go`

**Interfaces:**
- Produces: `execx.Runner` with `Run(ctx context.Context, name string, args ...string) (Result, error)` and `Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error)`.
- Produces: `homebrew.Client.Installed(ctx) ([]domain.InstalledPackage, error)`.

- [ ] **Step 1: Write a failing Homebrew JSON parsing test**

Fixture output must contain one formula with multiple installed versions and one cask. Assert the newest installed version, package type, full name, pinned state, and upgrade availability are retained.

Run: `go test ./internal/homebrew -run TestInstalled -v`

Expected: FAIL because `Client` is undefined.

- [ ] **Step 2: Implement injectable execution and installed-state parsing**

Execute `brew info --json=v2 --installed`. Decode only required fields with explicit JSON structs. Reject empty/malformed package names. Never concatenate arguments into a shell command.

- [ ] **Step 3: Test cancellation and stderr propagation**

Use a helper test process to block until context cancellation and another to exit 17 with stderr. Assert cancellation returns `context.Canceled` and failures include executable, args, exit code, and bounded stderr.

- [ ] **Step 4: Verify and commit**

Run: `go test -race ./internal/execx ./internal/homebrew`

Expected: PASS.

```bash
git add internal/execx internal/homebrew internal/testutil
git commit -m "feat: read installed Homebrew package state"
```

### Task 5: Git Source Discovery and Owned Mirrors

**Files:**
- Create: `internal/gitrepo/repository.go`
- Create: `internal/gitrepo/discovery.go`
- Create: `internal/gitrepo/history.go`
- Test: `internal/gitrepo/discovery_test.go`
- Test: `internal/gitrepo/history_test.go`
- Create: `internal/testutil/git.go`

**Interfaces:**
- Consumes: `execx.Runner`.
- Produces: `Source{Kind, Name, Path, RemoteURL string; Owned bool}`.
- Produces: `Discover(ctx, brewPrefix, cacheDir string, runner execx.Runner) ([]Source, error)`.
- Produces: `Repository.Ensure(ctx) error`, `Fetch(ctx) error`, `Head(ctx) (string, error)`, `MergeBase(ctx, a, b string) (string, error)`, `Commits(ctx, Range) ([]Commit, error)`, and `Blob(ctx, revision, path string) ([]byte, error)`.

- [ ] **Step 1: Write source-precedence tests**

Use temporary directories to prove a valid local tap wins, a missing/invalid tap produces an owned source, and local discovery never executes a mutating Git command.

- [ ] **Step 2: Implement discovery and owned partial mirrors**

Recognize Homebrew's conventional tap paths plus `brew --repository homebrew/core` and `homebrew/cask`. Owned sources use `git clone --mirror --filter=blob:none` and later `git fetch --prune`; local sources expose read-only history commands only.

- [ ] **Step 3: Write and implement history protocol tests**

Create a temporary repository with dated commits, a rename, and a divergent rewritten branch. Parse NUL-delimited output from `git log --format=... --name-status -z`. Test `MergeBase`, bounded date ranges, and paths containing spaces.

- [ ] **Step 4: Verify and commit**

Run: `go test -race ./internal/gitrepo -count=1`

Expected: PASS.

```bash
git add internal/gitrepo internal/testutil/git.go
git commit -m "feat: discover and query Homebrew Git sources"
```

### Task 6: Definition Classification and Incremental Indexing

**Files:**
- Create: `internal/history/parser.go`
- Create: `internal/history/classifier.go`
- Create: `internal/history/indexer.go`
- Test: `internal/history/parser_test.go`
- Test: `internal/history/classifier_test.go`
- Test: `internal/history/indexer_test.go`

**Interfaces:**
- Consumes: `gitrepo.Repository`, `store.Store`, and domain types.
- Produces: `Definition{Name, FullName, Version, Revision string; Type domain.PackageType; Homepage, URL string}`.
- Produces: `Classify(before, after *Definition) Classification`.
- Produces: `Request{Since time.Time; Installed []domain.PackageID; Kinds map[domain.EventKind]bool}` and `Indexer.Index(ctx context.Context, source gitrepo.Source, req Request) (Result, error)`.

- [ ] **Step 1: Write definition fixture tests**

Cover formula `url`/`version`/`revision`, cask `version`, live versions, deletion, rename, and metadata-only edits. Parsing must be conservative and return diagnostics instead of evaluating Ruby.

- [ ] **Step 2: Implement conservative text parsing and classification**

Use anchored Ruby-token patterns only. Classification priority is version, revision, metadata. Return `Ambiguous` when required identity/version data cannot be derived.

- [ ] **Step 3: Write failing indexer tests**

Prove 30-day indexing, backward range extension without reparsing covered commits, per-installed-package fallback for all three classifications, idempotent reruns, persisted cursor/range, alias/rename persistence, classification diagnostics, and atomic visibility. Simulate rewritten history and assert reconciliation from merge base removes/replaces only affected source events.

- [ ] **Step 4: Implement the indexer**

For each changed definition, read before/after blobs with `git show <commit>^:<path>` and `git show <commit>:<path>`. Batch resulting events and diagnostics into one `Store.Write` transaction per source. Record cursor only inside that transaction.

- [ ] **Step 5: Verify and commit**

Run: `go test -race ./internal/history ./internal/store -count=1`

Expected: PASS.

```bash
git add internal/history internal/store
git commit -m "feat: index and classify Homebrew update history"
```

### Task 7: Cached Changelog Artifacts and Version Matching

**Files:**
- Create: `internal/store/changelog.go`
- Create: `internal/changelog/resolver.go`
- Create: `internal/changelog/matcher.go`
- Test: `internal/changelog/matcher_test.go`
- Test: `internal/changelog/resolver_test.go`
- Create: `internal/changelog/testdata/`

**Interfaces:**
- Produces: `Artifact{ID, URL, MediaType, ETag, LastModified, Hash string; Raw, Extracted []byte; FetchedAt time.Time; ParentID *string}`.
- Produces: `Section{ArtifactID, Version, Body string; Confidence float64; SourceURL string}`.
- Produces: `MatchVersion(version string, artifacts []Artifact) (Section, bool)`.
- Produces: `PackageRef{Name, FullName, Homepage, RepositoryURL string; Type domain.PackageType}` and `Resolver.Resolve(ctx context.Context, pkg PackageRef, version string) (Section, error)`.

- [ ] **Step 1: Add failing version-matching fixtures**

Fixtures cover ATX/setext Markdown headings, `v1.2.3`, `1.2.3`, package-prefixed tags, ranges, unreleased sections, and ambiguous partial matches. Exact normalized tags score 1.0; heading matches score at least 0.8; substring-only matches never exceed 0.49.

- [ ] **Step 2: Implement artifact persistence and pure matching**

Deduplicate artifacts by URL plus content hash. Persist raw content and extracted sections separately. A failed refresh must not delete the newest successful artifact.

- [ ] **Step 3: Implement repository-file and GitHub Release candidates**

Search `CHANGELOG*`, `CHANGES*`, `NEWS*`, `HISTORY`, `HISTORY.md`, `HISTORY.txt`, `RELEASES*`, and `*WHATSNEW*`, in that order, matching case-insensitively. For GitHub, call the versioned Releases API and try normalized tag candidates. Support an optional token from `GITHUB_TOKEN`; work unauthenticated with cache/backoff.

- [ ] **Step 4: Verify and commit**

Run: `go test ./internal/changelog ./internal/store -count=1`

Expected: PASS.

```bash
git add internal/changelog internal/store/changelog.go
git commit -m "feat: cache and match package changelogs"
```

### Task 8: Safe Linked-Webpage Discovery and Extraction Spike

**Files:**
- Create: `internal/changelog/fetcher.go`
- Create: `internal/changelog/extractor.go`
- Create: `internal/changelog/links.go`
- Test: `internal/changelog/fetcher_test.go`
- Test: `internal/changelog/extractor_test.go`
- Create: `internal/changelog/testdata/pages/`

**Interfaces:**
- Produces: `Fetched{FinalURL *url.URL; MediaType, ETag, LastModified string; Body []byte; FetchedAt time.Time}` and `Fetcher.Fetch(ctx context.Context, rawURL string) (Fetched, error)`.
- Produces: `Extracted{Title, Text string; Links []Candidate}` and `ContentExtractor.Extract(base *url.URL, html []byte) (Extracted, error)`.
- Produces: `Candidate{URL *url.URL; Label string; Score float64; Depth int}` and `DiscoverLinks(base *url.URL, content []byte) []Candidate`.

- [ ] **Step 1: Write network-policy tests with local HTTP servers and a fake resolver**

Test HTTPS/HTTP acceptance, private/loopback denial before dialing, DNS rebinding defense on redirect, two-hop maximum, five candidates maximum, 10 MiB response maximum, 15-second request timeout, three redirects maximum, media-type rejection, validators, and per-host concurrency of two.

- [ ] **Step 2: Implement bounded fetching and labeled-link scoring**

Score links containing changelog, changes, release notes, release, or the selected version. Normalize and deduplicate URLs. Record denied and failed attempts with retry time; never recursively crawl arbitrary site links.

- [ ] **Step 3: Evaluate both extractors against committed fixtures**

Add `go-trafilatura` v1.13.4 and `go-readability` v0.3.1 behind adapters. The fixture table must score retained version headings, retained list items, boilerplate removal, deterministic output, and malformed-HTML behavior. Keep the higher-scoring implementation; remove the losing dependency and adapter in this same task. Document scores in `internal/changelog/testdata/pages/README.md`.

- [ ] **Step 4: Integrate linked artifacts into `Resolver`**

Resolve in rank order: repository file, structured GitHub release, labeled linked webpage, then best full document. Sanitize to terminal-safe plain text while retaining heading/list structure. Preserve artifact provenance and low-confidence fallback labels.

- [ ] **Step 5: Verify and commit**

Run: `go test -race ./internal/changelog -count=1`

Expected: PASS, no live internet required.

```bash
git add go.mod go.sum internal/changelog
git commit -m "feat: follow and extract linked changelog pages"
```

### Task 9: Feed and Inspector TUI with Stable Refresh

**Files:**
- Create: `internal/tui/model.go`
- Create: `internal/tui/messages.go`
- Create: `internal/tui/feed.go`
- Create: `internal/tui/inspector.go`
- Create: `internal/tui/styles.go`
- Test: `internal/tui/model_test.go`
- Test: `internal/tui/golden_test.go`
- Create: `internal/tui/testdata/golden/`

**Interfaces:**
- Consumes: `store.QueryFeed`, domain anchors, and changelog sections.
- Produces: `New(deps Dependencies) tea.Model`.
- Produces messages `FeedLoaded`, `DatasetChanged`, `ChangelogLoaded`, `WindowSize`, and `Notify` carrying request/selection IDs.

- [ ] **Step 1: Write model-state tests before rendering**

Send `WindowSize`, navigation, filter, roll-up, expansion, and `DatasetChanged` messages. Assert stable selection, viewport offset, pane focus, expanded groups, active filters, search text, and ignored stale changelog responses.

- [ ] **Step 2: Implement the model and async query commands**

Every async result carries its request ID and relevant event/package identity. On dataset change, capture an `Anchor`, requery, and call `RestoreSelection`. Debounce changelog requests until selection remains unchanged for 250 ms. Load persisted filters/grouping at initialization and write preference changes asynchronously without delaying input handling.

- [ ] **Step 3: Write wide/narrow golden tests**

Wide view uses left feed/right inspector. Below 100 columns, show the full-width feed and open inspector as a separate detail screen. Cover dark/light terminal styles, empty/loading/stale/error states, expanded roll-ups, and sync notifications.

- [ ] **Step 4: Implement Bubbles components and Lip Gloss styles**

Keep all color, border, spacing, and width calculations in `styles.go`. Status text occupies a fixed-height line so progress changes cannot shift the feed.

- [ ] **Step 5: Verify and commit**

Run: `UPDATE_GOLDEN=1 go test ./internal/tui && go test ./internal/tui -count=1`

Expected: the second command passes without modifying golden files.

```bash
git add internal/tui
git commit -m "feat: add stable package feed and inspector UI"
```

### Task 10: Background Sync Coordinator and Notifications

**Files:**
- Create: `internal/syncer/coordinator.go`
- Test: `internal/syncer/coordinator_test.go`
- Modify: `cmd/cicerone/main.go`

**Interfaces:**
- Consumes: Homebrew installed reader, Git sources, history indexer, store, and `Notify func(tea.Msg)` supplied by the TUI program.
- Produces: `Coordinator.Start(ctx context.Context)`, `Retry(ctx, source string)`, `EnsureRange(ctx context.Context, since time.Time)`, and events `SyncStarted`, `SyncCommitted`, `SyncFailed`.

- [ ] **Step 1: Write deterministic coordinator tests**

With channel-controlled fakes, prove cached feed is requested before refresh starts, installed state refresh precedes history fallback, at most two repository jobs run concurrently, each committed source emits one dataset change, one source failure does not cancel another, and cancellation stops all workers.

- [ ] **Step 2: Implement orchestration and status retention**

The coordinator owns one cancellable root context and a semaphore of two. Persist sync start/end, cursor, counts, last success, and bounded error text. Send notifications only after committed transactions. A horizon change sends an `EnsureRange(since time.Time)` request; index only the uncovered repository interval, then emit the normal committed-change event.

- [ ] **Step 3: Wire the real application**

`main.go` must open the store, construct adapters/services, create the TUI with cached-query capability, start Bubble Tea, and launch the coordinator through a Tea command after the initial model is ready. Close coordinator, subprocesses, and store in that order.

- [ ] **Step 4: Verify and commit**

Run: `go test -race ./internal/syncer ./internal/tui ./cmd/cicerone`

Expected: PASS.

```bash
git add internal/syncer cmd/cicerone/main.go
git commit -m "feat: synchronize package data in the background"
```

### Task 11: Confirmed Install and Upgrade Actions

**Files:**
- Create: `internal/homebrew/actions.go`
- Test: `internal/homebrew/actions_test.go`
- Create: `internal/tui/actions.go`
- Test: `internal/tui/actions_test.go`

**Interfaces:**
- Produces: `homebrew.Action{Kind Install|Upgrade; Package domain.PackageID; Type domain.PackageType}`.
- Produces: `Client.RunAction(ctx context.Context, action Action, output io.Writer) error`.
- Adds TUI messages `ActionRequested`, `ActionConfirmed`, `ActionOutput`, and `ActionFinished`.

- [ ] **Step 1: Write exact-argument and cancellation tests**

Formula install must execute `brew install --formula <name>`; cask install uses `brew install --cask <name>`; upgrades use corresponding `brew upgrade` commands. Reject names outside `[A-Za-z0-9@+_.\-/]+` and all names beginning with `-`. Test cancellation and bounded retained output.

- [ ] **Step 2: Implement streaming actions without a shell**

Capture stdout/stderr into a synchronized 1 MiB ring buffer and emit throttled output updates at most 20 times per second. Cancellation sends interrupt, waits two seconds, then kills the child.

- [ ] **Step 3: Write and implement modal model tests**

Require explicit confirmation. Preserve the underlying feed anchor. Disable duplicate actions while one runs. On success, refresh installed state and requery; on failure, retain output until dismissed and add a status-view error.

- [ ] **Step 4: Verify and commit**

Run: `go test -race ./internal/homebrew ./internal/tui -count=1`

Expected: PASS and no test executes a real `brew` process.

```bash
git add internal/homebrew/actions.go internal/homebrew/actions_test.go internal/tui/actions.go internal/tui/actions_test.go
git commit -m "feat: install and upgrade packages from the TUI"
```

### Task 12: End-to-End Fixtures, Recovery UX, and MVP Documentation

**Files:**
- Create: `internal/integration/app_test.go`
- Create: `internal/integration/testdata/`
- Create: `README.md`
- Create: `docs/cache-and-recovery.md`
- Create: `.github/workflows/ci.yml`
- Modify: `cmd/cicerone/main.go`

**Interfaces:**
- Consumes all prior public internal interfaces.
- Produces the tested MVP binary and user-facing operating documentation.

- [ ] **Step 1: Build a hermetic end-to-end fixture**

Create tiny core/cask Git repositories, fake Homebrew JSON, GitHub release JSON, and linked HTML changelogs. Drive initial indexing, cached restart, filter/group changes, changelog resolution, and a fake upgrade. Assert offline restart makes no HTTP/Git request and returns the same feed.

- [ ] **Step 2: Add corruption and migration-failure tests**

Open random bytes as the database and inject a failing migration. Assert Cicerone exits nonzero, prints the database path and backup/recovery commands, and leaves the original file byte-for-byte unchanged. Never auto-delete or auto-rebuild.

- [ ] **Step 3: Write user documentation**

README must document install/build, default keys, feed semantics, cache locations, local-versus-owned repositories, GitHub token behavior, supported macOS scope, and opt-in smoke tests. Recovery docs must include safe copy, integrity check, export, and explicit rebuild steps.

- [ ] **Step 4: Add CI and optional smoke test**

CI runs `gofmt` check, `go vet`, `go test -race ./...`, and `go build ./cmd/cicerone` on current macOS ARM64 and AMD64 runners. Add `go test -tags=homebrew_smoke ./internal/integration -run TestRealHomebrewReadOnly`; it may only read metadata.

- [ ] **Step 5: Run final verification**

Run:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./... -count=1
go build -trimpath ./cmd/cicerone
./cicerone --help
git status --short
```

Expected: format check is silent; vet, tests, and build exit 0; help documents keys/cache behavior; only intentional plan-tracking changes remain.

- [ ] **Step 6: Commit**

```bash
git add README.md docs/cache-and-recovery.md .github/workflows/ci.yml cmd/cicerone internal/integration
git commit -m "test: verify the Cicerone MVP end to end"
```

---

## Completion Gate

Before declaring the MVP complete:

1. Run the full Task 12 verification block and record its output.
2. Exercise the TUI manually against read-only real Homebrew metadata with network disabled after one successful sync.
3. Confirm a background sync inserts rows above and below the current selection without changing the selected stable identity or viewport-relative row.
4. Confirm an installed package with no event inside 30 days remains visible.
5. Confirm linked-page changelog provenance is visible and cached across restart.
6. Use the `requesting-code-review` skill for an independent requirements and quality review.
7. Use the `verification-before-completion` skill before any completion claim.

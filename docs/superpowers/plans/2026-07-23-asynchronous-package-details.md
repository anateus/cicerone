# Asynchronous Package Details and Document Cache Implementation Plan

**Goal:** Deliver cached-first package information, README, and changelog content through a debounced, deduplicated, throttled background download system with incremental inspector updates and aggregate progress.

**Supersedes:** Tasks 7, 8, and the changelog/detail portions of Task 9 in `2026-07-16-cicerone-implementation.md`. Completed code from those tasks is migration input, not a contract to preserve internally.

**Design:** `docs/superpowers/specs/2026-07-23-asynchronous-package-details-design.md`

## Slice 1: Generalize the durable document cache

**Outcome:** Existing changelog cache data and new README documents share one store API without data loss.

**Files:**

- Create `internal/store/schema/005_package_documents.sql`
- Create `internal/store/documents.go`
- Create `internal/store/documents_test.go`
- Modify `internal/store/store_test.go`
- Retain `internal/store/changelog.go` temporarily as a compatibility adapter

**Work:**

Add the generalized document, package-link, section, attempt, package-info, and FTS schema described by the design. Migrate every version-4 changelog row and preserve IDs/provenance where practical. Expose cache-snapshot, save-document, link-package, save-section, retry-state, freshness-update, and package-info operations. Make document kind explicit and constrained. Keep the old changelog methods delegating to the new API until callers move.

**Verification:**

```bash
go test ./internal/store -run 'Test(Document|Migration|Changelog)' -count=1
```

Assert a populated version-4 database opens at version 5, preserves raw/extracted bytes and sections, retains FTS matches, and can add a README linked to the same package.

## Slice 2: Extract a safe validator-aware URL downloader

**Outcome:** One reusable downloader owns HTTP safety and conditional download behavior.

**Files:**

- Create `internal/download/fetcher.go`
- Create `internal/download/fetcher_test.go`
- Modify `internal/changelog/fetcher.go`
- Modify `internal/changelog/fetcher_test.go`

**Dependencies:** Slice 1's document validator types.

**Work:**

Move the verified safety behavior from `internal/changelog/fetcher.go` behind a document-neutral API. Preserve scheme, credentials, redirect, DNS/IP, timeout, media-type, and body-size enforcement. Accept ETag/Last-Modified validators and represent `304 Not Modified` distinctly. Keep changelog call sites compiling through a thin adapter during the transition.

**Verification:**

```bash
go test -race ./internal/download ./internal/changelog -run 'TestFetch' -count=1
```

Cover conditional headers/304, redirect revalidation, unsafe addresses after DNS resolution, unsupported media, cancellation, timeout, and oversized bodies.

## Slice 3: Add the bounded priority download queue

**Outcome:** URL work is debounced before admission, deduplicated after admission, bounded, prioritized, and observable.

**Files:**

- Create `internal/download/queue.go`
- Create `internal/download/queue_test.go`

**Dependencies:** Slice 2.

**Work:**

Implement a runtime-owned queue keyed by canonical URL plus a non-secret retrieval profile. Give requests a priority, document intent, validators, and consumer identity. Coalesce pending/active duplicates, promote pending priority, fan completion out to consumers, and expose active/pending logical-job counts. Default to four global workers, 128 pending requests, two active requests per host, and a 100 ms per-host start interval. Make all four limits injectable in tests. When full, replace only a lower-priority pending speculative item. Make `Close` stop admission, cancel workers, and wait.

Do not persist queue entries or include authorization values in queue keys, events, logs, or stored provenance.

**Verification:**

```bash
go test -race ./internal/download -run 'TestQueue' -count=1
```

Use blocking fakes and a fake clock to prove priority order, canonical-URL deduplication, promotion, fan-out, capacity behavior, global/per-host bounds, start throttling, accurate progress transitions, and clean cancellation.

## Slice 4: Add structured per-package Homebrew information

**Outcome:** The cache can immediately display the last successful package metadata while a fresh typed Homebrew query runs independently.

**Files:**

- Create `internal/homebrew/info.go`
- Create `internal/homebrew/info_test.go`
- Modify `internal/store/documents.go`
- Modify `internal/store/documents_test.go`
- Add Homebrew JSON fixtures under `internal/homebrew/testdata/`

**Dependencies:** Slice 1.

**Work:**

Run `brew info --json=v2 <validated-package>` without a shell and decode only inspector fields. Normalize formula and cask results into a domain type and persist the latest successful snapshot. Preserve old cached info on command, decode, or store failure.

**Verification:**

```bash
go test ./internal/homebrew ./internal/store -run 'Test(PackageInfo|Info)' -count=1
```

Cover formula, cask, full token, caveats, empty result, malformed package name, malformed JSON, subprocess failure, and stale-cache preservation.

## Slice 5: Resolve and cache README documents through the queue

**Outcome:** A package repository README is discovered, downloaded, extracted, and cached independently of changelog work.

**Files:**

- Create `internal/documents/readme.go`
- Create `internal/documents/readme_test.go`
- Create fixtures under `internal/documents/testdata/`
- Modify `internal/changelog/resolver.go`
- Modify `internal/changelog/resolver_test.go`

**Dependencies:** Slices 1–3.

**Work:**

Introduce document-neutral package/repository references. Discover conventional README filenames case-insensitively from the known upstream GitHub repository, prefer raw Markdown, and use safe HTML extraction only as fallback. Submit URLs through the queue with current-selection priority and persist provenance through the generalized store. Convert the changelog resolver's actual URL fetches to the same queue while retaining version matching and GitHub Releases behavior.

Do not enqueue every discovered candidate at equal priority: submit the best candidate first and expand the bounded discovery graph only after failure or insufficient content.

**Verification:**

```bash
go test -race ./internal/documents ./internal/changelog ./internal/download -count=1
```

Cover Markdown README, HTML fallback, absent README, shared URL deduplication, bounded candidate expansion, cached retry suppression, and independent README/changelog outcomes.

## Slice 6: Introduce a package-detail coordinator

**Outcome:** One package selection returns a cached snapshot immediately and schedules independent refresh jobs after the debounce.

**Files:**

- Create `internal/details/coordinator.go`
- Create `internal/details/coordinator_test.go`
- Modify `cmd/cicerone/runtime.go`
- Modify `cmd/cicerone/main.go`
- Modify `cmd/cicerone/main_test.go`

**Dependencies:** Slices 3–5.

**Work:**

Define a cached `Snapshot` containing package info, README, version-matched changelog, freshness, provenance, and per-part errors. The coordinator reads this snapshot inside a Bubble Tea command, then schedules typed Homebrew and document refreshes. It publishes part-completion and aggregate detail-progress messages through the existing program-send seam. Aggregate counts combine typed Homebrew jobs with deduplicated URL jobs.

Construct the downloader, queue, README resolver, changelog resolver, and detail coordinator in `newRuntime`. Close the detail coordinator/queue before the store. Replace `changelogLoader` wiring only after its tests have equivalent coordinator coverage.

**Verification:**

```bash
go test -race ./internal/details ./cmd/cicerone -count=1
```

Assert cached-first return, three independent refresh paths, obsolete-consumer tolerance, persisted late results, partial failure, accurate progress publication, and shutdown-before-store ordering.

## Slice 7: Make Bubble Tea details incremental and selection-safe

**Outcome:** Navigation admits work only for a settled selection and currently relevant parts appear as soon as each completes.

**Files:**

- Modify `internal/tui/model.go`
- Modify `internal/tui/messages.go`
- Modify `internal/tui/model_test.go`
- Modify `internal/tui/actions_test.go` where dependency construction changes

**Dependencies:** Slice 6.

**Work:**

Replace `ChangelogSource`, `ChangelogDebounced`, and monolithic `ChangelogLoaded` state with a `DetailSource`, one 250 ms selection debounce, cached snapshot loading, independent part-completion messages, and download progress. Every selection-specific message carries request, selection, package, and event/version identity. Ignore obsolete UI updates without canceling cache persistence. Clear old visible details immediately on a true cache miss, but retain and label cached stale content during refresh.

Compose detail progress with existing sync status instead of reusing `m.err` for document failures.

**Verification:**

```bash
go test ./internal/tui -run 'Test(Detail|Selection|Progress|Debounce|Stale)' -count=1
```

Prove rapid navigation schedules only the settled package, out-of-order responses are ignored, partial results render, counts follow logical jobs, and feed selection/viewport behavior remains unchanged.

## Slice 8: Render README and changelog Markdown

**Outcome:** The inspector presents package information plus switchable, responsive README and changelog documents.

**Files:**

- Create `internal/tui/markdown.go`
- Create `internal/tui/markdown_test.go`
- Modify `internal/tui/inspector.go`
- Modify `internal/tui/styles.go`
- Modify `internal/tui/model.go`
- Modify `internal/tui/golden_test.go`
- Modify `internal/tui/testdata/golden/*.golden`
- Modify `go.mod`
- Modify `go.sum`

**Dependencies:** Slice 7.

**Work:**

Add Glamour behind a small renderer interface. Render sanitized Markdown for the available pane width and light/dark style, with plain-text fallback. Keep a compact package-information header visible and add README/Changelog section controls, defaulting to Changelog for update events. Preserve independent loading, stale, missing, source, and error labels. Rerender on resize and theme change; do not persist rendered ANSI.

Update the fixed-height status to show both repository synchronization and `N active · M queued` detail work without exceeding terminal width.

**Verification:**

```bash
UPDATE_GOLDEN=1 go test ./internal/tui
go test ./internal/tui -count=1
git diff --check -- internal/tui/testdata/golden
```

Inspect wide/narrow, light/dark, README, changelog, partial loading, stale-cache, renderer-fallback, and simultaneous sync/download progress goldens.

## Slice 9: Remove compatibility paths and verify end to end

**Outcome:** Production uses only the generalized cache and detail pipeline, with documentation matching runtime behavior.

**Files:**

- Remove or reduce `internal/store/changelog.go` compatibility methods
- Remove or reduce `internal/changelog/fetcher.go` compatibility adapter
- Modify `README.md`
- Modify `docs/cache-and-recovery.md`
- Modify `internal/integration/app_test.go`
- Add/update fixtures under `internal/integration/testdata/`

**Dependencies:** Slices 1–8.

**Work:**

Delete obsolete direct changelog-loading paths after verifying no imports or runtime wiring remain. Extend integration coverage through fake Homebrew, local HTTP, SQLite restart, and Bubble Tea messages. Document cached package details, offline behavior, progress counts, cache recovery, and the non-persistent queue.

**Verification:**

```bash
rg -n 'ChangelogSource|ChangelogDebounced|changelogLoader' internal cmd
go test -race ./... -count=1
go vet ./...
```

The search should return no production references to the superseded loading path. The integration test must demonstrate cached restart with no Homebrew, HTTP, or Git request while rendering the same package info, README, and changelog.

## Coordination and rollout notes

- Slices 1–3 all touch downloader/cache contracts and should be landed sequentially.
- Slices 4 and 5 can proceed independently after their prerequisites, but both converge in Slice 6 runtime wiring.
- Migration 005 is forward-only. Preserve a pre-migration database copy through the existing recovery path; do not add automatic rollback or destructive rebuild behavior.
- The untracked root-level `cicerone` binary is outside this plan and must not be modified or committed.

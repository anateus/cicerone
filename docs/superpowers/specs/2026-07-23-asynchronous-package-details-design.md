# Asynchronous Package Details and Document Cache Design

**Status:** Supersedes the package-detail, changelog-loading, and inspector-rendering portions of `2026-07-16-cicerone-design.md`. The history feed, action, and synchronization design remains in force.

## Problem

Cicerone's inspector currently loads one version-matched changelog after a 250 ms selection debounce and prints it as plain text. It cannot show the package information available from `brew info --json=v2`, cannot retrieve or cache a README, and turns discovery, download, extraction, and selection-specific rendering into one opaque request. Navigating quickly can also initiate work for packages the user no longer cares about, while the UI provides no aggregate indication of document work in progress.

The detail view needs to remain useful offline and responsive while package information and documents arrive independently in the background.

## Behavior

### Detail contents

For the selected package, the inspector shows:

- a compact package-information header derived from structured `brew info --json=v2 <package>` output;
- a README document for the package's upstream repository;
- the changelog section matching the selected update event's version;
- source, freshness, and stale/error state for independently cached content.

README and Changelog are switchable sections. Changelog is initially selected for an update event. Package information remains visible above either document.

### Cached-first loading

Selection reads package information, README, and changelog from SQLite immediately. A cache miss displays a local placeholder for only the missing part. A stale entry remains visible while refresh work runs. Failure preserves the last successful content and marks that part stale; it does not replace unrelated detail content with a global error.

README content is package-scoped and represents the repository's current default branch. Changelog sections remain package-and-version scoped. Rendered terminal output is not persisted because it depends on pane width, color mode, and renderer behavior.

### Admission, queueing, and throttling

After the selection remains unchanged for 250 ms, the detail coordinator requests refreshes for package information, README, and changelog. The package-information refresh is a typed Homebrew subprocess job. README and changelog discovery submit typed URL requests to a shared downloader.

The downloader:

- accepts only HTTP and HTTPS requests that pass the existing network safety checks;
- canonicalizes URLs and coalesces queued or active work by canonical URL plus a non-secret retrieval profile (for example, ordinary document versus authenticated GitHub API representation);
- promotes an existing request when a newer, higher-priority consumer needs it;
- initially permits four active requests globally and 128 pending requests;
- retains the existing limit of two active requests per host and initially enforces at least 100 ms between request starts for one host;
- uses cache validators when available and treats `304 Not Modified` as a successful freshness update;
- applies cached retry/backoff state before admitting known failing URLs;
- gives the current selection higher priority than speculative or previously selected work;
- never blocks Bubble Tea's update loop.

The selection debounce limits enqueueing caused by navigation. URL deduplication and host throttling limit work after admission. A full queue may discard the lowest-priority pending speculative request, but it must not discard active work or corrupt cached content.

Requests may have multiple consumers, such as README discovery and changelog discovery sharing a repository document. Completion is persisted once, then published to all interested package-detail consumers.

### Incremental availability

Package information, README, and changelog complete independently. Each completion carries its request identity, package identity, document kind, and selected event/version when applicable. The model ignores stale selection-specific responses but keeps their successfully persisted cache entries available for later navigation.

The inspector rerenders whenever currently selected content becomes available. Markdown is rendered through a shared, width-aware Glamour adapter. Renderer failures fall back to sanitized plain text.

### Progress

The fixed-height status line reports aggregate package-detail work without shifting the feed or inspector. Its counts include typed Homebrew metadata jobs and deduplicated URL jobs. While work exists it includes the number of active and pending jobs, for example:

`Loading package details · 2 active · 4 queued`

Repository synchronization and package-detail progress may coexist in the same status line. Counts represent logical jobs after deduplication, not the number of interested consumers. Completion and failure remove jobs from the counts. The status returns to the ordinary ready/sync state when both counts reach zero.

### Lifecycle

The downloader and detail coordinator start with the application runtime and share its root context. Shutdown stops admission, cancels active requests, drains bookkeeping, and does not close SQLite until workers have stopped. Queue contents are process-local; successfully downloaded artifacts and retry state are durable.

## Storage

A new generalized document cache replaces changelog-specific artifact ownership:

- `package_documents`: immutable fetched bodies and extracted Markdown/text, identified by canonical URL plus content hash, with media type, validators, source URL, discovery parent, fetch time, extraction state, and document kind;
- `package_document_links`: many-to-many links between packages and documents, with kind and discovery priority;
- `document_sections`: version-matched sections and confidence/provenance for changelog documents;
- `document_attempts`: durable success/failure/backoff history keyed by canonical URL;
- `package_info`: the latest successfully decoded structured Homebrew information and fetch time per package.

The migration preserves existing changelog artifacts, package links, sections, attempts, and FTS-searchable extracted text. Existing cache data must remain readable after migration, and failed migration checks continue to use Cicerone's copy-before-open recovery path.

## Constraints

- Package names remain validated and are passed as subprocess arguments without a shell.
- Remote content is untrusted. Existing redirect, DNS/IP, media-type, timeout, and 10 MiB body limits remain in force.
- Queue priority affects pending work only; active HTTP requests are not preempted.
- Cache writes continue through the store's serialized writer.
- The queue is not an application restart job ledger. Durable cached artifacts and retry state provide recovery; navigation can reconstruct demand.
- The UI must remain usable with no network, no GitHub token, missing README/changelog sources, or a failing Homebrew query.

## Acceptance Criteria

1. Selecting a previously visited package renders cached package information, README, and matching changelog without waiting for a subprocess or network request.
2. Moving through selections faster than 250 ms admits refresh work only for the settled selection.
3. Repeated demand for the same canonical URL and retrieval profile produces at most one queued or active HTTP request and one logical progress job.
4. Current-selection work runs ahead of pending speculative work without interrupting active requests.
5. Global worker, per-host concurrency, per-host start-rate, queue-capacity, timeout, redirect, address, media-type, and body-size bounds are enforced by tests.
6. README, changelog, and package information can complete or fail independently; successful parts appear without waiting for the others.
7. A failed refresh leaves prior content visible with stale/error provenance and records retry eligibility.
8. Navigating away ignores obsolete UI responses while preserving successfully cached results for a later visit.
9. The fixed-height status shows accurate active and queued logical-job counts and returns to the normal state at zero.
10. Markdown reflows after terminal resize and changes style with light/dark mode; rendering failure produces readable sanitized text.
11. Existing changelog cache rows survive migration and remain queryable through the generalized document API.
12. Runtime shutdown cancels workers and waits for their bookkeeping before closing the store.

## Verification Seams

- Downloader unit tests use a controlled HTTP transport/resolver and deterministic clock.
- Queue tests use blocking fake fetches to assert ordering, deduplication, capacity, promotion, throttling, cancellation, and progress events.
- Store migration tests open a version-4 fixture containing changelog artifacts and assert generalized document queries.
- Homebrew adapter tests use the existing fake runner and JSON fixtures.
- Bubble Tea model tests deliver cached snapshots, progress events, and out-of-order completions.
- Golden tests cover cached, partially loading, stale, README, changelog, narrow, wide, light, and dark inspector states.
- Runtime tests assert worker startup, notification wiring, and shutdown ordering.

## Non-goals

- Eagerly downloading documents for the entire Homebrew catalog.
- Persisting the in-memory queue across application restarts.
- Executing JavaScript or adding browser automation.
- Supporting arbitrary URL schemes or user-authored download commands.
- Rendering remote images in the terminal.
- GitLab- or Bitbucket-specific release APIs; their ordinary repository files and safe URLs remain usable.

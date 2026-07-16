# Cicerone Design

## Summary

Cicerone is a macOS-first terminal application for discovering, reviewing, installing, and upgrading Homebrew formulae and casks. Its defining feature is a feed sorted by recent package updates, derived from the Git history of `homebrew-core` and `homebrew-cask`. Consecutive versions of the same package can be rolled into one expandable feed entry.

Cicerone opens immediately from a durable local cache. Git indexing and changelog retrieval happen in the background and become visible without changing the user's selection, scroll position, filters, sort order, or grouping mode.

The first release supports:

- All formulae and casks, with installed packages highlighted and filterable.
- A 30-day default horizon for non-installed packages.
- The latest matching update for every installed package regardless of age.
- Version events by default, with revision and metadata events available through filters.
- Rolled-up consecutive versions by default, expandable in place.
- Package details, version-specific changelog content, installation, and upgrades.
- macOS behavior, with platform-specific paths and command execution isolated for future Linuxbrew support.

Uninstalling packages, managing taps, and switching between side and bottom inspector layouts are outside the first release.

## Technology choices

Cicerone is a single Go binary. The TUI uses Bubble Tea v2 for its event loop, Bubbles for reusable interactive components, and Lip Gloss for layout and styling. Bubble Tea's message/command model provides a natural boundary between background work and deterministic UI state updates.

SQLite is the application database. It suits Cicerone's indexed, low-latency feed queries, incremental transactional updates, mutable installed state and preferences, and full-text search better than an analytical database such as DuckDB. SQLite runs in WAL mode so the TUI can read while a background sync writes. All writes are serialized through a store-owned writer. The bundled SQLite version must contain the WAL-reset fix from SQLite 3.51.3 or an official patched backport.

The initial driver is `modernc.org/sqlite` to preserve pure-Go builds and straightforward cross-compilation. The store layer hides driver details.

Cicerone invokes the system `git` executable through a narrowly defined adapter instead of implementing Git in Go. This provides faithful support for the history and diff operations Cicerone needs and works with both users' existing tap repositories and Cicerone-owned mirrors.

## Architecture

The application is divided into four internal layers:

1. **Domain:** Package and update types, stable identities, event classification, grouping rules, filters, and selection anchors. It has no dependency on the TUI, Git, Homebrew, HTTP, or SQLite.
2. **Store:** Embedded schema migrations, transactions, repository cursors, cache policies, and feed/detail queries.
3. **Adapters:** Homebrew commands, Git commands, HTTP fetching, changelog extraction, filesystem paths, and OS-specific behavior.
4. **TUI:** Bubble Tea models and commands, Bubbles components, Lip Gloss styles, notifications, list/inspector navigation, and persisted user preferences.

Background services sit between adapters and the store:

- The sync coordinator selects sources and schedules bounded fetch/index jobs.
- The history indexer converts Git commits and package definition changes into classified update events.
- The changelog resolver discovers, fetches, extracts, version-matches, and caches upstream changelog content.

SQLite is the synchronization boundary. A source sync writes one transaction and emits a dataset-changed message only after commit. The UI then performs a fresh query and restores its state by stable identity rather than row number.

## Package and history model

An update event is immutable and records:

- Stable package identity, current name, and formula/cask type.
- Old and new versions and revisions when present.
- Classification: `version`, `revision`, or `metadata`.
- Source repository, definition path, Git commit, and commit timestamp.
- Diagnostic classification metadata when a change is ambiguous.

Aliases and confidently detected renames map historical names to one stable package identity. Ambiguous renames are retained as diagnostic information and do not silently merge identities.

The principal tables are:

- `packages`
- `package_aliases`
- `update_events`
- `installed_packages`
- `repositories`
- `repository_ranges`
- `sync_runs`
- `changelog_artifacts`
- `changelog_sections`
- `changelog_attempts`
- `preferences`

Exact columns, indexes, and foreign keys are implementation-plan concerns, but the schema must enforce uniqueness of source repository plus commit plus package change so indexing is idempotent.

## Feed semantics

Feed construction has a precise order:

1. Apply package type, installed state, event type, text search, and time filters.
2. For non-installed packages, include matching events inside the configured horizon. The default is 30 days.
3. For each installed package, include its newest matching event even when it predates the horizon.
4. Combine and sort events newest-first, using stable identity as a deterministic tie-breaker.
5. In rolled-up mode, collapse only adjacent events belonging to the same package in this filtered and sorted result.

The newest event represents a rolled-up group and shows its event count. Expanding it reveals the constituent versions in order. Because grouping happens after filtering, changing a filter may change which package events are adjacent and therefore grouped. Rolled-up mode is the default and the user's later choice is persisted.

Version events are visible by default. Revision and metadata events can be enabled independently.

## Repository discovery and history ingestion

Cicerone first searches for usable local clones of `homebrew-core` and `homebrew-cask`. Local repositories are read-only sources: Cicerone does not fetch, checkout, reset, or otherwise mutate them. If a usable local repository is absent, Cicerone maintains its own partial mirror with blob filtering in its cache directory.

Initial indexing is deliberately bounded:

- Index commits covering the active horizon, initially 30 days.
- Separately locate the newest event of each classification for every installed package when that event is not covered by the indexed range. This guarantees that enabling revision or metadata filters does not make an older installed package disappear.
- Persist the indexed time ranges and last successfully indexed commit for each repository.
- Extend indexed history backward when the user chooses a longer horizon, retaining all parsed events.

The indexer examines changed formula/cask paths and fetches only the definition blobs required to compare the package before and after a commit. Parsed update events in SQLite are the durable Git-log cache; exploring an already indexed range requires neither a Git fetch nor reparsing.

Incremental sync starts from the last successful repository cursor. If history was rewritten, Cicerone finds an available merge base and reconciles the affected bounded range transactionally. If no safe merge base exists, it marks the source as requiring a controlled reindex rather than mixing incompatible histories.

Deleted, renamed, migrated, and aliased definitions are tested explicitly. Classification failures do not abort an entire repository sync; they are recorded with enough context for diagnostics and future reprocessing.

## Changelog discovery and extraction

Changelog retrieval is lazy and follows a ranked, bounded discovery graph:

1. Resolve upstream repository candidates from Homebrew metadata.
2. Search conventional changelog filenames in the repository.
3. Parse a changelog file for the selected version and for links labeled as changelog, changes, release notes, or version details.
4. For GitHub repositories, query GitHub Releases for likely version tag forms and prefer the structured release body over rendered-page scraping.
5. Follow a relevant changelog webpage linked by Homebrew metadata, a repository file, or a release body.
6. Extract readable primary content from HTML and match headings or tags to the selected version.
7. If no exact section can be identified confidently, display the best cached full document with its source and a low-confidence label.

GitLab and Bitbucket release adapters are deferred; their repository files and ordinary linked webpages still work through the generic pipeline.

Every fetched input is stored as a separate changelog artifact with its URL, media type, validators, content hash, discovery parent, fetch time, raw content, extracted content, and extraction status. Version sections retain provenance to the source artifact. A failed refresh preserves prior cached content and marks it stale.

HTML extraction is hidden behind a `ContentExtractor` interface. `go-trafilatura` is the leading initial candidate, but a fixture corpus will compare it with a Readability-based implementation before the dependency is made permanent. Extractor changes can reprocess cached raw artifacts without network access.

Network discovery is constrained:

- Only HTTP and HTTPS are allowed.
- Redirects, request duration, response size, discovery depth, and candidates per package/version are bounded.
- Discovery follows at most two links beyond the initial resolved source.
- Loopback, private, link-local, and unsafe resolved addresses are blocked.
- Requests use validators, per-host concurrency limits, retries, and backoff.
- Browser automation and JavaScript execution are excluded from the first release.
- Extracted markup is sanitized before terminal rendering.
- Failed attempts are cached with retry eligibility so dead sources are not repeatedly queried.

## TUI layout and interaction

The initial layout is a feed on the left and a persistent inspector on the right. The inspector shows package metadata, installed and latest versions, event classification, the selected version's changelog section, source provenance, and available actions. On narrow terminals it becomes a separate detail screen. Switching between a side inspector and bottom preview is a future preference.

The application renders cached data immediately at startup. Header/footer status communicates background activity without shifting the main layout. Example statuses include `Syncing core…` and `18 new updates · core synced 14:32`.

When new data commits, the UI preserves:

- The selected event or rolled-up group by stable identity.
- The selected child within an expanded group when it still exists.
- The selected row's viewport-relative position.
- Expanded groups, focused pane, filters, sort, grouping mode, and search text.

If the selected item no longer matches the active query, the nearest surviving row is selected. New data never resets the feed to the first row.

Selecting a package shows cached changelog content immediately. A missing or expired artifact schedules a debounced background retrieval so rapid list navigation does not create a request storm. Completion updates only the inspector state relevant to the currently selected package/version.

Install and upgrade actions require confirmation and run `brew install` or `brew upgrade` as cancellable subprocesses. A modal shows live output while preserving the underlying feed state. Success refreshes installed metadata and affected rows. Failure retains the command output for inspection and emits a concise notification.

## Background work and errors

Startup schedules installed-state refresh followed by repository fetch/index jobs with bounded concurrency. Repository transactions are independent so one source may become current even if another fails.

Sync, classification, network, and changelog errors are non-fatal when cached data remains usable. A notification/status view retains concise errors, actionable retry controls, timestamps, and the last successful sync. Stale data is labeled rather than removed.

Database migration or integrity errors fail safely with a diagnostic and recovery instructions. Cicerone does not silently delete or rebuild the user's cache. Subprocess arguments are constructed without a shell, and untrusted package metadata is never interpreted as commands.

## Testing strategy

Testing is organized around layer boundaries:

- Table-driven domain tests cover version parsing, update classification, post-filter adjacency grouping, stable tie-breaking, and selection restoration.
- Temporary Git repositories cover commit traversal, definition changes, renames, rewritten history, shallow-range expansion, and local-versus-mirror selection.
- Temporary SQLite databases cover migrations, constraints, idempotent ingestion, transactional visibility, feed semantics, cache expiry, and WAL behavior.
- Changelog fixtures cover repository files, GitHub release bodies, linked webpages, redirects, ambiguous headings, extractor comparisons, malformed HTML, sanitization, and version-match confidence.
- Bubble Tea model tests send messages and assert state without a real terminal.
- Golden tests cover key Lip Gloss views at representative wide and narrow terminal sizes.
- Adapter integration tests use fake `brew` and `git` executables plus local HTTP servers.
- An opt-in macOS smoke test reads a real Homebrew installation. Automated tests never install, upgrade, or uninstall real packages.

## Success criteria

The first release is successful when a user can start Cicerone and immediately browse cached formula and cask updates sorted newest-first; see installed packages even when their latest update is older than the active horizon; expand rolled-up version groups; filter event types; inspect a version-specific cached changelog with provenance; and safely install or upgrade a package.

A background repository or changelog refresh must not reset selection, scroll position, filters, expansion state, or pane focus. Previously indexed history and fetched changelog artifacts must remain usable offline.

## Deferred work

- Linuxbrew runtime support.
- Uninstall actions and tap management.
- Side/bottom inspector switching.
- GitLab and Bitbucket structured release APIs.
- Browser automation for JavaScript-only changelog sites.
- Analytical exports or optional DuckDB-based analysis.

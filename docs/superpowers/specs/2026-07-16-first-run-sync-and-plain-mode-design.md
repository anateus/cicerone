# First-Run Sync and Plain Mode Design

## Goal

Make the real Cicerone application complete its first Homebrew synchronization successfully, and add a small plaintext execution mode that can exercise the same production services without an interactive terminal.

## Priority and Scope

The application fix is primary. Plain mode is an operational and diagnostic surface, not a separate test framework. It must reuse production wiring and must not introduce package mutations.

## First-Run History State

`SyncStarted` legitimately creates a repository row before history indexing begins. A repository row whose `head_commit` is empty represents sync bookkeeping only, not an indexed history cursor. `HistoryState` will report that state as unindexed so the indexer performs its initial bounded scan instead of calling `git merge-base` with an empty revision.

A regression test will reproduce the production sequence: create the sync run, then index a real temporary Git repository. It will assert successful indexing and a persisted non-empty cursor.

## Plaintext Execution

The command will accept `--plain`. It will:

1. Open the normal user database and caches.
2. Query and print the cached feed immediately.
3. Run one production Homebrew/Git synchronization using the same coordinator, store, repository, and history components as the TUI.
4. Query and print the refreshed feed and per-source results.
5. Exit after all source work completes.

Output will be deterministic, terminal-safe text suitable for logs. Source failures will be printed with their underlying errors and cause a nonzero exit. An empty feed is valid if synchronization succeeds.

Plain mode may read Homebrew metadata, clone/fetch Cicerone-owned mirrors, and access changelog sources when requested by shared services. It must never install, upgrade, or uninstall packages.

## Architecture

Application construction will be factored into shared production services with explicit lifecycle ownership. Interactive and plain modes will use the same store, Homebrew client, source discovery, repositories, history indexers, and sync coordinator. The TUI continues to defer external work until cached rendering; plain mode prints cached data before starting its one-shot sync.

The one-shot path will wait on coordinator notifications rather than polling or sleeping. Completion is reached after discovery and every scheduled source reports committed or failed. Context cancellation will stop outstanding work and return a nonzero result.

## Testing and Verification

- Unit regression for an empty repository cursor created by `SyncStarted`.
- Coordinator/plain-mode tests using deterministic fakes for cached-before-sync ordering, completion, failures, and exit status.
- Existing full race suite.
- Build the command and run `./cicerone --plain` against the developer's real Homebrew metadata and Cicerone cache.
- Confirm the original `merge base  <head>` error no longer occurs and no package mutation command is executed.

## Non-Goals

- TUI key-injection automation.
- A fixture-only alternate application.
- JSON output or long-running watch mode.
- Automated Homebrew install, upgrade, or uninstall operations.

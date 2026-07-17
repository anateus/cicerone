# Cicerone

Cicerone is a macOS terminal feed for recent Homebrew formula and cask changes. It opens from a durable SQLite cache first, then refreshes installed state and Git history in the background.

## Build and run

Requirements: macOS, Go 1.26.5 or newer, Homebrew, and Git.

```sh
go build -trimpath ./cmd/cicerone
./cicerone
```

Install the binary on your `PATH` with either:

```sh
go install ./cmd/cicerone
# or, after the build above:
install -m 0755 ./cicerone /usr/local/bin/cicerone
```

If `/usr/local/bin` is not writable, choose a user-owned directory already listed in `PATH` (for example `~/bin`).

Run `./cicerone --help` without opening the TUI. Cicerone's MVP is macOS-only; Linuxbrew support is deferred.

## Keys

| Key | Action |
| --- | --- |
| `j`, `↓` | Move down |
| `k`, `↑` | Move up |
| `enter` | Focus/open package details |
| `tab` | Switch panes in a wide terminal |
| `space` | Expand or collapse a rolled-up event |
| `a` | Request install or upgrade; a confirmation is always required |
| `y`, `enter` | Confirm a pending Homebrew action |
| `n`, `esc` | Cancel or close the current modal/detail |

## Feed behavior

The default feed contains version events from the last 30 days. An installed package remains visible through its newest matching event even if that event is older than the horizon. Results are sorted newest-first with a stable identity tie-breaker. Roll-up occurs only for adjacent events of the same package after filters are applied, so changing a filter can change grouping.

Cicerone displays cached rows immediately. Background commits requery the feed while preserving the selected stable event and its viewport-relative row. Installed versions and upgrade availability come from `brew info --json=v2 --installed`.

## Local data and repositories

- Database: `~/Library/Application Support/cicerone/cicerone.db`
- Cicerone-owned Git mirrors and other cache data: `~/Library/Caches/cicerone/`

Cicerone prefers usable local `homebrew-core` and `homebrew-cask` tap clones. Those user/Homebrew-owned repositories are read-only: Cicerone never fetches, checks out, resets, or rewrites them. If a local clone is unavailable, Cicerone creates and fetches its own bare, filtered mirror under its cache directory.

Cached feed and changelog content remains readable offline. See [Cache and recovery](docs/cache-and-recovery.md) before moving or rebuilding a damaged database; Cicerone never silently deletes it.

## GitHub access

Public GitHub release and repository APIs work without authentication but are rate-limited. Set `GITHUB_TOKEN` to send `Authorization: Bearer …` on GitHub API requests. Cicerone does not persist or print the token. Use a minimally scoped token suitable for repositories whose changelogs you inspect.

## Tests

```sh
go test ./...
go test -race ./...
```

The default suite is hermetic and never mutates a real Homebrew installation. On a macOS host with Homebrew, this opt-in smoke test only reads installed metadata:

```sh
go test -tags=homebrew_smoke ./internal/integration -run TestRealHomebrewReadOnly
```

It never installs, upgrades, uninstalls, fetches, checks out, or resets anything.

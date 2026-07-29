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

Run `./cicerone --plain` for a one-shot plaintext feed. It prints cached rows,
performs real read-only Homebrew metadata synchronization, prints refreshed
rows, and exits. This may update Cicerone's database and Cicerone-owned Git
caches, but it never installs, upgrades, or uninstalls Homebrew packages.
On a first run, history is streamed into durable 100-commit batches. Plain mode
prints numeric progress and newly queryable rows after each batch. Interrupted
scans retain valid rows without advancing the completed cursor, so retry is safe
and idempotent.

## Keys

| Key | Action |
| --- | --- |
| `j`, `↓` | Move down; scroll down while reading package details |
| `k`, `↑` | Move up; scroll up while reading package details |
| `1`, `2`, `3` | Show Formulae, Casks, or both |
| `/` | Enter package search; typing filters after a short debounce |
| `tab` while searching | Broaden search through names, descriptions, changelogs, and READMEs |
| `enter`, `esc` while searching | Apply and leave search, or leave search input |
| `h`, `←` / `l`, `→` | Switch toward package details; scroll horizontally while reading |
| `enter` | Enter or leave package-detail reading mode |
| `esc` | Leave package-detail reading mode; quit from the feed |
| `tab` | Switch panes in a wide terminal |
| `[`, `]` | Show the cached README or version changelog in package details |
| `m` | Load 10 more GitHub releases when offered at the end of a release-backed changelog |
| `space` | Expand or collapse a rolled-up event |
| `a` | Request install or upgrade; a confirmation is always required |
| `y`, `enter` | Confirm a pending Homebrew action |
| `n`, `esc` | Cancel or close the current modal/detail |
| Mouse click / wheel | Select tabs and packages, activate visible controls, or scroll the pane under the pointer |

## Feed behavior

The default feed contains version events from the last 30 days. An installed package remains visible through its newest matching event even if that event is older than the horizon. Results are sorted newest-first with a stable identity tie-breaker. Roll-up occurs only for adjacent events of the same package after filters are applied, so changing a filter can change grouping.

Search starts with package names. `tab` cycles through cumulative scopes: names; names and cached descriptions; those plus cached changelogs; then those plus cached READMEs. Unquoted terms are prefix searches, so `rip gre` matches tokens beginning with `rip` and `gre`. Surround the whole query with quotes for a non-prefix phrase search, such as `"rip grep"`. Document and description results are limited to content already present in Cicerone's durable cache.

Cicerone displays cached rows immediately. Background commits requery the feed while preserving the selected stable event and its viewport-relative row. Installed versions and upgrade availability come from `brew info --json=v2 --installed`.

When selection settles for 250 ms, Cicerone loads and refreshes package information, README, and changelog content independently. Visible cached descriptions are prefetched while navigating. URL work is deduplicated in a bounded priority queue and throttled per host. The fixed status line reports active and queued detail jobs while cached content remains usable. README and changelog Markdown is rendered for the current inspector width and terminal color mode.

When GitHub Releases supplies a changelog, the selected release renders first and the next 10 releases are appended in the background. If more releases are available, the end of the changelog offers another 10-release page.

## Local data and repositories

- Database: `~/Library/Application Support/cicerone/cicerone.db`
- Cicerone-owned Git mirrors and other cache data: `~/Library/Caches/cicerone/`

Cicerone prefers usable local `homebrew-core` and `homebrew-cask` tap clones. Those user/Homebrew-owned repositories are read-only: Cicerone never fetches, checks out, resets, or rewrites them. If a local clone is unavailable, Cicerone creates and fetches its own bare, filtered mirror under its cache directory.
On later runs, an existing Cicerone mirror is indexed before its network refresh, so locally cached history can populate the feed while newer commits are fetched.

Cached feed, package information, README, and changelog content remains readable offline. The in-memory download queue is reconstructed from navigation demand after restart. See [Cache and recovery](docs/cache-and-recovery.md) before moving or rebuilding a damaged database; Cicerone never silently deletes it.

## GitHub access

For GitHub API access, `GITHUB_TOKEN` takes precedence. If it is unset or empty, Cicerone tries `gh auth token`. When authentication is unavailable, Cicerone falls back to the public rate-limited API. Tokens are never persisted or printed.

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

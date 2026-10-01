# Cicerone

Cicerone is a macOS terminal feed for recent Homebrew formula and cask changes. On interactive startup it refreshes installed state and begins fetching and indexing Git history before showing the feed, falling back to the durable SQLite cache if synchronization fails.

## Install

Requirements: macOS, Go 1.26.5 or newer, Homebrew, and Git.

Install directly from GitHub without cloning the repository:

```sh
go install github.com/anateus/cicerone/cmd/cicerone@latest
```

Go builds the binary from source. No prebuilt artifacts or generation steps are needed; the SQL migrations are included in the repository and embedded during compilation. Run the same command to update.

The binary is installed in `$(go env GOPATH)/bin`, or `$(go env GOBIN)` if you've set it. Make sure that directory is on your `PATH`. With Go's default install location:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
cicerone
```

Add the `export` line to your shell configuration (for example `~/.zshrc`) to keep it across sessions.

## Build from a checkout

From the repository root:

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

## Run

Run `cicerone --help` without opening the TUI. Cicerone's MVP is macOS-only; Linuxbrew support is deferred. If you built from a checkout without installing, use `./cicerone` instead.

Run `cicerone --plain` for a one-shot plaintext feed. It prints cached rows,
performs real read-only Homebrew metadata synchronization, prints refreshed
rows, and exits. This may update Cicerone's database and Cicerone-owned Git
caches, but it never installs, upgrades, or uninstalls Homebrew packages.
On a first run, history is streamed into durable batches (1 commit initially,
then 100 commits at a time). Plain mode prints numeric progress and newly
queryable rows after each batch. Interrupted scans retain valid rows and
per-commit progress without advancing the completed cursor, so the next run can
resume without reopening already checkpointed commits. Rewritten history is
reconciled when that scan resumes.

## Keys

| Key | Action |
| --- | --- |
| `j`, `↓` | Move down; scroll down while reading package details |
| `k`, `↑` | Move up; scroll up while reading package details |
| `1`, `2`, `3` | Show Formulae, Casks, or both |
| `,`, `.` | Move to the previous or next group in the group strip |
| Mouse click on the group strip | Jump straight to a group |
| `g` | Assign the selected package to a group, create a group, or remove a group |
| `/` | Enter package search; typing filters after a short debounce |
| `r` | Force a newest-first repository refresh; active catch-up resumes from its durable checkpoint |
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

## Groups

Each type tab carries a group strip: All, Ungrouped, then any groups you have created in creation order, then Starred and Hidden. The strip filters the feed inside the current type tab. All shows everything except hidden packages; Ungrouped shows only packages not in any group; Starred and Hidden collect packages carrying that marker. The active group is remembered across restarts.

Press `g` on a selected package to open the group modal. It lists a new-group field first, then existing groups as radio options with the first group highlighted, and marks the package's current group. Enter assigns, typing on the new-group row names a group to create and assign, and Enter on the package's current group removes it from that group. Tab reaches the Remove button, which deletes the highlighted group and returns its members to Ungrouped. Starred and Hidden cannot be removed from the modal.

## Feed behavior

The default feed contains version events from the last 30 days. An installed package remains visible through its newest matching event even if that event is older than the horizon. Results are sorted newest-first with a stable identity tie-breaker. Roll-up occurs only for adjacent events of the same package after filters are applied, so changing a filter can change grouping.

Search starts with package names. `tab` cycles through cumulative scopes: names; names and descriptions; those plus cached changelogs; then those plus cached READMEs. The local index uses prefix searches for unquoted terms (`rip gre` matches tokens beginning with `rip` and `gre`); quote the whole query for a non-prefix phrase search, such as `"rip grep"`. Cicerone also asks `brew search` for names and `brew search --desc` for descriptions.

Matches without an indexed update appear as catalog rows when version updates are enabled, even outside the feed's time horizon. After a catalog row stays visible for a second, Cicerone loads its Homebrew metadata and, when the local mirror is available, checks that package's history in the core or cask Git repository. A matching update replaces the catalog row, including when the update is older than the feed horizon. Packages from other taps still gain Homebrew metadata, but their Git history isn't in Cicerone's repository cache. Catalog rows show when they're enriching, ready, or retrying, and the inspector explains when a tap's history isn't indexed. Changelog and README text still come from Cicerone's durable cache and indexed updates.

Cicerone queries the durable feed immediately on startup while repository synchronization runs in the background. It checks for repository updates every five minutes while the TUI is open, even when a long historical scan is active. Each durable history batch refreshes the feed while preserving the selected stable event and its viewport-relative row. After fetching, each repository publishes its newest commit and releases the startup loading marker before continuing the remaining recent catch-up. Pressing `r` preempts that catch-up, fetches both repositories immediately, publishes the same quick newest-first slice, and then resumes from durable checkpoints. Older installed-package history starts only after that catch-up and continues as resumable, low-priority enrichment. Exhaustive searches remember packages and event kinds that have no older match, and custom-tap or wrong-repository packages are excluded. The header shows active synchronization and retains the latest failed-attempt status; failures continue to fall back to cached rows. Installed versions and upgrade availability come from `brew info --json=v2 --installed`.

When selection settles for 250 ms, Cicerone loads and refreshes package information, README, repository tags, and changelog content independently. Each inspector field shows its own loading or refreshing indicator, while cached content remains usable. Visible cached descriptions are prefetched while navigating. URL work is deduplicated in a bounded priority queue and throttled per host, and the fixed status line reports active and queued detail jobs. README and changelog Markdown is rendered for the current inspector width and terminal color mode.

If no repository README is available, the README tab loads the package's homepage and becomes Homepage. HTML pages are converted to readable text with headings, emphasis, lists, links, code blocks, and tables. Cicerone prefers the page's main content when it has one, tries reader mode on longer pages without it, and keeps the result in the same offline cache. It doesn't run JavaScript or load styles and images. Pages that depend on scripts show their available static content, or their title and description when the body is empty, with a link to open the original in a browser.

When GitHub Releases supplies a changelog, the selected release renders first and the next 10 releases are appended in the background. If more releases are available, the end of the changelog offers another 10-release page.

## Local data and repositories

- Database: `~/Library/Application Support/cicerone/cicerone.db`
- Cicerone-owned Git mirrors and other cache data: `~/Library/Caches/cicerone/`

Cicerone prefers usable local `homebrew-core` and `homebrew-cask` tap clones. Those user/Homebrew-owned repositories are read-only: Cicerone never fetches, checks out, resets, or rewrites them. If a local clone is unavailable, Cicerone creates and fetches its own bare, filtered mirror under its cache directory.
On later runs, Cicerone fetches each existing mirror before indexing it so the first interactive feed is based on the newest available commits.

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

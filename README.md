# Cicerone

Cicerone is a macOS terminal feed for recent Homebrew formula and cask changes. It shows the cached feed immediately on startup, then fetches and indexes Homebrew's Git history in the background and refreshes the feed as new batches land. If synchronization fails, the durable SQLite cache keeps working. You can read changelogs and READMEs, sort packages into groups, and install or upgrade one package or a marked batch without leaving the terminal.

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

## The screen

In a terminal at least 100 columns wide, the feed sits on the left and the inspector on the right. Narrower terminals show one at a time: `enter` or `l` opens the selected package's details full-screen, and `esc` or `h` returns to the feed.

The top of the feed has the FORMULAE, CASKS, and ALL tabs, followed by the sync status, whether roll-up is on, and the active search. Sync status reads `Syncing homebrew-core…` while a repository is being indexed, `Sync <date>` after a successful run, or `Sync failed <date>`. A `! sync … old` warning appears once the last sync is more than a day old, and `! updates … behind` appears when the newest indexed update trails the last sync by more than a day. Under the tabs is the group strip (see [Groups](#groups)).

Each feed row shows the package name, an update-cadence badge, and the version change, with the description underneath when the pane is wide enough. The badge is the package's average time between version updates, rounded to a readable rate such as `🐇 3×week`. The animal tracks speed: 🐆 daily or faster, 🐇 up to weekly, 🐄 up to about three months, and 🐢 slower than that. Packages without enough history show `last update: 5 days ago` or `last update unknown` instead. Starred packages carry `★`, hidden ones are dimmed, and marked ones carry `●` and a tint. Rows you saw in an earlier session drop below a `--- previously seen ---` divider, and catalog-only search matches come last.

The inspector shows the selected package's type, installed version, last update, license, homepage, and repository tags, then README and CHANGELOG tabs for the cached documents.

The bottom line shows status on the left (marked count, refresh and error messages, detail and catalog job counts) and the keys that work right now on the right. Low-priority hints drop off first on narrow terminals, and the `a` hint can be clicked.

## Keys

Feed and navigation:

| Key | Action |
| --- | --- |
| `j`, `↓` / `k`, `↑` | Move down or up; scroll while reading package details |
| `h`, `←` / `l`, `→` | Move focus toward the feed or the details; scroll sideways while reading |
| `enter` | Open package details, or return to the feed while reading them |
| `tab` | Switch panes in a wide terminal |
| `1`, `2`, `3` | Show formulae, casks, or both |
| `,`, `.` | Move to the previous or next group in the group strip |
| `/` | Search packages; typing filters after a short debounce |
| `r` | Refresh both repositories now; any interrupted catch-up resumes from its checkpoint |
| `e` | Expand or collapse a rolled-up package |
| `space` | Mark or unmark the selected package |
| `a` | Install or upgrade the marked packages, or the selected one if nothing is marked |
| `g` | Open the group modal for the marked packages, or the selected one |
| `esc` | Leave package details; with packages marked, clear the marks; otherwise quit |
| `q` | Quit from anywhere |

Package details:

| Key | Action |
| --- | --- |
| `[`, `]` | Show the README (or homepage) or the changelog |
| `t` | Show all repository tags, or collapse them |
| `m` | Load 10 more GitHub releases when the changelog offers them |

Search:

| Key | Action |
| --- | --- |
| `tab` | Broaden the search: names, then descriptions, then changelogs, then READMEs |
| `backspace`, `ctrl+u` | Delete a character, or clear the query |
| `enter` | Apply the search and leave the input |
| `esc` | Leave the input |

Install and upgrade confirmation:

| Key | Action |
| --- | --- |
| `y`, `enter` | Run the listed `brew` commands (`enter` uses whichever button has focus) |
| `tab`, `←`, `→` | Move focus between the confirm and Cancel buttons |
| `n`, `esc` | Cancel |
| `esc` twice while Homebrew runs | Interrupt the running command and skip the rest |
| `enter`, `esc` after a failure | Close the result |

The mouse works too: click a tab, group, row, document tab, or modal button, and scroll the pane under the pointer with the wheel.

## Marking packages

Press `space` on a row to mark its package, and again to unmark it. Marked rows carry a `●` and a tint, the status line counts them, and marks persist while you move between tabs, groups, and searches. `esc` clears every mark. Marks last for the session only.

With packages marked, `a` acts on all of them instead of the selected row. Installed packages are upgraded and the rest are installed. Cicerone splits the batch into as few `brew` commands as it can (one per verb and package type, such as `brew install --formula ripgrep fd` and `brew upgrade --cask iterm2`) and lists them in the confirmation. Nothing runs until you confirm.

Once confirmed, the commands run one after another. The modal shows a checklist of commands and the last lines of Homebrew's output. A failed command doesn't stop the rest. When everything succeeds the modal closes and the status line says what was installed and upgraded. Otherwise it stays open with the failed command's output until you close it. Packages that succeed are unmarked, failed ones stay marked so you can retry, and Cicerone refreshes installed state and the feed after any success.

`g` likewise moves every marked package into the chosen group in one transaction.

## Groups

Each type tab carries a group strip: All, Ungrouped, then any groups you have created in creation order, then Starred and Hidden. The strip filters the feed inside the current type tab. All shows everything except hidden packages; Ungrouped shows only packages not in any group; Starred and Hidden collect packages carrying that marker. The active group is remembered across restarts. When the strip is wider than the pane, it scrolls to keep the active group visible. The TUI currently has no key that stars or hides a package, so those two views only show markers set by earlier versions.

Press `g` on a selected package to open the group modal. It lists a new-group field first, then existing groups as radio options with the first group highlighted, and marks the package's current group. Enter assigns, typing on the new-group row names a group to create and assign, and Enter on the package's current group removes it from that group. Tab reaches the Remove button, which deletes the highlighted group and returns its members to Ungrouped. Starred and Hidden cannot be removed from the modal. With packages marked, the modal lists them and assigns all of them at once; when they sit in different groups, no group reads as current and Enter always assigns.

## Feed behavior

The default feed contains version events from the last 30 days. An installed package remains visible through its newest matching event even if that event is older than the horizon. Results are sorted newest-first with a stable identity tie-breaker. Roll-up occurs only for adjacent events of the same package after filters are applied, so changing a filter can change grouping. Starting a search turns roll-up on, and the setting is saved with your other feed preferences.

Every event that appears in the feed is recorded as seen. On the next launch those rows move below the `previously seen` divider, so new updates stay at the top. Events that arrive during the current session stay above it until you restart.

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

# Global TUI Navigation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `q` and `esc` exit every live TUI state and add complete `h/j/k/l` navigation alongside arrow keys.

**Architecture:** A global quit guard runs before all state-specific key handling. Horizontal navigation is expressed as left/back and right/forward actions whose effect depends only on wide versus narrow layout, keeping Vim and arrow aliases behaviorally identical.

**Tech Stack:** Go 1.26, Bubble Tea v2, Go standard library tests.

## Global Constraints

- `q` and `esc` return Bubble Tea's quit command from every live model state.
- The global quit guard runs before modal, error, action-running, action-result, detail, focus, or navigation handling.
- `j`/down move to the next feed group and `k`/up move to the previous feed group.
- `h`/left mean left or back; `l`/right mean right or forward.
- Wide layouts use `h`/left to focus the feed and `l`/right to focus the inspector.
- Narrow layouts use `l`/right to open details and `h`/left to return to the feed.
- Help text documents `h/j/k/l`, arrows, and unconditional `q`/`esc` exit.
- Tests never touch Homebrew packages or user caches.

---

### Task 1: Add Global Exit and Vim Navigation

**Files:**
- Modify: `internal/tui/model.go`
- Modify: `internal/tui/model_test.go`
- Modify: `internal/tui/actions_test.go`
- Modify: `cmd/cicerone/main.go`
- Modify: `cmd/cicerone/main_test.go`

**Interfaces:**
- Consumes: `tea.KeyPressMsg` and existing model state.
- Produces: `handleKey` returning `tea.Quit` for `q` or `esc` before any other branch.
- Preserves: existing `tab`, `enter`, `j/k`, arrow, action-confirmation, and Homebrew action behavior except the explicitly changed `esc` semantics.

- [ ] **Step 1: Write failing global-exit tests**

Add a helper that calls `Model.Update`, retains the returned command, executes it, and checks for `tea.QuitMsg`. Add a table-driven test with model states named `normal`, `error`, `pending-action`, `action-running`, `action-result`, and `detail-open`; for each state and each key `q`, `esc`, assert a non-nil command producing `tea.QuitMsg`.

Update existing tests that expect `esc` to dismiss action results or return from details. Use the existing non-escape controls where the behavior remains relevant: `n` cancels confirmation, and `h` will return from narrow details after Step 3. Remove any assertion that requires `esc` to dismiss an overlay.

- [ ] **Step 2: Verify exit tests RED**

Run: `go test ./internal/tui -run 'TestGlobalQuitKeys|TestNarrowInspector|TestAction' -count=1 -v`

Expected: FAIL because `q` is not handled globally and `esc` is intercepted by several state branches.

- [ ] **Step 3: Implement global exit and horizontal aliases**

At the start of `handleKey`, before `pendingAction`, `actionResult`, or `actionRunning` checks, add:

```go
switch key.String() {
case "q", "esc":
	return m, tea.Quit
}
```

Extend the ordinary navigation switch:

```go
case "h", "left":
	if m.width >= 100 {
		m.focus = feedPane
	} else {
		m.detailOpen = false
	}
case "l", "right":
	if m.width >= 100 {
		m.focus = inspectorPane
	} else if len(m.groups) > 0 {
		m.detailOpen = true
	}
```

Remove the old ordinary `esc` branch. Keep `j/k`, down/up, `tab`, and `enter` behavior unchanged.

- [ ] **Step 4: Add failing then passing navigation equivalence tests**

Add table-driven tests covering `h` with left and `l` with right in both layouts. For width 120, assert feed/inspector focus. For width 99 with one group, assert detail closed/open. Run before implementation to capture the missing-alias failure, then after implementation to verify identical outcomes.

Run: `go test ./internal/tui -run 'TestGlobalQuitKeys|TestHorizontalNavigationAliases|TestNarrowInspector|TestAction' -count=1 -v`

Expected after implementation: PASS.

- [ ] **Step 5: Update help text and tests**

Change help output to state that `h/j/k/l` or arrows navigate and `q/esc` quit. Update `TestHelpDocumentsKeysAndCacheBehavior` to require the substrings `h/j/k/l`, `arrows`, and `q/esc`.

- [ ] **Step 6: Verify regressions**

Run:

```bash
go test -race ./internal/tui ./cmd/cicerone -count=1
go test -race ./... -count=1
test -z "$(gofmt -l internal/tui/model.go internal/tui/model_test.go internal/tui/actions_test.go cmd/cicerone/main.go cmd/cicerone/main_test.go)"
git diff --check
```

Expected: all commands exit 0 with pristine output.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/model.go internal/tui/model_test.go internal/tui/actions_test.go cmd/cicerone/main.go cmd/cicerone/main_test.go
git commit -m "feat: add global TUI quit and Vim navigation"
```

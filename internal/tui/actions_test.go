package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/homebrew"
	"github.com/charmbracelet/x/ansi"
)

type fakeActions struct {
	mu      sync.Mutex
	calls   int
	err     error
	errFor  map[domain.PackageID]error
	output  string
	actions []homebrew.Action
	block   chan struct{}
}

func (f *fakeActions) RunAction(ctx context.Context, action homebrew.Action, w io.Writer) error {
	f.mu.Lock()
	f.calls++
	f.actions = append(f.actions, action)
	block := f.block
	f.mu.Unlock()
	_, _ = io.WriteString(w, f.output)
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, id := range action.Packages {
		if err := f.errFor[id]; err != nil {
			return err
		}
	}
	return f.err
}
func (f *fakeActions) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type fakeInstalled struct {
	refreshes int
	err       error
}

func (f *fakeInstalled) RefreshInstalled(context.Context) error { f.refreshes++; return f.err }

func action() homebrew.Action {
	return homebrew.Action{Kind: homebrew.Upgrade, Packages: []domain.PackageID{"pkg-b"}, Type: domain.PackageFormula}
}

// runCommand executes a command and every command it batches, returning the
// first message of the requested type. Ticks are skipped so tests do not wait
// on the spinner.
func runCommand[T tea.Msg](t *testing.T, cmd tea.Cmd) (T, bool) {
	t.Helper()
	var zero T
	if cmd == nil {
		return zero, false
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(200 * time.Millisecond):
		return zero, false
	}
	if found, ok := msg.(T); ok {
		return found, true
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, inner := range batch {
			if found, ok := runCommand[T](t, inner); ok {
				return found, true
			}
		}
	}
	return zero, false
}

// confirmAndRun confirms the pending session and drives every step to
// completion, feeding each ActionFinished back into the model.
func confirmAndRun(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	for m.action != nil && m.action.phase == actionRunning {
		finished, ok := runCommand[ActionFinished](t, cmd)
		if !ok {
			t.Fatal("running step produced no ActionFinished")
		}
		next, cmd = m.Update(finished)
		m = next.(Model)
	}
	return m, cmd
}

func installedEvent(id, pkg string, installed bool, typ domain.PackageType) domain.UpdateEvent {
	e := event(id, pkg)
	e.Installed, e.Type = installed, typ
	return e
}

func feedOf(events ...domain.UpdateEvent) []domain.FeedGroup {
	result := make([]domain.FeedGroup, len(events))
	for i, e := range events {
		result[i] = domain.FeedGroup{ID: e.ID, Events: []domain.UpdateEvent{e}}
	}
	return result
}

func TestSpaceTogglesMarkAndEscClearsBeforeQuitting(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height = 120, 24
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b", "c")})

	m = update(t, m, key("space"))
	m = update(t, m, key("j"))
	m = update(t, m, key("j"))
	m = update(t, m, key("space"))
	if want := []domain.PackageID{"pkg-a", "pkg-c"}; !reflect.DeepEqual(m.markOrder, want) {
		t.Fatalf("marks = %v, want %v", m.markOrder, want)
	}
	if !strings.Contains(m.statusText(), "2 marked") {
		t.Fatalf("status = %q, want mark count", m.statusText())
	}
	view := ansi.Strip(m.render())
	if !strings.Contains(view, "● pkg-a") || !strings.Contains(view, "● pkg-c") || strings.Contains(view, "● pkg-b") {
		t.Fatalf("marked rows not drawn:\n%s", view)
	}

	m = update(t, m, key("space"))
	if want := []domain.PackageID{"pkg-a"}; !reflect.DeepEqual(m.markOrder, want) {
		t.Fatalf("second space did not unmark: %v", m.markOrder)
	}

	next, cmd := m.Update(key("esc"))
	m = next.(Model)
	if cmd != nil || len(m.markOrder) != 0 {
		t.Fatalf("esc with marks = cmd %v, marks %v; want marks cleared without quitting", cmd, m.markOrder)
	}
	if _, msg := updateAndRunCommand(t, m, key("esc")); msg == nil {
		t.Fatal("esc on a bare feed no longer quits")
	}
}

func TestMarksSurviveRefreshAndTrackInstalledState(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b")})
	m = update(t, m, key("space"))
	m.feedRequestID++
	fresh := feedOf(event("b", "pkg-b"), installedEvent("a", "pkg-a", true, domain.PackageFormula))
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: fresh})
	if !m.isMarked("pkg-a") || !m.marked["pkg-a"].Installed {
		t.Fatalf("mark lost or stale after refresh: %#v", m.marked)
	}
}

func TestEExpandsRollUps(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: []domain.FeedGroup{
		{ID: "roll", Events: []domain.UpdateEvent{event("roll", "rollup"), event("child", "rollup")}},
	}})
	m = update(t, m, key("e"))
	if !m.expanded["roll"] {
		t.Fatal("e did not expand the roll-up")
	}
	m = update(t, m, key("space"))
	if m.expanded["roll"] == false || !m.isMarked("rollup") {
		t.Fatal("space should mark, not collapse")
	}
}

func TestActionTargetsMarkedPackagesAndPlansBrewCommands(t *testing.T) {
	m := NewModel(Dependencies{Actions: &fakeActions{}})
	m.width, m.height = 120, 30
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: feedOf(
		installedEvent("a", "ripgrep", false, domain.PackageFormula),
		installedEvent("b", "git", true, domain.PackageFormula),
		installedEvent("c", "firefox", false, domain.PackageCask),
		installedEvent("d", "fd", false, domain.PackageFormula),
	)})
	for _, k := range []string{"space", "j", "space", "j", "space", "j", "space"} {
		m = update(t, m, key(k))
	}
	if label := m.actionHintLabel(); label != "install/upgrade 4" {
		t.Fatalf("footer label = %q", label)
	}
	_, msg := updateAndRunCommand(t, m, key("a"))
	request, ok := msg.(ActionRequested)
	if !ok {
		t.Fatalf("a produced %T", msg)
	}
	want := []homebrew.Action{
		{Kind: homebrew.Install, Type: domain.PackageFormula, Packages: []domain.PackageID{"ripgrep", "fd"}},
		{Kind: homebrew.Upgrade, Type: domain.PackageFormula, Packages: []domain.PackageID{"git"}},
		{Kind: homebrew.Install, Type: domain.PackageCask, Packages: []domain.PackageID{"firefox"}},
	}
	if !reflect.DeepEqual(request.Actions, want) {
		t.Fatalf("planned %#v\nwant %#v", request.Actions, want)
	}

	m = update(t, m, request)
	view := ansi.Strip(m.render())
	for _, text := range []string{"Install & upgrade 4 packages?", "brew install --formula ripgrep fd", "brew upgrade --formula git", "brew install --cask firefox", "Install & upgrade", "Cancel"} {
		if !strings.Contains(view, text) {
			t.Fatalf("confirmation missing %q:\n%s", text, view)
		}
	}
}

func TestActionRequiresExplicitConfirmationAndPreservesAnchor(t *testing.T) {
	runner := &fakeActions{}
	m := NewModel(Dependencies{Actions: runner})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b", "c")})
	m = update(t, m, key("j"))
	want := m.anchor()
	next, cmd := m.Update(ActionRequested{Actions: []homebrew.Action{action()}})
	m = next.(Model)
	if cmd != nil || runner.count() != 0 || m.action == nil || m.action.phase != actionConfirm {
		t.Fatal("request executed without confirmation or failed to open modal")
	}
	m = update(t, m, key("j"))
	if got := m.anchor(); got != want {
		t.Fatalf("modal changed underlying anchor: %#v != %#v", got, want)
	}
	m = update(t, m, key("n"))
	if m.action != nil || runner.count() != 0 {
		t.Fatal("dismissal executed action")
	}
}

func TestConfirmButtonFocusSwitchesWithTab(t *testing.T) {
	runner := &fakeActions{}
	m := NewModel(Dependencies{Actions: runner})
	m = update(t, m, ActionRequested{Actions: []homebrew.Action{action()}})
	m = update(t, m, key("tab"))
	next, cmd := m.Update(key("enter"))
	m = next.(Model)
	if cmd != nil || m.action != nil || runner.count() != 0 {
		t.Fatal("enter on focused Cancel did not dismiss")
	}
}

func TestConfirmationRunsOnceAndDisablesDuplicates(t *testing.T) {
	runner := &fakeActions{}
	m := NewModel(Dependencies{Actions: runner})
	m = update(t, m, ActionRequested{Actions: []homebrew.Action{action()}})
	next, cmd := m.Update(ActionConfirmed{})
	m = next.(Model)
	if cmd == nil || m.action == nil || m.action.phase != actionRunning {
		t.Fatal("confirmation did not start action")
	}
	duplicate, duplicateCmd := m.Update(ActionRequested{Actions: []homebrew.Action{action()}})
	m = duplicate.(Model)
	if duplicateCmd != nil || m.action.phase != actionRunning {
		t.Fatal("duplicate action was enabled while running")
	}
	if again, againCmd := m.Update(ActionConfirmed{}); againCmd != nil || again.(Model).action.phase != actionRunning {
		t.Fatal("second confirmation restarted the action")
	}
	if _, ok := runCommand[ActionFinished](t, cmd); !ok || runner.count() != 1 {
		t.Fatalf("calls = %d", runner.count())
	}
}

func TestMultiStepSessionRunsSequentiallyUnmarksAndSummarizes(t *testing.T) {
	runner := &fakeActions{}
	installed := &fakeInstalled{}
	m := NewModel(Dependencies{Actions: runner, Installed: installed, Data: &fakeData{}})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: feedOf(
		installedEvent("a", "ripgrep", false, domain.PackageFormula),
		installedEvent("b", "git", true, domain.PackageFormula),
	)})
	m = update(t, m, key("space"))
	m = update(t, m, key("j"))
	m = update(t, m, key("space"))
	_, msg := updateAndRunCommand(t, m, key("a"))
	m = update(t, m, msg)

	m, cmd := confirmAndRun(t, m)
	if runner.count() != 2 {
		t.Fatalf("brew ran %d times, want 2", runner.count())
	}
	if m.action != nil {
		t.Fatal("successful session left the modal open")
	}
	if len(m.markOrder) != 0 {
		t.Fatalf("installed packages stayed marked: %v", m.markOrder)
	}
	if m.notification != "Installed ripgrep · Upgraded git" {
		t.Fatalf("notification = %q", m.notification)
	}
	refreshed, ok := runCommand[installedRefreshed](t, cmd)
	if !ok || installed.refreshes != 1 {
		t.Fatal("success did not refresh installed state")
	}
	before := m.feedRequestID
	m = update(t, m, refreshed)
	if m.feedRequestID != before+1 || !m.loading {
		t.Fatal("refresh completion did not requery feed")
	}
}

func TestFailedStepContinuesKeepsFailedMarksAndShowsDismissibleResult(t *testing.T) {
	boom := errors.New("exit status 1")
	runner := &fakeActions{errFor: map[domain.PackageID]error{"ripgrep": boom}, output: "Error: ripgrep: no bottle\n"}
	m := NewModel(Dependencies{Actions: runner, Installed: &fakeInstalled{}})
	m.width, m.height = 100, 30
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: feedOf(
		installedEvent("a", "ripgrep", false, domain.PackageFormula),
		installedEvent("b", "git", true, domain.PackageFormula),
	)})
	m = update(t, m, key("space"))
	m = update(t, m, key("j"))
	m = update(t, m, key("space"))
	_, msg := updateAndRunCommand(t, m, key("a"))
	m = update(t, m, msg)

	m, cmd := confirmAndRun(t, m)
	if runner.count() != 2 {
		t.Fatalf("a failed step stopped the batch: %d runs", runner.count())
	}
	if m.action == nil || m.action.phase != actionDone {
		t.Fatal("failure did not leave a result modal")
	}
	if !reflect.DeepEqual(m.markOrder, []domain.PackageID{"ripgrep"}) {
		t.Fatalf("marks after partial failure = %v, want only the failed package", m.markOrder)
	}
	if _, ok := runCommand[installedRefreshed](t, cmd); !ok {
		t.Fatal("partial success did not refresh installed state")
	}
	view := ansi.Strip(m.render())
	for _, text := range []string{"Failed:", "✗ brew install --formula ripgrep", "✓ brew upgrade --formula git", "no bottle", "Close"} {
		if !strings.Contains(view, text) {
			t.Fatalf("result modal missing %q:\n%s", text, view)
		}
	}
	m = update(t, m, key("j"))
	if m.action == nil {
		t.Fatal("ordinary key dismissed failure")
	}
	m = update(t, m, key("enter"))
	if m.action != nil || m.err != nil {
		t.Fatal("enter did not dismiss the result")
	}
}

func TestDoubleEscInterruptsRunningBrewAndSkipsRemainingSteps(t *testing.T) {
	runner := &fakeActions{block: make(chan struct{})}
	m := NewModel(Dependencies{Actions: runner, Context: context.Background()})
	m = update(t, m, ActionRequested{Actions: []homebrew.Action{action(), {Kind: homebrew.Install, Type: domain.PackageCask, Packages: []domain.PackageID{"firefox"}}}})
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	finished := make(chan ActionFinished, 1)
	go func() {
		msg, _ := runCommand[ActionFinished](t, cmd)
		finished <- msg
	}()

	m = update(t, m, key("esc"))
	if !m.action.interruptArm || m.action.interrupted {
		t.Fatal("first esc should only arm the interrupt")
	}
	if !strings.Contains(ansi.Strip(m.render()), "press esc again") {
		t.Fatal("armed interrupt is not explained")
	}
	m = update(t, m, key("esc"))
	var msg ActionFinished
	select {
	case msg = <-finished:
	case <-time.After(time.Second):
		t.Fatal("interrupt did not cancel the running brew")
	}
	if !errors.Is(msg.Err, context.Canceled) {
		t.Fatalf("step error = %v, want canceled", msg.Err)
	}
	m = update(t, m, msg)
	if runner.count() != 1 || m.action.phase != actionDone || m.action.states[1] != stepSkipped {
		t.Fatalf("interrupt did not skip the remaining step: runs %d, states %v", runner.count(), m.action.states)
	}
	if !strings.Contains(ansi.Strip(m.render()), "Interrupted:") {
		t.Fatal("interrupted session not labeled")
	}
}

func TestStaleActionMessagesAreIgnored(t *testing.T) {
	m := NewModel(Dependencies{Actions: &fakeActions{}})
	m = update(t, m, ActionRequested{Actions: []homebrew.Action{action()}})
	m = update(t, m, ActionConfirmed{})
	id := m.action.id
	m = update(t, m, ActionOutput{Session: id + 1, Output: "other"})
	m = update(t, m, ActionFinished{Session: id + 1, Err: errors.New("other")})
	if m.action.output != "" || m.action.phase != actionRunning {
		t.Fatal("a different session's messages changed this one")
	}
	m = update(t, m, ActionOutput{Session: id, Output: "==> Pouring"})
	if m.action.output != "==> Pouring" {
		t.Fatal("current output not shown")
	}
}

func TestSuccessfulActionFailsClearlyWithoutInstalledRefreshDependency(t *testing.T) {
	m := NewModel(Dependencies{Actions: &fakeActions{}})
	m = update(t, m, ActionRequested{Actions: []homebrew.Action{action()}})
	m, cmd := confirmAndRun(t, m)
	refreshed, ok := runCommand[installedRefreshed](t, cmd)
	if !ok || refreshed.Err == nil {
		t.Fatal("missing installed refresh dependency silently succeeded")
	}
	m = update(t, m, refreshed)
	if m.err == nil || !strings.Contains(m.notification, "installed-state refresh") {
		t.Fatal("missing refresh dependency was not surfaced in status")
	}
}

func TestActionKeyRequestsInstallOrUpgradeAndIsDocumented(t *testing.T) {
	for _, tt := range []struct {
		name      string
		installed bool
		want      homebrew.ActionKind
	}{
		{"not installed", false, homebrew.Install}, {"installed", true, homebrew.Upgrade},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := NewModel(Dependencies{Actions: &fakeActions{}})
			e := event("a", "pkg-a")
			e.Installed = tt.installed
			m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: []domain.FeedGroup{{ID: "a", Events: []domain.UpdateEvent{e}}}})
			view := ansi.Strip(m.render())
			if !strings.Contains(strings.Join(strings.Fields(view), " "), "a "+string(tt.want)) || strings.Contains(ansi.Strip(m.feedControls(72)), "[a]") {
				t.Fatalf("action key was not moved from the tabs to the footer: %q", view)
			}
			next, cmd := m.Update(key("a"))
			m = next.(Model)
			if cmd == nil {
				t.Fatal("action key emitted no command")
			}
			msg := cmd().(ActionRequested)
			if len(msg.Actions) != 1 || msg.Actions[0].Kind != tt.want || !reflect.DeepEqual(msg.Actions[0].Packages, []domain.PackageID{e.PackageID}) || msg.Actions[0].Type != e.Type {
				t.Fatalf("actions = %#v", msg.Actions)
			}
			m = update(t, m, msg)
			if view := ansi.Strip(m.render()); !strings.Contains(strings.Join(strings.Fields(view), " "), "y/enter confirm") || strings.Contains(view, "[y/enter]") {
				t.Fatalf("confirmation keys were not presented cleanly in the footer: %q", view)
			}
		})
	}
}

func TestFooterActionHintRemainsClickable(t *testing.T) {
	m := NewModel(Dependencies{Actions: &fakeActions{}})
	m.width, m.height = 80, 24
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a")})

	hints := m.footerHints(m.width, m.statusText())
	x := m.width - footerHintsWidth(hints)
	actionX := -1
	for i, hint := range hints {
		if i > 0 {
			x += 2
		}
		if hint.key == "a" {
			actionX = x
			break
		}
		x += ansi.StringWidth(hint.key) + ansi.StringWidth(hint.label) + 3
	}
	if actionX < 0 {
		t.Fatal("footer omitted the action hint")
	}
	_, msg := updateAndRunCommand(t, m, tea.MouseClickMsg{X: actionX, Y: m.height - 1, Button: tea.MouseLeft})
	if _, ok := msg.(ActionRequested); !ok {
		t.Fatalf("footer action click emitted %T, want ActionRequested", msg)
	}
}

func TestActionModalButtonsAreClickableWhereDrawn(t *testing.T) {
	for _, size := range []struct{ width, height int }{{80, 24}, {120, 40}, {60, 14}, {50, 9}} {
		runner := &fakeActions{}
		m := NewModel(Dependencies{Actions: runner})
		m.width, m.height = size.width, size.height
		m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("pkg-a")})
		many := action()
		for i := range 30 {
			many.Packages = append(many.Packages, domain.PackageID(fmt.Sprintf("long-package-name-%d", i)))
		}
		m = update(t, m, ActionRequested{Actions: []homebrew.Action{action(), many}})

		view := strings.Split(ansi.Strip(m.render()), "\n")
		_, bounds := m.actionModalView(m.width, m.height-statusHeight)
		row := []rune(view[bounds.buttonY])
		confirm := string(row[bounds.confirmX : bounds.confirmX+bounds.confirmW])
		cancel := string(row[bounds.cancelX : bounds.cancelX+bounds.cancelW])
		if confirm != " Upgrade " || cancel != " Cancel " {
			t.Fatalf("%dx%d hit regions cover %q and %q", size.width, size.height, confirm, cancel)
		}
		if bounds.buttonY <= 1 || bounds.buttonY >= m.height-2 {
			t.Fatalf("%dx%d modal not centered: buttons on row %d", size.width, size.height, bounds.buttonY)
		}

		dismissed := update(t, m, tea.MouseClickMsg{X: bounds.cancelX, Y: bounds.buttonY, Button: tea.MouseLeft})
		if dismissed.action != nil {
			t.Fatal("clicking Cancel did not dismiss")
		}
		next, cmd := m.Update(tea.MouseClickMsg{X: bounds.confirmX + bounds.confirmW - 1, Y: bounds.buttonY, Button: tea.MouseLeft})
		if cmd == nil || next.(Model).action.phase != actionRunning {
			t.Fatal("clicking the confirm button did not start the action")
		}
	}
}

func TestOutputTailHandlesCarriageReturnsAndWidth(t *testing.T) {
	output := "==> Fetching\n######   20%\r###########  60%\r############ 100%\n\x1b[1mPouring\x1b[0m ripgrep--14.1.arm64.bottle.tar.gz\n"
	got := outputTail(output, 2, 20)
	want := []string{"############ 100%", "Pouring ripgrep--14…"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tail = %q, want %q", got, want)
	}
}

func TestActionOutputEmitterIsThrottledToTwentyPerSecond(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	w := newActionOutputWriter(func(tea.Msg) { mu.Lock(); times = append(times, time.Now()); mu.Unlock() })
	for range 100 {
		_, _ = w.Write([]byte("x"))
	}
	time.Sleep(120 * time.Millisecond)
	w.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(times) > 3 {
		t.Fatalf("emitted %d updates in 120ms, want <= 3", len(times))
	}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) < 45*time.Millisecond {
			t.Fatalf("updates %s apart", times[i].Sub(times[i-1]))
		}
	}
}

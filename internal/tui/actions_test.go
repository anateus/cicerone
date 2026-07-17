package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/homebrew"
)

type fakeActions struct {
	mu     sync.Mutex
	calls  int
	err    error
	output string
}

func (f *fakeActions) RunAction(_ context.Context, _ homebrew.Action, w io.Writer) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	_, _ = io.WriteString(w, f.output)
	return f.err
}
func (f *fakeActions) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type refreshData struct {
	fakeData
	refreshes int
}

func (f *refreshData) RefreshInstalled(context.Context) error { f.refreshes++; return nil }

func action() homebrew.Action {
	return homebrew.Action{Kind: homebrew.Upgrade, Package: "pkg-b", Type: domain.PackageFormula}
}

func TestActionRequiresExplicitConfirmationAndPreservesAnchor(t *testing.T) {
	runner := &fakeActions{}
	m := NewModel(Dependencies{Actions: runner})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b", "c")})
	m = update(t, m, key("j"))
	want := m.anchor()
	next, cmd := m.Update(ActionRequested{Action: action()})
	m = next.(Model)
	if cmd != nil || runner.count() != 0 || m.pendingAction == nil {
		t.Fatal("request executed without confirmation or failed to open modal")
	}
	m = update(t, m, key("j"))
	if got := m.anchor(); got != want {
		t.Fatalf("modal changed underlying anchor: %#v != %#v", got, want)
	}
	m = update(t, m, key("esc"))
	if m.pendingAction != nil || runner.count() != 0 {
		t.Fatal("dismissal executed action")
	}
}

func TestConfirmationRunsOnceAndDisablesDuplicates(t *testing.T) {
	runner := &fakeActions{}
	m := NewModel(Dependencies{Actions: runner})
	m = update(t, m, ActionRequested{Action: action()})
	next, cmd := m.Update(ActionConfirmed{})
	m = next.(Model)
	if cmd == nil || !m.actionRunning {
		t.Fatal("confirmation did not start action")
	}
	duplicate, duplicateCmd := m.Update(ActionRequested{Action: action()})
	m = duplicate.(Model)
	if duplicateCmd != nil || m.pendingAction != nil {
		t.Fatal("duplicate action was enabled while running")
	}
	msg := cmd()
	if runner.count() != 1 {
		t.Fatalf("calls = %d", runner.count())
	}
	m = update(t, m, msg)
}

func TestSuccessfulActionRefreshesInstalledStateThenRequeries(t *testing.T) {
	data := &refreshData{}
	m := NewModel(Dependencies{Data: data, Actions: &fakeActions{}})
	m.actionRunning = true
	before := m.feedRequestID
	next, cmd := m.Update(ActionFinished{Action: action()})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("success did not schedule installed refresh")
	}
	msg := cmd()
	if data.refreshes != 1 {
		t.Fatalf("refreshes = %d", data.refreshes)
	}
	m = update(t, m, msg)
	if m.feedRequestID != before+1 || !m.loading {
		t.Fatal("refresh completion did not requery feed")
	}
}

func TestFailedActionRetainsOutputAndStatusErrorUntilDismissed(t *testing.T) {
	boom := errors.New("brew failed")
	m := NewModel(Dependencies{})
	m.actionRunning = true
	m = update(t, m, ActionFinished{Action: action(), Output: "diagnostic output", Err: boom})
	if m.actionOutput != "diagnostic output" || !errors.Is(m.err, boom) || m.actionResult == nil {
		t.Fatal("failure details not retained")
	}
	if view := m.render(); !strings.Contains(view, "Error: brew failed") {
		t.Fatalf("status view omitted action error: %q", view)
	}
	m = update(t, m, key("j"))
	if m.actionOutput == "" {
		t.Fatal("ordinary key dismissed failure")
	}
	m = update(t, m, key("esc"))
	if m.actionResult != nil || m.actionOutput != "" {
		t.Fatal("escape did not dismiss failure output")
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

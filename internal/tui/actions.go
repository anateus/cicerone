package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/homebrew"
)

const actionOutputInterval = 50 * time.Millisecond

type installedRefresher interface{ RefreshInstalled(context.Context) error }

func (m Model) renderActionModal() string {
	if m.pendingAction != nil {
		return fmt.Sprintf("Confirm %s %s? [y/enter] confirm  [n/esc] cancel", m.pendingAction.Kind, m.pendingAction.Package)
	}
	if m.actionRunning {
		return "Homebrew action running…\n" + m.actionOutput
	}
	if m.actionResult != nil {
		return fmt.Sprintf("Homebrew %s failed — [esc] dismiss\n%s", m.actionResult.Kind, strings.TrimSpace(m.actionOutput))
	}
	return ""
}

func (m Model) runAction(action homebrew.Action) tea.Cmd {
	return func() tea.Msg {
		retained := homebrew.NewRetainedOutput()
		emitter := newActionOutputWriter(m.deps.Send)
		writer := io.MultiWriter(retained, emitter)
		var err error
		if m.deps.Actions == nil {
			err = fmt.Errorf("Homebrew actions are unavailable")
		} else {
			err = m.deps.Actions.RunAction(m.deps.Context, action, writer)
		}
		emitter.Close()
		return ActionFinished{Action: action, Output: retained.String(), Err: err}
	}
}

func (m Model) refreshInstalled() tea.Cmd {
	return func() tea.Msg {
		if source, ok := m.deps.Data.(installedRefresher); ok {
			return installedRefreshed{Err: source.RefreshInstalled(m.deps.Context)}
		}
		return installedRefreshed{}
	}
}

type actionOutputWriter struct {
	mu     sync.Mutex
	send   func(tea.Msg)
	last   time.Time
	output *homebrew.RetainedOutput
	timer  *time.Timer
	closed bool
}

func newActionOutputWriter(send func(tea.Msg)) *actionOutputWriter {
	return &actionOutputWriter{send: send, output: homebrew.NewRetainedOutput()}
}

func (w *actionOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.output.Write(p)
	if w.send == nil || w.closed {
		return len(p), nil
	}
	now := time.Now()
	if w.last.IsZero() || now.Sub(w.last) >= actionOutputInterval {
		w.emitLocked(now)
	} else if w.timer == nil {
		w.timer = time.AfterFunc(actionOutputInterval-now.Sub(w.last), w.emitPending)
	}
	return len(p), nil
}

func (w *actionOutputWriter) emitPending() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer = nil
	if !w.closed {
		w.emitLocked(time.Now())
	}
}
func (w *actionOutputWriter) emitLocked(now time.Time) {
	w.last = now
	output := w.output.String()
	w.send(ActionOutput{Output: output})
}
func (w *actionOutputWriter) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

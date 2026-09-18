package tui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/anateus/cicerone/internal/homebrew"
	"github.com/charmbracelet/x/ansi"
)

const actionOutputInterval = 50 * time.Millisecond

func (m Model) renderActionModal() string {
	if m.pendingAction != nil {
		return fmt.Sprintf("Confirm %s %s?", m.pendingAction.Kind, m.pendingAction.Package)
	}
	if m.actionRunning {
		return "Homebrew action running…\n" + m.actionOutput
	}
	if m.actionResult != nil {
		return fmt.Sprintf("Homebrew %s failed\n%s", m.actionResult.Kind, strings.TrimSpace(m.actionOutput))
	}
	return ""
}

type actionModalBounds struct {
	x, y, width, height int
	yesX, yesWidth      int
	noX, noWidth        int
	buttonY             int
}

// actionModalView builds a centered, fixed-size surface and the hit regions
// used by the mouse handler. Keeping the geometry in one place prevents the
// rendered buttons and their click targets from drifting apart.
func (m Model) actionModalView(width, height int) (string, actionModalBounds) {
	if m.pendingAction == nil && !m.actionRunning && m.actionResult == nil {
		return "", actionModalBounds{}
	}

	content := m.renderActionModal()
	lines := strings.Split(content, "\n")
	if m.pendingAction != nil {
		yes := lipgloss.NewStyle().Bold(true).Foreground(m.palette().selectedFG).Background(m.palette().selectedBG).Render(" Yes ")
		no := lipgloss.NewStyle().Bold(true).Foreground(m.palette().statusFG).Background(m.palette().recessedBG).Render(" No ")
		lines = append(lines, "", yes+"   "+no)
	}
	if m.actionRunning && strings.TrimSpace(m.actionOutput) == "" {
		lines = append(lines, "", "Waiting for Homebrew…")
	}

	// Output can be verbose. Keep the confirmation surface useful on small
	// terminals instead of allowing command output to push the status line off.
	maxLines := max(3, height-6)
	if len(lines) > maxLines {
		lines = append([]string(nil), lines[:maxLines]...)
		lines[maxLines-1] = "…"
	}

	p := m.palette()
	contentWidth := min(64, max(1, width-6))
	if contentWidth < 1 {
		contentWidth = 1
	}
	for index, line := range lines {
		lines[index] = ansi.Truncate(line, contentWidth, "…")
	}
	panelStyle := lipgloss.NewStyle().Width(contentWidth).Align(lipgloss.Center).
		Padding(1, 2).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(p.primary).
		Foreground(p.statusFG).
		Background(p.raisedBG)
	panel := panelStyle.Render(strings.Join(lines, "\n"))
	panelWidth, panelHeight := lipgloss.Width(panel), lipgloss.Height(panel)
	if panelWidth > width {
		panelWidth = width
	}
	if panelHeight > height {
		panelHeight = height
	}
	bounds := actionModalBounds{x: (width - panelWidth) / 2, y: (height - panelHeight) / 2, width: panelWidth, height: panelHeight}

	if m.pendingAction != nil {
		plain := strings.Split(ansi.Strip(panel), "\n")
		for row, line := range plain {
			if strings.Contains(line, "Yes") {
				bounds.buttonY = bounds.y + row
				yesIndex := strings.Index(line, "Yes")
				bounds.yesX = max(bounds.x, bounds.x+ansi.StringWidth(line[:yesIndex])-1)
				bounds.yesWidth = ansi.StringWidth(" Yes ")
				if no := strings.Index(line, "No"); no >= 0 {
					bounds.noX = max(bounds.x, bounds.x+ansi.StringWidth(line[:no])-1)
					bounds.noWidth = ansi.StringWidth(" No ")
				}
				break
			}
		}
	}
	return panel, bounds
}

func (m Model) actionModalHit(x, y, width, height int) (yes, no bool) {
	_, bounds := m.actionModalView(width, height)
	if m.pendingAction == nil || y != bounds.buttonY {
		return false, false
	}
	return x >= bounds.yesX && x < bounds.yesX+bounds.yesWidth,
		x >= bounds.noX && x < bounds.noX+bounds.noWidth
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
		if m.deps.Installed == nil {
			return installedRefreshed{Err: fmt.Errorf("installed-state refresh is unavailable")}
		}
		return installedRefreshed{Err: m.deps.Installed.RefreshInstalled(m.deps.Context)}
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

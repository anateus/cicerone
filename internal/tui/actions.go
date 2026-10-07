package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/homebrew"
	"github.com/charmbracelet/x/ansi"
)

const actionOutputInterval = 50 * time.Millisecond

// actionPhase is where a Homebrew action session stands. A session moves
// confirm → running → (closed on success | done on failure or interrupt).
type actionPhase uint8

const (
	actionConfirm actionPhase = iota
	actionRunning
	actionDone
)

type actionStepState uint8

const (
	stepPending actionStepState = iota
	stepRunning
	stepSucceeded
	stepFailed
	stepSkipped
)

// actionSession is one confirmed batch of brew invocations. Each step is a
// single brew command (one verb, one package type, several packages); steps
// run sequentially because Homebrew holds a global lock.
type actionSession struct {
	id            uint64
	phase         actionPhase
	steps         []homebrew.Action
	states        []actionStepState
	current       int
	output        string
	failedOutput  string
	err           error
	interrupted   bool
	interruptArm  bool
	cancelFocused bool
	ctx           context.Context
	cancel        context.CancelFunc
	spinnerFrame  int
}

func newActionSession(id uint64, steps []homebrew.Action) *actionSession {
	return &actionSession{id: id, steps: steps, states: make([]actionStepState, len(steps))}
}

func (s *actionSession) packageCount() int {
	total := 0
	for _, step := range s.steps {
		total += len(step.Packages)
	}
	return total
}

// verb names the session for headings and buttons: Install, Upgrade, or
// Install & upgrade when the batch mixes both.
func (s *actionSession) verb() string {
	install, upgrade := false, false
	for _, step := range s.steps {
		install = install || step.Kind == homebrew.Install
		upgrade = upgrade || step.Kind == homebrew.Upgrade
	}
	switch {
	case install && upgrade:
		return "Install & upgrade"
	case upgrade:
		return "Upgrade"
	default:
		return "Install"
	}
}

func (s *actionSession) title() string {
	count := s.packageCount()
	if count == 1 && len(s.steps) == 1 {
		return s.verb() + " " + string(s.steps[0].Packages[0])
	}
	return fmt.Sprintf("%s %d packages", s.verb(), count)
}

// succeededPackages lists packages whose step finished cleanly.
func (s *actionSession) succeededPackages() []domain.PackageID {
	var done []domain.PackageID
	for index, step := range s.steps {
		if s.states[index] == stepSucceeded {
			done = append(done, step.Packages...)
		}
	}
	return done
}

func (s *actionSession) allSucceeded() bool {
	for _, state := range s.states {
		if state != stepSucceeded {
			return false
		}
	}
	return true
}

// summary reports a finished session for the status line.
func (s *actionSession) summary() string {
	var parts []string
	for _, kind := range []homebrew.ActionKind{homebrew.Install, homebrew.Upgrade} {
		var names []string
		for index, step := range s.steps {
			if step.Kind == kind && s.states[index] == stepSucceeded {
				for _, name := range step.Packages {
					names = append(names, string(name))
				}
			}
		}
		if len(names) == 0 {
			continue
		}
		verb := "Installed "
		if kind == homebrew.Upgrade {
			verb = "Upgraded "
		}
		parts = append(parts, verb+summarizeNames(names))
	}
	return strings.Join(parts, " · ")
}

func summarizeNames(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:2], ", "), len(names)-2)
}

// actionTargets returns the packages an action would cover: every marked
// package when any are marked, otherwise the selected row.
func (m Model) actionTargets() []homebrew.ActionTarget {
	if len(m.markOrder) > 0 {
		targets := make([]homebrew.ActionTarget, 0, len(m.markOrder))
		for _, id := range m.markOrder {
			mark := m.marked[id]
			targets = append(targets, homebrew.ActionTarget{Package: id, Type: mark.Type, Installed: mark.Installed})
		}
		return targets
	}
	if !m.hasSelection() {
		return nil
	}
	e := m.selectedEvent()
	return []homebrew.ActionTarget{{Package: e.PackageID, Type: e.Type, Installed: e.Installed}}
}

// actionHintLabel is the footer label for a: install, upgrade, or a count.
func (m Model) actionHintLabel() string {
	targets := m.actionTargets()
	install, upgrade := false, false
	for _, target := range targets {
		install = install || !target.Installed
		upgrade = upgrade || target.Installed
	}
	label := "install"
	switch {
	case install && upgrade:
		label = "install/upgrade"
	case upgrade:
		label = "upgrade"
	}
	if len(m.markOrder) > 0 {
		label += fmt.Sprintf(" %d", len(targets))
	}
	return label
}

func (m Model) requestSelectedAction() (tea.Model, tea.Cmd) {
	actions := homebrew.PlanActions(m.actionTargets())
	if len(actions) == 0 {
		return m, nil
	}
	return m, func() tea.Msg { return ActionRequested{Actions: actions} }
}

func (m Model) startActionSession(actions []homebrew.Action) Model {
	m.actionSessionID++
	m.action = newActionSession(m.actionSessionID, actions)
	m.actionAnchor = m.anchor()
	return m
}

// confirmAction begins running the first step of a confirmed session.
func (m Model) confirmAction() (tea.Model, tea.Cmd) {
	if m.action == nil || m.action.phase != actionConfirm {
		return m, nil
	}
	session := *m.action
	session.phase = actionRunning
	session.ctx, session.cancel = context.WithCancel(m.deps.Context)
	session.states = append([]actionStepState(nil), session.states...)
	m.action = &session
	return m.runActionStep(0)
}

func (m Model) runActionStep(step int) (tea.Model, tea.Cmd) {
	session := *m.action
	session.current = step
	session.output = ""
	session.states = append([]actionStepState(nil), session.states...)
	session.states[step] = stepRunning
	m.action = &session
	return m, tea.Batch(m.runAction(session.ctx, session.id, step, session.steps[step]), m.actionSpinner(session.id))
}

// finishActionStep records a step result and either starts the next step or
// ends the session. Remaining steps still run after a failure so one bad
// package does not block unrelated installs; an interrupt skips them.
func (m Model) finishActionStep(msg ActionFinished) (tea.Model, tea.Cmd) {
	if m.action == nil || m.action.phase != actionRunning || msg.Session != m.action.id || msg.Step != m.action.current {
		return m, nil
	}
	session := *m.action
	session.states = append([]actionStepState(nil), session.states...)
	session.output = msg.Output
	session.interruptArm = false
	interrupted := errors.Is(msg.Err, context.Canceled) && session.interrupted
	switch {
	case msg.Err == nil:
		session.states[msg.Step] = stepSucceeded
	default:
		session.states[msg.Step] = stepFailed
		session.err = msg.Err
		session.failedOutput = msg.Output
	}
	next := msg.Step + 1
	if next < len(session.steps) && !interrupted {
		m.action = &session
		return m.runActionStep(next)
	}
	for index := next; index < len(session.steps); index++ {
		session.states[index] = stepSkipped
	}
	if session.cancel != nil {
		session.cancel()
		session.cancel = nil
	}
	succeeded := session.succeededPackages()
	for _, id := range succeeded {
		m.unmark(id)
	}
	if session.allSucceeded() {
		m.action = nil
		m.notification = session.summary()
	} else {
		session.phase = actionDone
		m.action = &session
		if session.interrupted {
			m.notification = "Homebrew action interrupted"
		} else {
			m.err = session.err
			m.notification = "Error: " + session.err.Error()
		}
	}
	if len(succeeded) == 0 {
		return m, nil
	}
	return m, m.refreshInstalled()
}

// interruptAction cancels the running brew process. The first press arms the
// interrupt so a stray esc cannot abort an install halfway through.
func (m Model) interruptAction() (tea.Model, tea.Cmd) {
	if m.action == nil || m.action.phase != actionRunning {
		return m, nil
	}
	session := *m.action
	if !session.interruptArm {
		session.interruptArm = true
		m.action = &session
		return m, nil
	}
	session.interrupted = true
	if session.cancel != nil {
		session.cancel()
	}
	m.action = &session
	return m, nil
}

func (m Model) handleActionKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	session := m.action
	switch session.phase {
	case actionConfirm:
		switch key.String() {
		case "y":
			return m.confirmAction()
		case "enter":
			if session.cancelFocused {
				m.action = nil
				return m, nil
			}
			return m.confirmAction()
		case "n", "esc":
			m.action = nil
		case "tab", "left", "right", "h", "l", "shift+tab":
			next := *session
			next.cancelFocused = !next.cancelFocused
			m.action = &next
		}
	case actionRunning:
		if key.String() == "esc" {
			return m.interruptAction()
		}
	case actionDone:
		switch key.String() {
		case "enter", "esc", "n", "y":
			m.action = nil
			m.err = nil
		}
	}
	return m, nil
}

type actionModalBounds struct {
	x, y, width, height int
	confirmX, confirmW  int
	cancelX, cancelW    int
	buttonY             int
}

const actionModalMaxWidth = 68

// actionModalView builds the modal and the hit regions used by the mouse
// handler from the same layout, so buttons and their click targets cannot
// drift apart.
func (m Model) actionModalView(width, height int) (string, actionModalBounds) {
	session := m.action
	if session == nil {
		return "", actionModalBounds{}
	}
	p := m.palette()
	inner := max(10, min(actionModalMaxWidth, width-6))
	title := lipgloss.NewStyle().Bold(true).Foreground(p.primary)
	faint := lipgloss.NewStyle().Faint(true)
	success := lipgloss.NewStyle().Foreground(lipgloss.Color("#4FBF79"))
	danger := lipgloss.NewStyle().Foreground(lipgloss.Color("#E06C75"))

	var lines []string
	heading := session.title()
	switch session.phase {
	case actionConfirm:
		heading += "?"
	case actionRunning:
		heading = detailSpinnerFrames[session.spinnerFrame%len(detailSpinnerFrames)] + " " + heading
	case actionDone:
		if session.interrupted {
			heading = "Interrupted: " + heading
		} else {
			heading = "Failed: " + heading
		}
	}
	lines = append(lines, title.Render(ansi.Truncate(heading, inner, "…")), "")

	for index, step := range session.steps {
		glyph := faint.Render("·")
		switch session.states[index] {
		case stepRunning:
			glyph = title.Render("›")
		case stepSucceeded:
			glyph = success.Render("✓")
		case stepFailed:
			glyph = danger.Render("✗")
		case stepSkipped:
			glyph = faint.Render("–")
		}
		if session.phase == actionConfirm {
			glyph = " "
		}
		command := fmt.Sprintf("brew %s --%s", step.Kind, step.Type)
		names := make([]string, len(step.Packages))
		for i, name := range step.Packages {
			names[i] = string(name)
		}
		prefix := glyph + " " + command + " "
		wrapped := wrapWords(names, inner-ansi.StringWidth(prefix))
		for row, text := range wrapped {
			if row == 0 {
				lines = append(lines, prefix+text)
				continue
			}
			lines = append(lines, strings.Repeat(" ", ansi.StringWidth(prefix))+text)
		}
	}

	// Fixed rows: border and padding (4), heading and gap (2), buttons or
	// hint (2), output gap (1). Output gets whatever the terminal has left.
	fixed := 4 + len(lines) + 3
	outputRows := min(10, height-fixed)
	output := session.output
	if session.phase == actionDone {
		output = session.failedOutput
	}
	if session.phase != actionConfirm && outputRows >= 1 {
		tail := outputTail(output, outputRows, inner)
		if len(tail) == 0 {
			tail = []string{"Waiting for Homebrew…"}
		}
		lines = append(lines, "")
		for _, line := range tail {
			lines = append(lines, faint.Render(line))
		}
	}
	if session.phase == actionDone && session.err != nil && !session.interrupted {
		lines = append(lines, "", danger.Render(ansi.Truncate(session.err.Error(), inner, "…")))
	}

	lines = append(lines, "")
	buttonRow := -1
	var confirmOffset, cancelOffset int
	confirmLabel, cancelLabel := "", ""
	switch session.phase {
	case actionConfirm:
		confirmLabel = " " + session.verb() + " "
		cancelLabel = " Cancel "
		primary := lipgloss.NewStyle().Bold(true).Foreground(p.selectedFG).Background(p.selectedBG)
		secondary := lipgloss.NewStyle().Bold(true).Foreground(p.statusFG).Background(p.recessedBG)
		confirmStyle, cancelStyle := primary, secondary
		if session.cancelFocused {
			confirmStyle, cancelStyle = secondary, primary
		}
		buttonRow = len(lines)
		cancelOffset = ansi.StringWidth(confirmLabel) + 3
		lines = append(lines, confirmStyle.Render(confirmLabel)+"   "+cancelStyle.Render(cancelLabel))
	case actionRunning:
		hint := "esc twice to interrupt"
		if session.interruptArm {
			hint = "press esc again to interrupt Homebrew"
		}
		lines = append(lines, faint.Render(hint))
	case actionDone:
		closeLabel := " Close "
		confirmLabel = closeLabel
		buttonRow = len(lines)
		lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(p.selectedFG).Background(p.selectedBG).Render(closeLabel))
	}

	maxLines := max(3, height-4)
	if len(lines) > maxLines {
		// Drop from the top so the buttons and newest output stay visible.
		drop := len(lines) - maxLines
		lines = lines[drop:]
		if buttonRow >= 0 {
			buttonRow -= drop
		}
	}
	// Shrink to the content (with a floor so the modal does not jump in
	// width as output streams in), never past the available width.
	natural := 40
	for _, line := range lines {
		natural = max(natural, ansi.StringWidth(line))
	}
	if session.phase != actionConfirm {
		natural = inner
	}
	inner = min(inner, natural)
	for index, line := range lines {
		lines[index] = fitANSI(line, inner)
	}
	panel := lipgloss.NewStyle().
		Padding(1, 2).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(p.primary).
		Foreground(p.statusFG).
		Background(p.raisedBG).
		Render(strings.Join(lines, "\n"))
	panelWidth, panelHeight := min(width, lipgloss.Width(panel)), min(height, lipgloss.Height(panel))
	bounds := actionModalBounds{x: max(0, (width-panelWidth)/2), y: max(0, (height-panelHeight)/2), width: panelWidth, height: panelHeight}
	if buttonRow >= 0 {
		// Border (1) and padding (1 row, 2 columns) sit before content.
		contentX := bounds.x + 3
		bounds.buttonY = bounds.y + 2 + buttonRow
		bounds.confirmX, bounds.confirmW = contentX+confirmOffset, ansi.StringWidth(confirmLabel)
		if cancelLabel != "" {
			bounds.cancelX, bounds.cancelW = contentX+cancelOffset, ansi.StringWidth(cancelLabel)
		}
	}
	return panel, bounds
}

// actionModalHit reports which modal button, if any, sits under the pointer.
func (m Model) actionModalHit(x, y, width, height int) (confirm, cancel bool) {
	_, bounds := m.actionModalView(width, height)
	if m.action == nil || bounds.confirmW == 0 || y != bounds.buttonY {
		return false, false
	}
	return x >= bounds.confirmX && x < bounds.confirmX+bounds.confirmW,
		bounds.cancelW > 0 && x >= bounds.cancelX && x < bounds.cancelX+bounds.cancelW
}

func (m Model) handleActionClick(x, y, width, height int) (tea.Model, tea.Cmd) {
	confirm, cancel := m.actionModalHit(x, y, width, height)
	switch {
	case confirm && m.action.phase == actionConfirm:
		return m.confirmAction()
	case confirm && m.action.phase == actionDone:
		m.action, m.err = nil, nil
	case cancel:
		m.action = nil
	}
	return m, nil
}

// wrapWords joins words with spaces into lines no wider than width.
func wrapWords(words []string, width int) []string {
	width = max(8, width)
	var lines []string
	current := ""
	for _, word := range words {
		word = ansi.Truncate(word, width, "…")
		switch {
		case current == "":
			current = word
		case ansi.StringWidth(current)+1+ansi.StringWidth(word) <= width:
			current += " " + word
		default:
			lines = append(lines, current)
			current = word
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// outputTail returns the last rows of brew output as plain, single-width
// lines. Carriage returns overwrite in place (progress bars), so only the text
// after the last one on each line is kept.
func outputTail(output string, rows, width int) []string {
	if rows <= 0 {
		return nil
	}
	output = strings.ReplaceAll(ansi.Strip(output), "\t", "    ")
	raw := strings.Split(strings.TrimRight(output, "\r\n "), "\n")
	var lines []string
	for _, line := range raw {
		if index := strings.LastIndex(strings.TrimRight(line, "\r"), "\r"); index >= 0 {
			line = line[index+1:]
		}
		line = strings.TrimRight(line, "\r ")
		lines = append(lines, ansi.Truncate(line, width, "…"))
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	return lines
}

type actionSpinnerTick struct{ Session uint64 }

func (m Model) actionSpinner(session uint64) tea.Cmd {
	return tea.Tick(detailSpinInterval, func(time.Time) tea.Msg { return actionSpinnerTick{Session: session} })
}

func (m Model) runAction(ctx context.Context, session uint64, step int, action homebrew.Action) tea.Cmd {
	send := m.deps.Send
	runner := m.deps.Actions
	return func() tea.Msg {
		retained := homebrew.NewRetainedOutput()
		emitter := newActionOutputWriter(func(msg tea.Msg) {
			if send != nil {
				output := msg.(ActionOutput)
				output.Session, output.Step = session, step
				send(output)
			}
		})
		writer := io.MultiWriter(retained, emitter)
		var err error
		if runner == nil {
			err = fmt.Errorf("Homebrew actions are unavailable")
		} else {
			err = runner.RunAction(ctx, action, writer)
		}
		emitter.Close()
		return ActionFinished{Session: session, Step: step, Action: action, Output: retained.String(), Err: err}
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

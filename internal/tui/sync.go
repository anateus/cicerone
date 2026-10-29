package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const syncStatusModalMaxWidth = 82

type syncModalBounds struct {
	x, y, width, height int
}

type syncFailureKey struct {
	source  string
	history bool
}

func (m Model) syncStatusAvailable() bool {
	if len(m.queuedSync) > 0 || len(m.syncFailures) > 0 {
		return true
	}
	for _, active := range m.activeSync {
		if active {
			return true
		}
	}
	return false
}

func (m Model) syncFooterStatus() string {
	active := 0
	for _, running := range m.activeSync {
		if running {
			active++
		}
	}
	queued, failed := len(m.queuedSync), len(m.syncFailures)
	if active+queued+failed == 0 {
		return ""
	}
	var counts []string
	if active > 0 {
		counts = append(counts, fmt.Sprintf("%d active", active))
	}
	if queued > 0 {
		counts = append(counts, fmt.Sprintf("%d queued", queued))
	}
	if failed > 0 {
		counts = append(counts, fmt.Sprintf("%d failed", failed))
	}
	return "Sync: " + strings.Join(counts, " · ")
}

func (m Model) syncStatusLines() []string {
	lines := []string{"Synchronization status"}
	status := m.syncFooterStatus()
	if status == "" {
		status = "No active or queued sync work"
	}
	lines = append(lines, status)

	var activeSources []string
	for source, active := range m.activeSync {
		if active {
			activeSources = append(activeSources, source)
		}
	}
	sort.Strings(activeSources)
	for _, source := range activeSources {
		lines = append(lines, fmt.Sprintf("Active · %s · %s", source, m.syncPhaseLabel(source)))
		if progress, ok := m.syncProgress[source]; ok {
			counts := fmt.Sprintf("Counts · %d commits · %d updates · %d batches", progress.Commits, progress.Events, progress.Batches)
			if progress.Diagnostics > 0 {
				counts += fmt.Sprintf(" · %d diagnostics", progress.Diagnostics)
			}
			lines = append(lines, counts)
		}
	}

	queued := make([]SyncQueued, 0, len(m.queuedSync))
	for _, job := range m.queuedSync {
		queued = append(queued, job)
	}
	sort.Slice(queued, func(i, j int) bool {
		if queued[i].Source != queued[j].Source {
			return queued[i].Source < queued[j].Source
		}
		return queued[i].ID < queued[j].ID
	})
	for _, job := range queued {
		kind := "refresh"
		if job.HistoricalFallback {
			kind = "history indexing"
		}
		lines = append(lines, fmt.Sprintf("Queued · %s · %s · job %d", job.Source, kind, job.ID))
	}

	for _, line := range m.syncFailureLines() {
		lines = append(lines, line)
	}

	if m.freshnessErr != nil {
		lines = append(lines, "Freshness unavailable · "+m.freshnessErr.Error())
	} else if !m.syncStatusAvailable() {
		if m.freshness.LastSync.IsZero() {
			lines = append(lines, "Last successful sync · never")
		} else {
			lines = append(lines, "Last successful sync · "+m.freshness.LastSync.In(time.Local).Format("Jan 2, 2006 15:04"))
		}
	}
	return lines
}

func (m Model) syncFailureLines() []string {
	keys := make([]syncFailureKey, 0, len(m.syncFailures))
	for key := range m.syncFailures {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].source != keys[j].source {
			return keys[i].source < keys[j].source
		}
		return !keys[i].history && keys[j].history
	})
	lines := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		kind := "refresh"
		if key.history {
			kind = "history indexing"
		}
		lines = append(lines, fmt.Sprintf("Failed · %s · %s · %s", key.source, kind, m.syncFailures[key]))
	}
	if m.freshness.LastError != "" &&
		(m.freshness.LastSync.IsZero() || m.freshness.LastAttempt.After(m.freshness.LastSync)) {
		alreadyShown := false
		for _, failure := range m.syncFailures {
			if strings.Contains(m.freshness.LastError, failure) || strings.Contains(failure, m.freshness.LastError) {
				alreadyShown = true
				break
			}
		}
		if !alreadyShown {
			lines = append(lines, "Last failure · "+m.freshness.LastError)
		}
	}
	return lines
}

func (m Model) syncPhaseLabel(source string) string {
	switch source {
	case "installed":
		return "refreshing installed packages"
	case "repositories":
		return "preparing repositories"
	}
	if m.syncPhase[source] == "fetching" {
		return "fetching/checking"
	}
	if m.fallbackSync[source] {
		return "history indexing"
	}
	if m.syncPhase[source] == "indexing" || m.syncProgress[source].Source != "" {
		return "indexing"
	}
	return "synchronizing"
}

func (m Model) syncStatusWindow(width, height int) ([]string, int) {
	lines := m.syncStatusLines()
	contentWidth, maxLines := width, height
	if width >= 8 && height >= 3 {
		contentWidth = min(syncStatusModalMaxWidth-4, width-4)
		maxLines = height - 2
	}
	contentWidth, maxLines = max(1, contentWidth), max(1, maxLines)
	var content []string
	for _, line := range lines[1:] {
		for _, part := range strings.Split(line, "\n") {
			var current string
			for _, word := range strings.Fields(part) {
				if current != "" && ansi.StringWidth(current)+1+ansi.StringWidth(word) <= contentWidth {
					current += " " + word
					continue
				}
				if current != "" {
					content = append(content, current)
					current = ""
				}
				wordWidth := ansi.StringWidth(word)
				for start := 0; start < wordWidth; start += contentWidth {
					chunk := ansi.Cut(word, start, start+contentWidth)
					if start+contentWidth < wordWidth {
						content = append(content, chunk)
					} else {
						current = chunk
					}
				}
			}
			content = append(content, current)
		}
	}
	if maxLines == 1 {
		last := max(0, len(content)-1)
		offset := min(max(0, m.syncStatusOffset), last)
		if len(content) == 0 {
			return lines[:1], 0
		}
		return content[offset : offset+1], last
	}
	capacity := maxLines - 1
	showPosition := len(content) > capacity && capacity > 1
	if showPosition {
		capacity--
	}
	last := max(0, len(content)-capacity)
	offset := min(max(0, m.syncStatusOffset), last)
	end := min(len(content), offset+capacity)
	window := append([]string{ansi.Truncate(lines[0], contentWidth, "")}, content[offset:end]...)
	if showPosition {
		window = append(window, fmt.Sprintf("↑↓ %d-%d/%d", offset+1, end, len(content)))
	}
	return window, last
}

func (m Model) syncStatusModalView(width, height int) (string, syncModalBounds) {
	if width <= 0 || height <= 0 {
		return "", syncModalBounds{}
	}
	lines, _ := m.syncStatusWindow(width, height)
	if width < 8 || height < 3 {
		for index, line := range lines {
			lines[index] = fitANSI(line, width)
		}
		return strings.Join(lines, "\n"), syncModalBounds{width: width, height: len(lines)}
	}

	innerWidth := max(1, min(syncStatusModalMaxWidth-4, width-4))
	for index, line := range lines {
		lines[index] = fitANSI(line, innerWidth)
	}
	p := m.palette()
	panel := lipgloss.NewStyle().
		Padding(0, 1).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(p.primary).
		Foreground(p.statusFG).
		Background(p.raisedBG).
		Render(strings.Join(lines, "\n"))
	panelWidth, panelHeight := lipgloss.Width(panel), lipgloss.Height(panel)
	bounds := syncModalBounds{
		x: max(0, (width-panelWidth)/2), y: max(0, (height-panelHeight)/2),
		width: min(width, panelWidth), height: min(height, panelHeight),
	}
	return panel, bounds
}

func (m Model) overlaySyncStatusModal(body, modal string, bounds syncModalBounds, width, height int) string {
	lines := strings.Split(body, "\n")
	for len(lines) < height {
		lines = append(lines, m.surfaceLine("", width, m.palette().canvasBG))
	}
	for row, panelLine := range strings.Split(modal, "\n") {
		y := bounds.y + row
		if y < 0 || y >= len(lines) {
			continue
		}
		base := fitANSI(lines[y], width)
		left := ansi.Cut(base, 0, bounds.x)
		right := ansi.Cut(base, bounds.x+bounds.width, width)
		lines[y] = left + fitANSI(panelLine, bounds.width) + right
	}
	return strings.Join(lines, "\n")
}

func (m Model) handleSyncStatusKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q":
		return m, tea.Quit
	case "esc", "enter", "s":
		m.syncStatusOpen = false
	case "j", "down":
		m.scrollSyncStatus(1)
	case "k", "up":
		m.scrollSyncStatus(-1)
	}
	return m, nil
}

func (m *Model) scrollSyncStatus(delta int) {
	width, height := m.width, m.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	_, last := m.syncStatusWindow(width, height-statusHeight)
	offset := min(last, max(0, m.syncStatusOffset))
	m.syncStatusOffset = min(last, max(0, offset+delta))
}

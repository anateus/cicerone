package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/store"
	"github.com/charmbracelet/x/ansi"
)

func (m Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return m, nil
	}
	mouse := msg.Mouse()
	if mouse.Y == m.height-1 {
		hints := m.footerHints(m.width, m.statusText())
		x := m.width - footerHintsWidth(hints)
		for i, hint := range hints {
			if i > 0 {
				x += 2
			}
			end := x + ansi.StringWidth(hint.key) + ansi.StringWidth(hint.label) + 3
			if hint.key == "a" && mouse.X >= x && mouse.X < end {
				return m.requestSelectedAction()
			}
			x = end
		}
		return m, nil
	}
	feedWidth, inspectorWidth := m.width, m.width
	wide := m.width >= narrowBreakpoint
	if wide {
		feedWidth, inspectorWidth = m.paneWidths()
	}

	inInspector := (!wide && m.detailOpen) || (wide && mouse.X > feedWidth)
	if inInspector {
		localX := mouse.X
		if wide {
			localX -= feedWidth + 1
			m.focus = inspectorPane
		}
		contentY := mouse.Y + m.inspectorViewport.YOffset()
		lines := strings.Split(ansi.Strip(m.renderInspector(inspectorWidth)), "\n")
		if contentY >= 0 && contentY < len(lines) {
			line := lines[contentY]
			upperLine := strings.ToUpper(line)
			if textHit(upperLine, "README", localX) {
				m.document = store.DocumentREADME
				m.documentExplicit = true
				m.inspectorViewport.SetYOffset(0)
				m.syncViewports()
			}
			if textHit(upperLine, "CHANGELOG", localX) {
				m.document = store.DocumentChangelog
				m.documentExplicit = true
				m.inspectorViewport.SetYOffset(0)
				m.syncViewports()
			}
			if strings.Contains(line, "m load 10 more releases") {
				return m.requestMoreReleases()
			}
		}
		return m, nil
	}

	if mouse.Y == 1 || mouse.Y == 2 {
		switch {
		case mouse.X >= 0 && mouse.X < 12:
			m.setPackageScope(true, false)
			return m.filterChanged()
		case mouse.X >= 12 && mouse.X < 21:
			m.setPackageScope(false, true)
			return m.filterChanged()
		case mouse.X >= 21 && mouse.X < 28:
			m.setPackageScope(true, true)
			return m.filterChanged()
		}
	}
	headerRows := m.feedHeaderRows()
	if mouse.Y < headerRows {
		return m, nil
	}
	contentY := mouse.Y - headerRows + m.viewportOffset
	if index := m.feedGroupAtLine(contentY); index >= 0 {
		return m.selectFeedIndex(index)
	}
	return m, nil
}

func (m Model) handleMouseWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	mouse := msg.Mouse()
	feedWidth := m.width
	if m.width >= narrowBreakpoint {
		feedWidth, _ = m.paneWidths()
	}
	inInspector := (m.width < narrowBreakpoint && m.detailOpen) || (m.width >= narrowBreakpoint && mouse.X > feedWidth)
	if inInspector {
		m.inspectorViewport, _ = m.inspectorViewport.Update(msg)
		return m, nil
	}
	m.feedViewport, _ = m.feedViewport.Update(msg)
	m.viewportOffset = m.feedViewport.YOffset()
	return m, tea.Batch(m.loadVisiblePackageDescriptions()...)
}

func textHit(line, label string, column int) bool {
	index := strings.Index(line, label)
	if index < 0 {
		return false
	}
	start := ansi.StringWidth(line[:index])
	return column >= start && column < start+ansi.StringWidth(label)
}

func (m Model) feedGroupAtLine(line int) int {
	cursor := 0
	boundary := m.seenBoundary()
	for index, group := range m.groups {
		if m.hasSeenSeparator() && index == boundary {
			if line == cursor {
				return -1
			}
			cursor++
		}
		rows := feedRowHeight(m.feedViewport.Width())
		if m.expanded[group.ID] {
			rows += len(group.Events) - 1
		}
		if line >= cursor && line < cursor+rows {
			return index
		}
		cursor += rows
	}
	return -1
}

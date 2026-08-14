package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"cicerone/internal/domain"
)

func (m Model) renderFeed(width int) string {
	return m.renderFeedHeader(width) + "\n" + m.renderFeedRows(width)
}

func (m Model) renderFeedHeader(width int) string {
	var b strings.Builder
	b.WriteString(m.feedTitle(width))
	b.WriteByte('\n')
	b.WriteString(m.feedControls(width))
	b.WriteByte('\n')
	b.WriteString(m.feedListHeader(width))
	return b.String()
}

func (m Model) renderFeedRows(width int) string {
	if m.loading && len(m.groups) == 0 {
		return m.contentLine(" Loading package history…", width)
	}
	if m.err != nil && len(m.groups) == 0 {
		return m.contentLine(" Error: "+m.err.Error(), width)
	}
	if len(m.groups) == 0 {
		return m.contentLine(" No package updates match these filters.", width)
	}
	var b strings.Builder
	boundary := m.seenBoundary()
	for i, g := range m.groups {
		if m.hasSeenSeparator() && i == boundary {
			b.WriteString(m.seenSeparator(width))
			b.WriteByte('\n')
		}
		for _, line := range m.renderFeedGroup(i, g, width) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (m Model) renderVisibleFeedRows(width, offset, height int) (string, int) {
	if len(m.groups) == 0 {
		content := m.renderFeedRows(width)
		return padLines(content, height), lineCountForViewport(content)
	}
	total := m.feedLineCount(width)
	end := min(total, offset+height)
	cursor := 0
	boundary := m.seenBoundary()
	lines := make([]string, 0, height)
	for index, group := range m.groups {
		if m.hasSeenSeparator() && index == boundary {
			if cursor >= offset && cursor < end {
				lines = append(lines, m.seenSeparator(width))
			}
			cursor++
		}
		groupHeight := m.feedGroupHeight(group, width)
		if cursor >= end {
			break
		}
		if cursor+groupHeight > offset {
			rendered := m.renderFeedGroup(index, group, width)
			from := max(0, offset-cursor)
			to := min(len(rendered), end-cursor)
			lines = append(lines, rendered[from:to]...)
		}
		cursor += groupHeight
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n"), total
}

func (m Model) renderFeedGroup(index int, group domain.FeedGroup, width int) []string {
	if m.snoozedGroupCollapsed(group) {
		return []string{m.collapsedSnoozedRow(index, width)}
	}
	marker := "  "
	if index == m.selected {
		marker = "› "
	}
	lines := m.feedGroupRows(marker, group.Events[0], width)
	p := m.palette()
	for row, line := range lines {
		if index == m.selected {
			lines[row] = m.selectedLine(line)
		} else if index%2 == 1 {
			lines[row] = m.surfaceLine(line, width, p.alternateRowBG)
		}
	}
	if m.expanded[group.ID] {
		for _, child := range group.Events[1:] {
			lines = append(lines, m.feedChildRow(child, width))
		}
	}
	return lines
}

func (m Model) collapsedSnoozedRow(index, width int) string {
	p := m.palette()
	background := p.feedBG
	if index%2 == 1 {
		background = p.alternateRowBG
	}
	ruleWidth := min(16, max(4, width/4))
	text := "   " + strings.Repeat("┄", ruleWidth)
	foreground := lipgloss.Color("#7C7C7C")
	if m.light {
		foreground = lipgloss.Color("#888888")
	}
	return preserveOuterStyle(lipgloss.NewStyle().Faint(true).Foreground(foreground).Background(background).Render(fit(text, width)))
}

func (m Model) feedLineCount(width int) int {
	if len(m.groups) == 0 {
		return 1
	}
	total := 0
	for _, group := range m.groups {
		total += m.feedGroupHeight(group, width)
	}
	if m.hasSeenSeparator() {
		total++
	}
	return total
}

func (m Model) snoozedGroupCollapsed(group domain.FeedGroup) bool {
	return !m.showSnoozed && len(group.Events) > 0 && group.Events[0].Status == domain.PackageStatusSnoozed
}

func (m Model) feedGroupHeight(group domain.FeedGroup, width int) int {
	if m.snoozedGroupCollapsed(group) {
		return 1
	}
	height := feedRowHeight(width)
	if m.expanded[group.ID] {
		height += len(group.Events) - 1
	}
	return height
}

func padLines(content string, height int) string {
	lines := strings.Split(content, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

package tui

import (
	"strings"
)

func (m Model) renderFeed(width int) string {
	var b strings.Builder
	b.WriteString(m.feedTitle(width))
	b.WriteByte('\n')
	b.WriteString(m.feedControls(width))
	b.WriteByte('\n')
	if m.loading && len(m.groups) == 0 {
		b.WriteString(m.contentLine(" Loading package history…", width))
		return b.String()
	}
	if m.err != nil && len(m.groups) == 0 {
		b.WriteString(m.contentLine(" Error: "+m.err.Error(), width))
		return b.String()
	}
	if len(m.groups) == 0 {
		b.WriteString(m.contentLine(" No package updates match these filters.", width))
		return b.String()
	}
	for i, g := range m.groups {
		marker := "  "
		if i == m.selected {
			marker = "› "
		}
		e := g.Events[0]
		line := m.feedGroupRow(marker, e, width)
		if i == m.selected {
			line = m.selectedLine(line)
		}
		b.WriteString(line)
		if i < len(m.groups)-1 || m.expanded[g.ID] {
			b.WriteByte('\n')
		}
		if m.expanded[g.ID] {
			for _, child := range g.Events[1:] {
				b.WriteString(m.feedChildRow(child, width))
				b.WriteByte('\n')
			}
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

package tui

import (
	"fmt"
	"strings"
)

func (m Model) renderFeed(width int) string {
	var b strings.Builder
	b.WriteString(m.titleLine(" Cicerone  Package updates", width))
	b.WriteByte('\n')
	b.WriteString(fit(fmt.Sprintf(" Search: %-18s  Roll up: %s", m.filter.Query, onOff(m.filter.RollUp)), width))
	b.WriteByte('\n')
	if m.loading && len(m.groups) == 0 {
		b.WriteString(fit(" Loading package history…", width))
		return b.String()
	}
	if m.err != nil && len(m.groups) == 0 {
		b.WriteString(fit(" Error: "+m.err.Error(), width))
		return b.String()
	}
	if len(m.groups) == 0 {
		b.WriteString(fit(" No package updates match these filters.", width))
		return b.String()
	}
	for i, g := range m.groups {
		marker := "  "
		if i == m.selected {
			marker = "› "
		}
		e := g.Events[0]
		row := fmt.Sprintf("%s%-18s %-8s %s → %s", marker, e.Name, e.Kind, e.OldVersion, e.NewVersion)
		line := fit(row, width)
		if i == m.selected {
			line = m.selectedLine(line)
		}
		b.WriteString(line)
		if i < len(m.groups)-1 || m.expanded[g.ID] {
			b.WriteByte('\n')
		}
		if m.expanded[g.ID] {
			for _, child := range g.Events[1:] {
				b.WriteString(fit(fmt.Sprintf("    └ %-16s %s → %s", child.Kind, child.OldVersion, child.NewVersion), width))
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

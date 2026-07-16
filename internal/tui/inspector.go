package tui

import (
	"fmt"
	"strings"
)

func (m Model) renderInspector(width int) string {
	var b strings.Builder
	b.WriteString(m.titleLine(" Inspector", width))
	b.WriteByte('\n')
	if len(m.groups) == 0 {
		b.WriteString(fit(" Select an update to inspect.", width))
		return b.String()
	}
	e := m.selectedEvent()
	for _, line := range []string{e.Name, fmt.Sprintf("%s  %s → %s", e.Kind, e.OldVersion, e.NewVersion), "", "Changelog"} {
		b.WriteString(fit(" "+line, width))
		b.WriteByte('\n')
	}
	if m.err != nil {
		b.WriteString(fit(" Error: "+m.err.Error(), width))
		return b.String()
	}
	if len(m.changelog) == 0 {
		b.WriteString(fit(" Waiting for selection…", width))
		return b.String()
	}
	for _, section := range m.changelog {
		b.WriteString(fit(" "+section.Version, width))
		b.WriteByte('\n')
		for _, line := range strings.Split(section.Body, "\n") {
			b.WriteString(fit(" "+line, width))
			b.WriteByte('\n')
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

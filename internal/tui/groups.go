package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/charmbracelet/x/ansi"
)

// groupTab is one entry in the group strip under the type tabs.
type groupTab struct {
	label  string
	scope  domain.GroupScope
	target domain.PackageGroupID
}

// groupTabs builds the strip order: All, Ungrouped, then user groups by their
// index. Starred and Hidden sit at the end so their position stays stable as
// the user accumulates groups.
func groupTabs(groups []domain.PackageGroup) []groupTab {
	tabs := []groupTab{
		{label: "All", scope: domain.GroupScopeAll},
		{label: "Ungrouped", scope: domain.GroupScopeUngrouped},
	}
	ordered := make([]domain.PackageGroup, len(groups))
	copy(ordered, groups)
	sort.SliceStable(ordered, func(a, b int) bool { return ordered[a].Index < ordered[b].Index })
	for _, group := range ordered {
		tabs = append(tabs, groupTab{label: group.Name, scope: domain.GroupScopeUser, target: group.ID})
	}
	tabs = append(tabs,
		groupTab{label: "Starred", scope: domain.GroupScopeStarred},
		groupTab{label: "Hidden", scope: domain.GroupScopeHidden})
	return tabs
}

// activeGroupTab returns the strip index matching the filter's group scope.
func activeGroupTab(groups []domain.PackageGroup, filter domain.FeedFilter) int {
	for index, tab := range groupTabs(groups) {
		if tab.scope == filter.GroupScope && tab.target == filter.GroupTarget {
			return index
		}
	}
	return 0
}

// groupModal is the package-group assignment modal opened with g.
// Row 0 is the new-group option; rows 1..n map to user groups. Selecting a
// row assigns the highlighted package; the Remove button clears membership.
type groupModal struct {
	groups     []domain.PackageGroup
	cursor     int
	name       string
	assignedTo domain.PackageGroupID
	removeHot  bool
}

func newGroupModal(groups []domain.PackageGroup, assigned domain.PackageGroupID) groupModal {
	// Default the cursor to the first non-new group; fall back to the
	// new-group row when no groups exist yet.
	cursor := 0
	if len(groups) > 0 {
		cursor = 1
	}
	if len(groups) == 0 && assigned == 0 {
		cursor = 0
	}
	return groupModal{groups: groups, cursor: cursor, assignedTo: assigned}
}

func (m groupModal) rowCount() int { return len(m.groups) + 1 }

// groupModalMaxRows bounds the group list height so the modal stays on screen
// when the user has accumulated many groups.
const groupModalMaxRows = 8

// visibleWindow returns the slice of group rows shown in the modal, keeping
// the cursor inside it.
func (m groupModal) visibleWindow() (start, end int) {
	total := m.rowCount()
	if total <= groupModalMaxRows {
		return 0, total
	}
	start = min(max(0, m.cursor-groupModalMaxRows/2), total-groupModalMaxRows)
	return start, start + groupModalMaxRows
}

func (m groupModal) selectedGroup() (domain.PackageGroup, bool) {
	if m.cursor <= 0 || m.cursor > len(m.groups) {
		return domain.PackageGroup{}, false
	}
	return m.groups[m.cursor-1], true
}

// assignRequest resolves the modal selection to a group assignment. Selecting
// the group the package already belongs to clears its membership instead.
func (m groupModal) assignRequest(packageID domain.PackageID) (groupAssignRequested, bool) {
	request := groupAssignRequested{PackageID: packageID}
	if m.cursor == 0 {
		if strings.TrimSpace(m.name) == "" {
			return request, false
		}
		request.NewName = strings.TrimSpace(m.name)
		return request, true
	}
	group, ok := m.selectedGroup()
	if !ok {
		return request, false
	}
	if group.ID == m.assignedTo {
		request.Clear = true
		return request, true
	}
	request.GroupID = group.ID
	return request, true
}

func (m Model) renderGroupModal() string {
	if m.groupAssign == nil {
		return ""
	}
	p := m.palette()
	modal := m.groupAssign
	event := m.selectedEvent()

	var b strings.Builder
	heading := "Group: " + event.Name
	if event.PackageID == "" {
		heading = "Group"
	}
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(p.primary).Render(heading))
	b.WriteByte('\n')

	radio := func(selected bool) string {
		if selected {
			return "(●)"
		}
		return "( )"
	}
	rowStart, rowEnd := modal.visibleWindow()
	if rowStart > 0 {
		b.WriteString(lipgloss.NewStyle().Faint(true).Render(fit(fmt.Sprintf("   … %d more above", rowStart), 44)))
		b.WriteByte('\n')
	}
	for index := rowStart; index < rowEnd; index++ {
		if index == 0 {
			// The new-group row leads the list.
			label := "New group"
			if modal.nameFocused() {
				label = "New group: " + modal.name + "█"
			}
			style := lipgloss.NewStyle().Foreground(p.primary)
			if modal.cursor == 0 {
				style = style.Bold(true)
			}
			b.WriteString(style.Render(fit(fmt.Sprintf(" %s %s", radio(modal.cursor == 0), label), 44)))
			b.WriteByte('\n')
			continue
		}
		group := modal.groups[index-1]
		row := modal.cursor == index && !modal.nameFocused()
		line := fmt.Sprintf(" %s %s", radio(row), group.Name)
		if modal.assignedTo == group.ID {
			if modal.cursor == index {
				line += "  ← enter to remove"
			} else {
				line += "  ←"
			}
		}
		style := lipgloss.NewStyle()
		if modal.cursor == index {
			style = style.Bold(true)
		}
		b.WriteString(style.Render(fit(line, 44)))
		b.WriteByte('\n')
	}
	if rowEnd < modal.rowCount() {
		b.WriteString(lipgloss.NewStyle().Faint(true).Render(fit(fmt.Sprintf("   … %d more below", modal.rowCount()-rowEnd), 44)))
		b.WriteByte('\n')
	}

	b.WriteByte('\n')
	confirmStyle := lipgloss.NewStyle().Bold(true).Foreground(p.selectedFG).Background(p.selectedBG)
	cancelStyle := lipgloss.NewStyle().Bold(true).Foreground(p.statusFG).Background(p.recessedBG)
	removeStyle := cancelStyle
	if modal.removeHot {
		removeStyle = confirmStyle
	}
	b.WriteString(" " + confirmStyle.Render(" Enter ") + "  " + removeStyle.Render(" Remove group ") + "  " + cancelStyle.Render(" Esc "))
	return b.String()
}

func (m groupModal) nameFocused() bool { return m.cursor == 0 && m.name != "" }

// overlayGroupModal centers the group modal over the feed body.
func (m Model) overlayGroupModal(body, modal string, width, height int) string {
	panelStyle := lipgloss.NewStyle().
		Padding(1, 2).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.palette().primary).
		Foreground(m.palette().statusFG).
		Background(m.palette().raisedBG)
	panel := panelStyle.Render(modal)
	panelWidth, panelHeight := lipgloss.Width(panel), lipgloss.Height(panel)
	lines := strings.Split(body, "\n")
	for len(lines) < height {
		lines = append(lines, m.surfaceLine("", width, m.palette().canvasBG))
	}
	x := max(0, (width-panelWidth)/2)
	y := max(0, (height-panelHeight)/2)
	panelLines := strings.Split(panel, "\n")
	for row, panelLine := range panelLines {
		target := y + row
		if target < 0 || target >= len(lines) {
			continue
		}
		base := fitANSI(lines[target], width)
		left := ansi.Cut(base, 0, x)
		right := ansi.Cut(base, x+panelWidth, width)
		lines[target] = left + fitANSI(panelLine, panelWidth) + right
	}
	return strings.Join(lines, "\n")
}

// handleGroupModalKey routes keys while the group modal is open.
func (m Model) handleGroupModalKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	modal := *m.groupAssign
	switch key.String() {
	case "esc":
		m.groupAssign = nil
		return m, nil
	case "j", "down":
		if modal.cursor < modal.rowCount()-1 {
			modal.cursor++
		} else {
			modal.cursor = 0
		}
		modal.removeHot = false
		m.groupAssign = &modal
		return m, nil
	case "k", "up":
		if modal.cursor > 0 {
			modal.cursor--
		} else {
			modal.cursor = modal.rowCount() - 1
		}
		modal.removeHot = false
		m.groupAssign = &modal
		return m, nil
	case "backspace":
		if modal.cursor == 0 && modal.name != "" {
			runes := []rune(modal.name)
			modal.name = string(runes[:len(runes)-1])
			m.groupAssign = &modal
		}
		return m, nil
	case "tab":
		modal.removeHot = !modal.removeHot
		m.groupAssign = &modal
		return m, nil
	case "enter":
		if modal.removeHot {
			return m.removeSelectedGroup()
		}
		if event := m.selectedEvent(); event.PackageID != "" {
			if request, ok := modal.assignRequest(event.PackageID); ok {
				m.groupAssign = nil
				return m, func() tea.Msg { return request }
			}
		}
		return m, nil
	}
	if text := key.Key().Text; text != "" && modal.cursor == 0 {
		if ansi.StringWidth(modal.name) < 32 {
			modal.name += text
			m.groupAssign = &modal
		}
		return m, nil
	}
	return m, nil
}

// removeSelectedGroup deletes the group highlighted in the modal. Built-in
// scopes (Starred, Hidden) and the All/Ungrouped views have no row in the
// group list, so only user groups can be removed here.
func (m Model) removeSelectedGroup() (tea.Model, tea.Cmd) {
	modal := *m.groupAssign
	group, ok := modal.selectedGroup()
	if !ok {
		return m, nil
	}
	m.groupAssign = nil
	return m, func() tea.Msg { return groupDeleteRequested{GroupID: group.ID} }
}

// cycleGroupTab moves to the next or previous group tab in the strip.
func (m Model) cycleGroupTab(direction int) (tea.Model, tea.Cmd) {
	tabs := groupTabs(m.userGroups)
	if len(tabs) == 0 {
		return m, nil
	}
	current := activeGroupTab(m.userGroups, m.filter)
	next := (current + direction + len(tabs)) % len(tabs)
	tab := tabs[next]
	m.filter.GroupScope = tab.scope
	m.filter.GroupTarget = tab.target
	return m.filterChanged()
}

// selectedPackageGroupID returns the package's current group for the modal.
func (m Model) selectedPackageGroupID() domain.PackageGroupID {
	if !m.hasSelection() {
		return 0
	}
	return m.selectedEvent().GroupID
}

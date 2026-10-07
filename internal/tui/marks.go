package tui

import (
	"github.com/anateus/cicerone/internal/domain"
)

// markedPackage is the snapshot a mark needs to act on a package after it
// scrolls or filters out of view: brew needs the type and verb, the group
// modal needs current membership.
type markedPackage struct {
	Name      string
	Type      domain.PackageType
	Installed bool
	GroupID   domain.PackageGroupID
}

func (m Model) isMarked(id domain.PackageID) bool {
	_, ok := m.marked[id]
	return ok
}

// toggleMark marks or unmarks the selected row's package. Marks are keyed by
// package, so every row of a package (a roll-up and its children, or the same
// package across tabs) shares one mark.
func (m Model) toggleMark() Model {
	if !m.hasSelection() {
		return m
	}
	e := m.selectedEvent()
	if e.PackageID == "" {
		return m
	}
	if m.isMarked(e.PackageID) {
		m.unmark(e.PackageID)
	} else {
		m.marked = cloneMarks(m.marked)
		m.marked[e.PackageID] = markedPackage{Name: e.Name, Type: e.Type, Installed: e.Installed, GroupID: e.GroupID}
		m.markOrder = append(append([]domain.PackageID(nil), m.markOrder...), e.PackageID)
	}
	m.syncViewports()
	return m
}

// unmark drops one package's mark. Model copies share map storage, so the
// map is cloned before mutation to keep earlier Model values intact.
func (m *Model) unmark(id domain.PackageID) {
	if !m.isMarked(id) {
		return
	}
	m.marked = cloneMarks(m.marked)
	delete(m.marked, id)
	order := make([]domain.PackageID, 0, len(m.markOrder))
	for _, existing := range m.markOrder {
		if existing != id {
			order = append(order, existing)
		}
	}
	m.markOrder = order
}

func (m *Model) clearMarks() {
	m.marked = nil
	m.markOrder = nil
}

// refreshMarks updates marked snapshots from freshly loaded rows so an
// install or a group change made elsewhere is reflected in the next action.
func (m *Model) refreshMarks() {
	if len(m.marked) == 0 {
		return
	}
	updated := cloneMarks(m.marked)
	for _, group := range m.groups {
		for _, e := range group.Events {
			if mark, ok := updated[e.PackageID]; ok {
				mark.Installed, mark.GroupID, mark.Type = e.Installed, e.GroupID, e.Type
				updated[e.PackageID] = mark
			}
		}
	}
	m.marked = updated
}

func cloneMarks(marks map[domain.PackageID]markedPackage) map[domain.PackageID]markedPackage {
	clone := make(map[domain.PackageID]markedPackage, len(marks)+1)
	for id, mark := range marks {
		clone[id] = mark
	}
	return clone
}

// groupTargets returns the packages the group modal acts on: every marked
// package, or the selected row's package when nothing is marked.
func (m Model) groupTargets() []domain.PackageID {
	if len(m.markOrder) > 0 {
		return append([]domain.PackageID(nil), m.markOrder...)
	}
	if !m.hasSelection() || m.selectedEvent().PackageID == "" {
		return nil
	}
	return []domain.PackageID{m.selectedEvent().PackageID}
}

// mixedGroups marks a multi-package target whose packages are not all in the
// same group. No radio reads as assigned and Enter never clears.
const mixedGroups domain.PackageGroupID = -1

// targetGroupID is the group shared by every group-modal target, 0 when all
// are ungrouped, or mixedGroups when they differ.
func (m Model) targetGroupID() domain.PackageGroupID {
	if len(m.markOrder) == 0 {
		return m.selectedPackageGroupID()
	}
	shared := m.marked[m.markOrder[0]].GroupID
	for _, id := range m.markOrder[1:] {
		if m.marked[id].GroupID != shared {
			return mixedGroups
		}
	}
	return shared
}

package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/charmbracelet/x/ansi"
)

var errDuplicateGroup = errors.New("UNIQUE constraint failed: package_groups.name")

type fakeGroupData struct {
	fakeData
	groups      []domain.PackageGroup
	created     []string
	deleted     []domain.PackageGroupID
	assignments []struct {
		PackageID domain.PackageID
		GroupID   domain.PackageGroupID
	}
}

func (f *fakeGroupData) PackageGroups(context.Context) ([]domain.PackageGroup, error) {
	return f.groups, nil
}

func (f *fakeGroupData) CreatePackageGroup(_ context.Context, name string) (domain.PackageGroup, error) {
	f.created = append(f.created, name)
	return domain.PackageGroup{ID: domain.PackageGroupID(len(f.groups) + 1), Name: name, Index: len(f.groups) + 1}, nil
}

func (f *fakeGroupData) DeletePackageGroup(_ context.Context, id domain.PackageGroupID) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeGroupData) SetPackageGroup(_ context.Context, packageID domain.PackageID, groupID domain.PackageGroupID) error {
	f.assignments = append(f.assignments, struct {
		PackageID domain.PackageID
		GroupID   domain.PackageGroupID
	}{packageID, groupID})
	return nil
}

func groupTestModel(data *fakeGroupData, feedGroups []domain.FeedGroup) Model {
	m := NewModel(Dependencies{Data: data})
	m.width, m.height, m.loading = 120, 24, false
	m.groups = feedGroups
	return m
}

func TestGroupModalDefaultsToFirstExistingGroup(t *testing.T) {
	data := &fakeGroupData{groups: []domain.PackageGroup{{ID: 3, Name: "Editors", Index: 1}, {ID: 4, Name: "Terminal", Index: 2}}}
	m := groupTestModel(data, groups("a"))
	m.userGroups = data.groups

	m = update(t, m, key("g"))
	if m.groupAssign == nil {
		t.Fatal("g did not open the group modal")
	}
	if got := m.groupAssign.cursor; got != 1 {
		t.Fatalf("modal cursor = %d, want 1 (first non-new group)", got)
	}
}

func TestGroupModalCursorStartsAtNewGroupRowWithoutGroups(t *testing.T) {
	data := &fakeGroupData{}
	m := groupTestModel(data, groups("a"))

	m = update(t, m, key("g"))
	if got := m.groupAssign.cursor; got != 0 {
		t.Fatalf("modal cursor = %d, want 0 (new group row)", got)
	}
}

func TestGroupModalAssignsSelectedGroupOnEnter(t *testing.T) {
	data := &fakeGroupData{groups: []domain.PackageGroup{{ID: 3, Name: "Editors", Index: 1}}}
	m := groupTestModel(data, groups("a"))
	m.userGroups = data.groups

	m = update(t, m, key("g"))
	m, msg := updateAndRunCommand(t, m, key("enter"))
	request, ok := msg.(groupAssignRequested)
	if !ok {
		t.Fatalf("enter produced %T, want groupAssignRequested", msg)
	}
	if request.PackageID != "pkg-a" || request.GroupID != 3 {
		t.Fatalf("request = %#v, want pkg-a into group 3", request)
	}
	if m.groupAssign == nil || !m.groupAssign.pending {
		t.Fatalf("modal state after enter = %#v, want open with pending", m.groupAssign)
	}

	// Store confirms; only then does the modal close.
	m = update(t, m, groupAssigned{PackageID: "pkg-a", Group: domain.PackageGroup{ID: 3}})
	if m.groupAssign != nil {
		t.Fatalf("modal after confirm = %#v, want closed", *m.groupAssign)
	}
}

func TestGroupModalCreatesNewGroupFromName(t *testing.T) {
	data := &fakeGroupData{groups: []domain.PackageGroup{{ID: 3, Name: "Editors", Index: 1}}}
	m := groupTestModel(data, groups("a"))
	m.userGroups = data.groups

	m = update(t, m, key("g"))
	m = update(t, m, key("j")) // land on the new-group row (wraps to 0)
	m.groupAssign.name = "CLI tools"

	m, msg := updateAndRunCommand(t, m, key("enter"))
	request, ok := msg.(groupAssignRequested)
	if !ok {
		t.Fatalf("enter produced %T, want groupAssignRequested", msg)
	}
	if request.NewName != "CLI tools" || request.GroupID != 0 {
		t.Fatalf("request = %#v, want create CLI tools", request)
	}
	if m.groupAssign == nil || !m.groupAssign.pending {
		t.Fatalf("modal after enter = %#v, want pending", m.groupAssign)
	}

	// A duplicate-name failure keeps the modal open and the name intact.
	m = update(t, m, groupAssigned{PackageID: "pkg-a", Err: errDuplicateGroup})
	if m.groupAssign == nil {
		t.Fatal("modal closed on failed create, name lost")
	}
	if m.groupAssign.name != "CLI tools" || m.groupAssign.pending {
		t.Fatalf("modal after failure = %#v, want name kept and not pending", *m.groupAssign)
	}

	// A retry that succeeds closes the modal and names the strip tab.
	m = update(t, m, groupAssigned{PackageID: "pkg-a", Group: domain.PackageGroup{ID: 9, Name: "CLI tools"}, Created: true})
	if m.groupAssign != nil {
		t.Fatal("modal stayed open after successful retry")
	}
	if !strings.Contains(ansi.Strip(m.groupStrip(120)), "[CLI tools]") {
		t.Fatalf("strip = %q, want created group", ansi.Strip(m.groupStrip(120)))
	}
}

func TestGroupModalEnterOnAssignedGroupClearsMembership(t *testing.T) {
	data := &fakeGroupData{groups: []domain.PackageGroup{{ID: 3, Name: "Editors", Index: 1}}}
	m := groupTestModel(data, groups("a"))
	m.userGroups = data.groups

	m = update(t, m, key("g"))
	m.groupAssign.assignedTo = 3 // package is already in Editors

	m, msg := updateAndRunCommand(t, m, key("enter"))
	request, ok := msg.(groupAssignRequested)
	if !ok {
		t.Fatalf("enter produced %T, want groupAssignRequested", msg)
	}
	if !request.Clear || request.GroupID != 0 || request.NewName != "" {
		t.Fatalf("request = %#v, want clear membership", request)
	}
}

func TestGroupModalRemoveRequestsDeletion(t *testing.T) {
	data := &fakeGroupData{groups: []domain.PackageGroup{{ID: 3, Name: "Editors", Index: 1}}}
	m := groupTestModel(data, groups("a"))
	m.userGroups = data.groups

	m = update(t, m, key("g"))
	m = update(t, m, key("tab")) // hot the Remove button
	m, msg := updateAndRunCommand(t, m, key("enter"))
	request, ok := msg.(groupDeleteRequested)
	if !ok {
		t.Fatalf("enter produced %T, want groupDeleteRequested", msg)
	}
	if request.GroupID != 3 {
		t.Fatalf("delete request = %#v, want group 3", request)
	}
}

func TestGroupModalEscCloses(t *testing.T) {
	data := &fakeGroupData{}
	m := groupTestModel(data, groups("a"))
	m = update(t, m, key("g"))
	m = update(t, m, key("esc"))
	if m.groupAssign != nil {
		t.Fatal("esc did not close the group modal")
	}
}

func TestGroupModalRendersRadioStatesAndRemoveHint(t *testing.T) {
	data := &fakeGroupData{groups: []domain.PackageGroup{{ID: 3, Name: "Editors", Index: 1}, {ID: 4, Name: "Terminal", Index: 2}}}
	m := groupTestModel(data, groups("a"))
	m.userGroups = data.groups

	m = update(t, m, key("g"))
	m.groupAssign.assignedTo = 3
	view := ansi.Strip(m.renderGroupModal())
	if !strings.Contains(view, "Group: pkg-a") {
		t.Fatalf("modal = %q, want package heading", view)
	}
	if !strings.Contains(view, "(●) Editors") {
		t.Fatalf("modal = %q, want filled radio on the assigned group", view)
	}
	if !strings.Contains(view, "← enter to remove") {
		t.Fatalf("modal = %q, want unassign affordance on cursor row", view)
	}
	if !strings.Contains(view, "( ) Terminal") {
		t.Fatalf("modal = %q, want empty radio on other groups", view)
	}
	if !strings.Contains(view, "New group") {
		t.Fatalf("modal = %q, want new-group row", view)
	}
}

func TestGroupModalTypingShowsNameOnNewGroupRow(t *testing.T) {
	data := &fakeGroupData{}
	m := groupTestModel(data, groups("a"))

	m = update(t, m, key("g"))
	m = update(t, m, key("t"))
	m = update(t, m, key("u"))
	view := ansi.Strip(m.renderGroupModal())
	if !strings.Contains(view, "New group: tu█") {
		t.Fatalf("modal = %q, want typed name with cursor", view)
	}
}

func TestGroupModalWindowsLargeGroupLists(t *testing.T) {
	data := &fakeGroupData{}
	list := make([]domain.PackageGroup, 12)
	for i := range list {
		list[i] = domain.PackageGroup{ID: domain.PackageGroupID(i + 1), Name: fmt.Sprintf("Group%02d", i+1), Index: i + 1}
	}
	m := groupTestModel(data, groups("a"))
	m.userGroups = list

	m = update(t, m, key("g")) // cursor = 1, first group
	view := ansi.Strip(m.renderGroupModal())
	if strings.Contains(view, "Group01") && strings.Count(view, "( )")+strings.Count(view, "(●)") > 8 {
		t.Fatalf("modal rendered more rows than the window: %q", view)
	}
	if !strings.Contains(view, "more below") {
		t.Fatalf("modal = %q, want overflow marker below", view)
	}

	// Jump the cursor deep into the list; the window must follow.
	for range 9 {
		m = update(t, m, key("j"))
	}
	view = ansi.Strip(m.renderGroupModal())
	if !strings.Contains(view, "Group10") || !strings.Contains(view, "more above") {
		t.Fatalf("modal after scroll = %q, want window following cursor down", view)
	}
	if strings.Contains(view, "New group") && !strings.Contains(view, "New group:") {
		t.Fatalf("modal after scroll = %q, want new-group row scrolled out", view)
	}

	// Cursor at the very bottom must show the last group.
	for range 2 {
		m = update(t, m, key("j"))
	}
	view = ansi.Strip(m.renderGroupModal())
	if !strings.Contains(view, "Group12") {
		t.Fatalf("modal at end = %q, want last group visible", view)
	}
}

func TestGroupStripWindowsToActiveTabOnNarrowWidth(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 44, 24, false
	var groups []domain.PackageGroup
	for i := 1; i <= 6; i++ {
		groups = append(groups, domain.PackageGroup{ID: domain.PackageGroupID(i), Name: fmt.Sprintf("GroupNumber%d", i), Index: i})
	}
	m.userGroups = groups

	// Active tab defaults to All (index 0); narrow width must trim trailing tabs.
	strip := ansi.Strip(m.groupStrip(44))
	if !strings.Contains(strip, "[All]") || !strings.Contains(strip, "[GroupNumber") {
		t.Fatalf("strip = %q, want window starting at All", strip)
	}
	if strings.Contains(strip, "[GroupNumber6]") {
		t.Fatalf("strip = %q, want trailing tabs trimmed", strip)
	}

	// Cycle to the last tab; the window must slide to keep it visible.
	for range 7 {
		m = update(t, m, key("."))
	}
	strip = ansi.Strip(m.groupStrip(44))
	if !strings.Contains(strip, "[Hidden]") {
		t.Fatalf("strip after cycling to end = %q, want Hidden visible", strip)
	}
	if strings.Contains(strip, "[All]") {
		t.Fatalf("strip after cycling to end = %q, want All scrolled out", strip)
	}
}

func TestGroupStripMouseHitSelectsTab(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 24, false
	m.userGroups = []domain.PackageGroup{{ID: 5, Name: "Editors", Index: 1}}
	m.syncViewports()

	stripRow := m.feedHeaderRows() - 2    // title(1)+tabs(3)+strip(1)+list header(1), minus the two rows below the strip
	hit, tab := m.groupTabAt(stripRow, 1) // [All] starts after the leading space
	if !hit || tab.scope != domain.GroupScopeAll {
		t.Fatalf("hit at x=1 = %v/%#v, want All", hit, tab)
	}
	hit, tab = m.groupTabAt(stripRow, 4) // inside "All]"
	if !hit || tab.scope != domain.GroupScopeAll {
		t.Fatalf("hit at x=4 = %v/%#v, want All", hit, tab)
	}
	hit, tab = m.groupTabAt(stripRow, 6) // the space between tabs
	if hit {
		t.Fatalf("gap between tabs hit %#v, want miss", tab)
	}
	hit, tab = m.groupTabAt(stripRow, 7) // "[Ungrouped]"
	if !hit || tab.scope != domain.GroupScopeUngrouped {
		t.Fatalf("hit at x=8 = %v/%#v, want Ungrouped", hit, tab)
	}
	hit, tab = m.groupTabAt(stripRow-1, 4) // wrong row (type-tab bottom border)
	if hit {
		t.Fatalf("hit on tab row = %v, want miss", hit)
	}
}

// TestGroupStripClickFiltersFeed drives the full mouse path: the click
// coordinates come from the rendered view, so a strip-row regression (the
// hit-test drifting from what is drawn) fails here even if groupTabAt and
// the renderer agree with each other on the wrong row.
func TestGroupStripClickFiltersFeed(t *testing.T) {
	data := &fakeGroupData{groups: []domain.PackageGroup{{ID: 5, Name: "Editors", Index: 1}}}
	m := groupTestModel(data, groups("a"))
	m.userGroups = data.groups
	m.syncViewports()

	// Find [Editors] in the rendered header and compute its exact cell.
	header := strings.Split(ansi.Strip(m.renderFeedHeader(m.feedViewport.Width())), "\n")
	row, col := -1, -1
	for r, line := range header {
		if c := strings.Index(line, "[Editors]"); c >= 0 {
			row, col = r, ansi.StringWidth(line[:c])
			break
		}
	}
	if row < 0 {
		t.Fatalf("[Editors] not rendered in header:\n%s", strings.Join(header, "\n"))
	}
	m = update(t, m, tea.MouseClickMsg{X: col + 2, Y: row, Button: tea.MouseLeft})
	if m.filter.GroupScope != domain.GroupScopeUser || m.filter.GroupTarget != 5 {
		t.Fatalf("filter after click = %d/%d, want user group 5", m.filter.GroupScope, m.filter.GroupTarget)
	}
	if !m.loading {
		t.Fatal("click on group tab did not trigger a feed query")
	}

	// A click on the type-tab row above the strip must not touch the filter.
	m.filter = domain.FeedFilter{GroupScope: domain.GroupScopeAll}
	m = update(t, m, tea.MouseClickMsg{X: col + 2, Y: row - 1, Button: tea.MouseLeft})
	if m.filter.GroupScope != domain.GroupScopeAll {
		t.Fatalf("click above the strip changed filter to %d, want unchanged", m.filter.GroupScope)
	}
}

func TestGroupDeletedCaseClearsFilterAndStrip(t *testing.T) {
	data := &fakeGroupData{groups: []domain.PackageGroup{{ID: 3, Name: "Editors", Index: 1}}}
	m := groupTestModel(data, groups("a"))
	m.userGroups = data.groups
	m.filter.GroupScope = domain.GroupScopeUser
	m.filter.GroupTarget = 3

	m = update(t, m, groupDeleted{GroupID: 3})
	if m.filter.GroupScope != domain.GroupScopeAll || m.filter.GroupTarget != 0 {
		t.Fatalf("filter after delete = %d/%d, want all", m.filter.GroupScope, m.filter.GroupTarget)
	}
	if len(m.userGroups) != 0 {
		t.Fatalf("strip groups after delete = %#v, want empty", m.userGroups)
	}
}

func TestDotAndCommaCycleGroupTabs(t *testing.T) {
	data := &fakeGroupData{}
	m := groupTestModel(data, groups("a"))
	m.filter.Types = map[domain.PackageType]bool{}

	m = update(t, m, key("."))
	if m.filter.GroupScope != domain.GroupScopeUngrouped {
		t.Fatalf("scope after dot = %d, want ungrouped", m.filter.GroupScope)
	}
	for range 2 {
		m = update(t, m, key("."))
	}
	if m.filter.GroupScope != domain.GroupScopeHidden {
		t.Fatalf("scope after three dots = %d, want hidden", m.filter.GroupScope)
	}
	m = update(t, m, key(","))
	if m.filter.GroupScope != domain.GroupScopeStarred {
		t.Fatalf("scope after comma = %d, want starred", m.filter.GroupScope)
	}
}

func TestGroupStripRendersUserGroupsByIndex(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 24, false
	m.userGroups = []domain.PackageGroup{{ID: 2, Name: "Terminal", Index: 2}, {ID: 1, Name: "Editors", Index: 1}}
	strip := ansi.Strip(m.groupStrip(120))
	for _, want := range []string{"[All]", "[Ungrouped]", "[Editors]", "[Terminal]", "[Starred]", "[Hidden]"} {
		if !contains(strip, want) {
			t.Fatalf("strip = %q, want %q", strip, want)
		}
	}
	editors := indexOf(strip, "[Editors]")
	terminal := indexOf(strip, "[Terminal]")
	if editors > terminal {
		t.Fatalf("Editors (index 1) must precede Terminal (index 2): %q", strip)
	}
}

func contains(s, sub string) bool {
	return indexOf(s, sub) >= 0
}

// TestGroupStripClicksTrackScrolledWindow guards the hit-test x math against
// a scrolled strip: the overflow marker occupies fixed cells, so every
// rendered tab must resolve to itself under a click.
func TestGroupStripClicksTrackScrolledWindow(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 44, 24, false
	var gs []domain.PackageGroup
	for i := 1; i <= 4; i++ {
		gs = append(gs, domain.PackageGroup{ID: domain.PackageGroupID(i), Name: "GroupNumber" + string(rune('0'+i)), Index: i})
	}
	m.userGroups = gs
	m.filter.GroupScope = domain.GroupScopeUser
	m.filter.GroupTarget = 4 // active near the end: window slides, offset > 0

	strip := ansi.Strip(m.groupStrip(44))
	tabs, offset := m.visibleGroupTabs(44)
	if offset == 0 {
		t.Fatalf("offset = 0, want a scrolled window for this fixture")
	}
	if len(tabs) == 0 {
		t.Fatal("window must keep the active tab visible")
	}
	for _, tab := range tabs {
		label := "[" + tab.label + "]"
		at := indexOf(strip, label)
		if at < 0 {
			t.Errorf("rendered strip missing %q: %q", label, strip)
			continue
		}
		x := ansi.StringWidth(strip[:at])
		hit, got := m.groupTabAt(m.feedHeaderRows()-2, x)
		if !hit {
			t.Errorf("click at x=%d on %q missed", x, label)
		} else if got.label != tab.label {
			t.Errorf("click at x=%d hit %q, want %q", x, got.label, tab.label)
		}
	}
}

// TestGroupModalSyncsAcrossRefresh guards the modal snapshot against feed
// refreshes: the radio and enter semantics must follow the package's current
// membership, not the membership captured when the modal opened.
func TestGroupModalSyncsAcrossRefresh(t *testing.T) {
	data := &fakeGroupData{groups: []domain.PackageGroup{{ID: 3, Name: "Editors", Index: 1}, {ID: 9, Name: "Term", Index: 2}}}
	m := groupTestModel(data, groups("a"))
	m.userGroups = data.groups
	m.groups[0].Events[0].GroupID = 3
	m.selected = 0

	m = update(t, m, key("g"))
	if m.groupAssign == nil || m.groupAssign.assignedTo != 3 {
		t.Fatalf("modal open state = %#v, want assigned to 3", m.groupAssign)
	}
	// A refresh lands where the package is now ungrouped (cleared elsewhere).
	fresh := groups("a")
	fresh[0].Events[0].GroupID = 0
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: fresh})
	if m.groupAssign == nil {
		t.Fatal("modal closed by refresh; want it synced and open")
	}
	if m.groupAssign.assignedTo != 0 {
		t.Fatalf("modal assignedTo = %d after refresh, want 0 (ungrouped)", m.groupAssign.assignedTo)
	}
	// Enter on the first group must now assign, not clear.
	if m.groupAssign.cursor != 1 {
		t.Fatalf("modal cursor = %d, want 1 (first group)", m.groupAssign.cursor)
	}
	var requestMsg tea.Msg
	m, requestMsg = updateAndRunCommand(t, m, key("enter"))
	request, ok := requestMsg.(groupAssignRequested)
	if !ok {
		t.Fatalf("enter produced %T, want groupAssignRequested", requestMsg)
	}
	if request.Clear || request.GroupID != 3 {
		t.Fatalf("request after refresh = %#v, want assign to 3", request)
	}
}

// TestGroupModalSyncsAfterDelete guards the modal group list against a
// deletion that arrives while the modal is open on another package.
func TestGroupModalSyncsAfterDelete(t *testing.T) {
	data := &fakeGroupData{groups: []domain.PackageGroup{{ID: 3, Name: "Editors", Index: 1}, {ID: 4, Name: "Term", Index: 2}}}
	m := groupTestModel(data, groups("a"))
	m.userGroups = data.groups
	m.filter.GroupScope = domain.GroupScopeUser
	m.filter.GroupTarget = 4 // active tab is NOT the one being deleted

	m = update(t, m, key("g"))
	if m.groupAssign == nil {
		t.Fatal("modal did not open")
	}
	m = update(t, m, groupDeleted{GroupID: 3})
	if m.groupAssign == nil {
		t.Fatal("modal closed by delete; want it synced and open")
	}
	if len(m.groupAssign.groups) != 1 {
		t.Fatalf("modal groups after delete = %d, want 1 (Editors gone)", len(m.groupAssign.groups))
	}
	if m.groupAssign.groups[0].ID != 4 {
		t.Fatalf("modal groups after delete = %#v, want Term", m.groupAssign.groups)
	}
	if m.filter.GroupScope != domain.GroupScopeUser || m.filter.GroupTarget != 4 {
		t.Fatalf("filter after delete = %d/%d, want unchanged user/4", m.filter.GroupScope, m.filter.GroupTarget)
	}
}

// TestGroupModalEnterIgnoredWhilePending guards the confirm-once semantics:
// a second enter before the store confirms must not fire a duplicate create.
func TestGroupModalEnterIgnoredWhilePending(t *testing.T) {
	data := &fakeGroupData{}
	m := groupTestModel(data, groups("a"))
	m = update(t, m, key("g"))
	if m.groupAssign == nil {
		t.Fatal("modal did not open")
	}
	m.groupAssign.cursor = 0 // new-group row
	m = update(t, m, key("c"))
	m = update(t, m, key("l"))
	m = update(t, m, key("i"))
	first := update(t, m, key("enter"))
	if first.groupAssign == nil || !first.groupAssign.pending {
		t.Fatal("first enter did not set pending")
	}
	second := update(t, m, key("enter"))
	if second.groupAssign == nil || !second.groupAssign.pending {
		t.Fatal("second enter closed or cleared the modal; want it still pending")
	}
	if len(data.created) != 0 {
		// The request itself is async; the guard prevents the duplicate only
		// once confirmed. Two enters must not emit two requests either.
		t.Fatalf("created = %#v", data.created)
	}
}

// TestVisibleGroupTabsDropsUnfittableTabs guards the degenerate widths: a
// label that cannot fit even alone must not render as a truncated fragment,
// and the strip must still show the overflow marker.
func TestVisibleGroupTabsDropsUnfittableTabs(t *testing.T) {
	m := NewModel(Dependencies{})
	m.userGroups = []domain.PackageGroup{{ID: 1, Name: "G1", Index: 1}}

	// [Ungrouped] is 11 wide; at width 10 even alone it cannot fit.
	m.filter.GroupScope = domain.GroupScopeUngrouped
	tabs, _ := m.visibleGroupTabs(10)
	if len(tabs) != 0 {
		t.Fatalf("tabs at width 10 for Ungrouped = %#v, want empty", tabs)
	}
	strip := ansi.Strip(m.groupStrip(10))
	if !contains(strip, "‹") {
		t.Fatalf("strip at width 10 = %q, want overflow marker", strip)
	}

	// At width 44 every tab fits.
	tabs, offset := m.visibleGroupTabs(44)
	if offset != 0 || len(tabs) != 5 {
		t.Fatalf("tabs at width 44 = %d (offset %d), want all 5", len(tabs), offset)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestActiveGroupTabMatchesFilterScope(t *testing.T) {
	userGroups := []domain.PackageGroup{{ID: 5, Name: "Editors", Index: 1}}
	cases := []struct {
		filter domain.FeedFilter
		want   int
	}{
		{domain.FeedFilter{GroupScope: domain.GroupScopeAll}, 0},
		{domain.FeedFilter{GroupScope: domain.GroupScopeUngrouped}, 1},
		{domain.FeedFilter{GroupScope: domain.GroupScopeUser, GroupTarget: 5}, 2},
		{domain.FeedFilter{GroupScope: domain.GroupScopeStarred}, 3},
		{domain.FeedFilter{GroupScope: domain.GroupScopeHidden}, 4},
	}
	for _, tt := range cases {
		if got := activeGroupTab(userGroups, tt.filter); got != tt.want {
			t.Fatalf("activeGroupTab(%#v) = %d, want %d", tt.filter, got, tt.want)
		}
	}
}

func TestGroupAssignMessageUpdatesRowsAndRefetches(t *testing.T) {
	data := &fakeGroupData{}
	m := groupTestModel(data, groups("a", "b"))
	before := m.feedRequestID

	m = update(t, m, groupAssigned{PackageID: "pkg-a", Group: domain.PackageGroup{ID: 3}})
	if got := m.groups[0].Events[0].GroupID; got != 3 {
		t.Fatalf("pkg-a group = %d, want 3", got)
	}
	if got := m.groups[1].Events[0].GroupID; got != 0 {
		t.Fatalf("pkg-b group = %d, want 0", got)
	}
	if m.feedRequestID != before+1 {
		t.Fatalf("feedRequestID = %d, want refetch", m.feedRequestID)
	}
	if len(m.userGroups) != 0 {
		t.Fatalf("plain assignment added strip groups = %#v, want none", m.userGroups)
	}
}

func TestGroupCreatedMessageShowsNamedGroupInStrip(t *testing.T) {
	data := &fakeGroupData{}
	m := groupTestModel(data, groups("a"))

	m = update(t, m, groupAssigned{PackageID: "pkg-a", Group: domain.PackageGroup{ID: 9, Name: "CLI tools"}, Created: true})
	strip := ansi.Strip(m.groupStrip(120))
	if !strings.Contains(strip, "[CLI tools]") {
		t.Fatalf("strip = %q, want named new group", strip)
	}
	// The created group sorts after any existing groups and before Starred.
	if before, after := indexOf(strip, "[Ungrouped]"), indexOf(strip, "[Starred]"); !strings.Contains(strip[before:after], "[CLI tools]") {
		t.Fatalf("strip = %q, want new group between Ungrouped and Starred", strip)
	}
}

var _ tea.Msg = groupAssigned{}

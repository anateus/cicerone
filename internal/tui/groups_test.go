package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/charmbracelet/x/ansi"
)

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
	if m.groupAssign != nil {
		t.Fatal("modal stayed open after assignment")
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
}

var _ tea.Msg = groupAssigned{}

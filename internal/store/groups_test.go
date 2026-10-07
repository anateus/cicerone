package store

import (
	"context"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/domain"
)

func TestCreatePackageGroupAppendsInOrder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	first, err := s.CreatePackageGroup(ctx, "Editors")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreatePackageGroup(ctx, "Terminal")
	if err != nil {
		t.Fatal(err)
	}
	if first.Index != 1 || second.Index != 2 {
		t.Fatalf("positions = %d, %d; want 1, 2", first.Index, second.Index)
	}
	groups, err := s.PackageGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	user := userGroupNames(groups)
	if len(user) != 2 || user[0] != "Editors" || user[1] != "Terminal" {
		t.Fatalf("groups = %#v, want Editors then Terminal", groups)
	}
}

func TestCreatePackageGroupRejectsBlankAndDuplicateNames(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.CreatePackageGroup(ctx, "   "); err == nil {
		t.Fatal("blank name accepted")
	}
	if _, err := s.CreatePackageGroup(ctx, "Editors"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePackageGroup(ctx, "Editors"); err == nil {
		t.Fatal("duplicate name accepted")
	}
}

func TestSetPackageGroupFiltersFeedScopes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	group, err := s.CreatePackageGroup(ctx, "Editors")
	if err != nil {
		t.Fatal(err)
	}
	events := []domain.UpdateEvent{
		testEvent("one", "grouped", domain.EventVersion, time.Now().UTC()),
		testEvent("two", "loose", domain.EventVersion, time.Now().UTC()),
	}
	if err := s.UpsertEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPackageGroup(ctx, "grouped", group.ID); err != nil {
		t.Fatal(err)
	}

	filters := []struct {
		name   string
		filter domain.FeedFilter
		want   int
	}{
		{"all", domain.FeedFilter{GroupScope: domain.GroupScopeAll}, 2},
		{"ungrouped", domain.FeedFilter{GroupScope: domain.GroupScopeUngrouped}, 1},
		{"user", domain.FeedFilter{GroupScope: domain.GroupScopeUser, GroupTarget: group.ID}, 1},
	}
	for _, tt := range filters {
		groups, err := s.QueryFeed(ctx, tt.filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != tt.want {
			t.Fatalf("%s scope returned %d groups, want %d", tt.name, len(groups), tt.want)
		}
	}
}

func TestSetPackagesGroupMovesAndClearsSeveralPackages(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	group, err := s.CreatePackageGroup(ctx, "Editors")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.UpsertEvents(ctx, []domain.UpdateEvent{
		testEvent("one", "a", domain.EventVersion, now),
		testEvent("two", "b", domain.EventVersion, now),
		testEvent("three", "c", domain.EventVersion, now),
	}); err != nil {
		t.Fatal(err)
	}
	count := func(filter domain.FeedFilter) int {
		t.Helper()
		groups, err := s.QueryFeed(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		return len(groups)
	}
	inGroup := domain.FeedFilter{GroupScope: domain.GroupScopeUser, GroupTarget: group.ID}
	if err := s.SetPackagesGroup(ctx, []domain.PackageID{"a", "b"}, group.ID); err != nil {
		t.Fatal(err)
	}
	if got := count(inGroup); got != 2 {
		t.Fatalf("group holds %d packages, want 2", got)
	}
	if err := s.SetPackagesGroup(ctx, []domain.PackageID{"a", "b"}, 0); err != nil {
		t.Fatal(err)
	}
	if got, ungrouped := count(inGroup), count(domain.FeedFilter{GroupScope: domain.GroupScopeUngrouped}); got != 0 || ungrouped != 3 {
		t.Fatalf("after clear: group %d, ungrouped %d; want 0, 3", got, ungrouped)
	}
}

func TestHiddenGroupExcludedFromAllAndShownInItsOwnTab(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	events := []domain.UpdateEvent{
		testEvent("one", "gone", domain.EventVersion, time.Now().UTC()),
		testEvent("two", "kept", domain.EventVersion, time.Now().UTC()),
	}
	if err := s.UpsertEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	hiddenGroup, err := s.BuiltinGroup(ctx, domain.GroupKindHidden)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPackageGroup(ctx, "gone", hiddenGroup.ID); err != nil {
		t.Fatal(err)
	}

	all, err := s.QueryFeed(ctx, domain.FeedFilter{GroupScope: domain.GroupScopeAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Events[0].PackageID != "kept" {
		t.Fatalf("all scope = %#v, want only kept", all)
	}
	ungrouped, err := s.QueryFeed(ctx, domain.FeedFilter{GroupScope: domain.GroupScopeUngrouped})
	if err != nil {
		t.Fatal(err)
	}
	if len(ungrouped) != 1 || ungrouped[0].Events[0].PackageID != "kept" {
		t.Fatalf("ungrouped scope = %#v, want only kept", ungrouped)
	}

	hidden, err := s.QueryFeed(ctx, domain.FeedFilter{GroupScope: domain.GroupScopeUser, GroupTarget: hiddenGroup.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 1 || hidden[0].Events[0].PackageID != "gone" || hidden[0].Events[0].GroupKind != domain.GroupKindHidden {
		t.Fatalf("hidden group = %#v, want gone carrying the hidden kind", hidden)
	}
}

func TestStarredGroupStaysInAll(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.UpsertEvents(ctx, []domain.UpdateEvent{testEvent("one", "fav", domain.EventVersion, time.Now().UTC())}); err != nil {
		t.Fatal(err)
	}
	starred, err := s.BuiltinGroup(ctx, domain.GroupKindStarred)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPackageGroup(ctx, "fav", starred.ID); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []domain.FeedFilter{{GroupScope: domain.GroupScopeAll}, {GroupScope: domain.GroupScopeUser, GroupTarget: starred.ID}} {
		groups, err := s.QueryFeed(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].Events[0].GroupKind != domain.GroupKindStarred {
			t.Fatalf("scope %d = %#v, want fav marked starred", filter.GroupScope, groups)
		}
	}
}

func TestPackageGroupsIncludeBuiltinsWithMemberCounts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.UpsertEvents(ctx, []domain.UpdateEvent{
		testEvent("one", "a", domain.EventVersion, time.Now().UTC()),
		testEvent("two", "b", domain.EventVersion, time.Now().UTC()),
	}); err != nil {
		t.Fatal(err)
	}
	editors, err := s.CreatePackageGroup(ctx, "Editors")
	if err != nil {
		t.Fatal(err)
	}
	starred, err := s.BuiltinGroup(ctx, domain.GroupKindStarred)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPackagesGroup(ctx, []domain.PackageID{"a", "b"}, starred.ID); err != nil {
		t.Fatal(err)
	}
	groups, err := s.PackageGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	kinds := map[string]domain.GroupKind{}
	for _, group := range groups {
		counts[group.Name], kinds[group.Name] = group.Members, group.Kind
	}
	if len(groups) != 3 || counts["Starred"] != 2 || counts["Hidden"] != 0 || counts["Editors"] != 0 {
		t.Fatalf("groups = %#v", groups)
	}
	if kinds["Starred"] != domain.GroupKindStarred || kinds["Hidden"] != domain.GroupKindHidden || kinds["Editors"] != domain.GroupKindUser || editors.Kind != domain.GroupKindUser {
		t.Fatalf("kinds = %v", kinds)
	}
}

func TestBuiltinGroupsCannotBeDeletedOrDuplicated(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, kind := range []domain.GroupKind{domain.GroupKindStarred, domain.GroupKindHidden} {
		group, err := s.BuiltinGroup(ctx, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.DeletePackageGroup(ctx, group.ID); err == nil {
			t.Fatalf("deleting %s succeeded", kind)
		}
		if _, err := s.CreatePackageGroup(ctx, group.Name); err == nil {
			t.Fatalf("creating a second %q succeeded", group.Name)
		}
	}
}

func TestDeletePackageGroupReturnsMembersToUngrouped(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	group, err := s.CreatePackageGroup(ctx, "Temp")
	if err != nil {
		t.Fatal(err)
	}
	event := testEvent("one", "member", domain.EventVersion, time.Now().UTC())
	if err := s.UpsertEvents(ctx, []domain.UpdateEvent{event}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPackageGroup(ctx, "member", group.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePackageGroup(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	ungrouped, err := s.QueryFeed(ctx, domain.FeedFilter{GroupScope: domain.GroupScopeUngrouped})
	if err != nil {
		t.Fatal(err)
	}
	if len(ungrouped) != 1 || ungrouped[0].Events[0].PackageID != "member" {
		t.Fatalf("ungrouped scope after delete = %#v, want member", ungrouped)
	}
	groups, err := s.PackageGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if user := userGroupNames(groups); len(user) != 0 {
		t.Fatalf("groups after delete = %#v, want only the built-ins", groups)
	}
}

func userGroupNames(groups []domain.PackageGroup) []string {
	var names []string
	for _, group := range groups {
		if group.Kind == domain.GroupKindUser {
			names = append(names, group.Name)
		}
	}
	return names
}

func TestGroupScopePreferencesRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	filter, err := s.Preferences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if filter.GroupScope != domain.GroupScopeAll {
		t.Fatalf("default group scope = %d, want all", filter.GroupScope)
	}
	filter.GroupScope = domain.GroupScopeUser
	filter.GroupTarget = 7
	if err := s.SetPreferences(ctx, filter); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Preferences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.GroupScope != domain.GroupScopeUser || loaded.GroupTarget != 7 {
		t.Fatalf("loaded group preference = %d/%d, want user/7", loaded.GroupScope, loaded.GroupTarget)
	}
}

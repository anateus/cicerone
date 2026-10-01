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
	if len(groups) != 2 || groups[0].Name != "Editors" || groups[1].Name != "Terminal" {
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

func TestHiddenStatusExcludedFromAllAndShownInHiddenScope(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	events := []domain.UpdateEvent{
		testEvent("one", "gone", domain.EventVersion, time.Now().UTC()),
		testEvent("two", "kept", domain.EventVersion, time.Now().UTC()),
	}
	if err := s.UpsertEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPackageStatus(ctx, "gone", domain.PackageStatusHidden); err != nil {
		t.Fatal(err)
	}

	all, err := s.QueryFeed(ctx, domain.FeedFilter{GroupScope: domain.GroupScopeAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Events[0].PackageID != "kept" {
		t.Fatalf("all scope = %#v, want only kept", all)
	}

	hidden, err := s.QueryFeed(ctx, domain.FeedFilter{GroupScope: domain.GroupScopeHidden})
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 1 || hidden[0].Events[0].PackageID != "gone" {
		t.Fatalf("hidden scope = %#v, want only gone", hidden)
	}
	if hidden[0].Events[0].Status != domain.PackageStatusHidden {
		t.Fatalf("hidden scope round-trip status = %q, want hidden", hidden[0].Events[0].Status)
	}
}

func TestStarredScopeFromLegacyStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	event := testEvent("one", "fav", domain.EventVersion, time.Now().UTC())
	if err := s.UpsertEvents(ctx, []domain.UpdateEvent{event}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPackageStatus(ctx, "fav", domain.PackageStatusStarred); err != nil {
		t.Fatal(err)
	}
	starred, err := s.QueryFeed(ctx, domain.FeedFilter{GroupScope: domain.GroupScopeStarred})
	if err != nil {
		t.Fatal(err)
	}
	if len(starred) != 1 || starred[0].Events[0].PackageID != "fav" {
		t.Fatalf("starred scope = %#v, want fav", starred)
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
	if len(groups) != 0 {
		t.Fatalf("groups after delete = %#v, want empty", groups)
	}
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

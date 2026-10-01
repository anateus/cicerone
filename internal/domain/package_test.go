package domain

import (
	"testing"
	"time"
)

func TestMatchesGroupScope(t *testing.T) {
	const group PackageGroupID = 7
	cases := []struct {
		name    string
		scope   GroupScope
		groupID PackageGroupID
		status  PackageStatus
		want    bool
	}{
		{"all default", GroupScopeAll, 0, PackageStatusDefault, true},
		{"all starred", GroupScopeAll, 0, PackageStatusStarred, true},
		{"all excludes hidden", GroupScopeAll, 0, PackageStatusHidden, false},
		{"all excludes hidden with group", GroupScopeAll, group, PackageStatusHidden, false},
		{"ungrouped takes empty", GroupScopeUngrouped, 0, PackageStatusDefault, true},
		{"ungrouped takes starred", GroupScopeUngrouped, 0, PackageStatusStarred, true},
		{"ungrouped rejects member", GroupScopeUngrouped, group, PackageStatusDefault, false},
		{"ungrouped rejects hidden", GroupScopeUngrouped, 0, PackageStatusHidden, false},
		{"starred takes starred", GroupScopeStarred, 0, PackageStatusStarred, true},
		{"starred rejects default", GroupScopeStarred, 0, PackageStatusDefault, false},
		{"starred rejects member", GroupScopeStarred, group, PackageStatusDefault, false},
		{"hidden takes hidden", GroupScopeHidden, 0, PackageStatusHidden, true},
		{"hidden rejects default", GroupScopeHidden, 0, PackageStatusDefault, false},
		{"user takes member", GroupScopeUser, group, PackageStatusDefault, true},
		{"user rejects other group", GroupScopeUser, 3, PackageStatusDefault, false},
		{"user rejects ungrouped", GroupScopeUser, 0, PackageStatusDefault, false},
		{"user rejects hidden member", GroupScopeUser, group, PackageStatusHidden, false},
	}
	for _, tt := range cases {
		if got := MatchesGroupScope(tt.scope, tt.groupID, tt.status, group); got != tt.want {
			t.Errorf("%s: MatchesGroupScope(%d, %d, %q) = %v, want %v", tt.name, tt.scope, tt.groupID, tt.status, got, tt.want)
		}
	}
}

func TestBuildFeedAppliesGroupScopes(t *testing.T) {
	now := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	events := []UpdateEvent{
		{ID: "a", PackageID: "plain", Kind: EventVersion, Time: now, Status: PackageStatusDefault},
		{ID: "b", PackageID: "member", Kind: EventVersion, Time: now, Status: PackageStatusDefault, GroupID: 7},
		{ID: "c", PackageID: "gone", Kind: EventVersion, Time: now, Status: PackageStatusHidden},
	}
	installed := map[PackageID]bool{}
	filter := func(scope GroupScope) FeedFilter {
		return FeedFilter{Now: now, GroupScope: scope, GroupTarget: 7}
	}

	if got := len(BuildFeed(events, installed, filter(GroupScopeAll))); got != 2 {
		t.Fatalf("all scope rows = %d, want 2 (hidden excluded)", got)
	}
	ungrouped := BuildFeed(events, installed, filter(GroupScopeUngrouped))
	if len(ungrouped) != 1 || ungrouped[0].Events[0].PackageID != "plain" {
		t.Fatalf("ungrouped scope = %#v, want only plain", ungrouped)
	}
	if got := len(BuildFeed(events, installed, filter(GroupScopeHidden))); got != 1 {
		t.Fatalf("hidden scope rows = %d, want 1", got)
	}
	user := BuildFeed(events, installed, filter(GroupScopeUser))
	if len(user) != 1 || user[0].Events[0].PackageID != "member" {
		t.Fatalf("user scope = %#v, want only member", user)
	}
	other := BuildFeed(events, installed, FeedFilter{Now: now, GroupScope: GroupScopeUser, GroupTarget: 3})
	if len(other) != 0 {
		t.Fatalf("other-group scope = %#v, want empty", other)
	}
}

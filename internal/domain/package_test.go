package domain

import (
	"testing"
	"time"
)

func TestMatchesGroupScope(t *testing.T) {
	const group, hidden PackageGroupID = 7, 9
	cases := []struct {
		name    string
		scope   GroupScope
		groupID PackageGroupID
		kind    GroupKind
		target  PackageGroupID
		want    bool
	}{
		{"all ungrouped", GroupScopeAll, 0, "", group, true},
		{"all user member", GroupScopeAll, group, GroupKindUser, group, true},
		{"all starred member", GroupScopeAll, group, GroupKindStarred, group, true},
		{"all excludes hidden", GroupScopeAll, hidden, GroupKindHidden, group, false},
		{"ungrouped takes empty", GroupScopeUngrouped, 0, "", group, true},
		{"ungrouped rejects member", GroupScopeUngrouped, group, GroupKindUser, group, false},
		{"ungrouped rejects hidden", GroupScopeUngrouped, hidden, GroupKindHidden, group, false},
		{"group takes member", GroupScopeUser, group, GroupKindUser, group, true},
		{"group rejects other group", GroupScopeUser, 3, GroupKindUser, group, false},
		{"group rejects ungrouped", GroupScopeUser, 0, "", group, false},
		{"hidden group shows its members", GroupScopeUser, hidden, GroupKindHidden, hidden, true},
		{"group with no target shows nothing", GroupScopeUser, 0, "", 0, false},
	}
	for _, tt := range cases {
		if got := MatchesGroupScope(tt.scope, tt.groupID, tt.kind, tt.target); got != tt.want {
			t.Errorf("%s: MatchesGroupScope(%d, %d, %q, %d) = %v, want %v", tt.name, tt.scope, tt.groupID, tt.kind, tt.target, got, tt.want)
		}
	}
}

func TestBuildFeedAppliesGroupScopes(t *testing.T) {
	now := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	events := []UpdateEvent{
		{ID: "a", PackageID: "plain", Kind: EventVersion, Time: now},
		{ID: "b", PackageID: "member", Kind: EventVersion, Time: now, GroupID: 7, GroupKind: GroupKindUser},
		{ID: "c", PackageID: "gone", Kind: EventVersion, Time: now, GroupID: 9, GroupKind: GroupKindHidden},
	}
	installed := map[PackageID]bool{}
	filter := func(scope GroupScope, target PackageGroupID) FeedFilter {
		return FeedFilter{Now: now, GroupScope: scope, GroupTarget: target}
	}

	if got := len(BuildFeed(events, installed, filter(GroupScopeAll, 0))); got != 2 {
		t.Fatalf("all scope rows = %d, want 2 (hidden excluded)", got)
	}
	ungrouped := BuildFeed(events, installed, filter(GroupScopeUngrouped, 0))
	if len(ungrouped) != 1 || ungrouped[0].Events[0].PackageID != "plain" {
		t.Fatalf("ungrouped scope = %#v, want only plain", ungrouped)
	}
	hidden := BuildFeed(events, installed, filter(GroupScopeUser, 9))
	if len(hidden) != 1 || hidden[0].Events[0].PackageID != "gone" {
		t.Fatalf("hidden group = %#v, want only gone", hidden)
	}
	user := BuildFeed(events, installed, filter(GroupScopeUser, 7))
	if len(user) != 1 || user[0].Events[0].PackageID != "member" {
		t.Fatalf("user scope = %#v, want only member", user)
	}
	if other := BuildFeed(events, installed, filter(GroupScopeUser, 3)); len(other) != 0 {
		t.Fatalf("other-group scope = %#v, want empty", other)
	}
}

func TestBuiltinGroupKinds(t *testing.T) {
	if GroupKindUser.Builtin() || !GroupKindStarred.Builtin() || !GroupKindHidden.Builtin() {
		t.Fatal("only Starred and Hidden are built in")
	}
}

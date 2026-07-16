package domain

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRestoreSelection(t *testing.T) {
	group := func(id EventID, children ...EventID) FeedGroup {
		events := make([]UpdateEvent, len(children))
		for i, child := range children {
			events[i] = UpdateEvent{ID: child}
		}
		return FeedGroup{ID: id, Events: events}
	}

	tests := []struct {
		name   string
		old    Anchor
		groups []FeedGroup
		want   Anchor
	}{
		{
			name:   "keeps group identity when a row is inserted above",
			old:    Anchor{GroupID: "selected", ViewportOffset: 4, FallbackIndex: 1},
			groups: []FeedGroup{group("new", "new"), group("first", "first"), group("selected", "selected")},
			want:   Anchor{GroupID: "selected", ViewportOffset: 4, FallbackIndex: 2},
		},
		{
			name:   "uses clamped prior index when selection is deleted",
			old:    Anchor{GroupID: "deleted", ViewportOffset: 4, FallbackIndex: 5},
			groups: []FeedGroup{group("first", "first"), group("last", "last")},
			want:   Anchor{GroupID: "last", FallbackIndex: 1},
		},
		{
			name: "restores expanded child even when its group identity changes",
			old:  Anchor{GroupID: "old-group", ChildEventID: "child", ViewportOffset: 3, FallbackIndex: 0},
			groups: []FeedGroup{
				group("other", "other"),
				group("new-group", "newest", "child"),
			},
			want: Anchor{GroupID: "new-group", ChildEventID: "child", ViewportOffset: 3, FallbackIndex: 1},
		},
		{
			name:   "returns no anchor for empty results",
			old:    Anchor{GroupID: "selected", ChildEventID: "child", ViewportOffset: 2, FallbackIndex: 4},
			groups: nil,
			want:   Anchor{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RestoreSelection(tt.old, tt.groups)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("RestoreSelection() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

package domain

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestBuildFeed(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	event := func(id, pkg, name string, kind EventKind, typ PackageType, age time.Duration) UpdateEvent {
		return UpdateEvent{ID: EventID(id), PackageID: PackageID(pkg), Name: name, Kind: kind, Type: typ, NewVersion: id, Time: now.Add(-age)}
	}

	tests := []struct {
		name      string
		events    []UpdateEvent
		installed map[PackageID]bool
		filter    FeedFilter
		want      [][]EventID
	}{
		{
			name: "excludes old uninstalled packages after thirty days",
			events: []UpdateEvent{
				event("recent", "foo", "Foo", EventVersion, PackageFormula, 29*24*time.Hour),
				event("old", "bar", "Bar", EventVersion, PackageFormula, 31*24*time.Hour),
			},
			filter: FeedFilter{Now: now, Horizon: 30 * 24 * time.Hour},
			want:   [][]EventID{{"recent"}},
		},
		{
			name: "retains newest old event per kind for installed packages",
			events: []UpdateEvent{
				event("old-version", "foo", "Foo", EventVersion, PackageFormula, 40*24*time.Hour),
				event("older-version", "foo", "Foo", EventVersion, PackageFormula, 50*24*time.Hour),
				event("old-revision", "foo", "Foo", EventRevision, PackageFormula, 45*24*time.Hour),
			},
			installed: map[PackageID]bool{"foo": true},
			filter:    FeedFilter{Now: now, Horizon: 30 * 24 * time.Hour},
			want:      [][]EventID{{"old-version"}, {"old-revision"}},
		},
		{
			name: "applies kind and type and query filters",
			events: []UpdateEvent{
				event("keep", "foo", "Foo Tool", EventRevision, PackageCask, time.Hour),
				event("wrong-kind", "foo", "Foo Tool", EventVersion, PackageCask, 2*time.Hour),
				event("wrong-type", "foo", "Foo Tool", EventRevision, PackageFormula, 3*time.Hour),
				event("wrong-query", "bar", "Bar Tool", EventRevision, PackageCask, 4*time.Hour),
			},
			filter: FeedFilter{Now: now, Horizon: 30 * 24 * time.Hour, Kinds: map[EventKind]bool{EventRevision: true}, Types: map[PackageType]bool{PackageCask: true}, Query: "foo"},
			want:   [][]EventID{{"keep"}},
		},
		{
			name: "sorts newest first with event ID tie breaker",
			events: []UpdateEvent{
				event("z", "z", "Zed", EventVersion, PackageFormula, 2*time.Hour),
				event("b", "b", "Bee", EventVersion, PackageFormula, time.Hour),
				event("a", "a", "Aye", EventVersion, PackageFormula, time.Hour),
			},
			filter: FeedFilter{Now: now, Horizon: 30 * 24 * time.Hour},
			want:   [][]EventID{{"a"}, {"b"}, {"z"}},
		},
		{
			name: "rolls up adjacent events for the same package",
			events: []UpdateEvent{
				event("foo-1.1", "foo", "Foo", EventVersion, PackageFormula, 3*time.Hour),
				event("bar-3.2", "bar", "Bar", EventVersion, PackageFormula, 4*time.Hour),
				event("foo-1.3", "foo", "Foo", EventVersion, PackageFormula, time.Hour),
				event("foo-1.2", "foo", "Foo", EventVersion, PackageFormula, 2*time.Hour),
			},
			filter: FeedFilter{Now: now, Horizon: 30 * 24 * time.Hour, RollUp: true},
			want:   [][]EventID{{"foo-1.3", "foo-1.2", "foo-1.1"}, {"bar-3.2"}},
		},
		{
			name: "does not roll up nonadjacent events",
			events: []UpdateEvent{
				event("foo-new", "foo", "Foo", EventVersion, PackageFormula, time.Hour),
				event("bar", "bar", "Bar", EventVersion, PackageFormula, 2*time.Hour),
				event("foo-old", "foo", "Foo", EventVersion, PackageFormula, 3*time.Hour),
			},
			filter: FeedFilter{Now: now, Horizon: 30 * 24 * time.Hour, RollUp: true},
			want:   [][]EventID{{"foo-new"}, {"bar"}, {"foo-old"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := BuildFeed(tt.events, tt.installed, tt.filter)
			got := make([][]EventID, len(groups))
			for i, group := range groups {
				for _, event := range group.Events {
					got[i] = append(got[i], event.ID)
				}
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("BuildFeed() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNewEventID(t *testing.T) {
	got := NewEventID("homebrew/core", "abc123", "foo", EventVersion)
	want := EventID("homebrew/core:abc123:foo:version")
	if got != want {
		t.Fatalf("NewEventID() = %q, want %q", got, want)
	}
}

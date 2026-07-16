package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/store"
)

type fakeData struct {
	groups []domain.FeedGroup
	prefs  domain.FeedFilter
}

func (f *fakeData) QueryFeed(context.Context, domain.FeedFilter) ([]domain.FeedGroup, error) {
	return f.groups, nil
}
func (f *fakeData) Preferences(context.Context) (domain.FeedFilter, error)  { return f.prefs, nil }
func (f *fakeData) SetPreferences(context.Context, domain.FeedFilter) error { return nil }
func (f *fakeData) LoadChangelog(context.Context, domain.PackageID, domain.EventID) ([]store.ChangelogSection, error) {
	return nil, nil
}

func event(id, pkg string) domain.UpdateEvent {
	return domain.UpdateEvent{ID: domain.EventID(id), PackageID: domain.PackageID(pkg), Name: pkg, Type: domain.PackageFormula, Kind: domain.EventVersion, OldVersion: "1", NewVersion: "2"}
}

func groups(ids ...string) []domain.FeedGroup {
	result := make([]domain.FeedGroup, len(ids))
	for i, id := range ids {
		result[i] = domain.FeedGroup{ID: domain.EventID(id), Events: []domain.UpdateEvent{event(id, "pkg-"+id)}}
	}
	return result
}

func key(s string) tea.KeyPressMsg {
	special := map[string]rune{"tab": tea.KeyTab, "enter": tea.KeyEnter, "esc": tea.KeyEscape}
	if code, ok := special[s]; ok {
		return tea.KeyPressMsg(tea.Key{Code: code})
	}
	return tea.KeyPressMsg(tea.Key{Code: []rune(s)[0], Text: s})
}

func update(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(Model)
}

func TestModelPreservesInteractionStateAcrossDatasetChange(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, WindowSize{Width: 120, Height: 12})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b", "c", "d")})
	m = update(t, m, key("j"))
	m = update(t, m, key("j"))
	m.viewportOffset = 1
	m = update(t, m, key("tab"))
	m = update(t, m, ToggleFilter{Kind: domain.EventRevision})
	m = update(t, m, SearchChanged{Text: "openssl"})
	m = update(t, m, ToggleRollUp{})
	m = update(t, m, ToggleExpanded{})

	selected := m.selectedEvent().ID
	m = update(t, m, DatasetChanged{})
	request := m.feedRequestID
	m = update(t, m, FeedLoaded{RequestID: request, Groups: groups("x", "c", "y")})

	if got := m.selectedEvent().ID; got != selected {
		t.Fatalf("selection = %q, want %q", got, selected)
	}
	if m.viewportOffset != 1 {
		t.Fatalf("viewport offset = %d", m.viewportOffset)
	}
	if m.focus != inspectorPane {
		t.Fatalf("focus = %v", m.focus)
	}
	if !m.expanded["c"] {
		t.Fatalf("expanded group not retained: %#v", m.expanded)
	}
	if !m.filter.Kinds[domain.EventRevision] || !m.filter.RollUp || m.filter.Query != "openssl" {
		t.Fatalf("filter state lost: %#v", m.filter)
	}
}

func TestNarrowInspectorIsSeparateDetailScreen(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, WindowSize{Width: 99, Height: 20})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a")})
	m = update(t, m, key("enter"))
	if !m.detailOpen {
		t.Fatal("enter did not open narrow detail screen")
	}
	m = update(t, m, key("esc"))
	if m.detailOpen {
		t.Fatal("escape did not return to feed")
	}
}

func TestStaleFeedAndChangelogResponsesAreIgnored(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID - 1, Groups: groups("stale")})
	if len(m.groups) != 0 {
		t.Fatal("accepted stale feed response")
	}
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("current")})
	m.changelogRequestID = 9
	m.changelog = []store.ChangelogSection{{Version: "current"}}
	m = update(t, m, ChangelogLoaded{RequestID: 8, EventID: "other", PackageID: "pkg-other", Sections: []store.ChangelogSection{{Version: "stale"}}})
	if got := m.changelog[0].Version; got != "current" {
		t.Fatalf("stale changelog replaced current: %q", got)
	}
}

func TestSelectionDebouncesChangelogFor250Milliseconds(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b")})
	m = update(t, m, key("j"))
	id := m.selectionID
	m = update(t, m, ChangelogDebounced{SelectionID: id - 1})
	if m.changelogRequestID != 0 {
		t.Fatal("stale debounce started request")
	}
	next, cmd := m.Update(ChangelogDebounced{SelectionID: id})
	m = next.(Model)
	if cmd == nil || m.changelogRequestID != 1 {
		t.Fatal("current debounce did not start changelog request")
	}
	if changelogDebounce != 250*time.Millisecond {
		t.Fatalf("debounce = %s", changelogDebounce)
	}
}

func TestPreferencesLoadAndPreferenceWritesAreCommands(t *testing.T) {
	f := &fakeData{prefs: domain.FeedFilter{Query: "git", RollUp: true}}
	m := NewModel(Dependencies{Data: f})
	for _, cmd := range m.Init()().(tea.BatchMsg) {
		if msg := cmd(); msg != nil {
			m = update(t, m, msg)
		}
	}
	if m.filter.Query != "git" || !m.filter.RollUp {
		t.Fatalf("preferences not loaded: %#v", m.filter)
	}
	_, cmd := m.Update(ToggleRollUp{})
	if cmd == nil {
		t.Fatal("preference mutation did not return async command")
	}
}

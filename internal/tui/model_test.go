package tui

import (
	"context"
	"errors"
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

func updateAndRunCommand(t *testing.T, m Model, msg tea.Msg) (Model, tea.Msg) {
	t.Helper()
	next, cmd := m.Update(msg)
	if cmd == nil {
		return next.(Model), nil
	}
	return next.(Model), cmd()
}

func TestGlobalQuitKeys(t *testing.T) {
	states := []struct {
		name  string
		model func() Model
	}{
		{name: "normal", model: func() Model { return NewModel(Dependencies{}) }},
		{name: "error", model: func() Model {
			m := NewModel(Dependencies{})
			m.err = errors.New("visible error")
			return m
		}},
		{name: "pending-action", model: func() Model {
			m := NewModel(Dependencies{})
			a := action()
			m.pendingAction = &a
			return m
		}},
		{name: "action-running", model: func() Model {
			m := NewModel(Dependencies{})
			m.actionRunning = true
			return m
		}},
		{name: "action-result", model: func() Model {
			m := NewModel(Dependencies{})
			a := action()
			m.actionResult = &a
			return m
		}},
		{name: "detail-open", model: func() Model {
			m := NewModel(Dependencies{})
			m.detailOpen = true
			return m
		}},
	}

	for _, state := range states {
		for _, quitKey := range []string{"q", "esc"} {
			t.Run(state.name+"/"+quitKey, func(t *testing.T) {
				_, msg := updateAndRunCommand(t, state.model(), key(quitKey))
				if _, ok := msg.(tea.QuitMsg); !ok {
					t.Fatalf("command result = %T, want tea.QuitMsg", msg)
				}
			})
		}
	}
}

func TestHorizontalNavigationAliases(t *testing.T) {
	for _, tt := range []struct {
		name       string
		width      int
		startFocus pane
		startOpen  bool
		keys       []string
		wantFocus  pane
		wantOpen   bool
	}{
		{name: "wide feed", width: 120, startFocus: inspectorPane, keys: []string{"h", "left"}, wantFocus: feedPane},
		{name: "wide inspector", width: 120, startFocus: feedPane, keys: []string{"l", "right"}, wantFocus: inspectorPane},
		{name: "narrow close", width: 99, startOpen: true, keys: []string{"h", "left"}, wantOpen: false},
		{name: "narrow open", width: 99, startOpen: false, keys: []string{"l", "right"}, wantOpen: true},
	} {
		for _, navigationKey := range tt.keys {
			t.Run(tt.name+"/"+navigationKey, func(t *testing.T) {
				m := NewModel(Dependencies{})
				m.width, m.focus, m.detailOpen = tt.width, tt.startFocus, tt.startOpen
				m.groups = groups("a")
				m = update(t, m, key(navigationKey))
				if m.focus != tt.wantFocus || m.detailOpen != tt.wantOpen {
					t.Fatalf("focus/detailOpen = %v/%t, want %v/%t", m.focus, m.detailOpen, tt.wantFocus, tt.wantOpen)
				}
			})
		}
	}
}

func TestModelPreservesInteractionStateAcrossDatasetChange(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, WindowSize{Width: 120, Height: 5})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b", "c", "d")})
	m = update(t, m, key("j"))
	m = update(t, m, key("j"))
	m.viewportOffset = 1
	m.feedViewport.SetYOffset(1)
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
	m = update(t, m, key("h"))
	if m.detailOpen {
		t.Fatal("h did not return to feed")
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

func TestNavigationKeepsSelectionVisibleInRealFeedViewport(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, WindowSize{Width: 72, Height: 8})
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups(ids...)})
	for range 8 {
		m = update(t, m, key("j"))
	}
	if m.feedViewport.YOffset() == 0 {
		t.Fatal("offscreen navigation did not scroll real feed viewport")
	}
	if m.feedViewport.YOffset() != m.viewportOffset {
		t.Fatalf("viewport model offset = %d, state offset = %d", m.feedViewport.YOffset(), m.viewportOffset)
	}
	visibleRows := m.height - statusHeight - 2
	if relative := m.selected - m.viewportOffset; relative < 0 || relative >= visibleRows {
		t.Fatalf("selection row %d is outside viewport offset %d height %d", m.selected, m.viewportOffset, visibleRows)
	}
}

func TestNavigationCountsExpandedChildrenBeforeSelection(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, WindowSize{Width: 72, Height: 6})
	children := []domain.UpdateEvent{
		event("roll", "rollup"), event("child-1", "rollup"), event("child-2", "rollup"),
		event("child-3", "rollup"), event("child-4", "rollup"),
	}
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: []domain.FeedGroup{
		{ID: "roll", Events: children},
		{ID: "next", Events: []domain.UpdateEvent{event("next", "next")}},
	}})
	m = update(t, m, ToggleExpanded{})
	m = update(t, m, key("j"))

	selectedLine := feedHeaderHeight + 1 + len(children) - 1
	top, height := m.feedViewport.YOffset(), m.feedViewport.Height()
	if selectedLine < top || selectedLine >= top+height {
		t.Fatalf("rendered selected line %d outside real viewport [%d,%d)", selectedLine, top, top+height)
	}
	if m.viewportOffset != top {
		t.Fatalf("stored offset %d differs from viewport offset %d", m.viewportOffset, top)
	}
}

func TestDatasetRefreshRestoresAnchorCapturedWhenRequestStarted(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, WindowSize{Width: 72, Height: 4})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b", "c")})
	m = update(t, m, key("j")) // b is the refresh anchor.
	m.viewportOffset = 1
	m.feedViewport.SetYOffset(1)
	m = update(t, m, DatasetChanged{})
	request := m.feedRequestID
	m = update(t, m, key("j")) // interim interaction moves to c.
	m = update(t, m, FeedLoaded{RequestID: request, Groups: groups("x", "b", "y")})
	if got := m.selectedEvent().ID; got != "b" {
		t.Fatalf("selection = %q, want request-start anchor b", got)
	}
	if m.viewportOffset != 1 || m.feedViewport.YOffset() != 1 {
		t.Fatalf("refresh did not restore viewport offset: state=%d viewport=%d", m.viewportOffset, m.feedViewport.YOffset())
	}
}

func TestNotifyRejectsStaleRequestID(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, Notify{RequestID: 7, Text: "current"})
	m = update(t, m, Notify{RequestID: 6, Text: "stale"})
	if m.notification != "current" {
		t.Fatalf("stale notification replaced current: %q", m.notification)
	}
}

func TestReadyCommandStartsOnlyAfterCachedFeedLoads(t *testing.T) {
	started := false
	m := NewModel(Dependencies{OnReady: func() tea.Msg { started = true; return nil }})
	if started {
		t.Fatal("ready callback ran before cached feed")
	}
	_, cmd := m.Update(FeedLoaded{RequestID: 1})
	if cmd == nil {
		t.Fatal("cached feed did not schedule ready callback")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, nested := range batch {
			_ = nested()
		}
	}
	if !started {
		t.Fatal("ready callback was not run")
	}
}

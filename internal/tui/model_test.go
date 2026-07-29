package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"cicerone/internal/domain"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/go-cmp/cmp"
)

type fakeData struct {
	groups []domain.FeedGroup
	prefs  domain.FeedFilter
}

type fakeSeenData struct {
	*fakeData
	marked []domain.EventID
}

func (f *fakeSeenData) MarkEventsSeen(_ context.Context, ids []domain.EventID) error {
	f.marked = append(f.marked, ids...)
	return nil
}

type fakeCachedInfo struct {
	values      map[domain.PackageID]homebrew.PackageInfo
	loads       []domain.PackageID
	cachedLoads []domain.PackageID
}

type blockingDetailSource struct {
	started  chan string
	canceled chan string
}

func (s *blockingDetailSource) wait(ctx context.Context, kind string) error {
	s.started <- kind
	<-ctx.Done()
	s.canceled <- kind
	return ctx.Err()
}

func (s *blockingDetailSource) LoadPackageInfo(ctx context.Context, _ domain.PackageID) (homebrew.PackageInfo, error) {
	return homebrew.PackageInfo{}, s.wait(ctx, "info")
}

func (s *blockingDetailSource) LoadREADME(ctx context.Context, _ domain.PackageID, _ domain.EventID) (store.PackageDocument, error) {
	return store.PackageDocument{}, s.wait(ctx, "readme")
}

func (s *blockingDetailSource) LoadChangelog(ctx context.Context, _ domain.PackageID, _ domain.EventID) ([]store.ChangelogSection, error) {
	return nil, s.wait(ctx, "changelog")
}

func (f *fakeCachedInfo) LoadPackageInfo(_ context.Context, id domain.PackageID) (homebrew.PackageInfo, error) {
	f.loads = append(f.loads, id)
	return f.values[id], nil
}
func (f *fakeCachedInfo) LoadCachedPackageInfo(_ context.Context, id domain.PackageID) (homebrew.PackageInfo, bool, error) {
	f.cachedLoads = append(f.cachedLoads, id)
	value, ok := f.values[id]
	return value, ok, nil
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

func TestSlashSearchModeCapturesTextInsteadOfGlobalKeys(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 72, 10, false
	m.groups = groups("a", "b")
	m.syncViewports()

	m = update(t, m, key("/"))
	for _, character := range []string{"a", "l", "p"} {
		m = update(t, m, key(character))
	}
	next, cmd := m.Update(key("q"))
	m = next.(Model)
	if m.filter.Query != "alpq" {
		t.Fatalf("search query = %q, want alpq", m.filter.Query)
	}
	if cmd == nil {
		t.Fatal("search input did not schedule a debounced query")
	}
	if _, ok := cmd().(tea.QuitMsg); ok {
		t.Fatal("q quit while search input was active")
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "SEARCH NAMES") {
		t.Fatal("active search mode and scope are not visible")
	}
}

func TestSearchTabCyclesBroaderCachedContentScopes(t *testing.T) {
	m := NewModel(Dependencies{})
	m.groups = groups("a", "b")
	m = update(t, m, key("/"))
	for _, want := range []domain.SearchScope{
		domain.SearchDescriptions,
		domain.SearchChangelogs,
		domain.SearchREADMEs,
		domain.SearchNames,
	} {
		m = update(t, m, key("tab"))
		if m.filter.Search != want {
			t.Fatalf("search scope after tab = %q, want %q", m.filter.Search, want)
		}
	}

	m = update(t, m, key("enter"))
	selected := m.selected
	m = update(t, m, key("j"))
	if m.selected == selected {
		t.Fatal("enter did not leave search input mode")
	}
}

func TestEscapeLeavesInspectorBeforeQuitting(t *testing.T) {
	for _, tt := range []struct {
		name       string
		width      int
		focus      pane
		detailOpen bool
	}{
		{name: "wide", width: 120, focus: inspectorPane},
		{name: "narrow", width: 99, detailOpen: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := NewModel(Dependencies{})
			m.width, m.focus, m.detailOpen = tt.width, tt.focus, tt.detailOpen
			next, msg := updateAndRunCommand(t, m, key("esc"))
			if msg != nil {
				t.Fatalf("escape command = %T, want nil", msg)
			}
			if next.focus != feedPane || next.detailOpen {
				t.Fatalf("focus/detailOpen = %v/%t, want feed/false", next.focus, next.detailOpen)
			}
		})
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
		{name: "wide inspector", width: 120, startFocus: feedPane, keys: []string{"l", "right"}, wantFocus: inspectorPane},
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

func TestPackageScopeDefaultsToFormulaeAndSupportsCasksAndAll(t *testing.T) {
	m := NewModel(Dependencies{})
	if !m.filter.Types[domain.PackageFormula] || m.filter.Types[domain.PackageCask] {
		t.Fatalf("default package scope = %#v, want formulae", m.filter.Types)
	}
	if controls := ansi.Strip(m.feedControls(100)); !strings.Contains(controls, "FORMULAE") {
		t.Fatalf("default controls do not select Formulae: %q", controls)
	}

	m = update(t, m, key("2"))
	if m.filter.Types[domain.PackageFormula] || !m.filter.Types[domain.PackageCask] {
		t.Fatalf("cask package scope = %#v", m.filter.Types)
	}
	m = update(t, m, key("3"))
	if !m.filter.Types[domain.PackageFormula] || !m.filter.Types[domain.PackageCask] {
		t.Fatalf("all package scope = %#v", m.filter.Types)
	}
	m = update(t, m, key("1"))
	if !m.filter.Types[domain.PackageFormula] || m.filter.Types[domain.PackageCask] {
		t.Fatalf("formula package scope = %#v", m.filter.Types)
	}
}

func TestInspectorSeparatesSummaryTabsAndDocumentSurface(t *testing.T) {
	m := NewModel(Dependencies{})
	m.height = 18
	m.groups = groups("a")
	m.packageInfo.Description = "Fast package search"
	m.readme = store.PackageDocument{ID: "readme", Extracted: []byte("# Usage")}
	m.document = store.DocumentREADME
	rendered := m.renderInspector(72)
	view := ansi.Strip(rendered)
	for _, want := range []string{"╭ PACKAGE · FORMULA", "├ DOCUMENTS", "│ README │", "│README", "Fast package search", "╰────"} {
		if !strings.Contains(view, want) {
			t.Fatalf("inspector missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(rendered, "48;2;") {
		t.Fatalf("inspector surfaces have no background styling:\n%q", rendered)
	}
}

func TestLoadingDocumentSurfaceExtendsThroughAvailableInspectorHeight(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height = 72, 18
	m.groups = groups("a")
	m.document = store.DocumentREADME
	lines := strings.Split(ansi.Strip(m.renderInspector(72)), "\n")
	framed := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "│") {
			framed++
		}
	}
	if framed < 4 {
		t.Fatalf("loading document canvas has only %d framed rows:\n%s", framed, strings.Join(lines, "\n"))
	}
}

func TestOverflowingFeedRendersViewportScrollbar(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 72, 8, false
	m.groups = groups("a", "b", "c", "d", "e", "f", "g", "h", "i")
	m.syncViewports()
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "PACKAGE") || !strings.Contains(view, "▐") || strings.Contains(view, "█") {
		t.Fatalf("feed does not look like a scrollable list:\n%s", view)
	}
}

func TestMouseSelectsFeedTabsRowsAndDocumentTabs(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 20, false
	m.groups = groups("a", "b", "c")
	m.syncViewports()

	m = update(t, m, tea.MouseClickMsg{X: 16, Y: 1, Button: tea.MouseLeft})
	if m.filter.Types[domain.PackageFormula] || !m.filter.Types[domain.PackageCask] {
		t.Fatalf("mouse-selected scope = %#v, want casks", m.filter.Types)
	}
	m = update(t, m, tea.MouseClickMsg{X: 5, Y: feedHeaderHeight + feedRowHeight(m.feedViewport.Width()), Button: tea.MouseLeft})
	if m.selected != 1 {
		t.Fatalf("mouse-selected feed index = %d, want 1", m.selected)
	}

	left, _ := m.paneWidths()
	lines := strings.Split(ansi.Strip(m.renderInspector(m.width-left-1)), "\n")
	for row, line := range lines {
		if column := strings.Index(line, "README"); column >= 0 {
			m.document = store.DocumentChangelog
			m = update(t, m, tea.MouseClickMsg{X: left + 1 + column, Y: row, Button: tea.MouseLeft})
			if m.document != store.DocumentREADME {
				t.Fatal("clicking README tab did not select it")
			}
			return
		}
	}
	t.Fatal("README tab was not rendered")
}

func TestDocumentTabClicksSurviveLateDocumentResults(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 20, false
	m.groups = groups("a")
	m.readme = store.PackageDocument{ID: "readme", Extracted: []byte("available")}
	m.syncViewports()

	clickDocumentTab := func(label string) {
		left, right := m.paneWidths()
		lines := strings.Split(ansi.Strip(m.renderInspector(right)), "\n")
		for row, line := range lines {
			if column := strings.Index(line, label); column >= 0 {
				m = update(t, m, tea.MouseClickMsg{
					X: left + 1 + column, Y: row, Button: tea.MouseLeft,
				})
				return
			}
		}
		t.Fatalf("%s tab was not rendered", label)
	}

	clickDocumentTab("CHANGELOG")
	m = update(t, m, READMELoaded{
		PackageID: "pkg-a",
		Document:  store.PackageDocument{ID: "readme", Extracted: []byte("refreshed")},
	})
	m = update(t, m, ChangelogLoaded{
		EventID: "a", PackageID: "pkg-a", Err: errors.New("not found"),
	})
	if m.document != store.DocumentChangelog {
		t.Fatalf("late results changed explicit tab to %q", m.document)
	}

	clickDocumentTab("README")
	clickDocumentTab("CHANGELOG")
	if m.document != store.DocumentChangelog {
		t.Fatalf("repeated tab clicks ended on %q", m.document)
	}
}

func TestMouseWheelScrollsFeedAndInspectorViewports(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 8, false
	m.groups = groups("a", "b", "c", "d", "e", "f", "g", "h", "i")
	m.readme = store.PackageDocument{ID: "readme", Extracted: []byte(strings.Repeat("line\n\n", 30))}
	m.document = store.DocumentREADME
	m.syncViewports()

	m = update(t, m, tea.MouseWheelMsg{X: 5, Y: 5, Button: tea.MouseWheelDown})
	if m.viewportOffset == 0 {
		t.Fatal("mouse wheel did not scroll feed viewport")
	}
	left, _ := m.paneWidths()
	m = update(t, m, tea.MouseWheelMsg{X: left + 4, Y: 5, Button: tea.MouseWheelDown})
	if m.inspectorViewport.YOffset() == 0 {
		t.Fatal("mouse wheel did not scroll inspector viewport")
	}
}

func TestNestedStylesPreserveSurfaceAndSelectionBackgrounds(t *testing.T) {
	m := NewModel(Dependencies{})
	inner := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#4FBF79")).Render("changed")
	surface := m.surfaceLine("▌ "+inner, 30, m.palette().raisedBG)
	selected := m.selectedLine(fit("› package "+inner, 30))
	for name, rendered := range map[string]string{"surface": surface, "selection": selected} {
		reset := strings.Index(rendered, "\x1b[m")
		if reset < 0 || !strings.HasPrefix(rendered[reset+3:], "\x1b[") {
			t.Fatalf("%s does not restore outer style after nested reset: %q", name, rendered)
		}
		if ansi.StringWidth(rendered) != 30 {
			t.Fatalf("%s width = %d, want 30", name, ansi.StringWidth(rendered))
		}
	}
}

func TestPaintBackgroundOwnsEveryCellAndRestoresAfterNestedStyles(t *testing.T) {
	background := lipgloss.Color("#242A34")
	inner := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#4FBF79")).Render("changed")
	rendered := paintBackground(" "+inner, 30, background)

	if !strings.HasPrefix(rendered, "\x1b[48;2;36;42;52m") {
		t.Fatalf("line does not start with the requested background: %q", rendered)
	}
	reset := strings.Index(rendered, "\x1b[m")
	if reset < 0 || !strings.HasPrefix(rendered[reset+3:], "\x1b[48;2;36;42;52m") {
		t.Fatalf("line does not restore its background after a nested reset: %q", rendered)
	}
	if ansi.StringWidth(rendered) != 30 {
		t.Fatalf("line width = %d, want 30", ansi.StringWidth(rendered))
	}
}

func TestViewDefinesTerminalBackground(t *testing.T) {
	m := NewModel(Dependencies{})
	if m.View().BackgroundColor == nil {
		t.Fatal("view leaves the terminal background undefined")
	}
}

func TestViewUsesAlternateScreenForFullHeightRendering(t *testing.T) {
	m := NewModel(Dependencies{})
	if !m.View().AltScreen {
		t.Fatal("full-height TUI is rendered inline, allowing repeated updates to scroll over pinned controls")
	}
}

func TestNavigationDefersSelectedPackageCacheReadsUntilItSettles(t *testing.T) {
	source := &fakeCachedInfo{values: map[domain.PackageID]homebrew.PackageInfo{
		"pkg-a": {Name: "pkg-a"},
		"pkg-b": {Name: "pkg-b"},
	}}
	m := NewModel(Dependencies{PackageInfo: source})
	m.width, m.height, m.loading = 72, 8, false
	m.groups = groups("a", "b")
	m.seenBoundaryIndex = len(m.groups)
	m.syncViewports()
	for _, group := range m.groups {
		m.descriptionRequests[group.Events[0].PackageID] = true
	}

	_, cmd := m.Update(key("j"))
	if cmd == nil {
		t.Fatal("navigation returned no settle command")
	}
	message := cmd()
	if _, ok := message.(ChangelogDebounced); !ok {
		t.Fatalf("navigation command result = %T, want ChangelogDebounced", message)
	}
	if len(source.cachedLoads) != 0 {
		t.Fatalf("navigation synchronously queued selected-package cache reads: %v", source.cachedLoads)
	}
}

func TestSelectionUsesMutedHighlightWithoutDecorativePaneStripes(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 10, false
	m.groups = groups("a")
	rendered := m.render()

	if strings.Contains(rendered, "48;2;199;155;255") {
		t.Fatal("selection still uses the overly bright lavender background")
	}
	if !strings.Contains(rendered, "48;2;118;92;145") {
		t.Fatal("selection does not use the muted lavender background")
	}
	if strings.Contains(ansi.Strip(rendered), "╲▒▓") || strings.Contains(ansi.Strip(rendered), "░▒▓") {
		t.Fatal("pane boundary still contains decorative shadow stripes")
	}
}

func TestFilterControlsAreClosedOnAContinuousShelf(t *testing.T) {
	m := NewModel(Dependencies{})
	rows := strings.Split(ansi.Strip(m.feedControls(72)), "\n")
	if len(rows) != 3 {
		t.Fatalf("control rows = %d, want 3", len(rows))
	}
	if !strings.Contains(rows[0], "╮╭") || !strings.Contains(rows[1], "││") {
		t.Fatalf("tabs are not adjacent like tui-studio's tab strip: %q / %q", rows[0], rows[1])
	}
	if !strings.HasPrefix(rows[2], "╯          ╰┴───────┴┴─────┴") || strings.Trim(rows[2], "─╯╰┴ ") != "" {
		t.Fatalf("tabs do not share a continuous baseline: %q", rows[2])
	}
}

func TestFeedHeaderRemainsPinnedWhileRowsScroll(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 72, 12, false
	m.groups = groups("a", "b", "c", "d", "e", "f")
	m.syncViewports()
	m.feedViewport.SetYOffset(9)
	m.viewportOffset = 9

	view := ansi.Strip(m.render())
	for _, want := range []string{"FORMULAE", "CASKS", "PACKAGE", "pkg-d"} {
		if !strings.Contains(view, want) {
			t.Fatalf("scrolled feed lost pinned header or visible rows (%q):\n%s", want, view)
		}
	}
}

func TestFeedUsesAlternatingThreeRowBands(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 72, 16, false
	m.groups = groups("a", "b")
	rendered := m.renderFeedRows(72)
	if !strings.Contains(rendered, "48;2;41;47;57") {
		t.Fatalf("feed has no alternate row surface: %q", rendered)
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

func TestSyncProgressRendersAuthoritativeCounts(t *testing.T) {
	m := NewModel(Dependencies{})
	next, cmd := m.Update(SyncProgress{Source: "homebrew-core", Commits: 100, Events: 8, Batches: 1})
	m = next.(Model)
	if cmd != nil {
		t.Fatal("determinate sync progress scheduled animation")
	}
	view := m.render()
	for _, want := range []string{"homebrew-core", "100 commits scanned", "8 updates", "1 batches"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %q", want, view)
		}
	}
	m = update(t, m, SyncProgress{Source: "homebrew-core", Commits: 200, Events: 12, Batches: 2})
	if got := m.syncProgress["homebrew-core"]; got.Commits != 200 || got.Events != 12 {
		t.Fatalf("progress=%#v", got)
	}
	m = update(t, m, SyncDone{Source: "homebrew-core"})
	if len(m.activeSync) != 0 {
		t.Fatalf("active=%v", m.activeSync)
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
	m = update(t, m, key("enter"))
	if m.detailOpen {
		t.Fatal("enter did not return to feed")
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

func TestNavigationClearsPreviousPackageDetailsBeforeDebouncedLoad(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b")})
	m.packageInfo.Name = "old"
	m.readme = store.PackageDocument{ID: "1", Extracted: []byte("old")}
	m.changelog = []store.ChangelogSection{{Body: "old"}}
	m = update(t, m, key("j"))
	if m.packageInfo.Name != "" || m.readme.ID != "" || len(m.changelog) != 0 {
		t.Fatalf("details survived navigation: info=%#v readme=%#v changelog=%#v", m.packageInfo, m.readme, m.changelog)
	}
}

func TestBackgroundREADMECompletionOnlyUpdatesCurrentPackage(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b")})
	m = update(t, m, READMELoaded{PackageID: "pkg-b", Document: store.PackageDocument{ID: "wrong"}})
	if m.readme.ID != "" {
		t.Fatalf("other package README applied: %#v", m.readme)
	}
	m = update(t, m, READMELoaded{PackageID: "pkg-a", Document: store.PackageDocument{ID: "right"}})
	if m.readme.ID != "right" {
		t.Fatalf("current package README = %#v", m.readme)
	}
}

func TestInspectorRendersRepositoryTagsForCurrentPackage(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 20, false
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b")})
	m = update(t, m, RepositoryTagsLoaded{
		PackageID: "pkg-b",
		Record:    store.PackageRepositoryTags{Tags: []string{"wrong"}},
	})
	m = update(t, m, RepositoryTagsLoaded{
		PackageID: "pkg-a",
		Record:    store.PackageRepositoryTags{Tags: []string{"terminal", "Go"}},
	})

	view := ansi.Strip(m.renderInspector(72))
	if !strings.Contains(view, "Tags       terminal, Go") || strings.Contains(view, "wrong") {
		t.Fatalf("inspector repository tags:\n%s", view)
	}

	m = update(t, m, key("j"))
	if len(m.repositoryTags) != 0 {
		t.Fatalf("repository tags survived package navigation: %v", m.repositoryTags)
	}
}

func TestInspectorRepositoryTagsCollapseToThreeLinesAndToggle(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading, m.focus = 120, 30, false, inspectorPane
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a")})
	m = update(t, m, RepositoryTagsLoaded{
		PackageID: "pkg-a",
		Record: store.PackageRepositoryTags{Tags: []string{
			"first-topic", "second-topic", "third-topic", "fourth-topic",
			"fifth-topic", "sixth-topic", "seventh-topic", "Rust",
		}},
	})

	collapsed := ansi.Strip(m.renderInspector(32))
	if strings.Contains(collapsed, "Rust") || !strings.Contains(collapsed, "t expand") {
		t.Fatalf("collapsed repository tags:\n%s", collapsed)
	}
	tagLines := 0
	inTags := false
	for _, line := range strings.Split(collapsed, "\n") {
		if strings.Contains(line, "Tags       ") {
			inTags = true
		}
		if strings.Contains(line, "DOCUMENTS") {
			break
		}
		if inTags {
			tagLines++
		}
	}
	if tagLines != 3 {
		t.Fatalf("collapsed repository tags use %d lines, want 3:\n%s", tagLines, collapsed)
	}
	m = update(t, m, key("t"))
	expanded := ansi.Strip(m.renderInspector(32))
	if !strings.Contains(expanded, "Rust") || !strings.Contains(expanded, "t collapse") {
		t.Fatalf("expanded repository tags:\n%s", expanded)
	}
	m = update(t, m, key("t"))
	if view := ansi.Strip(m.renderInspector(32)); strings.Contains(view, "Rust") {
		t.Fatalf("repository tags did not collapse again:\n%s", view)
	}
}

func TestDetailProgressIgnoresOutOfOrderEvents(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, DetailProgress{Active: 1, Pending: 2, Sequence: 8})
	m = update(t, m, DetailProgress{Active: 9, Pending: 9, Sequence: 7})
	if m.detailProgress.Active != 1 || m.detailProgress.Pending != 2 || m.detailProgress.Sequence != 8 {
		t.Fatalf("detail progress = %+v", m.detailProgress)
	}
}

func TestOrdinaryVersionFeedRowsDoNotRepeatVersionLabel(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a")})
	row := m.feedGroupRow("› ", m.selectedEvent(), 72)
	if strings.Contains(row, "version") {
		t.Fatalf("version row contains redundant kind label: %q", row)
	}
	visible := ansi.Strip(row)
	if !strings.Contains(visible, "pkg-a") || !strings.Contains(visible, "2 1") {
		t.Fatalf("version row lost package or transition: %q", row)
	}
}

func TestFeedRowsShowUpdateFrequencyOnVersionLine(t *testing.T) {
	m := NewModel(Dependencies{})
	e := event("event", "package")
	e.UpdateInterval = 56 * time.Hour
	rows := m.feedGroupRows("› ", e, 72)
	if len(rows) != 2 || !strings.Contains(ansi.Strip(rows[0]), "🐇 3×week") ||
		!strings.Contains(ansi.Strip(rows[0]), "2 1") {
		t.Fatalf("update frequency and version were not rendered on one line: %#v", rows)
	}
}

func TestUpdateCadenceLabelUsesAnimalsAndCompactRates(t *testing.T) {
	m := NewModel(Dependencies{})
	tests := []struct {
		interval time.Duration
		want     string
	}{
		{6 * time.Hour, "🐆 4×day"},
		{28 * time.Hour, "🐇 1×day"},
		{56 * time.Hour, "🐇 3×week"},
		{60 * 24 * time.Hour, "🐄 6×year"},
		{120 * 24 * time.Hour, "🐢 3×year"},
	}
	for _, tt := range tests {
		e := event("event", "package")
		e.UpdateInterval = tt.interval
		if got := ansi.Strip(m.updateCadenceLabel(e)); got != tt.want {
			t.Errorf("updateCadenceLabel(%s) = %q, want %q", tt.interval, got, tt.want)
		}
	}
}

func TestUpdateCadenceLabelsUseFrequencySpecificBackgrounds(t *testing.T) {
	m := NewModel(Dependencies{})
	tests := []struct {
		interval   time.Duration
		background string
	}{
		{6 * time.Hour, "48;2;36;61;74"},
		{56 * time.Hour, "48;2;38;63;53"},
		{60 * 24 * time.Hour, "48;2;71;61;37"},
		{120 * 24 * time.Hour, "48;2;73;51;58"},
	}
	for _, tt := range tests {
		e := event("event", "package")
		e.UpdateInterval = tt.interval
		if got := m.updateCadenceLabel(e); !strings.Contains(got, tt.background) {
			t.Errorf("updateCadenceLabel(%s) = %q, want background %s", tt.interval, got, tt.background)
		}
	}
}

func TestFeedRowsReplaceUnknownFrequencyWithFadedLastUpdate(t *testing.T) {
	m := NewModel(Dependencies{})
	m.filter.Now = time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	e := event("event", "package")
	e.Time = m.filter.Now.Add(-21 * 24 * time.Hour)
	row := m.feedGroupRow("› ", e, 72)
	visible := ansi.Strip(row)
	if !strings.Contains(visible, "last update: 3 weeks ago") || strings.Contains(visible, "frequency unknown") {
		t.Fatalf("unknown update frequency was not replaced with relative recency: %q", row)
	}
	if !strings.Contains(row, "48;2;39;45;54") {
		t.Fatalf("unknown cadence is not faded into the feed surface: %q", row)
	}
}

func TestFeedSeparatesNewUpdatesFromPreviouslySeenUpdates(t *testing.T) {
	m := NewModel(Dependencies{})
	seenA := event("seen-a", "seen-a")
	seenA.Seen = true
	newB := event("new-b", "new-b")
	seenC := event("seen-c", "seen-c")
	seenC.Seen = true
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: []domain.FeedGroup{
		{ID: seenA.ID, Events: []domain.UpdateEvent{seenA}},
		{ID: newB.ID, Events: []domain.UpdateEvent{newB}},
		{ID: seenC.ID, Events: []domain.UpdateEvent{seenC}},
	}})

	view := ansi.Strip(m.renderFeedRows(72))
	newIndex := strings.Index(view, "new-b")
	boundaryIndex := strings.Index(view, "previously seen")
	seenAIndex := strings.Index(view, "seen-a")
	seenCIndex := strings.Index(view, "seen-c")
	if newIndex < 0 || boundaryIndex < 0 || seenAIndex < 0 || seenCIndex < 0 ||
		!(newIndex < boundaryIndex && boundaryIndex < seenAIndex && seenAIndex < seenCIndex) {
		t.Fatalf("feed did not partition new and seen updates:\n%s", view)
	}
	if strings.Count(view, "previously seen") != 1 {
		t.Fatalf("seen boundary count = %d, want 1", strings.Count(view, "previously seen"))
	}

	newB.Seen = true
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: []domain.FeedGroup{
		{ID: seenA.ID, Events: []domain.UpdateEvent{seenA}},
		{ID: newB.ID, Events: []domain.UpdateEvent{newB}},
		{ID: seenC.ID, Events: []domain.UpdateEvent{seenC}},
	}})
	view = ansi.Strip(m.renderFeedRows(72))
	if !(strings.Index(view, "new-b") < strings.Index(view, "previously seen")) {
		t.Fatalf("new update crossed the boundary during its original session:\n%s", view)
	}
}

func TestFeedOmitsPreviouslySeenSeparatorWhenThereAreNoNewUpdates(t *testing.T) {
	m := NewModel(Dependencies{})
	seen := event("seen", "seen")
	seen.Seen = true
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: []domain.FeedGroup{
		{ID: seen.ID, Events: []domain.UpdateEvent{seen}},
	}})

	view := ansi.Strip(m.renderFeedRows(72))
	if strings.Contains(view, "previously seen") {
		t.Fatalf("feed rendered a leading previously-seen separator:\n%s", view)
	}
}

func TestFeedLoadPersistsDisplayedEventIDsAsSeen(t *testing.T) {
	source := &fakeSeenData{fakeData: &fakeData{}}
	m := NewModel(Dependencies{Data: source})
	rolled := domain.FeedGroup{ID: "a", Events: []domain.UpdateEvent{event("a", "pkg-a"), event("a-child", "pkg-a")}}
	next, command := m.Update(FeedLoaded{RequestID: m.feedRequestID, Groups: []domain.FeedGroup{rolled}})
	m = next.(Model)
	if command == nil {
		t.Fatal("feed load did not schedule seen persistence")
	}
	batch, ok := command().(tea.BatchMsg)
	if !ok {
		t.Fatalf("feed load command = %T, want batch", command())
	}
	for _, child := range batch {
		if msg := child(); msg != nil {
			m = update(t, m, msg)
		}
	}
	if diff := cmp.Diff([]domain.EventID{"a", "a-child"}, source.marked); diff != "" {
		t.Fatalf("marked event IDs mismatch (-want +got):\n%s", diff)
	}
}

func TestSeenSeparatorParticipatesInSelectionAndMouseGeometry(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 72, 12, false
	newEvent := event("new", "new")
	seenEvent := event("seen", "seen")
	seenEvent.Seen = true
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: []domain.FeedGroup{
		{ID: newEvent.ID, Events: []domain.UpdateEvent{newEvent}},
		{ID: seenEvent.ID, Events: []domain.UpdateEvent{seenEvent}},
	}})
	m.syncViewports()

	separatorLine := feedRowHeight(m.feedViewport.Width())
	if got := m.feedGroupAtLine(separatorLine); got != -1 {
		t.Fatalf("separator maps to feed group %d", got)
	}
	if got := m.feedGroupAtLine(separatorLine + 1); got != 1 {
		t.Fatalf("first seen row maps to feed group %d, want 1", got)
	}
	m = update(t, m, key("j"))
	if got := m.selectedFeedLine(); got != separatorLine+1 {
		t.Fatalf("seen selection line = %d, want %d", got, separatorLine+1)
	}
}

func TestNarrowFeedRowsKeepFrequencyAndVersionVisible(t *testing.T) {
	m := NewModel(Dependencies{})
	e := event("event", "package")
	e.UpdateInterval = 120 * 24 * time.Hour
	row := ansi.Strip(m.feedGroupRow("› ", e, 40))
	if !strings.Contains(row, "🐢 3×year") || !strings.Contains(row, "2 1") {
		t.Fatalf("narrow feed row lost frequency or version: %q", row)
	}
}

func TestFeedRowIncludesCachedPackageDescription(t *testing.T) {
	m := NewModel(Dependencies{})
	e := event("event", "package")
	m.packageDescriptions[e.PackageID] = "A concise package description"

	rows := m.feedGroupRows("› ", e, 72)
	if len(rows) != 2 || !strings.Contains(ansi.Strip(rows[1]), "A concise package description") {
		t.Fatalf("description was not integrated below the package name: %#v", rows)
	}
}

func TestVisibleRowsPrefetchCachedDescriptions(t *testing.T) {
	source := &fakeCachedInfo{values: map[domain.PackageID]homebrew.PackageInfo{
		"pkg-b": {Name: "b", Description: "cached description"},
	}}
	m := NewModel(Dependencies{PackageInfo: source})
	m.width, m.height, m.loading = 72, 10, false
	m.groups = groups("a", "b")
	m.syncViewports()

	commands := m.loadVisiblePackageDescriptions()
	for _, command := range commands {
		if command == nil {
			continue
		}
		message := command()
		if message != nil {
			m = update(t, m, message)
		}
	}
	if m.packageDescriptions["pkg-b"] != "cached description" {
		t.Fatalf("feed description cache = %q", m.packageDescriptions["pkg-b"])
	}
}

func TestChangingSelectionCancelsObsoleteDetailRefreshes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &blockingDetailSource{started: make(chan string, 3), canceled: make(chan string, 3)}
	m := NewModel(Dependencies{Context: ctx, PackageInfo: source, README: source, Changelog: source})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b")})

	next, command := m.Update(ChangelogDebounced{SelectionID: m.selectionID})
	m = next.(Model)
	batch, ok := command().(tea.BatchMsg)
	if !ok || len(batch) != 3 {
		t.Fatalf("detail refresh command = %T with %d children, want three", command(), len(batch))
	}
	for _, child := range batch {
		go child()
	}
	for range 3 {
		select {
		case <-source.started:
		case <-time.After(time.Second):
			t.Fatal("detail refresh did not start")
		}
	}

	m = update(t, m, key("j"))
	for range 3 {
		select {
		case <-source.canceled:
		case <-time.After(100 * time.Millisecond):
			t.Fatal("obsolete detail refresh survived the selection change")
		}
	}
}

func TestFeedLoadRequestsDescriptionsForVisiblePackages(t *testing.T) {
	source := &fakeCachedInfo{values: map[domain.PackageID]homebrew.PackageInfo{
		"pkg-a": {Description: "description a"},
		"pkg-b": {Description: "description b"},
	}}
	m := NewModel(Dependencies{PackageInfo: source})
	m.width, m.height = 72, 10

	next, command := m.Update(FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b", "c", "d")})
	m = next.(Model)
	batch := command().(tea.BatchMsg)
	for _, child := range batch {
		message := child()
		if message != nil {
			m = update(t, m, message)
		}
	}

	if got := source.loads; len(got) != 0 {
		t.Fatalf("visible rows triggered refreshing package info loads: %v", got)
	}
	if got := source.cachedLoads; len(got) != 2 || got[0] != "pkg-a" || got[1] != "pkg-b" {
		t.Fatalf("visible cached package info loads = %v, want [pkg-a pkg-b]", got)
	}
	if m.packageDescriptions["pkg-a"] != "description a" || m.packageDescriptions["pkg-b"] != "description b" {
		t.Fatalf("visible descriptions were not cached: %#v", m.packageDescriptions)
	}
}

func TestVersionTransitionHidesArchiveSuffixes(t *testing.T) {
	event := domain.UpdateEvent{OldVersion: "1.0.tar", NewVersion: "2.0.tgz"}
	if got := ansi.Strip(NewModel(Dependencies{}).versionTransition(event)); got != "2.0 1.0" {
		t.Fatalf("versionTransition = %q", got)
	}
}

func TestVersionTransitionHighlightsChangedCharacters(t *testing.T) {
	m := NewModel(Dependencies{})
	rendered := m.versionTransition(domain.UpdateEvent{OldVersion: "1.2.5", NewVersion: "1.2.6"})
	if got := ansi.Strip(rendered); got != "1.2.6 1.2.5" {
		t.Fatalf("visible transition = %q, want %q", got, "1.2.6 1.2.5")
	}
	for _, code := range []string{"38;2;215;95;95", "1;38;2;79;191;121"} {
		if !strings.Contains(rendered, code) {
			t.Fatalf("transition missing style %q: %q", code, rendered)
		}
	}
}

func TestVersionTransitionDiffsEachChangedSegmentFromLeftToRight(t *testing.T) {
	m := NewModel(Dependencies{})
	rendered := m.versionTransition(domain.UpdateEvent{OldVersion: "2.19.4", NewVersion: "2.20.0"})
	if got := ansi.Strip(rendered); got != "2.20.0 2.19.4" {
		t.Fatalf("visible transition = %q, want %q", got, "2.20.0 2.19.4")
	}
}

func TestChangelogFailureFallsBackToAvailableREADMEWithoutGlobalError(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a")})
	m.readme = store.PackageDocument{ID: "readme", Extracted: []byte("# Read me")}
	m.changelogRequestID = 4
	m = update(t, m, ChangelogLoaded{
		RequestID: 4, SelectionID: m.selectionID, EventID: "a", PackageID: "pkg-a",
		Err: errors.New("changelog refresh backed off"),
	})
	if m.document != store.DocumentREADME || m.err != nil || m.changelogErr == nil {
		t.Fatalf("fallback state: document=%q global=%v changelog=%v", m.document, m.err, m.changelogErr)
	}
	view := ansi.Strip(m.renderInspector(72))
	if strings.Contains(view, "changelog refresh backed off") || !strings.Contains(view, "Read me") {
		t.Fatalf("README fallback view:\n%s", view)
	}
}

func TestREADMEArrivalReplacesEmptyChangelog(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a")})
	m.document = store.DocumentChangelog
	m = update(t, m, READMELoaded{PackageID: "pkg-a", Document: store.PackageDocument{ID: "readme", Extracted: []byte("available")}})
	if m.document != store.DocumentREADME {
		t.Fatalf("document = %q, want README", m.document)
	}
}

func TestInspectorIsWiderByDefaultAndWidensWhenFocused(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width = 120
	feed, inspector := m.paneWidths()
	if inspector <= 120*2/5 {
		t.Fatalf("default pane widths = %d/%d, inspector did not grow", feed, inspector)
	}
	m.focus = inspectorPane
	focusedFeed, focusedInspector := m.paneWidths()
	if focusedInspector <= inspector || focusedFeed >= feed {
		t.Fatalf("focused pane widths = %d/%d, default = %d/%d", focusedFeed, focusedInspector, feed, inspector)
	}
}

func TestInspectorReadingModeScrollsWithoutChangingFeedSelection(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 8, false
	m.groups = groups("a", "b")
	m.document = store.DocumentREADME
	m.readme = store.PackageDocument{ID: "readme", Extracted: []byte(strings.Repeat("line\n\n", 30))}
	m.focus = inspectorPane
	m.syncViewports()
	selected := m.selected
	m = update(t, m, key("j"))
	if m.selected != selected || m.inspectorViewport.YOffset() == 0 {
		t.Fatalf("inspector scroll changed selection or did not scroll: selection=%d offset=%d", m.selected, m.inspectorViewport.YOffset())
	}
	m = update(t, m, key("enter"))
	if m.focus != feedPane {
		t.Fatalf("enter did not leave inspector reading mode: %v", m.focus)
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
	visibleRows := m.feedViewport.Height()
	selectedLine := m.selectedFeedLine()
	selectedBottom := selectedLine + feedRowHeight(m.feedViewport.Width()) - 1
	if selectedBottom < m.viewportOffset || selectedLine >= m.viewportOffset+visibleRows {
		t.Fatalf("selection band [%d,%d] is outside viewport offset %d height %d", selectedLine, selectedBottom, m.viewportOffset, visibleRows)
	}
}

func BenchmarkNavigationLargeFeed(b *testing.B) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 30, false
	m.groups = groups(makeIDs(5000)...)
	m.seenBoundaryIndex = len(m.groups)
	m.syncViewports()
	down := key("j")
	b.ResetTimer()
	for range b.N {
		next, _ := m.Update(down)
		m = next.(Model)
		if m.selected == len(m.groups)-1 {
			m.selected = 0
		}
	}
}

func TestLargeFeedMovementWorkIsBoundedToTheViewport(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 30, false
	m.groups = groups(makeIDs(5000)...)
	m.seenBoundaryIndex = len(m.groups)
	m.syncViewports()

	navigationAllocs := testing.AllocsPerRun(3, func() {
		next, _ := m.Update(key("j"))
		m = next.(Model)
	})
	renderAllocs := testing.AllocsPerRun(3, func() {
		_ = m.render()
	})
	if navigationAllocs > 20_000 || renderAllocs > 20_000 {
		t.Fatalf("large-feed work scales with all rows: navigation %.0f allocs, render %.0f allocs", navigationAllocs, renderAllocs)
	}
}

func BenchmarkRenderLargeFeed(b *testing.B) {
	m := NewModel(Dependencies{})
	m.width, m.height, m.loading = 120, 30, false
	m.groups = groups(makeIDs(5000)...)
	m.seenBoundaryIndex = len(m.groups)
	m.syncViewports()
	b.ResetTimer()
	for range b.N {
		_ = m.render()
	}
}

func makeIDs(count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("%d", i)
	}
	return ids
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

	selectedLine := m.selectedFeedLine()
	selectedBottom := selectedLine + feedRowHeight(m.feedViewport.Width()) - 1
	top, height := m.feedViewport.YOffset(), m.feedViewport.Height()
	if selectedBottom < top || selectedLine >= top+height {
		t.Fatalf("rendered selected band [%d,%d] outside real viewport [%d,%d)", selectedLine, selectedBottom, top, top+height)
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

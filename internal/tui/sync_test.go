package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/store"
	"github.com/charmbracelet/x/ansi"
)

func TestSyncFooterKeepsFiltersFreshnessAndOtherStatus(t *testing.T) {
	now := time.Date(2026, time.August, 3, 15, 0, 0, 0, time.Local)
	m := NewModel(Dependencies{Now: func() time.Time { return now }})
	m = update(t, m, WindowSize{Width: 120, Height: 20})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("cached")})
	m = update(t, m, FreshnessLoaded{RequestID: m.freshnessRequestID, Status: store.FreshnessStatus{LastSync: now.Add(-time.Hour)}})
	m.filter.Query = "cached"
	m.filter.Search = domain.SearchNames
	m = update(t, m, Notify{Text: "Package operation ready"})
	before := m.feedHeaderRows()
	m = update(t, m, SyncStarted{Source: "homebrew-core", ID: 1})
	m = update(t, m, SyncPhase{Source: "homebrew-core", ID: 1, Phase: "fetching"})
	m = update(t, m, SyncQueued{Source: "homebrew-cask", ID: 2, HistoricalFallback: true})

	view := ansi.Strip(m.render())
	lines := strings.Split(view, "\n")
	footer := lines[len(lines)-1]
	for _, want := range []string{"Sync: 1 active · 1 queued", "Package operation ready", "s  sync"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("footer missing %q: %q", want, footer)
		}
	}
	header := ansi.Strip(m.renderFeedHeader(m.feedViewport.Width()))
	for _, want := range []string{"search names: \"cached\"", "Sync Aug 3 2026 14:00"} {
		if !strings.Contains(header, want) {
			t.Fatalf("feed header missing %q: %q", want, header)
		}
	}
	for _, duplicate := range []string{"fetching/checking", "history queued", "Syncing homebrew-core"} {
		if strings.Contains(header, duplicate) {
			t.Fatalf("duplicate sync activity %q in header: %q", duplicate, header)
		}
	}
	if got := m.feedHeaderRows(); got != before {
		t.Fatalf("sync added header rows: before=%d after=%d", before, got)
	}
}

func TestSyncModalShowsConcurrentWorkFailuresAndLiveUpdates(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, WindowSize{Width: 120, Height: 24})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a")})
	m = update(t, m, SyncStarted{Source: "homebrew-cask", ID: 1})
	m = update(t, m, SyncDone{Source: "homebrew-cask", ID: 1, Err: errors.New("network offline")})
	m = update(t, m, SyncStarted{Source: "homebrew-core", ID: 2})
	m = update(t, m, SyncPhase{Source: "homebrew-core", ID: 2, Phase: "indexing"})
	m = update(t, m, SyncProgress{Source: "homebrew-core", ID: 2, Commits: 123, Events: 7, Batches: 4, Diagnostics: 2})
	m = update(t, m, SyncQueued{Source: "homebrew-core", ID: 3, HistoricalFallback: true})
	m = update(t, m, SyncQueued{Source: "homebrew-core", ID: 4})
	m = update(t, m, key("s"))

	view := ansi.Strip(m.render())
	for _, want := range []string{
		"Synchronization status", "Sync: 1 active · 2 queued · 1 failed",
		"homebrew-core · indexing", "123 commits · 7 updates · 4 batches · 2 diagnostics",
		"Queued · homebrew-core · history indexing · job 3",
		"Queued · homebrew-core · refresh · job 4",
		"Failed · homebrew-cask · refresh · network offline",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("sync modal missing %q:\n%s", want, view)
		}
	}
	m = update(t, m, SyncProgress{Source: "homebrew-core", ID: 2, Commits: 200, Events: 9, Batches: 5})
	view = ansi.Strip(m.render())
	if !strings.Contains(view, "200 commits · 9 updates · 5 batches") || strings.Contains(view, "123 commits") {
		t.Fatalf("open sync modal did not update: %s", view)
	}
	m = update(t, m, key("esc"))
	if got := ansi.Strip(m.render()); strings.Contains(got, "Synchronization status") || !strings.Contains(got, "Sync: 1 active") {
		t.Fatalf("escape did not return to the feed: %s", got)
	}
}

func TestSyncStatusClickAndKeyboardDismissal(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, WindowSize{Width: 120, Height: 20})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b")})
	m = update(t, m, SyncStarted{Source: "core", ID: 1})
	lines := strings.Split(ansi.Strip(m.render()), "\n")
	footer := lines[len(lines)-1]
	index := strings.Index(footer, "Sync:")
	if index < 0 {
		t.Fatalf("sync control missing: %q", footer)
	}
	m = update(t, m, tea.MouseClickMsg{X: ansi.StringWidth(footer[:index]), Y: m.height - 1, Button: tea.MouseLeft})
	if !strings.Contains(ansi.Strip(m.render()), "Synchronization status") {
		t.Fatal("clicking the visible footer status did not open the modal")
	}
	m = update(t, m, tea.MouseClickMsg{X: 5, Y: m.feedHeaderRows(), Button: tea.MouseLeft})
	if m.syncStatusOpen || m.selected != 0 {
		t.Fatal("modal click failed to dismiss or reached the feed")
	}
	m = update(t, m, key("s"))
	if !m.syncStatusOpen {
		t.Fatal("s did not open sync status")
	}
	m = update(t, m, key("enter"))
	if m.syncStatusOpen {
		t.Fatal("enter did not dismiss sync status")
	}

	footer = strings.Split(ansi.Strip(m.render()), "\n")[m.height-1]
	index = strings.Index(footer, " s  sync")
	if index < 0 {
		t.Fatalf("sync shortcut hint missing: %q", footer)
	}
	m = update(t, m, tea.MouseClickMsg{X: ansi.StringWidth(footer[:index]) + 1, Y: m.height - 1, Button: tea.MouseLeft})
	if !m.syncStatusOpen {
		t.Fatal("clicking the sync shortcut hint did not open the modal")
	}
	m = update(t, m, key("s"))
	if m.syncStatusOpen {
		t.Fatal("s did not dismiss sync status")
	}
}

func TestSyncStatusModalKeepsQuitShortcut(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, key("s"))
	_, msg := updateAndRunCommand(t, m, key("q"))
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("command result = %T, want tea.QuitMsg", msg)
	}
}

func TestSyncModalResizesClipsAndScrollsWithoutMovingFeed(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, WindowSize{Width: 100, Height: 20})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a", "b", "c")})
	m = update(t, m, SyncStarted{Source: "cask", ID: 1})
	m = update(t, m, SyncDone{Source: "cask", ID: 1, Err: errors.New("offline")})
	m = update(t, m, SyncStarted{Source: "core", ID: 2})
	m = update(t, m, SyncProgress{Source: "core", ID: 2, Commits: 17, Events: 3, Batches: 2})
	m = update(t, m, SyncQueued{Source: "core", ID: 3, HistoricalFallback: true})
	m = update(t, m, key("s"))
	m = update(t, m, WindowSize{Width: 36, Height: 8})

	var seen strings.Builder
	for range 30 {
		view := m.render()
		lines := strings.Split(view, "\n")
		if len(lines) != m.height {
			t.Fatalf("modal rendered %d rows, want %d", len(lines), m.height)
		}
		for row, line := range lines {
			if got := ansi.StringWidth(line); got != m.width {
				t.Fatalf("row %d width %d, want %d", row, got, m.width)
			}
		}
		seen.WriteString(ansi.Strip(view))
		m = update(t, m, key("j"))
	}
	for _, want := range []string{"Synchronization status", "17 commits", "Queued · core", "offline"} {
		if !strings.Contains(seen.String(), want) {
			t.Fatalf("scrolling never revealed %q:\n%s", want, seen.String())
		}
	}
	selected, offset := m.selected, m.viewportOffset
	m = update(t, m, tea.MouseWheelMsg{X: 2, Y: 2, Button: tea.MouseWheelUp})
	if m.selected != selected || m.viewportOffset != offset {
		t.Fatal("modal scroll leaked into the underlying feed")
	}
	m = update(t, m, WindowSize{Width: 120, Height: 24})
	m = update(t, m, key("k"))
	if got := ansi.Strip(m.render()); !strings.Contains(got, "17 commits") || !strings.Contains(got, "offline") {
		t.Fatalf("resized modal lost current details: %s", got)
	}
}

func TestSyncFailureRemainsUntilMatchingWorkSucceeds(t *testing.T) {
	m := NewModel(Dependencies{})
	m = update(t, m, WindowSize{Width: 120, Height: 20})
	m = update(t, m, SyncStarted{Source: "core", ID: 1, HistoricalFallback: true})
	m = update(t, m, SyncDone{Source: "core", ID: 1, HistoricalFallback: true, Err: errors.New("history unavailable")})
	m = update(t, m, SyncStarted{Source: "core", ID: 2})
	m = update(t, m, SyncDone{Source: "core", ID: 2})
	m = update(t, m, key("s"))
	if got := ansi.Strip(m.render()); !strings.Contains(got, "Failed · core · history indexing · history unavailable") {
		t.Fatalf("successful refresh hid history failure: %s", got)
	}
	m = update(t, m, SyncStarted{Source: "core", ID: 3, HistoricalFallback: true})
	m = update(t, m, SyncDone{Source: "core", ID: 3, HistoricalFallback: true})
	if got := ansi.Strip(m.render()); strings.Contains(got, "history unavailable") || !strings.Contains(got, "No active or queued sync work") {
		t.Fatalf("successful history retry did not clear the failure: %s", got)
	}
}

func TestSyncModalShowsPersistedFailureWhenIdle(t *testing.T) {
	now := time.Date(2026, time.August, 3, 15, 0, 0, 0, time.Local)
	m := NewModel(Dependencies{Now: func() time.Time { return now }})
	m = update(t, m, WindowSize{Width: 100, Height: 20})
	m = update(t, m, FreshnessLoaded{RequestID: m.freshnessRequestID, Status: store.FreshnessStatus{
		LastAttempt: now.Add(-time.Minute), LastError: "historical fallback: network offline",
	}})
	m = update(t, m, key("s"))
	if got := ansi.Strip(m.render()); !strings.Contains(got, "Last failure · historical fallback: network offline") ||
		!strings.Contains(got, "No active or queued sync work") {
		t.Fatalf("idle sync status omitted persisted failure: %s", got)
	}
}

func TestSyncStatusDoesNotOverrideSearchOrActionHint(t *testing.T) {
	m := NewModel(Dependencies{Actions: &fakeActions{}})
	m = update(t, m, WindowSize{Width: 120, Height: 20})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("a")})
	m = update(t, m, SyncStarted{Source: "core", ID: 1})
	footer := strings.Split(ansi.Strip(m.render()), "\n")[m.height-1]
	index := strings.Index(footer, " a ")
	if index < 0 {
		t.Fatalf("sync hid the package action: %q", footer)
	}
	next, msg := updateAndRunCommand(t, m, tea.MouseClickMsg{
		X: ansi.StringWidth(footer[:index]) + 1, Y: m.height - 1, Button: tea.MouseLeft,
	})
	if _, ok := msg.(ActionRequested); !ok || next.syncStatusOpen {
		t.Fatalf("action click opened sync status or emitted %T", msg)
	}
	m = update(t, m, key("/"))
	m = update(t, m, key("s"))
	if m.filter.Query != "s" || m.syncStatusOpen {
		t.Fatalf("sync shortcut intercepted search input: query=%q", m.filter.Query)
	}
}

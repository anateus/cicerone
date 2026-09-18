package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/store"
)

func TestDatasetRefreshMakesProgressWhileBatchesArrive(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "feed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	write := func(id string, age time.Duration) {
		t.Helper()
		e := event(id, "pkg-"+id)
		e.Time = now.Add(-age)
		if err := db.UpsertEvents(ctx, []domain.UpdateEvent{e}); err != nil {
			t.Fatal(err)
		}
	}
	write("cached", time.Hour)
	m := NewModel(Dependencies{Data: db, Now: func() time.Time { return now }})
	m = update(t, m, WindowSize{Width: 80, Height: 24})
	m = update(t, m, feedResult(t, m.Init()))

	write("first", time.Minute)
	next, command := m.Update(DatasetChanged{})
	m = next.(Model)
	// The query finishes, but its response waits behind further commits.
	first := feedResult(t, command)
	write("latest", 0)
	for range 3 {
		m = update(t, m, DatasetChanged{})
	}
	next, command = m.Update(first)
	m = next.(Model)
	if !strings.Contains(m.View().Content, "pkg-first") {
		t.Fatal("committed first batch was discarded while newer batches arrived")
	}
	if command == nil {
		t.Fatal("newer commits did not schedule a catch-up query")
	}
	m = update(t, m, feedResult(t, command))
	if !strings.Contains(m.View().Content, "pkg-latest") {
		t.Fatal("catch-up query did not display the latest committed batch")
	}

	restarted := NewModel(Dependencies{Data: db, Now: func() time.Time { return now }})
	restarted = update(t, restarted, feedResult(t, restarted.Init()))
	if len(m.groups) != len(restarted.groups) || m.groups[0].ID != restarted.groups[0].ID {
		t.Fatalf("live feed differs from restart: live=%v restart=%v", m.groups, restarted.groups)
	}
}

func TestRefreshKeyPreemptsInitialRefresh(t *testing.T) {
	refreshes := 0
	m := NewModel(Dependencies{
		OnReady: func() tea.Msg { return InitialRefreshDone{} },
		Refresh: func() tea.Msg { refreshes++; return RefreshDone{} },
	})
	next, cmd := m.Update(key("r"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("explicit refresh was ignored while startup synchronization was active")
	}
	_ = cmd()
	if refreshes != 1 || !m.manualRefreshRunning {
		t.Fatalf("refreshes=%d running=%v, want one active manual refresh", refreshes, m.manualRefreshRunning)
	}
	if _, cmd := m.Update(key("r")); cmd != nil {
		t.Fatal("repeated refresh started overlapping manual work")
	}
}

func TestRefreshCompletionsCoalesceBehindRunningQuery(t *testing.T) {
	for _, notification := range []tea.Msg{DatasetChanged{}, InitialRefreshDone{}, RefreshDone{}} {
		for _, queryErr := range []error{nil, errors.New("query failed")} {
			t.Run(fmt.Sprintf("%T/error=%v", notification, queryErr), func(t *testing.T) {
				data := &fakeData{groups: groups("latest")}
				m := NewModel(Dependencies{Data: data})
				requestID := m.feedRequestID
				for range 3 {
					next, cmd := m.Update(notification)
					m = next.(Model)
					if cmd != nil || m.feedRequestID != requestID {
						t.Fatal("refresh superseded an in-flight query")
					}
				}
				next, cmd := m.Update(FeedLoaded{RequestID: requestID, Groups: groups("first"), Err: queryErr})
				m = next.(Model)
				m = update(t, m, feedResult(t, cmd))
				if m.groups[0].ID != "latest" || m.loading || m.stale || m.feedRefreshPending {
					t.Fatalf("catch-up did not settle: groups=%v loading=%v stale=%v pending=%v", m.groups, m.loading, m.stale, m.feedRefreshPending)
				}
			})
		}
	}
}

func TestQueuedDatasetRefreshStillRejectsOldFilterResults(t *testing.T) {
	data := &fakeData{groups: groups("filtered")}
	m := NewModel(Dependencies{Data: data})
	old := FeedLoaded{RequestID: m.feedRequestID, Groups: groups("unfiltered")}
	m = update(t, m, DatasetChanged{})
	next, cmd := m.Update(SearchChanged{Text: "filtered"})
	m = next.(Model)
	filtered := feedResult(t, cmd)
	m = update(t, m, old)
	if len(m.groups) != 0 {
		t.Fatal("queued refresh allowed a response for the old filter")
	}
	m = update(t, m, filtered)
	if m.groups[0].ID != "filtered" {
		t.Fatal("current filter result was not displayed")
	}
}

func TestDatasetChangeDoesNotInvalidateSearchDebounce(t *testing.T) {
	data := &recordingData{}
	m := NewModel(Dependencies{Data: data})
	m = update(t, m, FeedLoaded{RequestID: m.feedRequestID, Groups: groups("cached")})
	m = update(t, m, key("/"))
	m = update(t, m, key("f"))
	searchID := m.feedRequestID
	m = update(t, m, DatasetChanged{})
	next, cmd := m.Update(SearchDebounced{RequestID: searchID})
	m = next.(Model)
	_ = feedResult(t, cmd)
	if data.queried.Query != "f" {
		t.Fatalf("search query = %q, want f", data.queried.Query)
	}
}

// Execute query commands without dispatching unrelated detail-loading messages.
func feedResult(t *testing.T, command tea.Cmd) FeedLoaded {
	t.Helper()
	var results []FeedLoaded
	var run func(tea.Cmd)
	run = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch msg := cmd().(type) {
		case FeedLoaded:
			results = append(results, msg)
		case tea.BatchMsg:
			for _, child := range msg {
				run(child)
			}
		}
	}
	run(command)
	if len(results) != 1 {
		t.Fatalf("got %d feed responses, want exactly one", len(results))
	}
	if results[0].Err != nil {
		t.Fatal(results[0].Err)
	}
	return results[0]
}

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"cicerone/internal/domain"
	"github.com/google/go-cmp/cmp"
)

func TestUpsertEventsIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	event := testEvent("one", "foo", domain.EventVersion, time.Now().UTC())
	if err := s.UpsertEvents(context.Background(), []domain.UpdateEvent{event, event}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEvents(context.Background(), []domain.UpdateEvent{event}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM update_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("event count = %d, want 1", count)
	}
}

func TestWriteIsAtomicallyVisibleToReaders(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	firstInserted := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO packages(id, name, type) VALUES ('foo', 'Foo', 'formula')`); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO update_events(id, package_id, kind, repository, commit_hash, event_time) VALUES ('one', 'foo', 'version', 'core', 'a', 1)`); err != nil {
				return err
			}
			close(firstInserted)
			<-finish
			_, err := tx.Exec(`INSERT INTO update_events(id, package_id, kind, repository, commit_hash, event_time) VALUES ('two', 'foo', 'version', 'core', 'b', 2)`)
			return err
		})
	}()
	<-firstInserted
	if got := eventCount(t, s); got != 0 {
		t.Fatalf("reader saw %d rows before commit, want 0", got)
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := eventCount(t, s); got != 2 {
		t.Fatalf("reader saw %d rows after commit, want 2", got)
	}
}

func TestQueryFeedSelectsRecentEventsAndInstalledFallback(t *testing.T) {
	s := openTestStore(t)
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	events := []domain.UpdateEvent{
		testEvent("recent", "recent", domain.EventVersion, now.Add(-time.Hour)),
		testEvent("installed-old", "installed", domain.EventVersion, now.Add(-40*24*time.Hour)),
		testEvent("installed-older", "installed", domain.EventVersion, now.Add(-50*24*time.Hour)),
		testEvent("uninstalled-old", "old", domain.EventVersion, now.Add(-40*24*time.Hour)),
	}
	if err := s.UpsertEvents(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInstalled(context.Background(), []domain.InstalledPackage{{PackageID: "installed", Name: "installed", Version: "1", Type: domain.PackageFormula}}); err != nil {
		t.Fatal(err)
	}
	groups, err := s.QueryFeed(context.Background(), domain.FeedFilter{Now: now, Horizon: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var got []domain.EventID
	for _, group := range groups {
		got = append(got, group.ID)
	}
	if diff := cmp.Diff([]domain.EventID{"recent", "installed-old"}, got); diff != "" {
		t.Fatalf("feed mismatch (-want +got):\n%s", diff)
	}
}

func TestPreferencesRoundTripTypedFilter(t *testing.T) {
	s := openTestStore(t)
	want := domain.FeedFilter{Horizon: 14 * 24 * time.Hour, Kinds: map[domain.EventKind]bool{domain.EventRevision: true}, Types: map[domain.PackageType]bool{domain.PackageCask: true}, Query: "foo", RollUp: false}
	if err := s.SetPreferences(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Preferences(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("preferences mismatch (-want +got):\n%s", diff)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func testEvent(id string, packageID domain.PackageID, kind domain.EventKind, at time.Time) domain.UpdateEvent {
	return domain.UpdateEvent{ID: domain.EventID(id), PackageID: packageID, Name: string(packageID), Type: domain.PackageFormula, Kind: kind, Repository: "core", Commit: id, Time: at}
}

func eventCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM update_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

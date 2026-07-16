package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestOpenConfiguresAndMigratesDatabase(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "cicerone.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	assertPragma(t, store.db, "journal_mode", "wal")
	assertPragma(t, store.db, "foreign_keys", "1")
	assertPragma(t, store.db, "busy_timeout", "5000")
	assertPragma(t, store.db, "user_version", "1")

	wantTables := []string{
		"changelog_artifacts", "changelog_artifacts_fts", "changelog_attempts",
		"changelog_sections", "installed_packages", "package_aliases", "packages",
		"packages_fts", "preferences", "repositories", "repository_ranges", "sync_runs",
		"update_events",
	}
	rows, err := store.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var gotTables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(name, "sqlite_") && !strings.HasSuffix(name, "_config") && !strings.HasSuffix(name, "_content") && !strings.HasSuffix(name, "_data") && !strings.HasSuffix(name, "_docsize") && !strings.HasSuffix(name, "_idx") {
			gotTables = append(gotTables, name)
		}
	}
	if !slices.Equal(gotTables, wantTables) {
		t.Fatalf("tables = %v, want %v", gotTables, wantTables)
	}

	var version string
	if err := store.db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if compareSQLiteVersions(version, "3.51.3") < 0 {
		t.Fatalf("sqlite_version() = %s, want >= 3.51.3", version)
	}

	_, err = store.db.ExecContext(ctx, `
		INSERT INTO packages(id, name, type) VALUES ('foo', 'Foo', 'formula');
		INSERT INTO update_events(id, package_id, kind, repository, commit_hash, event_time)
		VALUES ('one', 'foo', 'version', 'core', 'abc', 1),
		       ('two', 'foo', 'version', 'core', 'abc', 2);`)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate event identity error = %v, want UNIQUE constraint", err)
	}

	for _, index := range []string{"update_events_time", "update_events_package_kind_time", "installed_packages_identity"} {
		var found int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found != 1 {
			t.Errorf("index %s is missing", index)
		}
	}
	var ftsMatches int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM packages_fts WHERE packages_fts MATCH 'Foo'`).Scan(&ftsMatches); err != nil {
		t.Fatal(err)
	}
	if ftsMatches != 1 {
		t.Fatalf("package FTS matches = %d, want 1", ftsMatches)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO changelog_artifacts(url, extracted_text) VALUES ('https://example.test/foo', 'Important security fixes')`); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM changelog_artifacts_fts WHERE changelog_artifacts_fts MATCH 'security'`).Scan(&ftsMatches); err != nil {
		t.Fatal(err)
	}
	if ftsMatches != 1 {
		t.Fatalf("changelog FTS matches = %d, want 1", ftsMatches)
	}
}

func assertPragma(t *testing.T, db *sql.DB, name, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(`PRAGMA ` + name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, want) {
		t.Fatalf("PRAGMA %s = %q, want %q", name, got, want)
	}
}

func compareSQLiteVersions(a, b string) int {
	parse := func(s string) [3]int {
		var result [3]int
		for i, part := range strings.SplitN(s, ".", 3) {
			result[i], _ = strconv.Atoi(part)
		}
		return result
	}
	av, bv := parse(a), parse(b)
	for i := range av {
		if av[i] < bv[i] {
			return -1
		}
		if av[i] > bv[i] {
			return 1
		}
	}
	return 0
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidateSQLiteVersion(t *testing.T) {
	for _, version := range []string{"3.51.3", "3.51.10", "4.0.0"} {
		if err := validateSQLiteVersion(version); err != nil {
			t.Errorf("validateSQLiteVersion(%q) = %v, want nil", version, err)
		}
	}
	for _, version := range []string{"3.51.2", "3.50.99", "2.99.99", "not-a-version"} {
		err := validateSQLiteVersion(version)
		if err == nil {
			t.Errorf("validateSQLiteVersion(%q) = nil, want error", version)
			continue
		}
		if !strings.Contains(err.Error(), "3.51.3") || !strings.Contains(err.Error(), "modernc.org/sqlite") {
			t.Errorf("validateSQLiteVersion(%q) error = %q, want minimum and recovery action", version, err)
		}
	}
}

func TestMigrationVersionRejectsMalformedFilename(t *testing.T) {
	for _, name := range []string{"invalid.sql", "002.sql", "zero_initial.sql"} {
		_, err := migrationVersion(name)
		if err == nil {
			t.Errorf("migrationVersion(%q) = nil error, want malformed filename error", name)
		}
		if err != nil && !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not identify %q", err, name)
		}
	}
	got, err := migrationVersion("002_more.sql")
	if err != nil || got != 2 {
		t.Fatalf("migrationVersion(valid) = %d, %v; want 2, nil", got, err)
	}
}

func TestOpenRejectsDatabaseFromNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version=10`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(context.Background(), path)
	if store != nil {
		_ = store.Close()
		t.Fatal("Open returned a store for schema version 10 with latest migration 9")
	}
	if err == nil || !strings.Contains(err.Error(), "version 10") || !strings.Contains(err.Error(), "version 9") ||
		!strings.Contains(strings.ToLower(err.Error()), "backup") || !strings.Contains(strings.ToLower(err.Error()), "downgrade") {
		t.Fatalf("Open future schema error = %v, want versions and backup/downgrade recovery guidance", err)
	}
}

func TestSeenMigrationTreatsExistingEventsAsPreviouslySeen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	db := createMigrationFixture(t, path, 7)
	if _, err := db.Exec(`
		INSERT INTO packages(id,name,type) VALUES('widget','Widget','formula');
		INSERT INTO update_events(id,package_id,kind,repository,commit_hash,event_time)
		VALUES('existing','widget','version','core','abc',1);`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	assertScalar(t, s.db, `SELECT seen FROM update_events WHERE id='existing'`, "1")
}

func TestOpenUpgradesRepresentativeVersionTwoAndThreeDatabases(t *testing.T) {
	for _, version := range []int{2, 3} {
		t.Run("v"+strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.db")
			db := createMigrationFixture(t, path, version)
			if _, err := db.Exec(`
				INSERT INTO repositories(id,path,head_commit) VALUES('core','/repo','abc');
				INSERT INTO sync_runs(id,repository_id,started_at,completed_at,error) VALUES(41,'core',10,20,'old failure');
				INSERT INTO packages(id,name,type) VALUES('widget','Widget','formula');
				INSERT INTO changelog_artifacts(id,url,content_hash,extracted_text) VALUES(7,'https://example.test/changes','hash','old text');
				INSERT INTO package_changelog_artifacts(package_id,artifact_id) VALUES('widget',7);`); err != nil {
				t.Fatal(err)
			}
			if version == 3 {
				if _, err := db.Exec(`
					INSERT INTO history_aliases(repository,alias,package_id,commit_hash) VALUES('core','old-widget','widget','abc');
					INSERT INTO history_diagnostics(repository,commit_hash,definition_path,message) VALUES('core','abc','Formula/w/widget.rb','old diagnostic');`); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			s, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			assertPragma(t, s.db, "user_version", "9")
			assertScalar(t, s.db, `SELECT show_formula || ':' || show_cask FROM preferences WHERE id=1`, "1:0")
			assertScalar(t, s.db, `SELECT error FROM sync_runs WHERE id=41`, "old failure")
			assertScalar(t, s.db, `SELECT count(*) FROM package_changelog_artifacts WHERE package_id='widget' AND artifact_id=7`, "1")
			if version == 3 {
				assertScalar(t, s.db, `SELECT package_id FROM history_aliases WHERE repository='core' AND alias='old-widget'`, "widget")
				assertScalar(t, s.db, `SELECT message FROM history_diagnostics WHERE repository='core' AND commit_hash='abc'`, "old diagnostic")
			}
			for _, column := range []string{"cursor", "event_count", "diagnostic_count", "last_success_at"} {
				assertScalar(t, s.db, `SELECT count(*) FROM pragma_table_info('sync_runs') WHERE name=?`, "1", column)
			}
			assertScalar(t, s.db, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='history_aliases'`, "1")
			assertScalar(t, s.db, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='history_aliases_commit'`, "1")
			assertScalar(t, s.db, `SELECT count(*) FROM pragma_foreign_key_list('sync_runs') WHERE "table"='repositories' AND "from"='repository_id' AND on_delete='CASCADE'`, "1")
		})
	}
}

func createMigrationFixture(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	for migration := 1; migration <= version; migration++ {
		matches, err := migrationFiles.ReadDir("schema")
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range matches {
			got, err := migrationVersion(entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			if got != migration {
				continue
			}
			body, err := migrationFiles.ReadFile("schema/" + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(string(body)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`PRAGMA user_version=` + strconv.Itoa(migration)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func assertScalar(t *testing.T, db *sql.DB, query, want string, args ...any) {
	t.Helper()
	var got string
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("scalar query %q = %q, want %q", query, got, want)
	}
}

func TestWriteRejectsNilCallback(t *testing.T) {
	s := openTestStore(t)
	err := s.Write(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("Write(nil) = %v, want descriptive error", err)
	}
}

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
	assertPragma(t, store.db, "user_version", "9")

	wantTables := []string{
		"changelog_artifacts", "changelog_artifacts_fts", "changelog_attempts",
		"changelog_sections", "document_attempts", "document_sections", "history_aliases", "history_diagnostics", "installed_packages", "package_aliases", "package_changelog_artifacts",
		"package_document_links", "package_documents", "package_documents_fts", "package_info", "package_repositories", "package_repository_tags", "packages",
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
	assertScalar(t, store.db, `SELECT count(*) FROM pragma_table_info('update_events') WHERE name='seen'`, "1")
}

func TestSyncRunRetainsCountsCursorSuccessAndBoundedError(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	started := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	if err := s.SyncStarted(ctx, "core", started); err != nil {
		t.Fatal(err)
	}
	longError := errors.New(strings.Repeat("x", 2000))
	if err := s.SyncFinished(ctx, "core", started.Add(time.Minute), SyncResult{Cursor: "abc", Events: 4, Diagnostics: 2}, longError); err != nil {
		t.Fatal(err)
	}
	status, ok, err := s.SyncStatus(ctx, "core")
	if err != nil || !ok {
		t.Fatalf("SyncStatus = %+v, %v, %v", status, ok, err)
	}
	if status.Cursor != "abc" || status.Events != 4 || status.Diagnostics != 2 || len(status.Error) != 1024 || !status.LastSuccess.IsZero() {
		t.Fatalf("failure status = %+v", status)
	}
	if err := s.SyncStarted(ctx, "core", started.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFinished(ctx, "core", started.Add(3*time.Minute), SyncResult{Cursor: "def", Events: 5}, nil); err != nil {
		t.Fatal(err)
	}
	status, _, err = s.SyncStatus(ctx, "core")
	if err != nil || status.Cursor != "def" || status.Events != 5 || !status.LastSuccess.Equal(started.Add(3*time.Minute)) || status.Error != "" {
		t.Fatalf("success status = %+v, %v", status, err)
	}
}

func TestSyncFinishedRejectsMissingRun(t *testing.T) {
	s := openTestStore(t)
	err := s.SyncFinished(context.Background(), "missing", time.Now(), SyncResult{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no active sync run") {
		t.Fatalf("SyncFinished missing run error = %v", err)
	}
}

func TestSyncFinishedRejectsAlreadyCompletedRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.SyncStarted(ctx, "core", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFinished(ctx, "core", time.Now(), SyncResult{}, nil); err != nil {
		t.Fatal(err)
	}
	err := s.SyncFinished(ctx, "core", time.Now(), SyncResult{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no active sync run") {
		t.Fatalf("second SyncFinished error = %v", err)
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

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"cicerone/internal/domain"
)

func TestLoadChangelogReturnsCachedSectionForEventVersion(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	event := domain.UpdateEvent{ID: "event", PackageID: "fixture", Name: "fixture", Type: domain.PackageFormula, Kind: domain.EventVersion, NewVersion: "2.0", Repository: "core", Commit: "abc", Time: time.Now()}
	if err := s.UpsertEvents(ctx, []domain.UpdateEvent{event}); err != nil {
		t.Fatal(err)
	}
	artifact, err := s.SaveChangelogArtifact(ctx, "fixture", ChangelogArtifact{URL: "https://example.test/changes", Hash: "hash", MediaType: "text/html", Raw: []byte("<p>changes</p>"), Extracted: []byte("changes")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveChangelogSection(ctx, ChangelogSection{ArtifactID: artifact.ID, Version: "2.0", Body: "linked notes", Confidence: 1, SourceURL: artifact.URL}); err != nil {
		t.Fatal(err)
	}
	sections, err := s.LoadChangelog(ctx, event.PackageID, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].SourceURL != artifact.URL || sections[0].Body != "linked notes" {
		t.Fatalf("LoadChangelog = %#v", sections)
	}
}

func TestMigrationUpgradesVersionOneChangelogData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`PRAGMA foreign_keys=ON;
		CREATE TABLE packages(id TEXT PRIMARY KEY,name TEXT NOT NULL,type TEXT NOT NULL);
		CREATE TABLE update_events(id TEXT PRIMARY KEY,package_id TEXT NOT NULL REFERENCES packages(id),kind TEXT NOT NULL,repository TEXT NOT NULL,commit_hash TEXT NOT NULL,event_time INTEGER NOT NULL);
		CREATE TABLE changelog_artifacts(id INTEGER PRIMARY KEY,package_id TEXT REFERENCES packages(id),url TEXT NOT NULL UNIQUE,media_type TEXT NOT NULL DEFAULT '',etag TEXT NOT NULL DEFAULT '',last_modified TEXT NOT NULL DEFAULT '',content_hash TEXT NOT NULL DEFAULT '',discovery_parent TEXT NOT NULL DEFAULT '',fetched_at INTEGER NOT NULL DEFAULT 0,raw_content TEXT NOT NULL DEFAULT '',extracted_text TEXT NOT NULL DEFAULT '',extraction_status TEXT NOT NULL DEFAULT '');
		CREATE TABLE changelog_sections(id INTEGER PRIMARY KEY,artifact_id INTEGER NOT NULL REFERENCES changelog_artifacts(id) ON DELETE CASCADE,version TEXT NOT NULL DEFAULT '',content TEXT NOT NULL DEFAULT '');
		CREATE TABLE changelog_attempts(id INTEGER PRIMARY KEY,artifact_id INTEGER REFERENCES changelog_artifacts(id) ON DELETE CASCADE,attempted_at INTEGER NOT NULL,status TEXT NOT NULL,error TEXT NOT NULL DEFAULT '');
		CREATE TABLE preferences(id INTEGER PRIMARY KEY,horizon_seconds INTEGER NOT NULL,show_version INTEGER NOT NULL,show_revision INTEGER NOT NULL,show_metadata INTEGER NOT NULL,show_formula INTEGER NOT NULL,show_cask INTEGER NOT NULL,query TEXT NOT NULL,roll_up INTEGER NOT NULL);
		INSERT INTO preferences VALUES(1,2592000,1,0,0,1,1,'',1);
		INSERT INTO packages VALUES('widget','Widget','formula');
		INSERT INTO changelog_artifacts(id,package_id,url,content_hash,raw_content,extracted_text) VALUES(7,'widget','https://example.test/CHANGELOG','oldhash','oldraw','oldtext');
		INSERT INTO changelog_sections(id,artifact_id,version,content) VALUES(8,7,'1.0.0','old section');
		INSERT INTO changelog_attempts(id,artifact_id,attempted_at,status,error) VALUES(9,7,10,'failed','old failure');
		PRAGMA user_version=1;`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 11 {
		t.Fatalf("user_version=%d,want 11", version)
	}
	artifacts, err := s.ChangelogArtifacts(ctx, "widget")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].ID != "7" {
		t.Fatalf("upgraded artifacts=%#v", artifacts)
	}
	sections, err := s.ChangelogSections(ctx, "7")
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].Body != "old section" {
		t.Fatalf("upgraded sections=%#v", sections)
	}
}

func TestChangelogArtifactCanBelongToMultiplePackages(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	for _, id := range []string{"one", "two"} {
		if err := s.UpsertChangelogPackage(ctx, id, id, "formula"); err != nil {
			t.Fatal(err)
		}
	}
	a := ChangelogArtifact{URL: "https://example.test/shared", Hash: "same", Raw: []byte("body"), Extracted: []byte("body")}
	first, err := s.SaveChangelogArtifact(ctx, "one", a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SaveChangelogArtifact(ctx, "two", a)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("shared artifact IDs=%q,%q", first.ID, second.ID)
	}
	for _, id := range []string{"one", "two"} {
		got, err := s.ChangelogArtifacts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != first.ID {
			t.Fatalf("%s artifacts=%#v", id, got)
		}
	}
}

func TestChangelogArtifactPersistenceDeduplicatesAndPreservesSuccessfulContent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.UpsertChangelogPackage(ctx, "widget", "Widget", "formula"); err != nil {
		t.Fatal(err)
	}
	want := ChangelogArtifact{URL: "https://example.test/CHANGELOG.md", MediaType: "text/markdown", Hash: "abc", Raw: []byte("raw"), Extracted: []byte("extracted"), FetchedAt: time.Unix(123, 0).UTC()}
	first, err := s.SaveChangelogArtifact(ctx, "widget", want)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SaveChangelogArtifact(ctx, "widget", want)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("deduplicated IDs = %q, %q", first.ID, second.ID)
	}
	if err := s.SaveChangelogSection(ctx, ChangelogSection{ArtifactID: first.ID, Version: "1.2.3", Body: "release body", Confidence: .9, SourceURL: want.URL}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChangelogFailure(ctx, want.URL, time.Unix(200, 0).UTC(), errors.New("network down")); err != nil {
		t.Fatal(err)
	}

	artifacts, err := s.ChangelogArtifacts(ctx, "widget")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || string(artifacts[0].Raw) != "raw" || string(artifacts[0].Extracted) != "extracted" {
		t.Fatalf("artifacts after failure = %#v", artifacts)
	}
	sections, err := s.ChangelogSections(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].Body != "release body" {
		t.Fatalf("sections = %#v", sections)
	}
}

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestVersionFourChangelogMigratesToPackageDocument(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v4.db")
	db := createMigrationFixture(t, path, 4)
	if _, err := db.Exec(`
		INSERT INTO packages(id,name,type) VALUES('widget','Widget','formula');
		INSERT INTO changelog_artifacts(id,url,media_type,etag,last_modified,content_hash,fetched_at,raw_content,extracted_text,extraction_status)
		VALUES(7,'https://example.test/CHANGELOG.md','text/markdown','old-etag','old-date','old-hash',123,'old raw','old text','ok');
		INSERT INTO package_changelog_artifacts(package_id,artifact_id) VALUES('widget',7);
		INSERT INTO changelog_sections(id,artifact_id,version,content,confidence,source_url)
		VALUES(8,7,'1.2.3','old section',0.9,'https://example.test/CHANGELOG.md');
		INSERT INTO changelog_attempts(id,artifact_id,url,attempted_at,status,error)
		VALUES(9,7,'https://example.test/CHANGELOG.md',124,'failed','old failure');`); err != nil {
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

	documents, err := s.PackageDocuments(ctx, "widget", DocumentChangelog)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 {
		t.Fatalf("PackageDocuments count = %d, want 1", len(documents))
	}
	document := documents[0]
	if document.ID != "7" || document.Kind != DocumentChangelog || document.URL != "https://example.test/CHANGELOG.md" ||
		string(document.Raw) != "old raw" || string(document.Extracted) != "old text" {
		t.Fatalf("migrated document = %#v", document)
	}
	sections, err := s.DocumentSections(ctx, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].Version != "1.2.3" || sections[0].Body != "old section" || sections[0].Confidence != 0.9 {
		t.Fatalf("migrated sections = %#v", sections)
	}
	if got := scalar(t, s.db, `SELECT count(*) FROM document_attempts WHERE canonical_url=? AND status='failed' AND error='old failure'`, document.URL); got != "1" {
		t.Fatalf("migrated attempts = %s, want 1", got)
	}
}

func scalar(t *testing.T, db scalarQuerier, query string, args ...any) string {
	t.Helper()
	var value string
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

type scalarQuerier interface {
	QueryRow(string, ...any) *sql.Row
}

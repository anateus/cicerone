package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

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

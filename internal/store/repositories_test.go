package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPackageRepositoryRoundTripAndUpdate(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertChangelogPackage(ctx, "pter", "pter", "formula"); err != nil {
		t.Fatal(err)
	}
	first := PackageRepository{PackageID: "pter", URL: "https://codeberg.org/old/pter", SourceURL: "https://example.test", Confidence: .8, DiscoveredAt: time.Unix(1, 0)}
	if err := s.SavePackageRepository(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.URL = "https://codeberg.org/pter/pter"
	second.Confidence = 1
	second.DiscoveredAt = time.Unix(2, 0)
	if err := s.SavePackageRepository(ctx, second); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.PackageRepository(ctx, "pter")
	if err != nil || !ok {
		t.Fatalf("PackageRepository = %#v, %v, %v", got, ok, err)
	}
	if got.URL != second.URL || got.SourceURL != second.SourceURL || got.Confidence != 1 || !got.DiscoveredAt.Equal(second.DiscoveredAt) {
		t.Fatalf("PackageRepository = %#v, want %#v", got, second)
	}
}

func TestPackageRepositoryTagsRoundTripIncludingEmptySet(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertChangelogPackage(ctx, "widget", "widget", "formula"); err != nil {
		t.Fatal(err)
	}

	first := PackageRepositoryTags{
		PackageID: "widget", Tags: []string{"v1.0.0", "v2.0.0"}, FetchedAt: time.Unix(1, 0).UTC(),
	}
	if err := s.SavePackageRepositoryTags(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.PackageRepositoryTags(ctx, "widget")
	if err != nil || !ok || !reflect.DeepEqual(got, first) {
		t.Fatalf("PackageRepositoryTags = %#v, %v, %v; want %#v, true, nil", got, ok, err, first)
	}

	second := PackageRepositoryTags{PackageID: "widget", Tags: []string{}, FetchedAt: time.Unix(2, 0).UTC()}
	if err := s.SavePackageRepositoryTags(ctx, second); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.PackageRepositoryTags(ctx, "widget")
	if err != nil || !ok || len(got.Tags) != 0 || !got.FetchedAt.Equal(second.FetchedAt) {
		t.Fatalf("empty PackageRepositoryTags = %#v, %v, %v", got, ok, err)
	}
}

package upstream

import (
	"context"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"cicerone/internal/store"
)

func TestLocatorDiscoversAndCachesHomepageRepository(t *testing.T) {
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	if err := cache.UpsertChangelogPackage(ctx, "pter", "pter", "formula"); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	final, _ := url.Parse("https://vonshednob.cc/pter/")
	locator := &Locator{
		Store: cache,
		Now:   func() time.Time { return time.Unix(5, 0).UTC() },
		Fetch: func(context.Context, string) (FetchedPage, error) {
			fetches++
			return FetchedPage{FinalURL: final, MediaType: "text/html", Body: []byte(
				`<li>Repository: <a href="https://codeberg.org/pter/pter">repo</a></li>`,
			)}, nil
		},
	}
	for range 2 {
		got, err := locator.Resolve(ctx, "pter", "pter", "https://vonshednob.cc/pter/")
		if err != nil || got != "https://codeberg.org/pter/pter" {
			t.Fatalf("Resolve = %q, %v", got, err)
		}
	}
	if fetches != 1 {
		t.Fatalf("homepage fetches = %d, want 1", fetches)
	}
}

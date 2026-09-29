//go:build homebrew_smoke && darwin

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/gitrepo"
	"github.com/anateus/cicerone/internal/homebrew"
	"github.com/anateus/cicerone/internal/store"
)

// This test reads only Homebrew metadata and Cicerone's existing local mirror.
func TestRealPomodoroCatalogHydrationPromotesCachedCaskHistory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "Library", "Caches", "cicerone", "homebrew-cask.git")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("Cicerone cask mirror is unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	destination, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	brew := homebrew.NewClient(execx.NewRunner())
	data := searchableFeedData{Store: destination, catalog: brew}
	filter := domain.FeedFilter{Now: time.Now(), Horizon: 30 * 24 * time.Hour,
		Query: "pomodoro", Search: domain.SearchDescriptions,
		Kinds: map[domain.EventKind]bool{domain.EventVersion: true},
		Types: map[domain.PackageType]bool{domain.PackageCask: true}}
	before, err := data.QueryFeed(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	foundCatalog := false
	for _, group := range before {
		if group.Events[0].PackageID == "pomatez" && group.Events[0].Kind == domain.EventCatalog {
			foundCatalog = true
		}
	}
	if !foundCatalog {
		t.Fatal("pomatez was not present as an initially thin catalog result")
	}
	repository := gitrepo.New(gitrepo.Source{Name: "homebrew-cask", Kind: "cask", Path: path}, execx.NewRunner())
	details := &packageDetailLoader{store: destination, brew: brew}
	hydrator := catalogHydrator{details: details, store: destination,
		repository: func(context.Context, string) (gitrepo.Repository, error) { return repository, nil }}
	if err := hydrator.HydrateCatalog(ctx, "pomatez"); err != nil {
		t.Fatal(err)
	}
	after, err := data.QueryFeed(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range after {
		e := group.Events[0]
		if e.PackageID == "pomatez" {
			if e.Kind != domain.EventVersion || e.NewVersion == "" || e.Repository != "homebrew-cask" {
				t.Fatalf("pomatez did not promote to indexed update: %#v", e)
			}
			t.Logf("pomatez: catalog row promoted to %s %s event from %s (%s)", e.Kind, e.NewVersion, e.Repository, e.Time.Format(time.DateOnly))
			return
		}
	}
	t.Fatal("pomatez disappeared after targeted hydration")
}

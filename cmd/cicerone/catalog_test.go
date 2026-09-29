package main

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/gitrepo"
	"github.com/anateus/cicerone/internal/homebrew"
	"github.com/anateus/cicerone/internal/store"
	"github.com/anateus/cicerone/internal/testutil"
	"github.com/anateus/cicerone/internal/tui"
)

func TestCatalogHydratorKeepsThirdPartyTapMetadataWithoutInventingHistory(t *testing.T) {
	ctx := context.Background()
	destination, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	match := domain.CatalogPackage{ID: "tomobar", Type: domain.PackageCask, Description: "Pomodoro timer"}
	if err := destination.UpsertCatalogPackages(ctx, []domain.CatalogPackage{match}); err != nil {
		t.Fatal(err)
	}
	if err := destination.SavePackageInfo(ctx, store.PackageInfoRecord{PackageID: "tomobar", FetchedAt: time.Now(),
		Raw: []byte(`{}`), Normalized: []byte(`{"FullName":"tomobar","Description":"Old metadata"}`)}); err != nil {
		t.Fatal(err)
	}
	runner := &testutil.Runner{RunResult: execx.Result{Stdout: []byte(`{
		"formulae":[],"casks":[{"token":"tomobar","full_token":"artemyurov/tomobar/tomobar",
		"tap":"artemyurov/tomobar","ruby_source_path":"Casks/tomobar.rb",
		"name":["TomoBar"],"desc":"Pomodoro timer for macOS menu bar",
		"homepage":"https://example.test","version":"4.1.3"}]
	}`)}}
	lookups := 0
	refreshed := false
	hydrator := catalogHydrator{
		details: &packageDetailLoader{store: destination, brew: homebrew.NewClient(runner), send: func(msg tea.Msg) {
			if _, ok := msg.(tui.DatasetChanged); ok {
				refreshed = true
			}
		}},
		store: destination, repository: func(context.Context, string) (gitrepo.Repository, error) {
			lookups++
			return gitrepo.Repository{}, nil
		},
	}
	if err := hydrator.HydrateCatalog(ctx, "tomobar"); err != nil {
		t.Fatal(err)
	}
	if !refreshed || lookups != 0 || len(runner.RunCalls) != 1 {
		t.Fatalf("refresh=%v repo lookups=%d brew calls=%v", refreshed, lookups, runner.RunCalls)
	}
	groups, err := destination.QueryFeed(ctx, domain.FeedFilter{Query: "pomodoro", Search: domain.SearchDescriptions,
		Types: map[domain.PackageType]bool{domain.PackageCask: true}, CatalogPackages: []domain.CatalogPackage{match}})
	if err != nil || len(groups) != 1 || groups[0].Events[0].Kind != domain.EventCatalog ||
		groups[0].Events[0].Name != "TomoBar" ||
		groups[0].Events[0].CatalogVersion != "4.1.3" ||
		groups[0].Events[0].CatalogDescription != "Pomodoro timer for macOS menu bar" {
		t.Fatalf("hydrated third-party catalog=%#v err=%v", groups, err)
	}
}

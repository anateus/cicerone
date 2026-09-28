//go:build homebrew_smoke && darwin

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/homebrew"
	"github.com/anateus/cicerone/internal/store"
)

func TestRealHomebrewSearchDisplaysUnindexedCatalogPackages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	destination, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	runner := execx.NewRunner()
	data := searchableFeedData{Store: destination, catalog: homebrew.NewClient(runner)}
	for _, test := range []struct {
		query string
		scope domain.SearchScope
		want  domain.PackageID
	}{
		{"ripgrep", domain.SearchNames, "ripgrep"},
		{"solver", domain.SearchDescriptions, "cbc"},
	} {
		filter := domain.FeedFilter{Query: test.query, Search: test.scope, Now: time.Now(),
			Horizon: 30 * 24 * time.Hour, Kinds: map[domain.EventKind]bool{domain.EventVersion: true}}
		baseline, err := destination.QueryFeed(ctx, filter)
		if err != nil || len(baseline) != 0 {
			t.Fatalf("empty-history baseline: groups=%d err=%v", len(baseline), err)
		}
		want := make(map[domain.PackageID]bool)
		for _, kind := range []string{"--formula", "--cask"} {
			result, searchErr := runner.Run(ctx, "brew", "search", kind, test.query)
			if searchErr != nil && len(result.Stdout) == 0 {
				continue // Homebrew exits 1 for an empty package type.
			}
			for line := range strings.Lines(string(result.Stdout)) {
				name := strings.TrimSpace(line)
				if name != "" && !strings.ContainsAny(name, " \t") && !strings.HasPrefix(name, "==>") {
					want[domain.PackageID(name)] = true
				}
			}
		}
		if test.scope == domain.SearchDescriptions {
			result, searchErr := runner.Run(ctx, "brew", "search", "--desc", test.query)
			if searchErr != nil {
				t.Fatal(searchErr)
			}
			for line := range strings.Lines(string(result.Stdout)) {
				name, _, ok := strings.Cut(strings.TrimSpace(line), ":")
				if ok && name != "" {
					want[domain.PackageID(name)] = true
				}
			}
		}
		groups, err := data.QueryFeed(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[domain.PackageID]bool)
		for _, group := range groups {
			if group.Events[0].Kind != domain.EventCatalog {
				t.Fatalf("unexpected history event in empty store: %#v", group)
			}
			got[group.Events[0].PackageID] = true
		}
		if !got[test.want] || len(want) == 0 || len(got) < len(want) {
			t.Fatalf("%s search %q: catalog displayed %d of %d Homebrew matches (known hit %q: %v)",
				test.scope, test.query, len(got), len(want), test.want, got[test.want])
		}
		for id := range want {
			if !got[id] {
				t.Fatalf("%s search %q missing Homebrew match %q", test.scope, test.query, id)
			}
		}
		t.Logf("%s search %q: indexed baseline %d, Homebrew %d, Cicerone %d", test.scope,
			test.query, len(baseline), len(want), len(got))
	}
}

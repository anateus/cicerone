//go:build homebrew_smoke && darwin

package main

import (
	"context"
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
	data := searchableFeedData{Store: destination, catalog: homebrew.NewClient(execx.NewRunner())}
	for _, test := range []struct {
		query string
		scope domain.SearchScope
		want  domain.PackageID
	}{
		{"ripgrep", domain.SearchNames, "ripgrep"},
		{"solver", domain.SearchDescriptions, "cbc"},
	} {
		groups, err := data.QueryFeed(ctx, domain.FeedFilter{Query: test.query, Search: test.scope})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, group := range groups {
			if group.Events[0].PackageID == test.want && group.Events[0].Kind == domain.EventCatalog {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s search for %q did not return catalog package %q", test.scope, test.query, test.want)
		}
	}
}

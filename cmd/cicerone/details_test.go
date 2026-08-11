package main

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/changelog"
	"cicerone/internal/domain"
	"cicerone/internal/download"
	"cicerone/internal/store"
	"cicerone/internal/tui"
)

type repositoryMetadataResolver struct {
	tags []string
	url  string
}

func (*repositoryMetadataResolver) Resolve(context.Context, changelog.PackageRef, string) (changelog.Section, error) {
	return changelog.Section{}, nil
}

func (r *repositoryMetadataResolver) RepositoryMetadataTags(_ context.Context, repositoryURL string) ([]string, error) {
	r.url = repositoryURL
	return append([]string(nil), r.tags...), nil
}

func TestPackageDetailLoaderFetchesCachesAndPublishesRepositoryTags(t *testing.T) {
	ctx := context.Background()
	cache, err := store.Open(ctx, filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	if err := cache.UpsertChangelogPackage(ctx, "widget", "widget", "formula"); err != nil {
		t.Fatal(err)
	}
	queue := download.NewQueue(download.Options{Context: ctx, Workers: 1, HostInterval: -1})
	t.Cleanup(queue.Close)
	resolver := &repositoryMetadataResolver{tags: []string{"terminal", "Go", "Shell"}}
	messages := make(chan tea.Msg, 4)
	loader := &packageDetailLoader{
		store: cache, queue: queue, changelogs: changelogLoader{resolver: resolver},
		send: func(message tea.Msg) { messages <- message },
	}

	loader.enqueueRepositoryTags(ctx, "widget", "https://github.com/acme/widget")

	loadingStarted, loadingFinished, tagsLoaded := false, false, false
	deadline := time.After(2 * time.Second)
	for !loadingFinished || !tagsLoaded {
		select {
		case raw := <-messages:
			switch message := raw.(type) {
			case tui.DetailFieldLoading:
				if message.PackageID == domain.PackageID("widget") && message.Field == tui.DetailRepositoryTags {
					if message.Loading {
						loadingStarted = true
					} else {
						loadingFinished = true
					}
				}
			case tui.RepositoryTagsLoaded:
				if message.PackageID != domain.PackageID("widget") ||
					!reflect.DeepEqual(message.Record.Tags, []string{"terminal", "Go", "Shell"}) {
					t.Fatalf("repository tags message = %#v", raw)
				}
				tagsLoaded = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for repository tags")
		}
	}
	if !loadingStarted {
		t.Fatal("repository tags load did not publish its start")
	}
	record, found, err := loader.LoadCachedRepositoryTags(ctx, "widget")
	if err != nil || !found || !reflect.DeepEqual(record.Tags, []string{"terminal", "Go", "Shell"}) {
		t.Fatalf("cached repository tags = %#v, %v, %v", record, found, err)
	}
	if resolver.url != "https://github.com/acme/widget" {
		t.Fatalf("repository metadata URL = %q", resolver.url)
	}
}

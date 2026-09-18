package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/anateus/cicerone/internal/changelog"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/download"
	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/homebrew"
	"github.com/anateus/cicerone/internal/store"
	"github.com/anateus/cicerone/internal/tui"
)

type repositoryMetadataResolver struct {
	tags []string
	url  string
}

type blockingInfoRunner struct {
	started chan struct{}
	release chan struct{}
	calls   int
}

func (r *blockingInfoRunner) Run(ctx context.Context, _ string, _ ...string) (execx.Result, error) {
	r.calls++
	close(r.started)
	select {
	case <-r.release:
		return execx.Result{Stdout: []byte(`{"formulae":[{"name":"widget"}],"casks":[]}`)}, nil
	case <-ctx.Done():
		return execx.Result{}, ctx.Err()
	}
}

func (*blockingInfoRunner) Stream(context.Context, string, ...string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func TestPackageDetailLoaderCoalescesPackageInfoAndCancelsWaiters(t *testing.T) {
	cache, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	if err := cache.UpsertChangelogPackage(context.Background(), "widget", "widget", "formula"); err != nil {
		t.Fatal(err)
	}

	runner := &blockingInfoRunner{started: make(chan struct{}), release: make(chan struct{})}
	loader := &packageDetailLoader{store: cache, brew: homebrew.NewClient(runner)}
	leaderDone := make(chan error, 1)
	go func() {
		_, err := loader.refreshPackageInfo(context.Background(), "widget")
		leaderDone <- err
	}()
	<-runner.started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = loader.refreshPackageInfo(ctx, "widget")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting request error = %v, want context cancellation", err)
	}

	close(runner.release)
	if err := <-leaderDone; err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatalf("brew info calls = %d, want 1", runner.calls)
	}
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

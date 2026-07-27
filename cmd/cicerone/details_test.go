package main

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/download"
	"cicerone/internal/execx"
	"cicerone/internal/store"
	"cicerone/internal/tui"
)

type repositoryTagRunner struct {
	result execx.Result
	name   string
	args   []string
}

func (r *repositoryTagRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return r.result, nil
}

func (*repositoryTagRunner) Stream(context.Context, string, ...string) (io.ReadCloser, error) {
	panic("unused")
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
	runner := &repositoryTagRunner{result: execx.Result{Stdout: []byte(
		"aaa\trefs/tags/v2.0.0\nbbb\trefs/tags/v1.0.0\n",
	)}}
	messages := make(chan tea.Msg, 1)
	loader := &packageDetailLoader{
		store: cache, queue: queue, runner: runner, send: func(message tea.Msg) { messages <- message },
	}

	loader.enqueueRepositoryTags(ctx, "widget", "https://bitbucket.org/acme/widget")

	select {
	case raw := <-messages:
		message, ok := raw.(tui.RepositoryTagsLoaded)
		if !ok || message.PackageID != domain.PackageID("widget") ||
			!reflect.DeepEqual(message.Record.Tags, []string{"v1.0.0", "v2.0.0"}) {
			t.Fatalf("repository tags message = %#v", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for repository tags")
	}
	record, found, err := loader.LoadCachedRepositoryTags(ctx, "widget")
	if err != nil || !found || !reflect.DeepEqual(record.Tags, []string{"v1.0.0", "v2.0.0"}) {
		t.Fatalf("cached repository tags = %#v, %v, %v", record, found, err)
	}
	if runner.name != "git" || !reflect.DeepEqual(runner.args, []string{
		"ls-remote", "--tags", "--refs", "https://bitbucket.org/acme/widget",
	}) {
		t.Fatalf("runner call = %q %v", runner.name, runner.args)
	}
}

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/app"
	"cicerone/internal/changelog"
	"cicerone/internal/download"
	"cicerone/internal/execx"
	"cicerone/internal/gitrepo"
	"cicerone/internal/history"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
	"cicerone/internal/syncer"
	"cicerone/internal/tui"
	"cicerone/internal/upstream"
)

var newExecRunner = execx.NewRunner
var runtimePaths = app.DefaultPaths

type runtimeServices struct {
	ctx         context.Context
	store       *store.Store
	brew        *homebrew.Client
	coordinator *syncer.Coordinator
	changelogs  changelogLoader
	details     *packageDetailLoader
	downloads   *download.Queue
	cancel      context.CancelFunc
	closeOnce   sync.Once
	closeErr    error
}

func newRuntime(home string, notify func(tea.Msg)) (*runtimeServices, error) {
	paths := runtimePaths(home)
	for _, dir := range []string{paths.DataDir, paths.CacheDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	destination, err := openStore(ctx, paths.DBPath)
	if err != nil {
		cancel()
		return nil, recoveryError(paths.DBPath, err)
	}

	runner := newExecRunner()
	brew := homebrew.NewClient(runner)
	var repositoriesMu sync.RWMutex
	repositories := make(map[string]gitrepo.Repository)
	loadSources := func(loadCtx context.Context) ([]syncer.Source, error) {
		prefix := ""
		if result, prefixErr := runner.Run(loadCtx, "brew", "--prefix"); prefixErr == nil {
			prefix = strings.TrimSpace(string(result.Stdout))
		}
		discovered, discoverErr := gitrepo.Discover(loadCtx, prefix, paths.CacheDir, runner)
		if discoverErr != nil {
			return nil, discoverErr
		}
		sources := make([]syncer.Source, 0, len(discovered))
		for _, source := range discovered {
			repository := gitrepo.New(source, runner)
			repositoriesMu.Lock()
			repositories[source.Name] = repository
			repositoriesMu.Unlock()
			sources = append(sources, repositorySource{source: source, repository: repository, indexer: history.NewIndexer(repository, destination)})
		}
		return sources, nil
	}
	coordinator := syncer.New(syncer.Dependencies{
		Installed: brew, Store: syncStore{destination}, LoadSources: loadSources,
		InitialSince: time.Now().Add(-30 * 24 * time.Hour), Notify: func(msg tea.Msg) {
			if notify == nil {
				return
			}
			switch event := msg.(type) {
			case syncer.SyncStarted:
				notify(tui.Notify{Text: "Synchronizing " + event.Source + "…"})
			case syncer.SyncProgress:
				notify(tui.SyncProgress{Source: event.Source, Commits: event.Progress.Commits, Events: event.Progress.Events, Diagnostics: event.Progress.Diagnostics, Batches: event.Progress.Batches})
			case syncer.SyncCommitted:
				notify(tui.SyncDone{Source: event.Source})
				notify(tui.Notify{Text: fmt.Sprintf("%s synchronized · %d updates", event.Source, event.Result.Events)})
			case syncer.SyncFailed:
				notify(tui.SyncDone{Source: event.Source})
				notify(tui.Notify{Text: event.Source + " synchronization failed", Err: event.Err})
			}
			notify(msg)
		},
	})
	fetcher := &changelog.Fetcher{}
	progress := &detailProgressTracker{send: notify}
	downloads := download.NewQueue(download.Options{Context: ctx, OnProgress: progress.downloads})
	resolver := changelog.NewResolver(destination, nil, changelog.WithGitHubTokenRunner(newExecRunner()))
	resolver.Fetcher = fetcher
	resolver.Download = func(downloadCtx context.Context, rawURL string) (changelog.Fetched, error) {
		result, enqueueErr := downloads.Enqueue(download.Request{
			URL: rawURL, Profile: "document", Priority: download.Current, Context: downloadCtx,
			Fetch: func(fetchCtx context.Context) (any, error) { return fetcher.Fetch(fetchCtx, rawURL) },
		})
		if enqueueErr != nil {
			return changelog.Fetched{}, enqueueErr
		}
		completed := <-result
		if completed.Err != nil {
			return changelog.Fetched{}, completed.Err
		}
		fetched, ok := completed.Value.(changelog.Fetched)
		if !ok {
			return changelog.Fetched{}, fmt.Errorf("unexpected changelog download result")
		}
		return fetched, nil
	}
	locator := &upstream.Locator{Store: destination, Fetch: func(fetchCtx context.Context, rawURL string) (upstream.FetchedPage, error) {
		fetched, fetchErr := resolver.Download(fetchCtx, rawURL)
		if fetchErr != nil {
			return upstream.FetchedPage{}, fetchErr
		}
		return upstream.FetchedPage{FinalURL: fetched.FinalURL, MediaType: fetched.MediaType, Body: fetched.Body}, nil
	}}
	loader := changelogLoader{cache: destination, resolver: resolver, locator: locator, repository: func(loadCtx context.Context, name string) (gitrepo.Repository, error) {
		repositoriesMu.RLock()
		repository, ok := repositories[name]
		repositoriesMu.RUnlock()
		if !ok {
			if _, err := loadSources(loadCtx); err != nil {
				return gitrepo.Repository{}, err
			}
			repositoriesMu.RLock()
			repository, ok = repositories[name]
			repositoriesMu.RUnlock()
		}
		if !ok {
			return gitrepo.Repository{}, fmt.Errorf("repository %s is unavailable", name)
		}
		return repository, nil
	}}
	details := &packageDetailLoader{
		store: destination, brew: brew, changelogs: loader, queue: downloads, fetcher: fetcher,
		send: notify, progress: progress,
	}
	return &runtimeServices{ctx: ctx, store: destination, brew: brew, coordinator: coordinator, changelogs: loader, details: details, downloads: downloads, cancel: cancel}, nil
}

func (r *runtimeServices) Close() error {
	r.closeOnce.Do(func() {
		r.coordinator.Close()
		r.downloads.Close()
		r.cancel()
		r.closeErr = r.store.Close()
	})
	return r.closeErr
}

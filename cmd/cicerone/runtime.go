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
	"cicerone/internal/execx"
	"cicerone/internal/gitrepo"
	"cicerone/internal/history"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
	"cicerone/internal/syncer"
	"cicerone/internal/tui"
)

var newExecRunner = execx.NewRunner
var runtimePaths = app.DefaultPaths

type runtimeServices struct {
	ctx         context.Context
	store       *store.Store
	brew        *homebrew.Client
	coordinator *syncer.Coordinator
	changelogs  changelogLoader
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
			case syncer.SyncCommitted:
				notify(tui.Notify{Text: fmt.Sprintf("%s synchronized · %d updates", event.Source, event.Result.Events)})
			case syncer.SyncFailed:
				notify(tui.Notify{Text: event.Source + " synchronization failed", Err: event.Err})
			}
			notify(msg)
		},
	})
	resolver := changelog.NewResolver(destination, nil, changelog.WithGitHubTokenRunner(newExecRunner()))
	loader := changelogLoader{cache: destination, resolver: resolver, repository: func(loadCtx context.Context, name string) (gitrepo.Repository, error) {
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
	return &runtimeServices{ctx: ctx, store: destination, brew: brew, coordinator: coordinator, changelogs: loader, cancel: cancel}, nil
}

func (r *runtimeServices) Close() error {
	r.closeOnce.Do(func() {
		r.coordinator.Close()
		r.cancel()
		r.closeErr = r.store.Close()
	})
	return r.closeErr
}

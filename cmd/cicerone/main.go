package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/app"
	"cicerone/internal/execx"
	"cicerone/internal/gitrepo"
	"cicerone/internal/history"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
	"cicerone/internal/syncer"
	"cicerone/internal/tui"
)

type syncStore struct{ *store.Store }

func (s syncStore) SyncFinished(ctx context.Context, source string, at time.Time, result syncer.Result, err error) error {
	return s.Store.SyncFinished(ctx, source, at, store.SyncResult{Cursor: result.Cursor, Events: result.Events, Diagnostics: result.Diagnostics}, err)
}

type repositorySource struct {
	source     gitrepo.Source
	repository gitrepo.Repository
	indexer    *history.Indexer
}

func (s repositorySource) Name() string { return s.source.Name }
func (s repositorySource) Refresh(ctx context.Context) error {
	if err := s.repository.Ensure(ctx); err != nil {
		return err
	}
	if s.source.Owned {
		return s.repository.Fetch(ctx)
	}
	return nil
}
func (s repositorySource) Index(ctx context.Context, req syncer.Request) (syncer.Result, error) {
	result, err := s.indexer.Index(ctx, s.source, history.Request{Since: req.Since, Installed: req.Installed, Kinds: req.Kinds})
	return syncer.Result{Events: result.Events, Diagnostics: result.Diagnostics, Cursor: result.Head, Since: result.Since}, err
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	paths := app.DefaultPaths(home)
	for _, dir := range []string{paths.DataDir, paths.CacheDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	ctx, stopProcesses := context.WithCancel(context.Background())
	destination, err := store.Open(ctx, paths.DBPath)
	if err != nil {
		stopProcesses()
		return err
	}

	runner := execx.NewRunner()
	var program *tea.Program
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
			sources = append(sources, repositorySource{source: source, repository: repository, indexer: history.NewIndexer(repository, destination)})
		}
		return sources, nil
	}
	coordinator := syncer.New(syncer.Dependencies{Installed: homebrew.NewClient(runner), Store: syncStore{destination}, LoadSources: loadSources,
		InitialSince: time.Now().Add(-30 * 24 * time.Hour), Notify: func(msg tea.Msg) {
			if program != nil {
				switch event := msg.(type) {
				case syncer.SyncStarted:
					program.Send(tui.Notify{Text: "Synchronizing " + event.Source + "…"})
				case syncer.SyncCommitted:
					program.Send(tui.Notify{Text: fmt.Sprintf("%s synchronized · %d updates", event.Source, event.Result.Events)})
				case syncer.SyncFailed:
					program.Send(tui.Notify{Text: event.Source + " synchronization failed", Err: event.Err})
				}
				program.Send(msg)
			}
		}})
	model := tui.New(tui.Dependencies{Data: destination, Context: ctx, OnReady: func() tea.Msg { coordinator.Start(ctx); return nil }})
	program = tea.NewProgram(model)
	_, runErr := program.Run()
	coordinator.Close()
	stopProcesses()
	closeErr := destination.Close()
	if runErr != nil {
		return runErr
	}
	return closeErr
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

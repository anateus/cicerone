package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/app"
	"cicerone/internal/domain"
	"cicerone/internal/execx"
	"cicerone/internal/gitrepo"
	"cicerone/internal/history"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
	"cicerone/internal/syncer"
	"cicerone/internal/tui"
)

type syncStore struct{ *store.Store }

var openStore = openStorePreservingFailures

type installedReader interface {
	Installed(context.Context) ([]domain.InstalledPackage, error)
}

type installedWriter interface {
	SetInstalled(context.Context, []domain.InstalledPackage) error
}

type installedRefresher struct {
	client installedReader
	store  installedWriter
}

func (r installedRefresher) RefreshInstalled(ctx context.Context) error {
	packages, err := r.client.Installed(ctx)
	if err != nil {
		return err
	}
	return r.store.SetInstalled(ctx, packages)
}

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

func run() (runErr error) {
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
	destination, err := openStore(ctx, paths.DBPath)
	if err != nil {
		stopProcesses()
		return recoveryError(paths.DBPath, err)
	}
	defer func() {
		if closeErr := destination.Close(); runErr == nil {
			runErr = closeErr
		}
	}()
	defer stopProcesses()

	runner := execx.NewRunner()
	brew := homebrew.NewClient(runner)
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
	coordinator := syncer.New(syncer.Dependencies{Installed: brew, Store: syncStore{destination}, LoadSources: loadSources,
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
	defer coordinator.Close()
	dependencies := tuiDependencies(destination, ctx, func() tea.Msg { coordinator.Start(ctx); return nil }, brew,
		func(msg tea.Msg) {
			if program != nil {
				program.Send(msg)
			}
		})
	model := tui.New(dependencies)
	program = tea.NewProgram(model)
	_, runErr = program.Run()
	return runErr
}

func tuiDependencies(destination *store.Store, ctx context.Context, onReady tea.Cmd, brew *homebrew.Client, send func(tea.Msg)) tui.Dependencies {
	return tui.Dependencies{
		Data: destination, Changelog: destination, Context: ctx, OnReady: onReady,
		Actions: brew, Installed: installedRefresher{client: brew, store: destination}, Send: send,
	}
}

// openStorePreservingFailures runs SQLite's open and migrations against a
// sibling copy first. A corrupt database or failing migration therefore cannot
// alter the user's original bytes before recovery guidance is printed.
func openStorePreservingFailures(ctx context.Context, path string) (*store.Store, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store.Open(ctx, path)
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("database path is not a regular file")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".cicerone-open-check-*")
	if err != nil {
		return nil, fmt.Errorf("create database recovery check: %w", err)
	}
	temporaryPath := temporary.Name()
	_ = temporary.Close()
	cleanup := func() {
		_ = os.Remove(temporaryPath)
		_ = os.Remove(temporaryPath + "-wal")
		_ = os.Remove(temporaryPath + "-shm")
	}
	defer cleanup()
	if err := copyFile(path, temporaryPath, info.Mode().Perm()); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if sidecar, sidecarErr := os.Stat(path + suffix); sidecarErr == nil {
			if err := copyFile(path+suffix, temporaryPath+suffix, sidecar.Mode().Perm()); err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(sidecarErr) {
			return nil, sidecarErr
		}
	}
	checked, err := store.Open(ctx, temporaryPath)
	if err != nil {
		return nil, err
	}
	if err := checked.Close(); err != nil {
		return nil, err
	}
	return store.Open(ctx, path)
}

func copyFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func recoveryError(path string, cause error) error {
	quoted := shellQuote(path)
	backup := shellQuote(path + ".recovery-copy")
	return fmt.Errorf("open cache database %s: %w\n\nCicerone left the database in place. Before recovery, make a safe copy:\n  cp -p -- %s %s\nCheck the copy:\n  sqlite3 %s 'PRAGMA integrity_check;'\nExport readable data if possible:\n  sqlite3 %s .dump > cicerone-recovery.sql\nTo rebuild explicitly, preserve the original and restart Cicerone:\n  mv -- %s %s", path, cause, quoted, backup, backup, backup, quoted, shellQuote(path+".corrupt"))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

const helpText = `Cicerone — a cached Homebrew update feed

Usage: cicerone [--help]

Keys: j/k or arrows move · enter opens details · space expands · a installs/upgrades · esc closes

The feed shows 30 days of updates plus the newest matching event for every installed package.
Cached data is displayed before background Homebrew/Git refreshes begin.
Database: ~/Library/Application Support/cicerone/cicerone.db
Owned Git cache: ~/Library/Caches/cicerone
`

func execute(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, _ = io.WriteString(stdout, helpText)
		return 0
	}
	if len(args) != 0 {
		fmt.Fprintf(stderr, "unknown argument %q\n\n%s", args[0], helpText)
		return 2
	}
	if err := run(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

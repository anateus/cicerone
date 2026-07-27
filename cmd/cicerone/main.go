package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/changelog"
	"cicerone/internal/domain"
	"cicerone/internal/gitrepo"
	"cicerone/internal/history"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
	"cicerone/internal/syncer"
	"cicerone/internal/tui"
	"cicerone/internal/upstream"
)

type syncStore struct{ *store.Store }

var openStore = openStorePreservingFailures
var renameFile = os.Rename
var storeOpen = store.Open

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

type changelogResolver interface {
	Resolve(context.Context, changelog.PackageRef, string) (changelog.Section, error)
}

type changelogCache interface {
	LoadChangelog(context.Context, domain.PackageID, domain.EventID) ([]store.ChangelogSection, error)
	ChangelogTarget(context.Context, domain.PackageID, domain.EventID) (store.ChangelogTarget, error)
}

type changelogLoader struct {
	cache      changelogCache
	resolver   changelogResolver
	repository func(context.Context, string) (gitrepo.Repository, error)
	locator    *upstream.Locator
}

func (l changelogLoader) LoadChangelog(ctx context.Context, packageID domain.PackageID, eventID domain.EventID) ([]store.ChangelogSection, error) {
	cached, err := l.cache.LoadChangelog(ctx, packageID, eventID)
	if err != nil || len(cached) > 0 {
		return cached, err
	}
	target, err := l.cache.ChangelogTarget(ctx, packageID, eventID)
	if err != nil {
		return nil, err
	}
	repository, err := l.repository(ctx, target.Repository)
	if err != nil {
		return nil, err
	}
	body, err := repository.Blob(ctx, target.Commit, target.DefinitionPath)
	if err != nil {
		return nil, err
	}
	definition, _ := history.ParseDefinition(target.DefinitionPath, body)
	if definition == nil {
		return nil, fmt.Errorf("parse changelog metadata for %s", target.Name)
	}
	repositoryURL := githubRepositoryURL(definition.Homepage, definition.URL)
	if l.locator != nil {
		resolved, resolveErr := l.locator.Resolve(ctx, string(target.PackageID), target.Name, definition.Homepage, definition.URL)
		if resolveErr == nil {
			repositoryURL = resolved
		}
	}
	section, err := l.resolver.Resolve(ctx, changelog.PackageRef{Name: target.Name, FullName: string(target.PackageID), Homepage: definition.Homepage, RepositoryURL: repositoryURL, Type: target.Type}, target.Version)
	if err != nil {
		return nil, err
	}
	return []store.ChangelogSection{{ArtifactID: section.ArtifactID, Version: section.Version, Body: section.Body, Confidence: section.Confidence, SourceURL: section.SourceURL}}, nil
}

func (l changelogLoader) LoadCachedChangelog(ctx context.Context, packageID domain.PackageID, eventID domain.EventID) ([]store.ChangelogSection, error) {
	return l.cache.LoadChangelog(ctx, packageID, eventID)
}

func githubRepositoryURL(values ...string) string {
	for _, value := range values {
		if repositoryURL, ok := upstream.CanonicalRepository(value); ok {
			return repositoryURL
		}
		parsed, err := url.Parse(value)
		if err != nil {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		if strings.HasSuffix(host, ".github.io") {
			owner := strings.TrimSuffix(host, ".github.io")
			if owner == "" || strings.Contains(owner, ".") {
				continue
			}
			parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
			repo := owner + ".github.io"
			if len(parts) > 0 && parts[0] != "" {
				repo = parts[0]
			}
			return "https://github.com/" + owner + "/" + repo
		}
	}
	return ""
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
	result, err := s.indexer.Index(ctx, s.source, history.Request{Since: req.Since, Installed: req.Installed, Kinds: req.Kinds, Progress: func(progress history.Progress) {
		if req.Progress != nil {
			req.Progress(syncer.Progress{Commits: progress.Commits, Events: progress.Events, Diagnostics: progress.Diagnostics, Batches: progress.Batches})
		}
	}})
	return syncer.Result{Events: result.Events, Diagnostics: result.Diagnostics, Cursor: result.Head, Since: result.Since}, err
}

func run() (runErr error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var program *tea.Program
	runtime, err := newRuntime(home, func(msg tea.Msg) {
		if program != nil {
			program.Send(msg)
		}
	})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := runtime.Close(); runErr == nil {
			runErr = closeErr
		}
	}()
	dependencies := tuiDependencies(runtime.store, runtime.changelogs, runtime.ctx, func() tea.Msg { runtime.coordinator.Start(runtime.ctx); return nil }, runtime.brew,
		func(msg tea.Msg) {
			if program != nil {
				program.Send(msg)
			}
		})
	dependencies.PackageInfo = runtime.details
	dependencies.README = runtime.details
	dependencies.Tags = runtime.details
	model := tui.New(dependencies)
	program = tea.NewProgram(model)
	_, runErr = program.Run()
	return runErr
}

func tuiDependencies(destination *store.Store, changelogs tui.ChangelogSource, ctx context.Context, onReady tea.Cmd, brew *homebrew.Client, send func(tea.Msg)) tui.Dependencies {
	return tui.Dependencies{
		Data: destination, Changelog: changelogs, Context: ctx, OnReady: onReady,
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
			return storeOpen(ctx, path)
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
	promoted := false
	cleanup := func() {
		if !promoted {
			_ = os.Remove(temporaryPath)
		}
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
	checked, err := storeOpen(ctx, temporaryPath)
	if err != nil {
		return nil, err
	}
	if err := checked.Close(); err != nil {
		return nil, err
	}
	backupPath := temporaryPath + ".original"
	if err := copyFile(path, backupPath, info.Mode().Perm()); err != nil {
		return nil, err
	}
	backupPaths := []string{backupPath}
	for _, suffix := range []string{"-wal", "-shm"} {
		if sidecar, sidecarErr := os.Stat(path + suffix); sidecarErr == nil {
			if err := copyFile(path+suffix, backupPath+suffix, sidecar.Mode().Perm()); err != nil {
				return nil, err
			}
			backupPaths = append(backupPaths, backupPath+suffix)
		}
	}
	if err := renameFile(temporaryPath, path); err != nil {
		removeFiles(backupPaths)
		return nil, fmt.Errorf("promote migrated database: %w", err)
	}
	promoted = true
	// The promoted database was cleanly closed and contains any committed WAL
	// content from the copied snapshot. Old sidecars belong to the replaced inode.
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	opened, err := storeOpen(ctx, path)
	if err == nil {
		removeFiles(backupPaths)
		return opened, nil
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	if restoreErr := renameFile(backupPath, path); restoreErr != nil {
		return nil, fmt.Errorf("open promoted database: %v; restore original: %v; preserved backups: %s", err, restoreErr, strings.Join(existingFiles(backupPaths), ", "))
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, sidecarErr := os.Stat(backupPath + suffix); sidecarErr == nil {
			if restoreErr := renameFile(backupPath+suffix, path+suffix); restoreErr != nil {
				return nil, fmt.Errorf("open promoted database: %v; restore original %s: %v; preserved backups: %s", err, suffix, restoreErr, strings.Join(existingFiles(backupPaths), ", "))
			}
		}
	}
	return nil, err
}

func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func existingFiles(paths []string) []string {
	var existing []string
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		}
	}
	return existing
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

Usage: cicerone [--help] [--plain]

Keys: h/j/k/l or arrows navigate · enter opens details · space expands · a installs/upgrades · q/esc quit

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
	if len(args) == 1 && args[0] == "--plain" {
		return executePlain(stdout, stderr)
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

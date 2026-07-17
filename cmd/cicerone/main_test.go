package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/changelog"
	"cicerone/internal/domain"
	"cicerone/internal/execx"
	"cicerone/internal/gitrepo"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
	"cicerone/internal/testutil"
)

type fakeInstalledClient struct {
	packages []domain.InstalledPackage
	err      error
}

func (f fakeInstalledClient) Installed(context.Context) ([]domain.InstalledPackage, error) {
	return f.packages, f.err
}

type fakeInstalledStore struct {
	packages []domain.InstalledPackage
	err      error
}

type fakeChangelogResolver struct {
	ref     changelog.PackageRef
	version string
}

func (f *fakeChangelogResolver) Resolve(_ context.Context, ref changelog.PackageRef, version string) (changelog.Section, error) {
	f.ref, f.version = ref, version
	return changelog.Section{Version: version, Body: "resolved", SourceURL: "https://github.com/acme/fixture/releases/v2.0", Confidence: 1}, nil
}

func TestChangelogLoaderResolvesCacheMissFromDefinition(t *testing.T) {
	ctx := context.Background()
	destination, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	repo := testutil.NewGitRepo(t)
	commit := repo.Commit("Formula/f/fixture.rb", `class Fixture < Formula
  homepage "https://github.com/acme/fixture"
  url "https://github.com/acme/fixture/archive/v2.0.tar.gz"
  version "2.0"
end`, "fixture 2.0", time.Now())
	event := domain.UpdateEvent{ID: "event", PackageID: "fixture", Name: "fixture", Type: domain.PackageFormula, Kind: domain.EventVersion, NewVersion: "2.0", Repository: "homebrew-core", DefinitionPath: "Formula/f/fixture.rb", Commit: commit, Time: time.Now()}
	if err := destination.UpsertEvents(ctx, []domain.UpdateEvent{event}); err != nil {
		t.Fatal(err)
	}
	resolver := &fakeChangelogResolver{}
	loader := changelogLoader{cache: destination, resolver: resolver, repository: func(context.Context, string) (gitrepo.Repository, error) {
		return gitrepo.New(gitrepo.Source{Name: "homebrew-core", Path: repo.Path}, execx.NewRunner()), nil
	}}
	sections, err := loader.LoadChangelog(ctx, event.PackageID, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].Body != "resolved" {
		t.Fatalf("sections = %#v", sections)
	}
	if resolver.ref.RepositoryURL != "https://github.com/acme/fixture" || resolver.ref.Homepage != "https://github.com/acme/fixture" || resolver.version != "2.0" {
		t.Fatalf("resolver input = %#v, %q", resolver.ref, resolver.version)
	}
}

func (f *fakeInstalledStore) SetInstalled(_ context.Context, packages []domain.InstalledPackage) error {
	f.packages = append([]domain.InstalledPackage(nil), packages...)
	return f.err
}

func TestInstalledRefresherPersistsClientSnapshot(t *testing.T) {
	want := []domain.InstalledPackage{{PackageID: "jq", Name: "jq", Version: "1.7", Type: domain.PackageFormula}}
	destination := &fakeInstalledStore{}
	refresher := installedRefresher{client: fakeInstalledClient{packages: want}, store: destination}
	if err := refresher.RefreshInstalled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(destination.packages) != 1 || destination.packages[0] != want[0] {
		t.Fatalf("persisted packages = %#v, want %#v", destination.packages, want)
	}
}

func TestInstalledRefresherDoesNotReplaceSnapshotWhenReadFails(t *testing.T) {
	destination := &fakeInstalledStore{packages: []domain.InstalledPackage{{PackageID: "old"}}}
	refresher := installedRefresher{client: fakeInstalledClient{err: errors.New("offline")}, store: destination}
	if err := refresher.RefreshInstalled(context.Background()); err == nil {
		t.Fatal("RefreshInstalled error = nil, want offline error")
	}
	if got := destination.packages[0].PackageID; got != "old" {
		t.Fatalf("stored package = %q, want old snapshot", got)
	}
}

func TestHelpDocumentsKeysAndCacheBehavior(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := execute([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("execute --help = %d, stderr %q", code, stderr.String())
	}
	for _, text := range []string{"j/k", "30 days", "Library/Application Support/cicerone/cicerone.db", "cached"} {
		if !strings.Contains(stdout.String(), text) {
			t.Errorf("help missing %q:\n%s", text, stdout.String())
		}
	}
}

func TestRecoveryErrorIncludesDatabasePathAndSafeCommands(t *testing.T) {
	err := recoveryError("/tmp/cicerone db/cache.db", errors.New("database disk image is malformed"))
	for _, text := range []string{"/tmp/cicerone db/cache.db", "cp -p", "PRAGMA integrity_check", "sqlite3", "mv"} {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("recovery error missing %q: %v", text, err)
		}
	}
}

func TestCorruptDatabaseIsPreservedAndReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "Library", "Application Support", "cicerone", "cicerone.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("not a sqlite database\x00\xff")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := execute(nil, &stdout, &stderr); code == 0 {
		t.Fatalf("execute corrupt database = 0, stderr %q", stderr.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("database bytes changed: got %x, want %x", got, want)
	}
	for _, text := range []string{path, "cp -p", "PRAGMA integrity_check", "mv"} {
		if !strings.Contains(stderr.String(), text) {
			t.Errorf("stderr missing %q:\n%s", text, stderr.String())
		}
	}
}

func TestMigrationFailureIsPreservedAndReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "Library", "Application Support", "cicerone", "cicerone.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("existing database bytes")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := openStore
	openStore = func(context.Context, string) (*store.Store, error) {
		return nil, errors.New("migration 005_test.sql: injected failure")
	}
	t.Cleanup(func() { openStore = previous })
	var stdout, stderr bytes.Buffer
	if code := execute(nil, &stdout, &stderr); code == 0 {
		t.Fatalf("execute failing migration = 0, stderr %q", stderr.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("database bytes changed: got %x, want %x", got, want)
	}
	if !strings.Contains(stderr.String(), path) || !strings.Contains(stderr.String(), "migration 005_test.sql") {
		t.Fatalf("stderr lacks path/migration failure:\n%s", stderr.String())
	}
}

func TestRealMigrationFailureLeavesDatabaseBytesUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cicerone.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE packages(unexpected TEXT); PRAGMA user_version=0;`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantSidecars := map[string][]byte{"-wal": []byte("preserved wal"), "-shm": []byte("preserved shm")}
	for suffix, body := range wantSidecars {
		if err := os.WriteFile(path+suffix, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := openStorePreservingFailures(context.Background(), path); err == nil {
		t.Fatal("openStorePreservingFailures error = nil, want migration failure")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("failed migration changed original database bytes")
	}
	for suffix, wantSidecar := range wantSidecars {
		gotSidecar, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotSidecar, wantSidecar) {
			t.Fatalf("failed migration changed %s bytes", suffix)
		}
	}
}

func TestPromotionFailureLeavesDatabaseAndSidecarsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cicerone.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(path)
	wantSidecars := map[string][]byte{"-wal": []byte("wal bytes"), "-shm": []byte("shm bytes")}
	for suffix, body := range wantSidecars {
		if err := os.WriteFile(path+suffix, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	previous := renameFile
	renameFile = func(string, string) error { return errors.New("injected promotion failure") }
	t.Cleanup(func() { renameFile = previous })
	if _, err := openStorePreservingFailures(context.Background(), path); err == nil {
		t.Fatal("promotion error = nil")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, want) {
		t.Fatal("promotion failure changed database")
	}
	for suffix, body := range wantSidecars {
		got, _ := os.ReadFile(path + suffix)
		if !bytes.Equal(got, body) {
			t.Fatalf("promotion failure changed %s", suffix)
		}
	}
}

func TestPostPromotionOpenFailureRestoresDatabaseAndSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cicerone.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(path)
	wantSidecars := map[string][]byte{"-wal": []byte("original wal"), "-shm": []byte("original shm")}
	for suffix, body := range wantSidecars {
		if err := os.WriteFile(path+suffix, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	previous := storeOpen
	calls := 0
	storeOpen = func(ctx context.Context, candidate string) (*store.Store, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("injected final open failure")
		}
		return store.Open(ctx, candidate)
	}
	t.Cleanup(func() { storeOpen = previous })
	if _, err := openStorePreservingFailures(context.Background(), path); err == nil {
		t.Fatal("final open error = nil")
	}
	if calls != 2 {
		t.Fatalf("store open calls = %d, want checked copy plus promoted open", calls)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, want) {
		t.Fatal("final open failure did not restore database")
	}
	for suffix, body := range wantSidecars {
		got, _ := os.ReadFile(path + suffix)
		if !bytes.Equal(got, body) {
			t.Fatalf("final open failure did not restore %s", suffix)
		}
	}
}

func TestProductionTUIDependenciesWireActionsInstalledAndSend(t *testing.T) {
	destination, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	brew := homebrew.NewClient(execx.NewRunner())
	var sent tea.Msg
	loader := changelogLoader{cache: destination}
	deps := tuiDependencies(destination, loader, context.Background(), nil, brew, func(msg tea.Msg) { sent = msg })
	if deps.Data == nil || deps.Changelog == nil || deps.Actions == nil || deps.Installed == nil || deps.Send == nil {
		t.Fatalf("incomplete production dependencies: %#v", deps)
	}
	if _, ok := deps.Changelog.(changelogLoader); !ok {
		t.Fatalf("Changelog dependency = %T, want resolving loader", deps.Changelog)
	}
	deps.Send("live output")
	if sent != "live output" {
		t.Fatalf("Send delivered %#v", sent)
	}
}

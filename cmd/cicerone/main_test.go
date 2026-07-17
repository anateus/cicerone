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

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/execx"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
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

func TestProductionTUIDependenciesWireActionsInstalledAndSend(t *testing.T) {
	destination, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	brew := homebrew.NewClient(execx.NewRunner())
	var sent tea.Msg
	deps := tuiDependencies(destination, context.Background(), nil, brew, func(msg tea.Msg) { sent = msg })
	if deps.Data == nil || deps.Changelog == nil || deps.Actions == nil || deps.Installed == nil || deps.Send == nil {
		t.Fatalf("incomplete production dependencies: %#v", deps)
	}
	deps.Send("live output")
	if sent != "live output" {
		t.Fatalf("Send delivered %#v", sent)
	}
}

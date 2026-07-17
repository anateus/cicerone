package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/syncer"
)

type fakePlainRuntime struct {
	out           *bytes.Buffer
	queries       [][]domain.FeedGroup
	notifications []tea.Msg
	queryCount    int
	started       bool
	waited        bool
}

type failOnceStatusWriter struct {
	buffer *bytes.Buffer
	err    error
	failed bool
}

func (w *failOnceStatusWriter) Write(p []byte) (int, error) {
	if !w.failed && bytes.HasPrefix(p, []byte("Synchronizing ")) {
		w.failed = true
		return len(p) / 2, w.err
	}
	return w.buffer.Write(p)
}

func (f *fakePlainRuntime) Preferences(context.Context) (domain.FeedFilter, error) {
	return domain.FeedFilter{}, nil
}

func (f *fakePlainRuntime) QueryFeed(context.Context, domain.FeedFilter) ([]domain.FeedGroup, error) {
	if f.queryCount == 0 && f.started {
		panic("initial query happened after synchronization started")
	}
	if f.queryCount == 1 && !f.waited {
		panic("refreshed query happened before synchronization completed")
	}
	result := f.queries[f.queryCount]
	f.queryCount++
	return result, nil
}

func (f *fakePlainRuntime) StartSync(context.Context) {
	if !strings.HasPrefix(f.out.String(), "Cached feed\n") {
		panic("synchronization started before cached output was written")
	}
	f.started = true
}

func (f *fakePlainRuntime) WaitSync() { f.waited = true }

func (f *fakePlainRuntime) plainNotifications() []tea.Msg {
	return append([]tea.Msg(nil), f.notifications...)
}

func plainGroup(version string) domain.FeedGroup {
	event := domain.UpdateEvent{
		ID: "foo-" + domain.EventID(version), PackageID: "foo", Name: "foo",
		Kind: domain.EventVersion, NewVersion: version,
		Time: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
	}
	return domain.FeedGroup{ID: event.ID, Events: []domain.UpdateEvent{event}}
}

func TestRunPlainWritesCachedFeedBeforeSyncAndWaitsBeforeRefresh(t *testing.T) {
	var out bytes.Buffer
	runtime := &fakePlainRuntime{
		out:     &out,
		queries: [][]domain.FeedGroup{{plainGroup("2.0")}, {plainGroup("3.0")}},
		notifications: []tea.Msg{
			syncer.SyncStarted{Source: "homebrew-core"},
			syncer.SyncCommitted{Source: "homebrew-core", Result: syncer.Result{Events: 1}},
		},
	}

	if err := runPlain(context.Background(), runtime, &out); err != nil {
		t.Fatal(err)
	}
	want := "Cached feed\n" +
		"foo 2.0 · version · 2026-07-16\n" +
		"Synchronizing homebrew-core…\n" +
		"homebrew-core synchronized · 1 updates\n" +
		"Refreshed feed\n" +
		"foo 3.0 · version · 2026-07-16\n"
	if got := out.String(); got != want {
		t.Fatalf("output mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunPlainReturnsCollectedSyncFailures(t *testing.T) {
	wantErr := errors.New("network unavailable")
	var out bytes.Buffer
	runtime := &fakePlainRuntime{
		out: &out, queries: [][]domain.FeedGroup{nil, nil},
		notifications: []tea.Msg{
			syncer.SyncStarted{Source: "homebrew-cask"},
			syncer.SyncFailed{Source: "homebrew-cask", Err: wantErr},
		},
	}

	err := runPlain(context.Background(), runtime, &out)
	if !errors.Is(err, wantErr) {
		t.Fatalf("runPlain error = %v, want wrapped %v", err, wantErr)
	}
	if got := out.String(); !strings.Contains(got, "homebrew-cask synchronization failed · network unavailable\n") {
		t.Fatalf("failure output missing detail:\n%s", got)
	}
}

func TestRunPlainReturnsStatusWriteErrorWithCollectedSyncFailure(t *testing.T) {
	wantSyncErr := errors.New("network unavailable")
	wantWriteErr := errors.New("broken pipe")
	var out bytes.Buffer
	runtime := &fakePlainRuntime{
		out: &out, queries: [][]domain.FeedGroup{nil, nil},
		notifications: []tea.Msg{
			syncer.SyncStarted{Source: "homebrew-cask"},
			syncer.SyncFailed{Source: "homebrew-cask", Err: wantSyncErr},
		},
	}
	writer := &failOnceStatusWriter{buffer: &out, err: wantWriteErr}

	err := runPlain(context.Background(), runtime, writer)
	if !errors.Is(err, wantWriteErr) {
		t.Fatalf("runPlain error = %v, want wrapped writer error %v", err, wantWriteErr)
	}
	if !errors.Is(err, wantSyncErr) {
		t.Fatalf("runPlain error = %v, want wrapped sync error %v", err, wantSyncErr)
	}
}

func TestRunPlainGroupsConcurrentNotificationsInStableSourceOrder(t *testing.T) {
	var out bytes.Buffer
	runtime := &fakePlainRuntime{
		out: &out, queries: [][]domain.FeedGroup{nil, nil},
		notifications: []tea.Msg{
			syncer.SyncStarted{Source: "homebrew-core"},
			syncer.SyncStarted{Source: "homebrew-cask"},
			syncer.SyncCommitted{Source: "homebrew-cask", Result: syncer.Result{Events: 2}},
			syncer.SyncCommitted{Source: "homebrew-core", Result: syncer.Result{Events: 1}},
		},
	}

	if err := runPlain(context.Background(), runtime, &out); err != nil {
		t.Fatal(err)
	}
	want := "Cached feed\n" +
		"Synchronizing homebrew-cask…\n" +
		"homebrew-cask synchronized · 2 updates\n" +
		"Synchronizing homebrew-core…\n" +
		"homebrew-core synchronized · 1 updates\n" +
		"Refreshed feed\n"
	if got := out.String(); got != want {
		t.Fatalf("output mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestExecutePlainRoutesToPlainExecutor(t *testing.T) {
	previous := executePlain
	called := false
	executePlain = func(stdout, stderr io.Writer) int {
		called = true
		_, _ = io.WriteString(stdout, "plain route\n")
		return 0
	}
	t.Cleanup(func() { executePlain = previous })
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"--plain"}, &stdout, &stderr); code != 0 {
		t.Fatalf("execute --plain = %d, stderr %q", code, stderr.String())
	}
	if !called {
		t.Fatal("plain executor was not called")
	}
	if got := stdout.String(); got != "plain route\n" {
		t.Fatalf("stdout = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

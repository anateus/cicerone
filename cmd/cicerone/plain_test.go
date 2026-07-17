package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/store"
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

type streamingPlainRuntime struct {
	mu          sync.Mutex
	queryCount  int
	stream      chan tea.Msg
	release     chan struct{}
	incremental chan struct{}
}

type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

type realStorePlainRuntime struct {
	store     *store.Store
	stream    chan tea.Msg
	published chan struct{}
	release   chan struct{}
	err       error
}

func (r *realStorePlainRuntime) Preferences(context.Context) (domain.FeedFilter, error) {
	return domain.FeedFilter{}, nil
}
func (r *realStorePlainRuntime) QueryFeed(ctx context.Context, filter domain.FeedFilter) ([]domain.FeedGroup, error) {
	return r.store.QueryFeed(ctx, filter)
}
func (r *realStorePlainRuntime) StartSync(ctx context.Context) {
	go func() {
		event := domain.UpdateEvent{ID: "durable-event", PackageID: "durable", Name: "durable", Type: domain.PackageFormula, Kind: domain.EventVersion, NewVersion: "2.0", Repository: "core", Commit: "commit", Time: time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)}
		r.err = r.store.UpsertEvents(ctx, []domain.UpdateEvent{event})
		if r.err == nil {
			r.stream <- syncer.SyncProgress{Source: "homebrew-core", Progress: syncer.Progress{Commits: 100, Events: 1, Batches: 1}}
		}
		close(r.published)
	}()
}
func (r *realStorePlainRuntime) WaitSync()                                { <-r.release }
func (r *realStorePlainRuntime) plainNotificationChannel() <-chan tea.Msg { return r.stream }
func (r *realStorePlainRuntime) plainNotifications() []tea.Msg {
	return []tea.Msg{syncer.SyncStarted{Source: "homebrew-core"}, syncer.SyncCommitted{Source: "homebrew-core", Result: syncer.Result{Events: 1}}}
}

func TestRunPlainRendersRealStoreBatchBeforeCompletion(t *testing.T) {
	ctx := context.Background()
	destination, err := store.Open(ctx, filepath.Join(t.TempDir(), "plain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destination.Close() })
	runtime := &realStorePlainRuntime{store: destination, stream: make(chan tea.Msg, 2), published: make(chan struct{}), release: make(chan struct{})}
	var out synchronizedBuffer
	done := make(chan error, 1)
	go func() { done <- runPlain(ctx, runtime, &out) }()
	<-runtime.published
	if runtime.err != nil {
		t.Fatal(runtime.err)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(out.String(), "durable 2.0") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := out.String(); !strings.Contains(got, "100 commits scanned") || !strings.Contains(got, "durable 2.0 · version · 2026-07-16") {
		t.Fatalf("real store output before completion=%q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("completed before release: %v", err)
	default:
	}
	close(runtime.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "durable 2.0") != 1 {
		t.Fatalf("duplicate output=%q", out.String())
	}
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func (r *streamingPlainRuntime) Preferences(context.Context) (domain.FeedFilter, error) {
	return domain.FeedFilter{}, nil
}
func (r *streamingPlainRuntime) QueryFeed(context.Context, domain.FeedFilter) ([]domain.FeedGroup, error) {
	r.mu.Lock()
	r.queryCount++
	count := r.queryCount
	r.mu.Unlock()
	if count == 1 {
		return nil, nil
	}
	if count == 2 {
		close(r.incremental)
	}
	return []domain.FeedGroup{plainGroup("2.0")}, nil
}
func (r *streamingPlainRuntime) StartSync(context.Context) {
	r.stream <- syncer.SyncProgress{Source: "homebrew-core", Progress: syncer.Progress{Commits: 100, Events: 1, Batches: 1}}
}
func (r *streamingPlainRuntime) WaitSync()                                { <-r.release }
func (r *streamingPlainRuntime) plainNotificationChannel() <-chan tea.Msg { return r.stream }
func (r *streamingPlainRuntime) plainNotifications() []tea.Msg {
	return []tea.Msg{syncer.SyncStarted{Source: "homebrew-core"}, syncer.SyncCommitted{Source: "homebrew-core", Result: syncer.Result{Events: 1}}}
}

func TestRunPlainRendersBatchesBeforeCompletion(t *testing.T) {
	runtime := &streamingPlainRuntime{stream: make(chan tea.Msg, 2), release: make(chan struct{}), incremental: make(chan struct{})}
	var out synchronizedBuffer
	done := make(chan error, 1)
	go func() { done <- runPlain(context.Background(), runtime, &out) }()
	select {
	case <-runtime.incremental:
	case <-time.After(time.Second):
		t.Fatal("incremental feed was not queried")
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(out.String(), "foo 2.0") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := out.String(); !strings.Contains(got, "100 commits scanned") || !strings.Contains(got, "foo 2.0") {
		t.Fatalf("output before completion=%q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("completed before release: %v", err)
	default:
	}
	close(runtime.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "foo 2.0") != 1 || strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("output=%q", out.String())
	}
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

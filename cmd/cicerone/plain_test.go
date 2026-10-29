package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/store"
	"github.com/anateus/cicerone/internal/syncer"
)

type fakePlainRuntime struct {
	out           *bytes.Buffer
	queries       [][]domain.FeedGroup
	filter        domain.FeedFilter
	queryFilters  []domain.FeedFilter
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
	mu            sync.Mutex
	queryCount    int
	stream        chan tea.Msg
	release       chan struct{}
	notifications []tea.Msg
	incremental   chan struct{}
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

type phasePlainSource struct {
	refreshStarted chan struct{}
	refreshRelease chan struct{}
	indexStarted   chan struct{}
	indexRelease   chan struct{}
}

func (s *phasePlainSource) Name() string { return "core" }
func (s *phasePlainSource) Refresh(ctx context.Context) error {
	close(s.refreshStarted)
	select {
	case <-s.refreshRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *phasePlainSource) Index(ctx context.Context, req syncer.Request) (syncer.Result, error) {
	close(s.indexStarted)
	select {
	case <-s.indexRelease:
		return syncer.Result{Events: 1}, nil
	case <-ctx.Done():
		return syncer.Result{}, ctx.Err()
	}
}

type phaseErrorWriter struct {
	buffer *bytes.Buffer
	err    error
}

func (w *phaseErrorWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("· fetching/checking…")) {
		return 0, w.err
	}
	return w.buffer.Write(p)
}

type slowPlainSource struct {
	started chan struct{}
	release <-chan struct{}
	calls   int
}

func (s *slowPlainSource) Name() string                  { return "core" }
func (s *slowPlainSource) Refresh(context.Context) error { return nil }
func (s *slowPlainSource) Index(ctx context.Context, req syncer.Request) (syncer.Result, error) {
	s.calls++
	if req.Progress != nil {
		req.Progress(syncer.Progress{Commits: 1, Batches: 1})
	}
	close(s.started)
	select {
	case <-s.release:
		return syncer.Result{Events: 1}, nil
	case <-ctx.Done():
		return syncer.Result{}, ctx.Err()
	}
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
	if r.notifications != nil {
		return r.notifications
	}
	return []tea.Msg{syncer.SyncStarted{Source: "homebrew-core"}, syncer.SyncCommitted{Source: "homebrew-core", Result: syncer.Result{Events: 1}}}
}

func TestRunPlainRendersBatchesBeforeCompletion(t *testing.T) {
	runtime := &streamingPlainRuntime{stream: make(chan tea.Msg, 4), release: make(chan struct{}), incremental: make(chan struct{})}
	runtime.stream <- syncer.SyncQueued{Source: "homebrew-core", ID: 2, HistoricalFallback: true}
	runtime.stream <- syncer.SyncStarted{Source: "homebrew-core", ID: 2, HistoricalFallback: true}
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
	if got := out.String(); !strings.Contains(got, "100 commits scanned") || !strings.Contains(got, "foo 2.0") ||
		!strings.Contains(got, "homebrew-core historical indexing queued") || !strings.Contains(got, "Indexing homebrew-core history") {
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

func TestRunPlainStreamsFallbackFailureBeforeOtherWorkCompletes(t *testing.T) {
	wantErr := errors.New("history read failed")
	runtime := &streamingPlainRuntime{stream: make(chan tea.Msg, 4), release: make(chan struct{}), incremental: make(chan struct{})}
	runtime.stream <- syncer.SyncStarted{Source: "homebrew-core", ID: 2, HistoricalFallback: true}
	var out synchronizedBuffer
	done := make(chan error, 1)
	go func() { done <- runPlain(context.Background(), runtime, &out) }()
	select {
	case <-runtime.incremental:
	case <-time.After(time.Second):
		t.Fatal("incremental feed was not queried")
	}
	runtime.stream <- syncer.SyncFailed{Source: "homebrew-core", ID: 2, HistoricalFallback: true, Err: wantErr}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(out.String(), "historical indexing failed") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := out.String(); !strings.Contains(got, "homebrew-core historical indexing failed · history read failed") {
		t.Fatalf("fallback failure not visible while waiting: %q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("completed before release: %v", err)
	default:
	}
	close(runtime.release)
	if err := <-done; !errors.Is(err, wantErr) {
		t.Fatalf("runPlain error=%v, want fallback failure", err)
	}
}

func TestRunPlainStreamsNormalLifecycleAndDeduplicatesRecap(t *testing.T) {
	wantErr := errors.New("network unavailable")
	events := []tea.Msg{
		syncer.SyncStarted{Source: "core", ID: 1},
		syncer.SyncFailed{Source: "core", ID: 1, Err: wantErr},
		syncer.SyncStarted{Source: "core", ID: 2},
		syncer.SyncCommitted{Source: "core", ID: 2, Result: syncer.Result{Events: 3}},
		syncer.SyncStarted{Source: "core", ID: 1, HistoricalFallback: true},
		syncer.SyncCancelled{Source: "core", ID: 1, HistoricalFallback: true, Reason: "paused"},
		syncer.SyncStarted{Source: "cask", ID: 1},
		syncer.SyncCancelled{Source: "cask", ID: 1, Reason: "stopped"},
		syncer.SyncStarted{Source: "installed", ID: 3},
		syncer.SyncCommitted{Source: "installed", ID: 3},
	}
	runtime := &streamingPlainRuntime{
		stream: make(chan tea.Msg, len(events)+1), release: make(chan struct{}),
		incremental: make(chan struct{}), notifications: events,
	}
	for _, event := range events {
		runtime.stream <- event
	}
	var out synchronizedBuffer
	done := make(chan error, 1)
	go func() { done <- runPlain(context.Background(), runtime, &out) }()
	select {
	case <-runtime.incremental:
	case <-time.After(time.Second):
		t.Fatal("incremental feed was not queried")
	}
	got := out.String()
	for _, want := range []string{
		"Synchronizing core…", "core synchronization failed · network unavailable",
		"core synchronized · 3 updates", "Indexing core history…",
		"core historical indexing canceled · paused", "cask synchronization canceled · stopped",
		"Preparing installed…", "installed preparation complete",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing live %q: %s", want, got)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("completed before release: %v", err)
	default:
	}
	close(runtime.release)
	err := <-done
	if !errors.Is(err, wantErr) || strings.Count(err.Error(), wantErr.Error()) != 1 {
		t.Fatalf("runPlain error=%v, want failure once", err)
	}
	got = out.String()
	for _, check := range []struct {
		text  string
		count int
	}{
		{"Synchronizing core…", 2},
		{"core synchronization failed · network unavailable", 1},
		{"core synchronized · 3 updates", 1},
		{"Indexing core history…", 1},
		{"core historical indexing canceled · paused", 1},
		{"Synchronizing cask…", 1},
		{"cask synchronization canceled · stopped", 1},
		{"Preparing installed…", 1},
		{"installed preparation complete", 1},
	} {
		if count := strings.Count(got, check.text); count != check.count {
			t.Errorf("%q appears %d times, want %d: %s", check.text, count, check.count, got)
		}
	}
}

func TestRunPlainRecapsCompanionFailureAfterLiveCancellation(t *testing.T) {
	wantErr := errors.New("canceled during setup")
	start := syncer.SyncStarted{Source: "core", ID: 5}
	cancel := syncer.SyncCancelled{Source: "core", ID: 5, Reason: "shutdown"}
	failure := syncer.SyncFailed{Source: "core", ID: 5, Err: wantErr}
	runtime := &streamingPlainRuntime{
		stream: make(chan tea.Msg, 3), release: make(chan struct{}),
		incremental: make(chan struct{}), notifications: []tea.Msg{start, cancel, failure},
	}
	runtime.stream <- start
	runtime.stream <- cancel
	var out synchronizedBuffer
	done := make(chan error, 1)
	go func() { done <- runPlain(context.Background(), runtime, &out) }()
	select {
	case <-runtime.incremental:
	case <-time.After(time.Second):
		t.Fatal("incremental feed was not queried")
	}
	close(runtime.release)
	err := <-done
	if !errors.Is(err, wantErr) || strings.Count(err.Error(), wantErr.Error()) != 1 {
		t.Fatalf("runPlain error=%v, want companion failure once", err)
	}
	for _, text := range []string{
		"Synchronizing core…",
		"core synchronization canceled · shutdown",
		"core synchronization failed · canceled during setup",
	} {
		if count := strings.Count(out.String(), text); count != 1 {
			t.Errorf("%q appears %d times, want once: %s", text, count, out.String())
		}
	}
}

func TestRunPlainRecapsSequentialRuns(t *testing.T) {
	var out bytes.Buffer
	runtime := &fakePlainRuntime{
		out: &out, queries: [][]domain.FeedGroup{nil, nil},
		notifications: []tea.Msg{
			syncer.SyncStarted{Source: "core", ID: 1},
			syncer.SyncCommitted{Source: "core", ID: 1, Result: syncer.Result{Events: 1}},
			syncer.SyncStarted{Source: "core", ID: 2},
			syncer.SyncCommitted{Source: "core", ID: 2, Result: syncer.Result{Events: 2}},
		},
	}
	if err := runPlain(context.Background(), runtime, &out); err != nil {
		t.Fatal(err)
	}
	want := "Cached feed\nSynchronizing core…\ncore synchronized · 1 updates\n" +
		"Synchronizing core…\ncore synchronized · 2 updates\nRefreshed feed\n"
	if got := out.String(); got != want {
		t.Fatalf("output mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunPlainShowsSavedFiltersWithoutChangingQueries(t *testing.T) {
	filter := domain.FeedFilter{
		Types:      map[domain.PackageType]bool{domain.PackageCask: true},
		GroupScope: domain.GroupScopeUser, GroupTarget: 42,
		Kinds: map[domain.EventKind]bool{domain.EventRevision: true},
		Query: "editor", Search: domain.SearchChangelogs, Horizon: 48 * time.Hour,
	}
	wantFilter := filter
	wantFilter.Types = map[domain.PackageType]bool{domain.PackageCask: true}
	wantFilter.Kinds = map[domain.EventKind]bool{domain.EventRevision: true}
	var out bytes.Buffer
	runtime := &fakePlainRuntime{out: &out, filter: filter, queries: [][]domain.FeedGroup{nil, nil}}
	if err := runPlain(context.Background(), runtime, &out); err != nil {
		t.Fatal(err)
	}
	for _, visible := range []string{"cask", "42", "revision", "editor", "changelogs"} {
		if !strings.Contains(out.String(), visible) {
			t.Errorf("plain feed does not show active filter %q:\n%s", visible, out.String())
		}
	}
	if len(runtime.queryFilters) != 2 {
		t.Fatalf("query count=%d, want 2", len(runtime.queryFilters))
	}
	for _, got := range runtime.queryFilters {
		if !reflect.DeepEqual(got, wantFilter) {
			t.Fatalf("query filter=%#v, want %#v", got, wantFilter)
		}
	}
	if !reflect.DeepEqual(runtime.filter, wantFilter) {
		t.Fatalf("preferences changed: %#v", runtime.filter)
	}
}

func TestProductionPlainRuntimeCompletesOneSlowSync(t *testing.T) {
	ctx := context.Background()
	destination, err := store.Open(ctx, filepath.Join(t.TempDir(), "plain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	source := &slowPlainSource{started: started, release: release}
	runtime := &productionPlainRuntime{stream: make(chan tea.Msg, 32)}
	coordinator := syncer.New(syncer.Dependencies{
		Store: syncStore{destination}, Sources: []syncer.Source{source}, RefreshInterval: 0,
		Notify: func(event syncer.Event) { publishSyncEvent(runtime.notify, event) },
	})
	runtime.services = &runtimeServices{store: destination, coordinator: coordinator}
	defer func() {
		releaseOnce.Do(func() { close(release) })
		coordinator.Close()
	}()
	var out synchronizedBuffer
	done := make(chan error, 1)
	go func() { done <- runPlain(ctx, runtime, &out) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("plain sync did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("plain sync finished before its source: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("one-shot plain sync did not finish")
	}
	if source.calls != 1 {
		t.Fatalf("plain source calls=%d, want one sync", source.calls)
	}
}

func TestProductionPlainRuntimeStreamsFetchingAndIndexing(t *testing.T) {
	ctx := context.Background()
	destination, err := store.Open(ctx, filepath.Join(t.TempDir(), "plain-phases.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	source := &phasePlainSource{
		refreshStarted: make(chan struct{}), refreshRelease: make(chan struct{}),
		indexStarted: make(chan struct{}), indexRelease: make(chan struct{}),
	}
	var releaseRefresh, releaseIndex sync.Once
	runtime := &productionPlainRuntime{stream: make(chan tea.Msg, 32)}
	coordinator := syncer.New(syncer.Dependencies{
		Store: syncStore{destination}, Sources: []syncer.Source{source},
		Notify: func(event syncer.Event) { publishSyncEvent(runtime.notify, event) },
	})
	runtime.services = &runtimeServices{store: destination, coordinator: coordinator}
	defer func() {
		releaseRefresh.Do(func() { close(source.refreshRelease) })
		releaseIndex.Do(func() { close(source.indexRelease) })
		coordinator.Close()
	}()
	var out synchronizedBuffer
	done := make(chan error, 1)
	go func() { done <- runPlain(ctx, runtime, &out) }()
	select {
	case <-source.refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(out.String(), "core · fetching/checking…") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := out.String()
	if !strings.Contains(got, "Synchronizing core…") || !strings.Contains(got, "core · fetching/checking…") ||
		strings.Contains(got, "core · indexing…") || strings.Contains(got, "core synchronized") {
		t.Fatalf("output during refresh: %q", got)
	}
	releaseRefresh.Do(func() { close(source.refreshRelease) })
	select {
	case <-source.indexStarted:
	case <-time.After(time.Second):
		t.Fatal("index did not start")
	}
	deadline = time.Now().Add(time.Second)
	for !strings.Contains(out.String(), "core · indexing…") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got = out.String()
	if !strings.Contains(got, "core · indexing…") || strings.Contains(got, "core synchronized") ||
		strings.Index(got, "core · fetching/checking…") > strings.Index(got, "core · indexing…") {
		t.Fatalf("output during indexing: %q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("completed before indexing release: %v", err)
	default:
	}
	releaseIndex.Do(func() { close(source.indexRelease) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("plain sync did not finish")
	}
	got = out.String()
	for _, text := range []string{"core · fetching/checking…", "core · indexing…", "core synchronized · 1 updates"} {
		if count := strings.Count(got, text); count != 1 {
			t.Errorf("%q appears %d times: %s", text, count, got)
		}
	}
	var phases []syncer.Phase
	for _, event := range runtime.plainNotifications() {
		if phase, ok := event.(syncer.SyncPhase); ok {
			phases = append(phases, phase.Phase)
		}
	}
	if !reflect.DeepEqual(phases, []syncer.Phase{syncer.PhaseFetching, syncer.PhaseIndexing}) {
		t.Fatalf("recorded phases=%v", phases)
	}
}

func TestRunPlainRecapsMissingPhasesByFullKey(t *testing.T) {
	fetch := syncer.SyncPhase{Source: "core", ID: 1, Phase: syncer.PhaseFetching}
	index := syncer.SyncPhase{Source: "core", ID: 1, Phase: syncer.PhaseIndexing}
	fallback := syncer.SyncPhase{Source: "core", ID: 1, HistoricalFallback: true, Phase: syncer.PhaseIndexing}
	otherID := syncer.SyncPhase{Source: "core", ID: 2, Phase: syncer.PhaseFetching}
	otherSource := syncer.SyncPhase{Source: "cask", ID: 1, Phase: syncer.PhaseFetching}
	events := []tea.Msg{fetch, index, fallback, otherID, otherSource}
	runtime := &streamingPlainRuntime{
		stream: make(chan tea.Msg, 4), release: make(chan struct{}),
		incremental: make(chan struct{}), notifications: events,
	}
	runtime.stream <- fetch
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(runtime.release) })
	runtime.stream <- fallback
	var out synchronizedBuffer
	done := make(chan error, 1)
	go func() { done <- runPlain(context.Background(), runtime, &out) }()
	select {
	case <-runtime.incremental:
	case <-time.After(time.Second):
		t.Fatal("incremental feed was not queried")
	}
	got := out.String()
	if !strings.Contains(got, "core · fetching/checking…") || !strings.Contains(got, "core history · indexing…") ||
		strings.Contains(got, "core · indexing…") || strings.Contains(got, "core history · fetching/checking…") {
		t.Fatalf("partial live output: %q", got)
	}
	releaseOnce.Do(func() { close(runtime.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got = out.String()
	for _, check := range []struct {
		text  string
		count int
	}{
		{"core · fetching/checking…", 2},
		{"core · indexing…", 1},
		{"core history · indexing…", 1},
		{"core history · fetching/checking…", 0},
		{"cask · fetching/checking…", 1},
	} {
		if count := strings.Count(got, check.text); count != check.count {
			t.Errorf("%q appears %d times, want %d: %s", check.text, count, check.count, got)
		}
	}
}

func TestRunPlainRecapsPhasesWithoutStream(t *testing.T) {
	var out bytes.Buffer
	runtime := &fakePlainRuntime{
		out: &out, queries: [][]domain.FeedGroup{nil, nil},
		notifications: []tea.Msg{
			syncer.SyncStarted{Source: "core", ID: 1},
			syncer.SyncPhase{Source: "core", ID: 1, Phase: syncer.PhaseFetching},
			syncer.SyncPhase{Source: "core", ID: 1, Phase: syncer.PhaseIndexing},
			syncer.SyncCommitted{Source: "core", ID: 1},
			syncer.SyncStarted{Source: "core", ID: 2, HistoricalFallback: true},
			syncer.SyncPhase{Source: "core", ID: 2, HistoricalFallback: true, Phase: syncer.PhaseIndexing},
		},
	}
	if err := runPlain(context.Background(), runtime, &out); err != nil {
		t.Fatal(err)
	}
	want := "Cached feed\nSynchronizing core…\ncore · fetching/checking…\ncore · indexing…\n" +
		"core synchronized · 0 updates\nIndexing core history…\ncore history · indexing…\nRefreshed feed\n"
	if got := out.String(); got != want {
		t.Fatalf("output mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunPlainReturnsLivePhaseWriterError(t *testing.T) {
	wantErr := errors.New("live phase output failed")
	phase := syncer.SyncPhase{Source: "core", ID: 1, Phase: syncer.PhaseFetching}
	runtime := &streamingPlainRuntime{
		stream: make(chan tea.Msg, 2), release: make(chan struct{}),
		incremental: make(chan struct{}), notifications: []tea.Msg{phase},
	}
	runtime.stream <- phase
	close(runtime.release)
	var out bytes.Buffer
	err := runPlain(context.Background(), runtime, &phaseErrorWriter{buffer: &out, err: wantErr})
	if !errors.Is(err, wantErr) || strings.Count(err.Error(), wantErr.Error()) != 1 {
		t.Fatalf("runPlain error=%v, want live writer error once", err)
	}
}

func TestRunPlainRecapsPhasesWithoutStreamAndReturnsWriterError(t *testing.T) {
	wantErr := errors.New("phase output failed")
	var out bytes.Buffer
	runtime := &fakePlainRuntime{
		out: &out, queries: [][]domain.FeedGroup{nil, nil},
		notifications: []tea.Msg{
			syncer.SyncStarted{Source: "core", ID: 1},
			syncer.SyncPhase{Source: "core", ID: 1, Phase: syncer.PhaseFetching},
			syncer.SyncPhase{Source: "core", ID: 1, Phase: syncer.PhaseIndexing},
			syncer.SyncCommitted{Source: "core", ID: 1},
		},
	}
	err := runPlain(context.Background(), runtime, &phaseErrorWriter{buffer: &out, err: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("runPlain error=%v, want writer error", err)
	}
	got := out.String()
	for _, text := range []string{"Synchronizing core…", "core · indexing…", "core synchronized · 0 updates"} {
		if !strings.Contains(got, text) {
			t.Errorf("missing %q: %s", text, got)
		}
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
	return f.filter, nil
}

func (f *fakePlainRuntime) QueryFeed(_ context.Context, filter domain.FeedFilter) ([]domain.FeedGroup, error) {
	f.queryFilters = append(f.queryFilters, filter)
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
	if !strings.HasPrefix(f.out.String(), "Cached feed") {
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

func TestRunPlainKeepsFallbackFailuresSeparateFromSuccessfulRefresh(t *testing.T) {
	wantErr := errors.New("history blob unavailable")
	var out bytes.Buffer
	runtime := &fakePlainRuntime{
		out: &out, queries: [][]domain.FeedGroup{nil, nil},
		notifications: []tea.Msg{
			syncer.SyncStarted{Source: "core", ID: 1},
			syncer.SyncCommitted{Source: "core", ID: 1, Result: syncer.Result{Events: 7}},
			syncer.SyncStarted{Source: "core", ID: 2, HistoricalFallback: true},
			syncer.SyncFailed{Source: "core", ID: 2, HistoricalFallback: true, Err: wantErr},
		},
	}
	err := runPlain(context.Background(), runtime, &out)
	if !errors.Is(err, wantErr) {
		t.Fatalf("runPlain error=%v, want fallback failure", err)
	}
	for _, want := range []string{"core synchronized · 7 updates", "Indexing core history", "core historical indexing failed · history blob unavailable"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q: %s", want, out.String())
		}
	}
}

func TestRunPlainReportsFallbackCancellationReason(t *testing.T) {
	var out bytes.Buffer
	runtime := &fakePlainRuntime{
		out: &out, queries: [][]domain.FeedGroup{nil, nil},
		notifications: []tea.Msg{
			syncer.SyncStarted{Source: "core", HistoricalFallback: true},
			syncer.SyncCancelled{Source: "core", HistoricalFallback: true, Reason: "yield for queued refresh at durable checkpoint"},
		},
	}
	if err := runPlain(context.Background(), runtime, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "core historical indexing canceled · yield for queued refresh at durable checkpoint") {
		t.Fatalf("fallback cancellation missing from %s", out.String())
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

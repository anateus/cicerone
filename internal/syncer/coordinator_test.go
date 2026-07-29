package syncer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/tui"
)

type fakeCache struct{ called chan struct{} }

func (f fakeCache) LoadCached(context.Context) error { close(f.called); return nil }

type fakeInstalled struct {
	called   chan struct{}
	release  <-chan struct{}
	packages []domain.InstalledPackage
}

func (f fakeInstalled) Installed(ctx context.Context) ([]domain.InstalledPackage, error) {
	close(f.called)
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.packages, nil
}

type fakeDestination struct {
	mu                  sync.Mutex
	installed           bool
	starts, finishes    []string
	startErr, finishErr error
}

func (f *fakeDestination) SetInstalled(context.Context, []domain.InstalledPackage) error {
	f.mu.Lock()
	f.installed = true
	f.mu.Unlock()
	return nil
}
func (f *fakeDestination) SyncStarted(_ context.Context, source string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, source)
	return f.startErr
}
func (f *fakeDestination) SyncFinished(_ context.Context, source string, _ time.Time, _ Result, _ error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes = append(f.finishes, source)
	return f.finishErr
}

func TestRetryAndEnsureRangeQueueDuringDiscovery(t *testing.T) {
	destination := &fakeDestination{installed: true}
	discoveryStarted, releaseDiscovery := make(chan struct{}), make(chan struct{})
	requests := make(chan Request, 3)
	var refreshes atomic.Int32
	job := fakeJob{name: "core", destination: destination, refresh: func(context.Context) error { refreshes.Add(1); return nil }, index: func(_ context.Context, req Request) (Result, error) { requests <- req; return Result{}, nil }}
	c := New(Dependencies{Store: destination, LoadSources: func(ctx context.Context) ([]Source, error) {
		close(discoveryStarted)
		select {
		case <-releaseDiscovery:
			return []Source{job}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	c.Start(context.Background())
	<-discoveryStarted
	since := time.Now().Add(-180 * 24 * time.Hour).UTC()
	c.Retry(context.Background(), "core")
	c.EnsureRange(context.Background(), since)
	close(releaseDiscovery)
	c.Wait()
	if refreshes.Load() != 2 {
		t.Fatalf("refreshes = %d, want startup and retry", refreshes.Load())
	}
	seenRange := false
	for range 3 {
		if req := <-requests; req.Since.Equal(since) {
			seenRange = true
		}
	}
	if !seenRange {
		t.Fatal("queued range request was dropped")
	}
}

func TestRetryRestartsFailedDiscoveryAndDrainsPendingOperationsOnce(t *testing.T) {
	destination := &fakeDestination{installed: true}
	discoveryStarted, failDiscovery := make(chan struct{}), make(chan struct{})
	requests := make(chan Request, 3)
	var loads, refreshes atomic.Int32
	job := fakeJob{
		name:        "core",
		destination: destination,
		refresh: func(context.Context) error {
			refreshes.Add(1)
			return nil
		},
		index: func(_ context.Context, req Request) (Result, error) {
			requests <- req
			return Result{}, nil
		},
	}
	var c *Coordinator
	c = New(Dependencies{
		Store: destination,
		LoadSources: func(context.Context) ([]Source, error) {
			if loads.Add(1) == 1 {
				close(discoveryStarted)
				<-failDiscovery
				return nil, errors.New("discovery failed once")
			}
			return []Source{job}, nil
		},
		Notify: func(msg tea.Msg) {
			if event, ok := msg.(SyncFailed); ok && event.Source == "repositories" {
				c.Retry(context.Background(), "core")
			}
		},
	})
	c.Start(context.Background())
	waitClosed(t, discoveryStarted, "first discovery attempt")
	since := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	c.EnsureRange(context.Background(), since)
	close(failDiscovery)
	c.Wait()

	if got := loads.Load(); got != 2 {
		t.Fatalf("source loads = %d, want failed attempt and one retry", got)
	}
	if got := refreshes.Load(); got != 2 {
		t.Fatalf("refreshes = %d, want startup and queued retry exactly once", got)
	}
	seenRange := 0
	for range 3 {
		if req := <-requests; req.Since.Equal(since) {
			seenRange++
		}
	}
	if seenRange != 1 {
		t.Fatalf("range operations = %d, want queued request exactly once", seenRange)
	}
}

func TestCloseDuringDiscoveryAndCallsAfterCloseDoNoWork(t *testing.T) {
	discoveryStarted := make(chan struct{})
	loaderStopped := make(chan struct{})
	var work atomic.Int32
	c := New(Dependencies{LoadSources: func(ctx context.Context) ([]Source, error) {
		work.Add(1)
		close(discoveryStarted)
		<-ctx.Done()
		close(loaderStopped)
		return nil, ctx.Err()
	}})
	c.Start(context.Background())
	<-discoveryStarted
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	waitClosed(t, loaderStopped, "discovery cancellation")
	waitClosed(t, done, "close")
	c.Retry(context.Background(), "core")
	c.EnsureRange(context.Background(), time.Now())
	c.Start(context.Background())
	if work.Load() != 1 {
		t.Fatalf("source loads = %d, want only initial load", work.Load())
	}
}

func TestSyncStartedPersistenceFailureSkipsSourceWorkAndCommit(t *testing.T) {
	destination := &fakeDestination{installed: true, startErr: errors.New("start write failed")}
	var indexed atomic.Int32
	var messages []tea.Msg
	var mu sync.Mutex
	job := fakeJob{name: "core", destination: destination, index: func(context.Context, Request) (Result, error) { indexed.Add(1); return Result{}, nil }}
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(msg tea.Msg) { mu.Lock(); messages = append(messages, msg); mu.Unlock() }})
	c.Start(context.Background())
	c.Wait()
	if indexed.Load() != 0 {
		t.Fatal("indexed after SyncStarted persistence failed")
	}
	mu.Lock()
	defer mu.Unlock()
	failed, committed, changed := 0, 0, 0
	for _, msg := range messages {
		switch msg.(type) {
		case SyncFailed:
			failed++
		case SyncCommitted:
			committed++
		case tui.DatasetChanged:
			changed++
		}
	}
	if failed != 1 || committed != 0 || changed != 0 {
		t.Fatalf("messages failed=%d committed=%d changed=%d", failed, committed, changed)
	}
}

type fakeJob struct {
	name        string
	destination *fakeDestination
	refresh     func(context.Context) error
	index       func(context.Context, Request) (Result, error)
}

func (f fakeJob) Name() string { return f.name }
func (f fakeJob) Refresh(ctx context.Context) error {
	if f.refresh != nil {
		return f.refresh(ctx)
	}
	return nil
}
func (f fakeJob) Index(ctx context.Context, req Request) (Result, error) {
	f.destination.mu.Lock()
	installed := f.destination.installed
	f.destination.mu.Unlock()
	if !installed {
		return Result{}, errors.New("index started before installed snapshot committed")
	}
	if f.index != nil {
		return f.index(ctx, req)
	}
	return Result{Events: 1, Cursor: "head"}, nil
}

type fakeCachedJob struct {
	fakeJob
	indexCached func(context.Context, Request) (Result, bool, error)
}

func (f fakeCachedJob) IndexCached(ctx context.Context, req Request) (Result, bool, error) {
	return f.indexCached(ctx, req)
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestStartLoadsCacheBeforeExternalRefreshAndInstalledBeforeIndex(t *testing.T) {
	cacheCalled, installedCalled, releaseInstalled, refreshed, indexed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	destination := &fakeDestination{}
	job := fakeJob{name: "core", destination: destination, refresh: func(context.Context) error {
		select {
		case <-cacheCalled:
		default:
			return errors.New("refresh before cached feed")
		}
		close(refreshed)
		return nil
	}, index: func(context.Context, Request) (Result, error) { close(indexed); return Result{Events: 1}, nil }}
	c := New(Dependencies{Cache: fakeCache{cacheCalled}, Installed: fakeInstalled{installedCalled, releaseInstalled, nil}, Store: destination, Sources: []Source{job}})
	c.Start(context.Background())
	waitClosed(t, cacheCalled, "cached feed")
	waitClosed(t, installedCalled, "installed read")
	select {
	case <-refreshed:
		t.Fatal("repository refreshed before installed state completed")
	default:
	}
	close(releaseInstalled)
	waitClosed(t, indexed, "history index")
	c.Close()
}

func TestRefreshIndexesUsableRepositoryCacheBeforeNetwork(t *testing.T) {
	destination := &fakeDestination{installed: true}
	var sequence []string
	job := fakeCachedJob{
		fakeJob: fakeJob{
			name:        "core",
			destination: destination,
			refresh: func(context.Context) error {
				sequence = append(sequence, "refresh")
				return nil
			},
			index: func(_ context.Context, req Request) (Result, error) {
				sequence = append(sequence, "fresh")
				req.Progress(Progress{Commits: 50, Events: 2, Batches: 1})
				return Result{Events: 2, Cursor: "fresh-head"}, nil
			},
		},
		indexCached: func(_ context.Context, req Request) (Result, bool, error) {
			sequence = append(sequence, "cached")
			req.Progress(Progress{Commits: 100, Events: 4, Batches: 1})
			return Result{Events: 4, Cursor: "cached-head"}, true, nil
		},
	}
	var progress []Progress
	var committed Result
	c := New(Dependencies{
		Store: destination, Sources: []Source{job},
		Notify: func(msg tea.Msg) {
			if event, ok := msg.(SyncProgress); ok {
				progress = append(progress, event.Progress)
			}
			if event, ok := msg.(SyncCommitted); ok {
				committed = event.Result
			}
		},
	})

	c.Start(context.Background())
	c.Wait()

	if want := []string{"cached", "refresh", "fresh"}; !slices.Equal(sequence, want) {
		t.Fatalf("work sequence = %v, want %v", sequence, want)
	}
	wantProgress := []Progress{
		{Commits: 100, Events: 4, Batches: 1},
		{Commits: 150, Events: 6, Batches: 2},
	}
	if !slices.Equal(progress, wantProgress) {
		t.Fatalf("cached progress = %#v", progress)
	}
	if committed.Events != 6 || committed.Cursor != "fresh-head" {
		t.Fatalf("committed result = %#v, want 6 events at fresh-head", committed)
	}
}

func TestSourceDiscoveryStartsAfterCachedFeed(t *testing.T) {
	cacheCalled := make(chan struct{})
	loaded := make(chan struct{})
	c := New(Dependencies{Cache: fakeCache{cacheCalled}, Installed: fakeInstalled{called: make(chan struct{})}, LoadSources: func(context.Context) ([]Source, error) {
		select {
		case <-cacheCalled:
		default:
			return nil, errors.New("source discovery before cached feed")
		}
		close(loaded)
		return nil, nil
	}})
	c.Start(context.Background())
	waitClosed(t, loaded, "source discovery")
	c.Close()
}

func TestCoordinatorPublishesDurableBatchProgress(t *testing.T) {
	destination := &fakeDestination{installed: true}
	job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
		req.Progress(Progress{Commits: 100, Events: 8, Batches: 1})
		req.Progress(Progress{Commits: 150, Events: 12, Batches: 2})
		return Result{Events: 12}, nil
	}}
	var messages []tea.Msg
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(msg tea.Msg) { messages = append(messages, msg) }})
	c.Start(context.Background())
	c.Wait()
	var sequence []string
	for _, message := range messages {
		switch event := message.(type) {
		case SyncStarted:
			sequence = append(sequence, "started")
		case SyncProgress:
			sequence = append(sequence, fmt.Sprintf("progress-%d", event.Progress.Batches))
		case tui.DatasetChanged:
			sequence = append(sequence, "changed")
		case SyncCommitted:
			sequence = append(sequence, "committed")
		}
	}
	want := []string{"started", "progress-1", "changed", "progress-2", "changed", "committed", "changed"}
	if !slices.Equal(sequence, want) {
		t.Fatalf("sequence=%v want=%v", sequence, want)
	}
}

func TestRepositoryConcurrencyIsTwoAndFailuresAreIsolated(t *testing.T) {
	destination := &fakeDestination{}
	var active, maximum atomic.Int32
	release := make(chan struct{})
	started := make(chan string, 3)
	jobs := make([]Source, 3)
	for i, name := range []string{"core", "cask", "extra"} {
		name := name
		jobs[i] = fakeJob{name: name, destination: destination, index: func(ctx context.Context, _ Request) (Result, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if n <= old || maximum.CompareAndSwap(old, n) {
					break
				}
			}
			started <- name
			select {
			case <-release:
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
			if name == "core" {
				return Result{}, errors.New("broken")
			}
			return Result{Events: 1}, nil
		}}
	}
	var mu sync.Mutex
	var messages []tea.Msg
	c := New(Dependencies{Installed: fakeInstalled{called: make(chan struct{})}, Store: destination, Sources: jobs, Notify: func(msg tea.Msg) { mu.Lock(); messages = append(messages, msg); mu.Unlock() }})
	c.Start(context.Background())
	<-started
	<-started
	select {
	case third := <-started:
		t.Fatalf("third source %s started while two active", third)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	c.Wait()
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	committed, failed, changed := 0, 0, 0
	for _, msg := range messages {
		switch msg.(type) {
		case SyncCommitted:
			committed++
		case SyncFailed:
			failed++
		case tui.DatasetChanged:
			changed++
		}
	}
	if committed != 2 || failed != 1 || changed != 2 {
		t.Fatalf("messages committed=%d failed=%d changed=%d", committed, failed, changed)
	}
}

func TestCancellationStopsAllWorkers(t *testing.T) {
	destination := &fakeDestination{}
	started := make(chan struct{}, 2)
	stopped := make(chan struct{}, 2)
	job := func(name string) Source {
		return fakeJob{name: name, destination: destination, index: func(ctx context.Context, _ Request) (Result, error) {
			started <- struct{}{}
			<-ctx.Done()
			stopped <- struct{}{}
			return Result{}, ctx.Err()
		}}
	}
	c := New(Dependencies{Installed: fakeInstalled{called: make(chan struct{})}, Store: destination, Sources: []Source{job("core"), job("cask")}})
	c.Start(context.Background())
	<-started
	<-started
	c.Close()
	<-stopped
	<-stopped
}

func TestEnsureRangeIndexesOnlyEarlierRangeAndRetrySelectsSource(t *testing.T) {
	destination := &fakeDestination{installed: true}
	since := time.Now().Add(-90 * 24 * time.Hour).UTC()
	requests := make(chan Request, 2)
	refreshed := atomic.Int32{}
	job := fakeJob{name: "core", destination: destination, refresh: func(context.Context) error { refreshed.Add(1); return nil }, index: func(_ context.Context, req Request) (Result, error) { requests <- req; return Result{}, nil }}
	c := New(Dependencies{Store: destination, Sources: []Source{job}})
	c.EnsureRange(context.Background(), since)
	c.Wait()
	if got := (<-requests).Since; !got.Equal(since) {
		t.Fatalf("range since = %v, want %v", got, since)
	}
	if refreshed.Load() != 0 {
		t.Fatal("range extension fetched repository")
	}
	c.Retry(context.Background(), "core")
	c.Wait()
	if refreshed.Load() != 1 {
		t.Fatalf("retry refresh count = %d", refreshed.Load())
	}
}

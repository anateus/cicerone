package syncer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/domain"
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
	finishErrors        []error
	startedRuns         atomic.Int64
	finishedRuns        []int64
	startErr, finishErr error
	finishContextErr    error
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
func (f *fakeDestination) SyncStartedRun(ctx context.Context, source string, at time.Time) (int64, error) {
	runID := f.startedRuns.Add(1)
	return runID, f.SyncStarted(ctx, source, at)
}
func (f *fakeDestination) SyncFinished(ctx context.Context, source string, _ time.Time, _ Result, syncErr error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes = append(f.finishes, source)
	f.finishErrors = append(f.finishErrors, syncErr)
	f.finishContextErr = ctx.Err()
	return f.finishErr
}
func (f *fakeDestination) SyncFinishedRun(ctx context.Context, runID int64, source string, at time.Time, result Result, syncErr error) error {
	f.mu.Lock()
	f.finishedRuns = append(f.finishedRuns, runID)
	f.mu.Unlock()
	return f.SyncFinished(ctx, source, at, result, syncErr)
}

func TestRetryAndEnsureRangeQueueDuringDiscovery(t *testing.T) {
	destination := &fakeDestination{installed: true}
	discoveryStarted, releaseDiscovery := make(chan struct{}), make(chan struct{})
	requests := make(chan Request, 2)
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
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want discovery retry coalesced with startup", refreshes.Load())
	}
	seenRange := false
	for range 2 {
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
	requests := make(chan Request, 2)
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
		Notify: func(msg Event) {
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
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshes = %d, want queued retry coalesced with startup", got)
	}
	seenRange := 0
	for range 2 {
		if req := <-requests; req.Since.Equal(since) {
			seenRange++
		}
	}
	if seenRange != 1 {
		t.Fatalf("range operations = %d, want queued request exactly once", seenRange)
	}
}

func TestPeriodicRefreshRetriesFailedDiscovery(t *testing.T) {
	destination := &fakeDestination{installed: true}
	var loads atomic.Int32
	refreshed := make(chan struct{})
	var once sync.Once
	job := fakeJob{name: "core", destination: destination, refresh: func(context.Context) error {
		once.Do(func() { close(refreshed) })
		return nil
	}}
	c := New(Dependencies{Store: destination, RefreshInterval: 10 * time.Millisecond,
		LoadSources: func(context.Context) ([]Source, error) {
			if loads.Add(1) == 1 {
				return nil, errors.New("temporary discovery failure")
			}
			return []Source{job}, nil
		},
	})
	c.Start(context.Background())
	defer c.Close()
	waitClosed(t, refreshed, "automatic discovery retry")
	if got := loads.Load(); got != 2 {
		t.Fatalf("source discovery attempts = %d, want 2", got)
	}
}

func TestManualRequestDuringPeriodicDiscoveryRetryWaitsForItsBatch(t *testing.T) {
	destination := &fakeDestination{installed: true}
	discoveryStarted, indexStarted := make(chan struct{}), make(chan struct{})
	releaseDiscovery, allowBatch, releaseIndex := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var discoveryOnce, batchOnce, releaseOnce sync.Once
	var loads, indexes atomic.Int32
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, req Request) (Result, error) {
		indexes.Add(1)
		close(indexStarted)
		select {
		case <-allowBatch:
			req.Progress(Progress{Commits: 1, Batches: 1})
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
		select {
		case <-releaseIndex:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	c := New(Dependencies{Store: destination, RefreshInterval: 5 * time.Millisecond,
		LoadSources: func(ctx context.Context) ([]Source, error) {
			if loads.Add(1) == 1 {
				return nil, errors.New("temporary discovery failure")
			}
			close(discoveryStarted)
			select {
			case <-releaseDiscovery:
				return []Source{job}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})
	defer func() {
		c.Stop()
		discoveryOnce.Do(func() { close(releaseDiscovery) })
		batchOnce.Do(func() { close(allowBatch) })
		releaseOnce.Do(func() { close(releaseIndex) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, discoveryStarted, "periodic discovery retry")
	initialDone := make(chan struct{})
	go func() { c.WaitInitial(); close(initialDone) }()
	waitClosed(t, initialDone, "startup despite background discovery retry")

	c.Retry(context.Background(), "core")
	manualDone := make(chan struct{})
	go func() { c.WaitInitial(); close(manualDone) }()
	select {
	case <-manualDone:
		t.Fatal("manual retry did not wait for in-flight discovery")
	case <-time.After(20 * time.Millisecond):
	}
	discoveryOnce.Do(func() { close(releaseDiscovery) })
	waitClosed(t, indexStarted, "discovered source")
	select {
	case <-manualDone:
		t.Fatal("manual retry returned before the source's first durable batch")
	case <-time.After(20 * time.Millisecond):
	}
	batchOnce.Do(func() { close(allowBatch) })
	waitClosed(t, manualDone, "manual retry first durable batch")
	if indexes.Load() != 1 {
		t.Fatalf("index attempts=%d, want one coalesced startup and retry", indexes.Load())
	}
}

func TestCloseDuringDiscoveryAndCallsAfterCloseDoNoWork(t *testing.T) {
	discoveryStarted := make(chan struct{})
	loaderStopped := make(chan struct{})
	canceled := make(chan SyncCancelled, 1)
	var work atomic.Int32
	c := New(Dependencies{LoadSources: func(ctx context.Context) ([]Source, error) {
		work.Add(1)
		close(discoveryStarted)
		<-ctx.Done()
		close(loaderStopped)
		return nil, ctx.Err()
	}, Notify: func(event Event) {
		if event, ok := event.(SyncCancelled); ok {
			canceled <- event
		}
	}})
	c.Start(context.Background())
	<-discoveryStarted
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	waitClosed(t, loaderStopped, "discovery cancellation")
	waitClosed(t, done, "close")
	select {
	case event := <-canceled:
		if event.Source != "repositories" || event.Reason != "shutdown" {
			t.Fatalf("discovery cancellation=%#v", event)
		}
	default:
		t.Fatal("discovery shutdown was not reported")
	}
	c.Retry(context.Background(), "core")
	c.EnsureRange(context.Background(), time.Now())
	c.Start(context.Background())
	if work.Load() != 1 {
		t.Fatalf("source loads = %d, want only initial load", work.Load())
	}
}

func TestCloseDuringInstalledReadReportsCancellation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan SyncCancelled, 1)
	c := New(Dependencies{
		Installed: fakeInstalled{called: started, release: make(chan struct{})},
		Notify: func(event Event) {
			if event, ok := event.(SyncCancelled); ok {
				canceled <- event
			}
		},
	})
	c.Start(context.Background())
	waitClosed(t, started, "installed read")
	c.Close()
	select {
	case event := <-canceled:
		if event.Source != "installed" || event.Reason != "shutdown" {
			t.Fatalf("installed cancellation=%#v", event)
		}
	default:
		t.Fatal("installed shutdown was not reported")
	}
}

func TestSyncStartedPersistenceFailureSkipsSourceWorkAndCommit(t *testing.T) {
	destination := &fakeDestination{installed: true, startErr: errors.New("start write failed")}
	var indexed atomic.Int32
	var messages []Event
	var mu sync.Mutex
	job := fakeJob{name: "core", destination: destination, index: func(context.Context, Request) (Result, error) { indexed.Add(1); return Result{}, nil }}
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(msg Event) { mu.Lock(); messages = append(messages, msg); mu.Unlock() }})
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
		case DatasetChanged:
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

func TestManualRefreshDuringInstalledReadWaitsAndCoalesces(t *testing.T) {
	destination := &fakeDestination{}
	installedStarted, releaseInstalled := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var refreshes atomic.Int32
	job := fakeJob{name: "core", destination: destination, refresh: func(context.Context) error {
		refreshes.Add(1)
		return nil
	}, index: func(_ context.Context, req Request) (Result, error) {
		req.Progress(Progress{Commits: 1, Batches: 1})
		return Result{}, nil
	}}
	c := New(Dependencies{Installed: fakeInstalled{called: installedStarted, release: releaseInstalled}, Store: destination, Sources: []Source{job}})
	defer func() {
		releaseOnce.Do(func() { close(releaseInstalled) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, installedStarted, "installed read")
	c.Refresh(context.Background())
	if got := refreshes.Load(); got != 0 {
		t.Fatalf("refreshes=%d before installed state was ready", got)
	}
	manualDone := make(chan struct{})
	go func() { c.WaitInitial(); close(manualDone) }()
	select {
	case <-manualDone:
		t.Fatal("manual refresh returned before installed state was ready")
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseInstalled) })
	waitClosed(t, manualDone, "coalesced startup refresh")
	c.Wait()
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshes=%d, want one coalesced startup and manual refresh", got)
	}
}

func TestStartupIndexesRecentHistoryBeforeInstalledFallback(t *testing.T) {
	installedCalled := make(chan struct{})
	requests := make(chan Request, 2)
	destination := &fakeDestination{}
	job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
		requests <- req
		return Result{}, nil
	}}
	c := New(Dependencies{
		Installed: fakeInstalled{called: installedCalled, packages: []domain.InstalledPackage{{PackageID: "foo", Type: domain.PackageFormula}}},
		Store:     destination, Sources: []Source{job}, InitialSince: time.Now().Add(-30 * 24 * time.Hour),
	})
	c.Start(context.Background())
	defer c.Close()

	var recent, fallback Request
	select {
	case recent = <-requests:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recent index")
	}
	select {
	case fallback = <-requests:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for installed fallback index")
	}
	if len(recent.Installed) != 0 {
		t.Fatalf("recent index installed targets=%v, want none", recent.Installed)
	}
	if !slices.Equal(fallback.Installed, []domain.PackageID{"foo"}) {
		t.Fatalf("fallback installed targets=%v, want foo", fallback.Installed)
	}
}

func TestWaitInitialReturnsWhileHistoricalFallbackContinues(t *testing.T) {
	installedCalled := make(chan struct{})
	fallbackStarted, releaseFallback := make(chan struct{}), make(chan struct{})
	destination := &fakeDestination{}
	job := fakeJob{name: "homebrew-core", destination: destination, index: func(ctx context.Context, req Request) (Result, error) {
		if !req.HistoricalFallback {
			return Result{Cursor: "current"}, nil
		}
		close(fallbackStarted)
		select {
		case <-releaseFallback:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	c := New(Dependencies{
		Installed: fakeInstalled{called: installedCalled, packages: []domain.InstalledPackage{{PackageID: "foo", Type: domain.PackageFormula}}},
		Store:     destination, Sources: []Source{job},
	})
	c.Start(context.Background())
	waitClosed(t, fallbackStarted, "historical fallback")

	initialDone := make(chan struct{})
	go func() { c.WaitInitial(); close(initialDone) }()
	waitClosed(t, initialDone, "initial synchronization")

	close(releaseFallback)
	c.Close()
}

func TestWaitInitialReturnsAfterFirstDurableRecentBatch(t *testing.T) {
	destination := &fakeDestination{installed: true}
	batchPublished, releaseIndex := make(chan struct{}), make(chan struct{})
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, req Request) (Result, error) {
		req.Progress(Progress{Commits: 10, Events: 3, Batches: 1})
		close(batchPublished)
		select {
		case <-releaseIndex:
			return Result{Events: 3, Cursor: "current"}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	c := New(Dependencies{Store: destination, Sources: []Source{job}})
	c.Start(context.Background())
	defer func() {
		close(releaseIndex)
		c.Close()
	}()
	waitClosed(t, batchPublished, "first durable recent batch")

	initialDone := make(chan struct{})
	go func() { c.WaitInitial(); close(initialDone) }()
	waitClosed(t, initialDone, "initial synchronization after first batch")
}

func TestRefreshFetchesBeforeIndexingRepository(t *testing.T) {
	destination := &fakeDestination{installed: true}
	var sequence []string
	job := fakeJob{
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
	}
	var progress []Progress
	var committed Result
	c := New(Dependencies{
		Store: destination, Sources: []Source{job},
		Notify: func(msg Event) {
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

	if want := []string{"refresh", "fresh"}; !slices.Equal(sequence, want) {
		t.Fatalf("work sequence = %v, want %v", sequence, want)
	}
	wantProgress := []Progress{{Commits: 50, Events: 2, Batches: 1}}
	if !slices.Equal(progress, wantProgress) {
		t.Fatalf("progress = %#v, want %#v", progress, wantProgress)
	}
	if committed.Events != 2 || committed.Cursor != "fresh-head" {
		t.Fatalf("committed result = %#v, want 2 events at fresh-head", committed)
	}
}

func TestRefreshCoalescesWithoutCancelingOrOverlappingActiveIndexing(t *testing.T) {
	destination := &fakeDestination{installed: true}
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	var calls, active, maximum atomic.Int32
	queued := make(chan SyncQueued, 10)
	var messagesMu sync.Mutex
	var messages []Event
	job := fakeJob{
		name: "core", destination: destination,
		index: func(ctx context.Context, req Request) (Result, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := maximum.Load(); n > old; old = maximum.Load() {
				if maximum.CompareAndSwap(old, n) {
					break
				}
			}
			switch calls.Add(1) {
			case 1:
				req.Progress(Progress{Commits: 10, Events: 1, Batches: 1})
				close(firstStarted)
				select {
				case <-releaseFirst:
					return Result{Events: 1, Cursor: "old-head"}, nil
				case <-ctx.Done():
					return Result{}, ctx.Err()
				}
			case 2:
				req.Progress(Progress{Commits: 10, Events: 2, Batches: 1})
				close(secondStarted)
				return Result{Events: 2, Cursor: "new-head"}, nil
			default:
				return Result{}, fmt.Errorf("unexpected index call %d", calls.Load())
			}
		},
	}
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(msg Event) {
		messagesMu.Lock()
		messages = append(messages, msg)
		messagesMu.Unlock()
		if event, ok := msg.(SyncQueued); ok {
			queued <- event
		}
	}})
	defer func() {
		releaseOnce.Do(func() { close(releaseFirst) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, firstStarted, "initial catch-up")
	c.WaitInitial()
	firstQueued := <-queued
	for range 20 {
		c.Refresh(context.Background())
		c.Retry(context.Background(), "core")
	}
	var refreshQueued SyncQueued
	select {
	case refreshQueued = <-queued:
	case <-time.After(time.Second):
		t.Fatal("refresh was not reported as queued")
	}
	if firstQueued.ID == refreshQueued.ID || refreshQueued.HistoricalFallback {
		t.Fatalf("queued refresh = %#v after %#v", refreshQueued, firstQueued)
	}
	select {
	case <-secondStarted:
		t.Fatal("refresh overlapped active indexing")
	default:
	}
	waitInitial := make(chan struct{})
	go func() { c.WaitInitial(); close(waitInitial) }()
	select {
	case <-waitInitial:
		t.Fatal("WaitInitial returned before the queued refresh started")
	default:
	}
	releaseOnce.Do(func() { close(releaseFirst) })
	waitClosed(t, secondStarted, "coalesced refresh")
	waitClosed(t, waitInitial, "coalesced refresh initial batch")
	c.Wait()
	if calls.Load() != 2 || maximum.Load() != 1 {
		t.Fatalf("index calls=%d maximum concurrency=%d, want 2 serialized calls", calls.Load(), maximum.Load())
	}
	destination.mu.Lock()
	finishedRuns := append([]int64(nil), destination.finishedRuns...)
	destination.mu.Unlock()
	if len(finishedRuns) != 2 || finishedRuns[0] == finishedRuns[1] {
		t.Fatalf("finished run IDs = %v, want distinct completed attempts", finishedRuns)
	}
	messagesMu.Lock()
	defer messagesMu.Unlock()
	for _, msg := range messages {
		if event, ok := msg.(SyncCancelled); ok {
			t.Fatalf("refresh canceled useful indexing: %#v", event)
		}
	}
}

func TestRefreshTakesPriorityOverUnstartedFallback(t *testing.T) {
	destination := &fakeDestination{}
	initialSince := time.Now().Add(-30 * 24 * time.Hour).UTC()
	earlier := initialSince.Add(-60 * 24 * time.Hour)
	initialStarted, rangeStarted := make(chan struct{}), make(chan struct{})
	releaseInitial, releaseRange := make(chan struct{}), make(chan struct{})
	var initialOnce, rangeOnce sync.Once
	fallbackQueued := make(chan struct{}, 1)
	order := make(chan string, 4)
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, req Request) (Result, error) {
		switch {
		case req.HistoricalFallback:
			order <- "fallback"
		case req.Since.Equal(initialSince):
			order <- "initial"
			close(initialStarted)
			select {
			case <-releaseInitial:
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		case req.Since.Equal(earlier):
			order <- "range"
			close(rangeStarted)
			select {
			case <-releaseRange:
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		default:
			order <- "refresh"
		}
		return Result{}, nil
	}}
	c := New(Dependencies{
		Store: destination, InitialSince: initialSince,
		Installed: fakeInstalled{called: make(chan struct{}), packages: []domain.InstalledPackage{{PackageID: "pkg"}}},
		Sources:   []Source{job}, Notify: func(event Event) {
			if event, ok := event.(SyncQueued); ok && event.HistoricalFallback {
				select {
				case fallbackQueued <- struct{}{}:
				default:
				}
			}
		},
	})
	defer func() {
		initialOnce.Do(func() { close(releaseInitial) })
		rangeOnce.Do(func() { close(releaseRange) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, initialStarted, "initial index")
	c.EnsureRange(context.Background(), earlier)
	initialOnce.Do(func() { close(releaseInitial) })
	waitClosed(t, rangeStarted, "range index")
	select {
	case <-fallbackQueued:
	case <-time.After(time.Second):
		t.Fatal("fallback was not queued behind range")
	}
	c.Refresh(context.Background())
	rangeOnce.Do(func() { close(releaseRange) })
	c.Wait()
	var got []string
	for range 4 {
		got = append(got, <-order)
	}
	if want := []string{"initial", "range", "refresh", "fallback"}; !slices.Equal(got, want) {
		t.Fatalf("source work order=%v want=%v", got, want)
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
	var messages []Event
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(msg Event) { messages = append(messages, msg) }})
	c.Start(context.Background())
	c.Wait()
	var sequence []string
	for _, message := range messages {
		switch event := message.(type) {
		case SyncStarted:
			sequence = append(sequence, "started")
		case SyncProgress:
			sequence = append(sequence, fmt.Sprintf("progress-%d", event.Progress.Batches))
		case DatasetChanged:
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

func TestCoordinatorRefreshesSourcesAtConfiguredInterval(t *testing.T) {
	destination := &fakeDestination{installed: true}
	var refreshes atomic.Int32
	secondRefresh := make(chan struct{}, 1)
	job := fakeJob{
		name:        "core",
		destination: destination,
		refresh: func(context.Context) error {
			if refreshes.Add(1) == 2 {
				secondRefresh <- struct{}{}
			}
			return nil
		},
		index: func(context.Context, Request) (Result, error) { return Result{}, nil },
	}
	c := New(Dependencies{Store: destination, Sources: []Source{job}, RefreshInterval: 5 * time.Millisecond})
	c.Start(context.Background())
	defer c.Close()

	select {
	case <-secondRefresh:
	case <-time.After(time.Second):
		t.Fatalf("background refreshes = %d, want at least two", refreshes.Load())
	}
}

func TestPeriodicTickDoesNotExtendStartupReadiness(t *testing.T) {
	destination := &fakeDestination{installed: true}
	started, batch, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var batchOnce, releaseOnce sync.Once
	var indexes atomic.Int32
	queued := make(chan SyncQueued, 4)
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, req Request) (Result, error) {
		if indexes.Add(1) != 1 {
			return Result{}, errors.New("periodic index overlapped initial work")
		}
		close(started)
		select {
		case <-batch:
			req.Progress(Progress{Commits: 1, Batches: 1})
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
		select {
		case <-release:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	c := New(Dependencies{Store: destination, Sources: []Source{job}, RefreshInterval: 5 * time.Millisecond, Notify: func(event Event) {
		if event, ok := event.(SyncQueued); ok {
			queued <- event
		}
	}})
	defer func() {
		c.Stop()
		batchOnce.Do(func() { close(batch) })
		releaseOnce.Do(func() { close(release) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, started, "initial index")
	<-queued
	select {
	case event := <-queued:
		t.Fatalf("periodic work queued before initial readiness: %#v", event)
	case <-time.After(20 * time.Millisecond):
	}
	batchOnce.Do(func() { close(batch) })
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("periodic refresh not queued after first durable batch")
	}
	initialDone := make(chan struct{})
	go func() { c.WaitInitial(); close(initialDone) }()
	waitClosed(t, initialDone, "initial readiness with periodic work queued")
}

func TestPeriodicRefreshDoesNotBlockInitialWaitButManualCoalescingDoes(t *testing.T) {
	destination := &fakeDestination{}
	fallbackStarted, refreshStarted := make(chan struct{}), make(chan struct{})
	allowCheckpoint, allowBatch, releaseRefresh := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var checkpointOnce, batchOnce, releaseOnce sync.Once
	var normalIndexes atomic.Int32
	queued := make(chan SyncQueued, 10)
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, req Request) (Result, error) {
		if req.HistoricalFallback {
			close(fallbackStarted)
			select {
			case <-allowCheckpoint:
				req.Progress(Progress{Commits: 1, Batches: 1})
				return Result{}, ctx.Err()
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		}
		if normalIndexes.Add(1) == 1 {
			return Result{}, nil
		}
		close(refreshStarted)
		select {
		case <-allowBatch:
			req.Progress(Progress{Commits: 1, Batches: 1})
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
		select {
		case <-releaseRefresh:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	c := New(Dependencies{
		Store: destination, Installed: fakeInstalled{called: make(chan struct{}), packages: []domain.InstalledPackage{{PackageID: "pkg"}}},
		Sources: []Source{job}, RefreshInterval: 10 * time.Millisecond,
		Notify: func(event Event) {
			if event, ok := event.(SyncQueued); ok && !event.HistoricalFallback {
				queued <- event
			}
		},
	})
	defer func() {
		c.Stop()
		checkpointOnce.Do(func() { close(allowCheckpoint) })
		batchOnce.Do(func() { close(allowBatch) })
		releaseOnce.Do(func() { close(releaseRefresh) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, fallbackStarted, "historical fallback")
	<-queued // startup source
	select {
	case <-queued: // periodic refresh, queued behind fallback
	case <-time.After(time.Second):
		t.Fatal("periodic refresh was not queued")
	}
	initialDone := make(chan struct{})
	go func() { c.WaitInitial(); close(initialDone) }()
	waitClosed(t, initialDone, "startup despite queued periodic refresh")

	c.Refresh(context.Background())
	manualDone := make(chan struct{})
	go func() { c.WaitInitial(); close(manualDone) }()
	select {
	case <-manualDone:
		t.Fatal("manual request did not wait for the coalesced refresh")
	case <-time.After(20 * time.Millisecond):
	}
	checkpointOnce.Do(func() { close(allowCheckpoint) })
	waitClosed(t, refreshStarted, "coalesced periodic refresh")
	select {
	case <-manualDone:
		t.Fatal("manual request returned before the refresh's first durable batch")
	case <-time.After(20 * time.Millisecond):
	}
	batchOnce.Do(func() { close(allowBatch) })
	waitClosed(t, manualDone, "coalesced refresh's first durable batch")
	if got := normalIndexes.Load(); got != 2 {
		t.Fatalf("normal index attempts=%d, want startup and one coalesced refresh", got)
	}
}

func TestPeriodicRefreshYieldsFallbackOnlyAtDurableCheckpoint(t *testing.T) {
	destination := &fakeDestination{installed: true}
	var refreshes, fallbacks atomic.Int32
	fallbackStarted, resumeStarted, secondRefresh := make(chan struct{}), make(chan struct{}), make(chan struct{})
	allowCheckpoint, releaseResume := make(chan struct{}), make(chan struct{})
	var checkpointOnce, resumeOnce sync.Once
	queued := make(chan SyncQueued, 10)
	canceled := make(chan SyncCancelled, 10)
	job := fakeJob{
		name: "core", destination: destination,
		refresh: func(context.Context) error {
			if refreshes.Add(1) == 2 {
				close(secondRefresh)
			}
			return nil
		},
		index: func(ctx context.Context, req Request) (Result, error) {
			if !req.HistoricalFallback {
				return Result{}, nil
			}
			if fallbacks.Add(1) == 1 {
				close(fallbackStarted)
				select {
				case <-allowCheckpoint:
				case <-ctx.Done():
					return Result{}, ctx.Err()
				}
				req.Progress(Progress{Commits: 1, Batches: 1})
				return Result{}, ctx.Err()
			}
			resumeOnce.Do(func() { close(resumeStarted) })
			select {
			case <-releaseResume:
				return Result{}, nil
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		},
	}
	c := New(Dependencies{
		Store: destination, Installed: fakeInstalled{called: make(chan struct{}), packages: []domain.InstalledPackage{{PackageID: "pkg"}}},
		Sources: []Source{job}, RefreshInterval: 10 * time.Millisecond,
		Notify: func(event Event) {
			switch event := event.(type) {
			case SyncQueued:
				if !event.HistoricalFallback {
					queued <- event
				}
			case SyncCancelled:
				canceled <- event
			}
		},
	})
	defer func() {
		checkpointOnce.Do(func() { close(allowCheckpoint) })
		close(releaseResume)
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, fallbackStarted, "historical fallback")
	<-queued // startup
	select {
	case <-queued: // periodic refresh
	case <-time.After(time.Second):
		t.Fatal("periodic refresh was not queued")
	}
	select {
	case <-secondRefresh:
		t.Fatal("periodic refresh overlapped the fallback")
	case event := <-canceled:
		t.Fatalf("fallback canceled before its checkpoint: %#v", event)
	default:
	}
	checkpointOnce.Do(func() { close(allowCheckpoint) })
	waitClosed(t, secondRefresh, "periodic refresh after checkpoint")
	waitClosed(t, resumeStarted, "resumed fallback")
	select {
	case event := <-canceled:
		if !event.HistoricalFallback || event.Reason != errYield.Error() {
			t.Fatalf("yield event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("fallback yield reason was not reported")
	}
	destination.mu.Lock()
	defer destination.mu.Unlock()
	found := false
	for _, err := range destination.finishErrors {
		if err != nil && strings.Contains(err.Error(), "historical fallback: canceled (yield for queued refresh at durable checkpoint)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("persisted fallback yield missing: %v", destination.finishErrors)
	}
}

func TestHistoricalFallbackReportsErrorsWithoutUpdatingSuccessfulSync(t *testing.T) {
	for _, fallbackErr := range []error{nil, errors.New("history read failed")} {
		t.Run(fmt.Sprintf("error=%v", fallbackErr), func(t *testing.T) {
			destination := &fakeDestination{}
			var messages []Event
			job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
				if req.HistoricalFallback {
					req.Progress(Progress{Commits: 1, Batches: 1})
					return Result{Events: 1}, fallbackErr
				}
				return Result{Cursor: "head"}, nil
			}}
			c := New(Dependencies{
				Store: destination, Installed: fakeInstalled{called: make(chan struct{}), packages: []domain.InstalledPackage{{PackageID: "pkg"}}},
				Sources: []Source{job}, Notify: func(event Event) { messages = append(messages, event) },
			})
			c.Start(context.Background())
			c.Wait()
			defer c.Close()
			started, progress, committed, failed := false, false, false, false
			for _, event := range messages {
				switch event := event.(type) {
				case SyncStarted:
					started = started || event.HistoricalFallback
				case SyncProgress:
					progress = progress || event.HistoricalFallback
				case SyncCommitted:
					committed = committed || event.HistoricalFallback
				case SyncFailed:
					failed = failed || event.HistoricalFallback
				}
			}
			if !started || !progress || committed != (fallbackErr == nil) || failed != (fallbackErr != nil) {
				t.Fatalf("fallback lifecycle started=%v progress=%v committed=%v failed=%v", started, progress, committed, failed)
			}
			destination.mu.Lock()
			defer destination.mu.Unlock()
			if fallbackErr == nil && len(destination.finishes) != 1 {
				t.Fatalf("successful fallback changed refresh freshness: %v", destination.finishes)
			}
			if fallbackErr != nil && (len(destination.finishErrors) != 2 || destination.finishErrors[1] == nil || !strings.Contains(destination.finishErrors[1].Error(), "historical fallback: history read failed")) {
				t.Fatalf("fallback failure not persisted distinctly: %v", destination.finishErrors)
			}
		})
	}
}

func TestCoordinatorPublishesScanHeartbeatsWithoutReloadingUndurableData(t *testing.T) {
	destination := &fakeDestination{installed: true}
	job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
		req.Progress(Progress{Commits: 18_158, Events: 12_751, Batches: 0})
		req.Progress(Progress{Commits: 18_248, Events: 12_760, Batches: 1})
		return Result{Events: 12_760}, nil
	}}
	var messages []Event
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(msg Event) { messages = append(messages, msg) }})
	c.Start(context.Background())
	c.Wait()
	heartbeats, changed := 0, 0
	for _, message := range messages {
		switch message.(type) {
		case SyncProgress:
			heartbeats++
		case DatasetChanged:
			changed++
		}
	}
	if heartbeats != 2 || changed != 2 {
		t.Fatalf("progress messages=%d dataset changes=%d, want two status updates and durable/final reloads", heartbeats, changed)
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
	var messages []Event
	c := New(Dependencies{Installed: fakeInstalled{called: make(chan struct{})}, Store: destination, Sources: jobs, Notify: func(msg Event) { mu.Lock(); messages = append(messages, msg); mu.Unlock() }})
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
		case DatasetChanged:
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

func TestCancellationRecordsFinishedRunWithLiveCleanupContext(t *testing.T) {
	destination := &fakeDestination{installed: true}
	canceled := make(chan SyncCancelled, 1)
	started := make(chan struct{})
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, _ Request) (Result, error) {
		close(started)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(event Event) {
		if event, ok := event.(SyncCancelled); ok {
			canceled <- event
		}
	}})
	c.Start(context.Background())
	<-started
	c.Close()

	destination.mu.Lock()
	defer destination.mu.Unlock()
	if !slices.Equal(destination.finishes, []string{"core"}) {
		t.Fatalf("finished runs=%v, want canceled core run", destination.finishes)
	}
	if destination.finishContextErr != nil {
		t.Fatalf("cleanup context error=%v, want live persistence context", destination.finishContextErr)
	}
	if len(destination.finishErrors) != 1 || destination.finishErrors[0] == nil || !strings.Contains(destination.finishErrors[0].Error(), "canceled (shutdown)") {
		t.Fatalf("recorded cancellation = %v, want shutdown reason", destination.finishErrors)
	}
	select {
	case event := <-canceled:
		if event.Reason != "shutdown" {
			t.Fatalf("cancellation reason = %q, want shutdown", event.Reason)
		}
	default:
		t.Fatal("shutdown cancellation was not published")
	}
}

func TestNestedNormalizedCancellationIsNotReportedAsFailure(t *testing.T) {
	destination := &fakeDestination{installed: true}
	started := make(chan struct{})
	failures := make(chan SyncFailed, 1)
	canceled := make(chan SyncCancelled, 1)
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, _ Request) (Result, error) {
		close(started)
		<-ctx.Done()
		return Result{}, fmt.Errorf("read history: %w", errors.Join(ctx.Err(), fmt.Errorf("close stream: %w", ctx.Err())))
	}}
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(event Event) {
		switch event := event.(type) {
		case SyncCancelled:
			canceled <- event
		case SyncFailed:
			failures <- event
		}
	}})
	c.Start(context.Background())
	waitClosed(t, started, "history read")
	c.Close()
	select {
	case event := <-canceled:
		if event.Reason != "shutdown" {
			t.Fatalf("cancellation reason=%q", event.Reason)
		}
	default:
		t.Fatal("shutdown cancellation was not reported")
	}
	select {
	case event := <-failures:
		t.Fatalf("normalized cancellation reported as failure: %v", event.Err)
	default:
	}
	destination.mu.Lock()
	defer destination.mu.Unlock()
	if len(destination.finishErrors) != 1 || destination.finishErrors[0] == nil || !strings.Contains(destination.finishErrors[0].Error(), "canceled (shutdown)") {
		t.Fatalf("recorded cancellation=%v", destination.finishErrors)
	}
}

func TestParentCancellationRetainsItsCause(t *testing.T) {
	destination := &fakeDestination{installed: true}
	started := make(chan struct{})
	canceled := make(chan SyncCancelled, 1)
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, _ Request) (Result, error) {
		close(started)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}
	ctx, cancel := context.WithCancelCause(context.Background())
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(event Event) {
		if event, ok := event.(SyncCancelled); ok {
			canceled <- event
		}
	}})
	c.Start(ctx)
	waitClosed(t, started, "source index")
	cancel(errors.New("parent shutdown"))
	c.Wait()
	defer c.Close()
	select {
	case event := <-canceled:
		if event.Reason != "parent shutdown" {
			t.Fatalf("cancellation reason = %q", event.Reason)
		}
	default:
		t.Fatal("parent cancellation was not published")
	}
	destination.mu.Lock()
	defer destination.mu.Unlock()
	if len(destination.finishErrors) != 1 || destination.finishErrors[0] == nil || !strings.Contains(destination.finishErrors[0].Error(), "canceled (parent shutdown)") {
		t.Fatalf("recorded cancellation = %v", destination.finishErrors)
	}
}

func TestSourceDeadlineHasDistinctCancellationReason(t *testing.T) {
	destination := &fakeDestination{installed: true}
	canceled := make(chan SyncCancelled, 1)
	job := fakeJob{name: "core", destination: destination, index: func(context.Context, Request) (Result, error) {
		return Result{}, context.DeadlineExceeded
	}}
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(event Event) {
		if event, ok := event.(SyncCancelled); ok {
			canceled <- event
		}
	}})
	c.Start(context.Background())
	c.Wait()
	defer c.Close()
	select {
	case event := <-canceled:
		if event.Reason != context.DeadlineExceeded.Error() {
			t.Fatalf("deadline reason=%q", event.Reason)
		}
	default:
		t.Fatal("deadline cancellation was not reported")
	}
	destination.mu.Lock()
	defer destination.mu.Unlock()
	if len(destination.finishErrors) != 1 || destination.finishErrors[0] == nil || !strings.Contains(destination.finishErrors[0].Error(), "canceled (context deadline exceeded)") {
		t.Fatalf("recorded deadline=%v", destination.finishErrors)
	}
}

func TestCancellationDoesNotHideCheckpointFailure(t *testing.T) {
	destination := &fakeDestination{installed: true, finishErr: errors.New("status write failed")}
	started := make(chan struct{})
	failures := make(chan SyncFailed, 1)
	canceled := make(chan SyncCancelled, 1)
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, _ Request) (Result, error) {
		close(started)
		<-ctx.Done()
		return Result{}, errors.Join(ctx.Err(), errors.New("checkpoint write failed"))
	}}
	c := New(Dependencies{Store: destination, Sources: []Source{job}, Notify: func(event Event) {
		switch event := event.(type) {
		case SyncCancelled:
			canceled <- event
		case SyncFailed:
			failures <- event
		}
	}})
	c.Start(context.Background())
	waitClosed(t, started, "source index")
	c.Close()
	select {
	case event := <-canceled:
		if event.Reason != "shutdown" {
			t.Fatalf("cancellation reason=%q", event.Reason)
		}
	default:
		t.Fatal("shutdown cancellation was not reported")
	}
	select {
	case event := <-failures:
		if !strings.Contains(event.Err.Error(), "checkpoint write failed") || !strings.Contains(event.Err.Error(), "status write failed") {
			t.Fatalf("combined failure=%v", event.Err)
		}
	default:
		t.Fatal("checkpoint and status failures were hidden")
	}
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

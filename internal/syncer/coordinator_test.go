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

type installedFunc func(context.Context) ([]domain.InstalledPackage, error)

func (f installedFunc) Installed(ctx context.Context) ([]domain.InstalledPackage, error) {
	return f(ctx)
}

type fakeDestination struct {
	mu                  sync.Mutex
	installed           bool
	packages            []domain.InstalledPackage
	starts, finishes    []string
	finishErrors        []error
	startedRuns         atomic.Int64
	finishedRuns        []int64
	startErr, finishErr error
	installedErr        error
	finishContextErr    error
}

func (f *fakeDestination) SetInstalled(_ context.Context, packages []domain.InstalledPackage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installedErr != nil {
		return f.installedErr
	}
	f.installed = true
	f.packages = append([]domain.InstalledPackage(nil), packages...)
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
	phaseStarted := make(chan SyncStarted, 1)
	canceled := make(chan SyncCancelled, 1)
	var work atomic.Int32
	c := New(Dependencies{LoadSources: func(ctx context.Context) ([]Source, error) {
		work.Add(1)
		close(discoveryStarted)
		<-ctx.Done()
		close(loaderStopped)
		return nil, ctx.Err()
	}, Notify: func(event Event) {
		switch event := event.(type) {
		case SyncStarted:
			if event.Source == "repositories" {
				phaseStarted <- event
			}
		case SyncCancelled:
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
		start := <-phaseStarted
		if start.ID == 0 || start.ID != event.ID {
			t.Fatalf("discovery cancellation ID=%d, start ID=%d", event.ID, start.ID)
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

func TestCloseDuringInstalledReadAndIndexReportsCancellation(t *testing.T) {
	installedStarted, indexStarted := make(chan struct{}), make(chan struct{})
	canceled := make(chan SyncCancelled, 2)
	destination := &fakeDestination{}
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, _ Request) (Result, error) {
		close(indexStarted)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}
	c := New(Dependencies{
		Installed: fakeInstalled{called: installedStarted, release: make(chan struct{})},
		Store:     destination, Sources: []Source{job},
		Notify: func(event Event) {
			if event, ok := event.(SyncCancelled); ok {
				canceled <- event
			}
		},
	})
	c.Start(context.Background())
	waitClosed(t, installedStarted, "installed read")
	waitClosed(t, indexStarted, "recent index")
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	waitClosed(t, closed, "close after installed and index cancellation")
	seen := map[string]uint64{}
	for range 2 {
		select {
		case event := <-canceled:
			if event.Reason != "shutdown" || event.ID == 0 {
				t.Fatalf("cancellation=%#v", event)
			}
			seen[event.Source] = event.ID
		default:
			t.Fatal("missing cancellation for installed read or index")
		}
	}
	if seen["installed"] == 0 || seen["core"] == 0 || seen["installed"] == seen["core"] {
		t.Fatalf("canceled jobs=%v, want distinct installed and core IDs", seen)
	}
	initialDone := make(chan struct{})
	go func() { c.WaitInitial(); close(initialDone) }()
	waitClosed(t, initialDone, "initial wait after close")
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
	if req.HistoricalFallback && f.destination != nil {
		f.destination.mu.Lock()
		installed := f.destination.installed
		f.destination.mu.Unlock()
		if !installed {
			return Result{}, errors.New("fallback started before installed snapshot committed")
		}
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

func waitInstalledJobs(t *testing.T, c *Coordinator, count int) {
	t.Helper()
	ready := make(chan struct{})
	go func() {
		c.mu.Lock()
		for !c.closed && len(c.installedJobs) < count {
			c.cond.Wait()
		}
		c.mu.Unlock()
		close(ready)
	}()
	waitClosed(t, ready, "installed refresh queue")
}

func TestStartIndexesRecentHistoryWhileInstalledLookupIsSlow(t *testing.T) {
	cacheCalled, installedCalled, releaseInstalled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	destination := &fakeDestination{}
	packages := []domain.InstalledPackage{
		{PackageID: "formula", Type: domain.PackageFormula},
		{PackageID: "cask", Type: domain.PackageCask},
	}
	type indexedRequest struct {
		source string
		req    Request
	}
	requests := make(chan indexedRequest, 4)
	var changes atomic.Int32
	sources := make([]Source, 0, 2)
	for _, name := range []string{"core", "cask"} {
		name := name
		sources = append(sources, fakeJob{name: name, destination: destination, refresh: func(context.Context) error {
			select {
			case <-cacheCalled:
				return nil
			default:
				return errors.New("refresh before cached feed")
			}
		}, index: func(_ context.Context, req Request) (Result, error) {
			if !req.HistoricalFallback {
				req.Progress(Progress{Commits: 1, Events: 1, Batches: 1})
			}
			requests <- indexedRequest{source: name, req: req}
			return Result{Events: 1}, nil
		}})
	}
	c := New(Dependencies{
		Cache: fakeCache{cacheCalled}, Installed: fakeInstalled{installedCalled, releaseInstalled, packages},
		Store: destination, Sources: sources, Notify: func(event Event) {
			if _, ok := event.(DatasetChanged); ok {
				changes.Add(1)
			}
		},
	})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(releaseInstalled) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, installedCalled, "installed read")
	seenRecent := map[string]bool{}
	for range 2 {
		select {
		case got := <-requests:
			if got.req.HistoricalFallback || len(got.req.Installed) != 0 {
				t.Fatalf("expected recent index before installed lookup completed, got %#v", got)
			}
			seenRecent[got.source] = true
		case <-time.After(time.Second):
			t.Fatal("recent indexing waited for the installed lookup")
		}
	}
	if !seenRecent["core"] || !seenRecent["cask"] {
		t.Fatalf("recent indexes = %v, want both sources", seenRecent)
	}
	initialDone := make(chan struct{})
	go func() { c.WaitInitial(); close(initialDone) }()
	select {
	case <-initialDone:
		t.Fatal("startup returned before the installed snapshot was committed")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case got := <-requests:
		t.Fatalf("fallback started before the installed snapshot: %#v", got)
	default:
	}

	releaseOnce.Do(func() { close(releaseInstalled) })
	waitClosed(t, initialDone, "initial synchronization")
	c.Wait()
	destination.mu.Lock()
	gotPackages := append([]domain.InstalledPackage(nil), destination.packages...)
	destination.mu.Unlock()
	if !slices.Equal(gotPackages, packages) {
		t.Fatalf("installed snapshot = %v, want %v", gotPackages, packages)
	}
	seenFallback := map[string]bool{}
	for range 2 {
		select {
		case got := <-requests:
			if !got.req.HistoricalFallback || !slices.Equal(got.req.Installed, []domain.PackageID{"formula", "cask"}) {
				t.Fatalf("fallback request = %#v", got)
			}
			seenFallback[got.source] = true
		case <-time.After(time.Second):
			t.Fatal("missing fallback after installed snapshot")
		}
	}
	if !seenFallback["core"] || !seenFallback["cask"] {
		t.Fatalf("fallback indexes = %v, want both sources", seenFallback)
	}
	if got := changes.Load(); got != 7 {
		t.Fatalf("dataset changes = %d, want recent batches and completions, installed snapshot, and fallback completions", got)
	}
}

func TestActionInstalledRefreshWaitsForStartupSnapshot(t *testing.T) {
	destination := &fakeDestination{}
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
	recentStarted := make(chan struct{})
	fallbacks := make(chan Request, 2)
	var firstOnce, secondOnce sync.Once
	var reads atomic.Int32
	job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
		if req.HistoricalFallback {
			fallbacks <- req
		} else {
			close(recentStarted)
		}
		return Result{}, nil
	}}
	c := New(Dependencies{
		Store: destination, Sources: []Source{job},
		Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
			var started chan struct{}
			var release <-chan struct{}
			var id domain.PackageID
			switch reads.Add(1) {
			case 1:
				started, release, id = firstStarted, releaseFirst, "old"
			case 2:
				started, release, id = secondStarted, releaseSecond, "new"
			default:
				return nil, errors.New("unexpected installed lookup")
			}
			close(started)
			select {
			case <-release:
				return []domain.InstalledPackage{{PackageID: id}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}),
	})
	defer func() {
		firstOnce.Do(func() { close(releaseFirst) })
		secondOnce.Do(func() { close(releaseSecond) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, firstStarted, "startup installed lookup")
	waitClosed(t, recentStarted, "recent index")
	actionDone := make(chan error, 1)
	go func() { actionDone <- c.RefreshInstalled(context.Background()) }()
	waitInstalledJobs(t, c, 2)
	select {
	case <-secondStarted:
		t.Fatal("action lookup overlapped the startup snapshot")
	default:
	}
	firstOnce.Do(func() { close(releaseFirst) })
	waitClosed(t, secondStarted, "action installed lookup")
	select {
	case req := <-fallbacks:
		t.Fatalf("fallback ran before the action snapshot: %#v", req)
	default:
	}
	secondOnce.Do(func() { close(releaseSecond) })
	select {
	case err := <-actionDone:
		if err != nil {
			t.Fatalf("action installed refresh: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("action installed refresh did not finish")
	}
	c.Wait()
	destination.mu.Lock()
	packages := append([]domain.InstalledPackage(nil), destination.packages...)
	destination.mu.Unlock()
	if !slices.Equal(packages, []domain.InstalledPackage{{PackageID: "new"}}) {
		t.Fatalf("final installed snapshot = %v, want action snapshot", packages)
	}
	select {
	case req := <-fallbacks:
		if !slices.Equal(req.Installed, []domain.PackageID{"new"}) {
			t.Fatalf("fallback targets = %v, want action snapshot", req.Installed)
		}
	default:
		t.Fatal("fallback was not scheduled for the action snapshot")
	}
	select {
	case req := <-fallbacks:
		t.Fatalf("duplicate fallback after serialized refresh: %#v", req)
	default:
	}
}

func TestActionInstalledRefreshSchedulesFallbackForNewPackages(t *testing.T) {
	destination := &fakeDestination{}
	requests := make(chan Request, 3)
	var reads atomic.Int32
	job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
		requests <- req
		return Result{}, nil
	}}
	c := New(Dependencies{
		Store: destination, Sources: []Source{job},
		Installed: installedFunc(func(context.Context) ([]domain.InstalledPackage, error) {
			if reads.Add(1) == 1 {
				return []domain.InstalledPackage{{PackageID: "old"}}, nil
			}
			return []domain.InstalledPackage{{PackageID: "new"}}, nil
		}),
	})
	defer c.Close()
	c.Start(context.Background())
	c.Wait()
	var recent, initialFallback Request
	select {
	case recent = <-requests:
	default:
		t.Fatal("initial recent index did not run")
	}
	select {
	case initialFallback = <-requests:
	default:
		t.Fatal("initial installed fallback did not run")
	}
	if recent.HistoricalFallback || !initialFallback.HistoricalFallback || !slices.Equal(initialFallback.Installed, []domain.PackageID{"old"}) {
		t.Fatalf("initial requests recent=%#v fallback=%#v", recent, initialFallback)
	}
	if err := c.RefreshInstalled(context.Background()); err != nil {
		t.Fatalf("action installed refresh: %v", err)
	}
	c.Wait()
	select {
	case req := <-requests:
		if !req.HistoricalFallback || !slices.Equal(req.Installed, []domain.PackageID{"new"}) {
			t.Fatalf("action fallback request = %#v", req)
		}
	default:
		t.Fatal("new installed snapshot did not schedule fallback")
	}
}

func TestInstalledSnapshotReadinessAndCopy(t *testing.T) {
	secondStarted := make(chan struct{})
	var reads atomic.Int32
	type snapshot struct {
		ids   []domain.PackageID
		ready bool
	}
	committed := make(chan snapshot, 2)
	var c *Coordinator
	c = New(Dependencies{
		Store: &fakeDestination{},
		Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
			switch reads.Add(1) {
			case 1:
				return []domain.InstalledPackage{{PackageID: "foo"}}, nil
			case 2:
				close(secondStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			default:
				return nil, nil
			}
		}),
		Notify: func(event Event) {
			if event, ok := event.(SyncCommitted); ok && event.Source == "installed" {
				ids, ready := c.InstalledSnapshot()
				committed <- snapshot{ids: ids, ready: ready}
			}
		},
	})
	defer c.Close()
	if ids, ready := c.InstalledSnapshot(); ready || ids != nil {
		t.Fatalf("snapshot before first lookup = %v, ready=%v", ids, ready)
	}
	if err := c.RefreshInstalled(context.Background()); err != nil {
		t.Fatalf("first installed refresh: %v", err)
	}
	first := <-committed
	if !first.ready || !slices.Equal(first.ids, []domain.PackageID{"foo"}) {
		t.Fatalf("snapshot at commit = %#v", first)
	}
	first.ids[0] = "modified"
	if ids, ready := c.InstalledSnapshot(); !ready || !slices.Equal(ids, []domain.PackageID{"foo"}) {
		t.Fatalf("snapshot changed through returned slice: %v, ready=%v", ids, ready)
	}
	ctx, cancel := context.WithCancel(context.Background())
	actionDone := make(chan error, 1)
	go func() { actionDone <- c.RefreshInstalled(ctx) }()
	waitClosed(t, secondStarted, "second installed lookup")
	if ids, ready := c.InstalledSnapshot(); ready || ids != nil {
		t.Fatalf("snapshot during lookup = %v, ready=%v", ids, ready)
	}
	cancel()
	select {
	case err := <-actionDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second lookup cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second lookup ignored cancellation")
	}
	c.Wait()
	if ids, ready := c.InstalledSnapshot(); ready || ids != nil {
		t.Fatalf("snapshot after canceled lookup = %v, ready=%v", ids, ready)
	}
	if err := c.RefreshInstalled(context.Background()); err != nil {
		t.Fatalf("empty installed refresh: %v", err)
	}
	empty := <-committed
	if !empty.ready || len(empty.ids) != 0 {
		t.Fatalf("empty committed snapshot = %#v", empty)
	}
}

func TestInstalledSnapshotUnavailableWhileLaterLookupQueued(t *testing.T) {
	firstCommitted, releaseCommit := make(chan struct{}), make(chan struct{})
	secondStarted, releaseSecond := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	var commitOnce, releaseOnce, secondOnce sync.Once
	c := New(Dependencies{
		Store: &fakeDestination{},
		Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
			if reads.Add(1) == 1 {
				return []domain.InstalledPackage{{PackageID: "old"}}, nil
			}
			close(secondStarted)
			select {
			case <-releaseSecond:
				return []domain.InstalledPackage{{PackageID: "new"}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}),
		Notify: func(event Event) {
			if event, ok := event.(SyncCommitted); ok && event.Source == "installed" {
				commitOnce.Do(func() {
					close(firstCommitted)
					<-releaseCommit
				})
			}
		},
	})
	defer func() {
		releaseOnce.Do(func() { close(releaseCommit) })
		secondOnce.Do(func() { close(releaseSecond) })
		c.Close()
	}()
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { firstDone <- c.RefreshInstalled(context.Background()) }()
	waitClosed(t, firstCommitted, "first installed commit")
	if ids, ready := c.InstalledSnapshot(); !ready || !slices.Equal(ids, []domain.PackageID{"old"}) {
		t.Fatalf("first committed snapshot = %v, ready=%v", ids, ready)
	}
	go func() { secondDone <- c.RefreshInstalled(context.Background()) }()
	waitInstalledJobs(t, c, 2)
	if ids, ready := c.InstalledSnapshot(); ready || ids != nil {
		t.Fatalf("snapshot while a later lookup is queued = %v, ready=%v", ids, ready)
	}
	releaseOnce.Do(func() { close(releaseCommit) })
	waitClosed(t, secondStarted, "queued installed lookup")
	secondOnce.Do(func() { close(releaseSecond) })
	for _, done := range []<-chan error{firstDone, secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("installed refresh: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("installed refresh did not finish")
		}
	}
	if ids, ready := c.InstalledSnapshot(); !ready || !slices.Equal(ids, []domain.PackageID{"new"}) {
		t.Fatalf("latest committed snapshot = %v, ready=%v", ids, ready)
	}
}

func TestRefreshInstalledBeforeStartPreservesSnapshotOnFailure(t *testing.T) {
	destination := &fakeDestination{}
	lookupErr := errors.New("installed lookup failed")
	var reads, changes atomic.Int32
	c := New(Dependencies{
		Store: destination,
		Installed: installedFunc(func(context.Context) ([]domain.InstalledPackage, error) {
			if reads.Add(1) == 1 {
				return []domain.InstalledPackage{{PackageID: "kept"}}, nil
			}
			return nil, lookupErr
		}),
		Notify: func(event Event) {
			if _, ok := event.(DatasetChanged); ok {
				changes.Add(1)
			}
		},
	})
	defer c.Close()
	if err := c.RefreshInstalled(context.Background()); err != nil {
		t.Fatalf("initial installed refresh: %v", err)
	}
	if err := c.RefreshInstalled(context.Background()); !errors.Is(err, lookupErr) {
		t.Fatalf("failed installed refresh error = %v, want %v", err, lookupErr)
	}
	if ids, ready := c.InstalledSnapshot(); ready || ids != nil {
		t.Fatalf("failed lookup exposed snapshot %v, ready=%v", ids, ready)
	}
	destination.mu.Lock()
	packages := append([]domain.InstalledPackage(nil), destination.packages...)
	destination.mu.Unlock()
	if !slices.Equal(packages, []domain.InstalledPackage{{PackageID: "kept"}}) {
		t.Fatalf("failed refresh replaced installed snapshot: %v", packages)
	}
	if got := changes.Load(); got != 1 {
		t.Fatalf("dataset changes = %d, want only the successful snapshot", got)
	}
}

func TestStartupDoesNotReuseCallerOwnedInstalledLookup(t *testing.T) {
	for _, cancelBeforeStart := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel-before-start=%v", cancelBeforeStart), func(t *testing.T) {
			firstStarted, firstCanceled, releaseFirst := make(chan struct{}), make(chan struct{}), make(chan struct{})
			recentStarted := make(chan struct{})
			fallbacks := make(chan Request, 1)
			destination := &fakeDestination{}
			var reads atomic.Int32
			job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
				if req.HistoricalFallback {
					fallbacks <- req
				} else {
					close(recentStarted)
				}
				return Result{}, nil
			}}
			c := New(Dependencies{
				Store: destination, Sources: []Source{job},
				Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
					if reads.Add(1) == 1 {
						close(firstStarted)
						<-ctx.Done()
						close(firstCanceled)
						<-releaseFirst
						return nil, ctx.Err()
					}
					return []domain.InstalledPackage{{PackageID: "latest"}}, nil
				}),
			})
			ctx, cancel := context.WithCancel(context.Background())
			var releaseOnce sync.Once
			defer func() {
				cancel()
				releaseOnce.Do(func() { close(releaseFirst) })
				c.Close()
			}()
			actionDone := make(chan error, 1)
			go func() { actionDone <- c.RefreshInstalled(ctx) }()
			waitClosed(t, firstStarted, "caller-owned installed lookup")
			if cancelBeforeStart {
				cancel()
				waitClosed(t, firstCanceled, "caller lookup cancellation")
			}
			c.Start(context.Background())
			waitClosed(t, recentStarted, "recent index during canceled lookup")
			waitInstalledJobs(t, c, 2)
			if !cancelBeforeStart {
				cancel()
				waitClosed(t, firstCanceled, "caller lookup cancellation")
			}
			select {
			case err := <-actionDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("caller refresh cancellation = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("caller refresh did not return cancellation")
			}
			initialDone := make(chan struct{})
			go func() { c.WaitInitial(); close(initialDone) }()
			select {
			case <-initialDone:
				t.Fatal("startup finished before its own installed snapshot")
			case <-time.After(20 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(releaseFirst) })
			waitClosed(t, initialDone, "startup installed snapshot")
			c.Wait()
			if got := reads.Load(); got != 2 {
				t.Fatalf("installed reads = %d, want canceled caller and startup", got)
			}
			if ids, ready := c.InstalledSnapshot(); !ready || !slices.Equal(ids, []domain.PackageID{"latest"}) {
				t.Fatalf("startup snapshot = %v, ready=%v", ids, ready)
			}
			select {
			case req := <-fallbacks:
				if !slices.Equal(req.Installed, []domain.PackageID{"latest"}) {
					t.Fatalf("fallback targets = %v", req.Installed)
				}
			default:
				t.Fatal("startup did not schedule fallback for its installed snapshot")
			}
		})
	}
}

func TestCanceledPreStartRefreshDoesNotCancelLaterStartup(t *testing.T) {
	firstStarted, recentStarted := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	destination := &fakeDestination{}
	job := fakeJob{name: "core", destination: destination, index: func(context.Context, Request) (Result, error) {
		close(recentStarted)
		return Result{}, nil
	}}
	c := New(Dependencies{
		Store: destination, Sources: []Source{job},
		Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
			if reads.Add(1) == 1 {
				close(firstStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return nil, nil
		}),
	})
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	actionDone := make(chan error, 1)
	go func() { actionDone <- c.RefreshInstalled(ctx) }()
	waitClosed(t, firstStarted, "pre-start installed lookup")
	cancel()
	select {
	case err := <-actionDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-start refresh cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-start refresh ignored cancellation")
	}
	c.Wait()
	c.Start(context.Background())
	waitClosed(t, recentStarted, "startup after canceled pre-start refresh")
	c.Wait()
	if got := reads.Load(); got != 2 {
		t.Fatalf("installed lookup count = %d, want a new startup lookup", got)
	}
}

func TestStartupParentCancellationStopsPreStartRefresh(t *testing.T) {
	installedStarted, recentStarted := make(chan struct{}), make(chan struct{})
	destination := &fakeDestination{}
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, _ Request) (Result, error) {
		close(recentStarted)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}
	c := New(Dependencies{
		Store: destination, Sources: []Source{job},
		Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
			close(installedStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}),
	})
	defer c.Close()
	actionDone := make(chan error, 1)
	go func() { actionDone <- c.RefreshInstalled(context.Background()) }()
	waitClosed(t, installedStarted, "pre-start installed lookup")
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	waitClosed(t, recentStarted, "startup index")
	cancel()
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	waitClosed(t, done, "parent cancellation of pre-start lookup and source")
	select {
	case err := <-actionDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-start refresh after parent cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-start refresh did not stop with startup parent")
	}
}

func TestRefreshInstalledPreservesSnapshotWhenStoreWriteFails(t *testing.T) {
	destination := &fakeDestination{}
	writeErr := errors.New("installed snapshot write failed")
	var reads, changes atomic.Int32
	c := New(Dependencies{
		Store: destination,
		Installed: installedFunc(func(context.Context) ([]domain.InstalledPackage, error) {
			if reads.Add(1) == 1 {
				return []domain.InstalledPackage{{PackageID: "kept"}}, nil
			}
			return []domain.InstalledPackage{{PackageID: "rejected"}}, nil
		}),
		Notify: func(event Event) {
			if _, ok := event.(DatasetChanged); ok {
				changes.Add(1)
			}
		},
	})
	defer c.Close()
	if err := c.RefreshInstalled(context.Background()); err != nil {
		t.Fatalf("initial installed refresh: %v", err)
	}
	destination.mu.Lock()
	destination.installedErr = writeErr
	destination.mu.Unlock()
	if err := c.RefreshInstalled(context.Background()); !errors.Is(err, writeErr) {
		t.Fatalf("failed installed snapshot write = %v, want %v", err, writeErr)
	}
	if ids, ready := c.InstalledSnapshot(); ready || ids != nil {
		t.Fatalf("failed write exposed snapshot %v, ready=%v", ids, ready)
	}
	destination.mu.Lock()
	packages := append([]domain.InstalledPackage(nil), destination.packages...)
	destination.mu.Unlock()
	c.mu.Lock()
	ids := append([]domain.PackageID(nil), c.installed...)
	c.mu.Unlock()
	if !slices.Equal(packages, []domain.InstalledPackage{{PackageID: "kept"}}) ||
		!slices.Equal(ids, []domain.PackageID{"kept"}) {
		t.Fatalf("failed snapshot write changed packages=%v ids=%v", packages, ids)
	}
	if got := changes.Load(); got != 1 {
		t.Fatalf("dataset changes = %d, want only the successful snapshot", got)
	}
}

func TestCanceledQueuedInstalledRefreshDoesNotBlockShutdown(t *testing.T) {
	started := make(chan struct{})
	var reads atomic.Int32
	c := New(Dependencies{Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
		reads.Add(1)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})})
	c.Start(context.Background())
	waitClosed(t, started, "startup installed lookup")
	ctx, cancel := context.WithCancel(context.Background())
	actionDone := make(chan error, 1)
	go func() { actionDone <- c.RefreshInstalled(ctx) }()
	waitInstalledJobs(t, c, 2)
	cancel()
	select {
	case err := <-actionDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued refresh cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued installed refresh ignored cancellation")
	}
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	waitClosed(t, closed, "shutdown with queued installed refresh")
	if got := reads.Load(); got != 1 {
		t.Fatalf("installed lookups = %d, want canceled queued lookup skipped", got)
	}
}

func TestManualRefreshDuringInstalledReadWaitsAndCoalesces(t *testing.T) {
	destination := &fakeDestination{}
	installedStarted, releaseInstalled := make(chan struct{}), make(chan struct{})
	firstStarted, releaseFirst, manualBatch := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var installedOnce, firstOnce sync.Once
	var refreshes, indexes atomic.Int32
	job := fakeJob{name: "core", destination: destination, refresh: func(context.Context) error {
		refreshes.Add(1)
		return nil
	}, index: func(ctx context.Context, req Request) (Result, error) {
		n := indexes.Add(1)
		if n == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		}
		req.Progress(Progress{Commits: 1, Batches: 1})
		if n == 2 {
			close(manualBatch)
		}
		return Result{}, nil
	}}
	c := New(Dependencies{Installed: fakeInstalled{called: installedStarted, release: releaseInstalled}, Store: destination, Sources: []Source{job}})
	defer func() {
		installedOnce.Do(func() { close(releaseInstalled) })
		firstOnce.Do(func() { close(releaseFirst) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, installedStarted, "installed read")
	waitClosed(t, firstStarted, "startup index")
	c.Refresh(context.Background())
	c.Refresh(context.Background())
	manualDone := make(chan struct{})
	go func() { c.WaitInitial(); close(manualDone) }()
	installedOnce.Do(func() { close(releaseInstalled) })
	select {
	case <-manualDone:
		t.Fatal("manual refresh returned before a durable source batch")
	case <-time.After(20 * time.Millisecond):
	}
	firstOnce.Do(func() { close(releaseFirst) })
	waitClosed(t, manualBatch, "coalesced manual refresh batch")
	waitClosed(t, manualDone, "initial and manual refresh")
	c.Wait()
	if got := refreshes.Load(); got != 2 {
		t.Fatalf("refreshes=%d, want startup and one coalesced manual refresh", got)
	}
}

func TestDiscoveryRetrySharesInFlightInstalledLookup(t *testing.T) {
	destination := &fakeDestination{}
	installedStarted, releaseInstalled := make(chan struct{}), make(chan struct{})
	requests := make(chan Request, 2)
	var reads, loads atomic.Int32
	job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
		requests <- req
		return Result{}, nil
	}}
	var c *Coordinator
	c = New(Dependencies{
		Store: destination,
		Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
			if reads.Add(1) == 1 {
				close(installedStarted)
			}
			select {
			case <-releaseInstalled:
				return []domain.InstalledPackage{{PackageID: "latest"}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}),
		LoadSources: func(context.Context) ([]Source, error) {
			if loads.Add(1) == 1 {
				<-installedStarted
				return nil, errors.New("discovery failed")
			}
			return []Source{job}, nil
		},
		Notify: func(event Event) {
			if failed, ok := event.(SyncFailed); ok && failed.Source == "repositories" {
				c.Retry(context.Background(), "core")
			}
		},
	})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(releaseInstalled) })
		c.Close()
	}()
	c.Start(context.Background())
	select {
	case req := <-requests:
		if req.HistoricalFallback {
			t.Fatal("fallback ran before the retried recent index")
		}
	case <-time.After(time.Second):
		t.Fatal("discovery retry waited for the installed lookup")
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("concurrent installed reads = %d, want one shared lookup", got)
	}
	releaseOnce.Do(func() { close(releaseInstalled) })
	c.Wait()
	if got := loads.Load(); got != 2 {
		t.Fatalf("discovery attempts = %d, want 2", got)
	}
	select {
	case req := <-requests:
		if !req.HistoricalFallback || !slices.Equal(req.Installed, []domain.PackageID{"latest"}) {
			t.Fatalf("retried fallback = %#v", req)
		}
	default:
		t.Fatal("fallback was not scheduled after the shared installed lookup")
	}
}

func TestDiscoveryRetryReplacesSharedLookupThatLaterFails(t *testing.T) {
	destination := &fakeDestination{}
	installedStarted, failRead := make(chan struct{}), make(chan struct{})
	requests := make(chan Request, 2)
	var reads, loads atomic.Int32
	job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
		requests <- req
		return Result{}, nil
	}}
	var c *Coordinator
	c = New(Dependencies{
		Store: destination,
		Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
			if reads.Add(1) == 1 {
				close(installedStarted)
				select {
				case <-failRead:
					return nil, errors.New("installed lookup failed")
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return []domain.InstalledPackage{{PackageID: "latest"}}, nil
		}),
		LoadSources: func(context.Context) ([]Source, error) {
			if loads.Add(1) == 1 {
				<-installedStarted
				return nil, errors.New("discovery failed")
			}
			return []Source{job}, nil
		},
		Notify: func(event Event) {
			if failed, ok := event.(SyncFailed); ok && failed.Source == "repositories" {
				c.Retry(context.Background(), "core")
			}
		},
	})
	var failOnce sync.Once
	defer func() {
		failOnce.Do(func() { close(failRead) })
		c.Close()
	}()
	c.Start(context.Background())
	select {
	case req := <-requests:
		if req.HistoricalFallback {
			t.Fatal("fallback ran before retried recent index")
		}
	case <-time.After(time.Second):
		t.Fatal("discovery retry waited for installed lookup")
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("installed reads before failure = %d, want shared lookup", got)
	}
	failOnce.Do(func() { close(failRead) })
	c.Wait()
	if got := reads.Load(); got != 2 {
		t.Fatalf("installed reads = %d, want replacement after shared failure", got)
	}
	select {
	case req := <-requests:
		if !req.HistoricalFallback || !slices.Equal(req.Installed, []domain.PackageID{"latest"}) {
			t.Fatalf("fallback after replacement = %#v", req)
		}
	default:
		t.Fatal("shared lookup failure lost the replacement fallback")
	}
}

func TestDiscoveryRetryDoesNotReuseFailedInstalledLookup(t *testing.T) {
	destination := &fakeDestination{}
	installedStarted, releaseRead := make(chan struct{}), make(chan struct{})
	discoveryFailed, lookupFailed, releaseFailure := make(chan struct{}), make(chan struct{}), make(chan struct{})
	requests := make(chan Request, 2)
	var reads, loads atomic.Int32
	var readOnce, failureOnce sync.Once
	job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
		requests <- req
		return Result{}, nil
	}}
	c := New(Dependencies{
		Store: destination,
		Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
			if reads.Add(1) == 1 {
				close(installedStarted)
				select {
				case <-releaseRead:
					return nil, errors.New("installed lookup failed")
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return []domain.InstalledPackage{{PackageID: "latest"}}, nil
		}),
		LoadSources: func(context.Context) ([]Source, error) {
			if loads.Add(1) == 1 {
				<-installedStarted
				return nil, errors.New("discovery failed")
			}
			return []Source{job}, nil
		},
		Notify: func(event Event) {
			if event, ok := event.(SyncFailed); ok {
				switch event.Source {
				case "repositories":
					close(discoveryFailed)
				case "installed":
					close(lookupFailed)
					<-releaseFailure
				}
			}
		},
	})
	defer func() {
		readOnce.Do(func() { close(releaseRead) })
		failureOnce.Do(func() { close(releaseFailure) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, discoveryFailed, "initial discovery failure")
	readOnce.Do(func() { close(releaseRead) })
	waitClosed(t, lookupFailed, "installed lookup failure")
	c.Retry(context.Background(), "core")
	waitInstalledJobs(t, c, 2)
	select {
	case req := <-requests:
		if req.HistoricalFallback {
			t.Fatal("fallback ran before retried recent index")
		}
	case <-time.After(time.Second):
		t.Fatal("discovery retry waited for failed installed notification")
	}
	failureOnce.Do(func() { close(releaseFailure) })
	c.Wait()
	if got := reads.Load(); got != 2 {
		t.Fatalf("installed reads = %d, want failed lookup and retry", got)
	}
	select {
	case req := <-requests:
		if !req.HistoricalFallback || !slices.Equal(req.Installed, []domain.PackageID{"latest"}) {
			t.Fatalf("retried fallback = %#v", req)
		}
	default:
		t.Fatal("retry did not schedule fallback for the new snapshot")
	}
}

func TestFailedInstalledLookupKeepsSnapshotAndDoesNotBlockRecentIndex(t *testing.T) {
	cached := []domain.InstalledPackage{{PackageID: "cached"}}
	destination := &fakeDestination{installed: true, packages: append([]domain.InstalledPackage(nil), cached...)}
	installedStarted, releaseInstalled := make(chan struct{}), make(chan struct{})
	requests := make(chan Request, 2)
	started, failed := make(chan SyncStarted, 1), make(chan SyncFailed, 1)
	var changes atomic.Int32
	job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
		requests <- req
		return Result{}, nil
	}}
	c := New(Dependencies{
		Store: destination, Sources: []Source{job},
		Installed: installedFunc(func(ctx context.Context) ([]domain.InstalledPackage, error) {
			close(installedStarted)
			select {
			case <-releaseInstalled:
				return nil, errors.New("installed lookup failed")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}),
		Notify: func(event Event) {
			switch event := event.(type) {
			case SyncStarted:
				if event.Source == "installed" {
					started <- event
				}
			case SyncFailed:
				if event.Source == "installed" {
					failed <- event
				}
			case DatasetChanged:
				changes.Add(1)
			}
		},
	})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(releaseInstalled) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, installedStarted, "installed lookup")
	select {
	case req := <-requests:
		if req.HistoricalFallback {
			t.Fatal("fallback ran without an installed snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("recent index waited for failing installed lookup")
	}
	releaseOnce.Do(func() { close(releaseInstalled) })
	c.Wait()
	start, failure := <-started, <-failed
	if start.ID == 0 || start.ID != failure.ID || !strings.Contains(failure.Err.Error(), "installed lookup failed") {
		t.Fatalf("installed lifecycle start=%#v failure=%#v", start, failure)
	}
	destination.mu.Lock()
	got := append([]domain.InstalledPackage(nil), destination.packages...)
	destination.mu.Unlock()
	if !slices.Equal(got, cached) {
		t.Fatalf("failed lookup replaced snapshot: %v", got)
	}
	if got := changes.Load(); got != 1 {
		t.Fatalf("dataset changes = %d, want only the recent index completion", got)
	}
	select {
	case req := <-requests:
		t.Fatalf("unexpected fallback after failed lookup: %#v", req)
	default:
	}
}

func TestSetupPhaseEventsUseMatchedUniqueIDs(t *testing.T) {
	var mu sync.Mutex
	var events []Event
	c := New(Dependencies{
		Installed: fakeInstalled{called: make(chan struct{})}, Store: &fakeDestination{},
		LoadSources: func(context.Context) ([]Source, error) { return nil, nil },
		Notify: func(event Event) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		},
	})
	c.Start(context.Background())
	c.Wait()
	defer c.Close()
	started, committed := map[string]uint64{}, map[string]uint64{}
	for _, event := range events {
		switch event := event.(type) {
		case SyncStarted:
			started[event.Source] = event.ID
		case SyncCommitted:
			if started[event.Source] != event.ID {
				t.Fatalf("setup completion without matching start: %#v", event)
			}
			committed[event.Source] = event.ID
		}
	}
	if started["installed"] == 0 || started["repositories"] == 0 || started["installed"] == started["repositories"] ||
		committed["installed"] != started["installed"] || committed["repositories"] != started["repositories"] {
		t.Fatalf("setup events started=%v committed=%v", started, committed)
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

func TestSourcePhaseEventsTrackWorkBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		rangeOnly  bool
		fallback   bool
		refreshErr error
		want       []string
	}{
		{
			name: "refresh and index",
			want: []string{"queued", "started", "phase:fetching", "refresh", "phase:indexing", "index", "progress", "committed"},
		},
		{
			name:       "failed refresh",
			refreshErr: errors.New("fetch failed"),
			want:       []string{"queued", "started", "phase:fetching", "refresh", "failed"},
		},
		{
			name: "range without refresh", rangeOnly: true,
			want: []string{"queued", "started", "phase:indexing", "index", "progress", "committed"},
		},
		{
			name: "historical fallback", fallback: true,
			want: []string{"queued", "started", "phase:fetching", "refresh", "phase:indexing", "index", "progress", "committed",
				"fallback:queued", "fallback:started", "fallback:phase:indexing", "fallback:index", "fallback:progress", "fallback:committed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type observation struct {
				label    string
				id       uint64
				fallback bool
				event    bool
				at       time.Time
			}
			trace := make(chan observation, 32)
			push := func(label string, id uint64, fallback, event bool, at time.Time) {
				if fallback {
					label = "fallback:" + label
				}
				trace <- observation{label: label, id: id, fallback: fallback, event: event, at: at}
			}
			destination := &fakeDestination{installed: true}
			job := fakeJob{name: "core", destination: destination, refresh: func(context.Context) error {
				push("refresh", 0, false, false, time.Time{})
				return tt.refreshErr
			}, index: func(_ context.Context, req Request) (Result, error) {
				push("index", 0, req.HistoricalFallback, false, time.Time{})
				req.Progress(Progress{Commits: 1, Batches: 1})
				return Result{}, nil
			}}
			now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			deps := Dependencies{
				Store: destination, Sources: []Source{job}, Now: func() time.Time { return now },
				Notify: func(event Event) {
					switch event := event.(type) {
					case SyncQueued:
						if event.Source == "core" {
							push("queued", event.ID, event.HistoricalFallback, true, event.At)
						}
					case SyncStarted:
						if event.Source == "core" {
							push("started", event.ID, event.HistoricalFallback, true, event.At)
						}
					case SyncPhase:
						if event.Source == "core" {
							push("phase:"+string(event.Phase), event.ID, event.HistoricalFallback, true, event.At)
						}
					case SyncProgress:
						if event.Source == "core" {
							push("progress", event.ID, event.HistoricalFallback, true, event.At)
						}
					case SyncCommitted:
						if event.Source == "core" {
							push("committed", event.ID, event.HistoricalFallback, true, event.At)
						}
					case SyncFailed:
						if event.Source == "core" {
							push("failed", event.ID, event.HistoricalFallback, true, event.At)
						}
					}
				},
			}
			if tt.fallback {
				deps.Installed = fakeInstalled{called: make(chan struct{}), packages: []domain.InstalledPackage{{PackageID: "pkg"}}}
			}
			c := New(deps)
			defer c.Close()
			if tt.rangeOnly {
				c.EnsureRange(context.Background(), now.Add(-24*time.Hour))
			} else {
				c.Start(context.Background())
			}
			c.Wait()
			close(trace)
			var got []string
			ids := map[bool]uint64{}
			for item := range trace {
				got = append(got, item.label)
				if !item.event {
					continue
				}
				if item.id == 0 || !item.at.Equal(now) {
					t.Fatalf("event %q has id=%d time=%v", item.label, item.id, item.at)
				}
				if previous := ids[item.fallback]; previous != 0 && previous != item.id {
					t.Fatalf("event %q changed source job ID from %d to %d", item.label, previous, item.id)
				}
				ids[item.fallback] = item.id
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("phase sequence = %v, want %v", got, tt.want)
			}
			if tt.fallback && ids[false] == ids[true] {
				t.Fatalf("recent and fallback jobs share ID %d", ids[false])
			}
		})
	}
}

func TestCanceledFetchDoesNotPublishIndexingPhase(t *testing.T) {
	destination := &fakeDestination{installed: true}
	fetchStarted := make(chan struct{})
	starts, phases, canceled := make(chan SyncStarted, 1), make(chan SyncPhase, 2), make(chan SyncCancelled, 1)
	var indexed atomic.Bool
	job := fakeJob{name: "core", destination: destination, refresh: func(ctx context.Context) error {
		close(fetchStarted)
		<-ctx.Done()
		return ctx.Err()
	}, index: func(context.Context, Request) (Result, error) {
		indexed.Store(true)
		return Result{}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	c := New(Dependencies{
		Store: destination, Sources: []Source{job},
		Notify: func(event Event) {
			switch event := event.(type) {
			case SyncStarted:
				starts <- event
			case SyncPhase:
				phases <- event
			case SyncCancelled:
				canceled <- event
			}
		},
	})
	defer func() { cancel(); c.Close() }()
	c.Start(ctx)
	waitClosed(t, fetchStarted, "fetch")
	var start SyncStarted
	var phase SyncPhase
	select {
	case start = <-starts:
	default:
		t.Fatal("source start was not reported before fetch")
	}
	select {
	case phase = <-phases:
	default:
		t.Fatal("fetch phase was not reported before fetch")
	}
	if phase.Source != "core" || phase.ID == 0 || phase.ID != start.ID || phase.Phase != PhaseFetching || phase.HistoricalFallback {
		t.Fatalf("fetch phase = %#v, start = %#v", phase, start)
	}
	cancel()
	c.Wait()
	select {
	case event := <-canceled:
		if event.ID != start.ID || event.Source != "core" || event.HistoricalFallback {
			t.Fatalf("fetch cancellation = %#v", event)
		}
	default:
		t.Fatal("fetch cancellation was not reported")
	}
	select {
	case event := <-phases:
		t.Fatalf("unexpected phase after canceled fetch: %#v", event)
	default:
	}
	if indexed.Load() {
		t.Fatal("indexing ran after canceled fetch")
	}
}

func TestSourcePhasesRemainIndependentDuringConcurrentFetches(t *testing.T) {
	destination := &fakeDestination{installed: true}
	coreStarted, caskStarted := make(chan struct{}), make(chan struct{})
	releaseCore, releaseCask := make(chan struct{}), make(chan struct{})
	var coreOnce, caskOnce sync.Once
	job := func(name string, started chan struct{}, release <-chan struct{}) Source {
		return fakeJob{name: name, destination: destination, refresh: func(ctx context.Context) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	}
	messages := make(chan Event, 8)
	c := New(Dependencies{
		Store: destination, Sources: []Source{job("core", coreStarted, releaseCore), job("cask", caskStarted, releaseCask)},
		Notify: func(event Event) {
			switch event.(type) {
			case SyncStarted, SyncPhase:
				messages <- event
			}
		},
	})
	defer func() {
		coreOnce.Do(func() { close(releaseCore) })
		caskOnce.Do(func() { close(releaseCask) })
		c.Close()
	}()
	c.Start(context.Background())
	waitClosed(t, coreStarted, "core fetch")
	waitClosed(t, caskStarted, "cask fetch")
	ids, fetching := map[string]uint64{}, map[string]bool{}
	for len(fetching) < 2 {
		select {
		case event := <-messages:
			switch event := event.(type) {
			case SyncStarted:
				ids[event.Source] = event.ID
			case SyncPhase:
				if event.Phase != PhaseFetching || event.ID == 0 || ids[event.Source] != event.ID || event.HistoricalFallback {
					t.Fatalf("fetch phase = %#v, starts=%v", event, ids)
				}
				fetching[event.Source] = true
			}
		case <-time.After(time.Second):
			t.Fatal("missing per-source fetch phase")
		}
	}
	if ids["core"] == 0 || ids["cask"] == 0 || ids["core"] == ids["cask"] {
		t.Fatalf("source job IDs = %v", ids)
	}
	coreOnce.Do(func() { close(releaseCore) })
	select {
	case event := <-messages:
		phase, ok := event.(SyncPhase)
		if !ok || phase.Source != "core" || phase.ID != ids["core"] || phase.Phase != PhaseIndexing {
			t.Fatalf("phase while cask fetch is blocked = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("core did not advance to indexing")
	}
	caskOnce.Do(func() { close(releaseCask) })
	select {
	case event := <-messages:
		phase, ok := event.(SyncPhase)
		if !ok || phase.Source != "cask" || phase.ID != ids["cask"] || phase.Phase != PhaseIndexing {
			t.Fatalf("cask indexing phase = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("cask did not advance to indexing")
	}
	c.Wait()
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

func TestEmptySnapshotFallbackDoesNotSwallowReplacement(t *testing.T) {
	destination := &fakeDestination{}
	initialSince := time.Now().Add(-30 * 24 * time.Hour).UTC()
	earlier := initialSince.Add(-60 * 24 * time.Hour)
	initialStarted, rangeStarted := make(chan struct{}), make(chan struct{})
	releaseInitial, releaseRange := make(chan struct{}), make(chan struct{})
	installedCommitted, fallbackQueued := make(chan struct{}), make(chan struct{}, 2)
	fallbackRequests := make(chan Request, 1)
	replacementDone := make(chan error, 1)
	var initialOnce, rangeOnce, installedOnce sync.Once
	var reads atomic.Int32
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, req Request) (Result, error) {
		if req.HistoricalFallback {
			fallbackRequests <- req
			return Result{}, nil
		}
		var started chan struct{}
		var release <-chan struct{}
		if req.Since.Equal(initialSince) {
			started, release = initialStarted, releaseInitial
		} else if req.Since.Equal(earlier) {
			started, release = rangeStarted, releaseRange
		} else {
			return Result{}, errors.New("unexpected recent request")
		}
		close(started)
		select {
		case <-release:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	var c *Coordinator
	c = New(Dependencies{
		Store: destination, InitialSince: initialSince, Sources: []Source{job},
		Installed: installedFunc(func(context.Context) ([]domain.InstalledPackage, error) {
			switch reads.Add(1) {
			case 1:
				return []domain.InstalledPackage{{PackageID: "old"}}, nil
			case 2:
				return nil, nil
			case 3:
				return []domain.InstalledPackage{{PackageID: "new"}}, nil
			default:
				return nil, errors.New("unexpected installed lookup")
			}
		}),
		Notify: func(event Event) {
			switch event := event.(type) {
			case SyncCommitted:
				if event.Source == "installed" {
					installedOnce.Do(func() { close(installedCommitted) })
				}
			case SyncQueued:
				if event.HistoricalFallback {
					fallbackQueued <- struct{}{}
				}
			case SyncCancelled:
				if event.HistoricalFallback && event.Reason == "installed snapshot is empty" {
					replacementDone <- c.RefreshInstalled(context.Background())
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
	waitClosed(t, installedCommitted, "initial installed snapshot")
	c.EnsureRange(context.Background(), earlier)
	initialOnce.Do(func() { close(releaseInitial) })
	waitClosed(t, rangeStarted, "range index")
	waitClosed(t, fallbackQueued, "queued old fallback")
	if err := c.RefreshInstalled(context.Background()); err != nil {
		t.Fatalf("empty installed refresh: %v", err)
	}
	rangeOnce.Do(func() { close(releaseRange) })
	select {
	case err := <-replacementDone:
		if err != nil {
			t.Fatalf("replacement installed refresh: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement installed refresh did not complete")
	}
	c.Wait()
	select {
	case req := <-fallbackRequests:
		if !slices.Equal(req.Installed, []domain.PackageID{"new"}) {
			t.Fatalf("replacement fallback targets = %v, want new", req.Installed)
		}
	default:
		t.Fatal("empty-snapshot fallback swallowed the replacement")
	}
	if got := reads.Load(); got != 3 {
		t.Fatalf("installed lookups = %d, want old, empty, and new snapshots", got)
	}
}

func TestQueuedFallbackUsesLatestInstalledSnapshot(t *testing.T) {
	destination := &fakeDestination{}
	initialSince := time.Now().Add(-30 * 24 * time.Hour).UTC()
	earlier := initialSince.Add(-60 * 24 * time.Hour)
	initialStarted, rangeStarted := make(chan struct{}), make(chan struct{})
	releaseInitial, releaseRange := make(chan struct{}), make(chan struct{})
	installedCommitted, fallbackQueued := make(chan struct{}), make(chan struct{}, 1)
	fallbackRequests := make(chan Request, 1)
	var initialOnce, rangeOnce, installedOnce sync.Once
	var reads atomic.Int32
	job := fakeJob{name: "core", destination: destination, index: func(ctx context.Context, req Request) (Result, error) {
		if req.HistoricalFallback {
			fallbackRequests <- req
			return Result{}, nil
		}
		var started chan struct{}
		var release <-chan struct{}
		if req.Since.Equal(initialSince) {
			started, release = initialStarted, releaseInitial
		} else if req.Since.Equal(earlier) {
			started, release = rangeStarted, releaseRange
		} else {
			return Result{}, errors.New("unexpected recent request")
		}
		close(started)
		select {
		case <-release:
			return Result{}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	c := New(Dependencies{
		Store: destination, InitialSince: initialSince, Sources: []Source{job},
		Installed: installedFunc(func(context.Context) ([]domain.InstalledPackage, error) {
			if reads.Add(1) == 1 {
				return []domain.InstalledPackage{{PackageID: "old"}}, nil
			}
			return []domain.InstalledPackage{{PackageID: "new"}}, nil
		}),
		Notify: func(event Event) {
			switch event := event.(type) {
			case SyncCommitted:
				if event.Source == "installed" {
					installedOnce.Do(func() { close(installedCommitted) })
				}
			case SyncQueued:
				if event.HistoricalFallback {
					select {
					case fallbackQueued <- struct{}{}:
					default:
					}
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
	waitClosed(t, installedCommitted, "first installed snapshot")
	c.EnsureRange(context.Background(), earlier)
	initialOnce.Do(func() { close(releaseInitial) })
	waitClosed(t, rangeStarted, "range index")
	waitClosed(t, fallbackQueued, "queued historical fallback")
	if err := c.RefreshInstalled(context.Background()); err != nil {
		t.Fatalf("refresh installed snapshot: %v", err)
	}
	rangeOnce.Do(func() { close(releaseRange) })
	c.Wait()
	select {
	case req := <-fallbackRequests:
		if !slices.Equal(req.Installed, []domain.PackageID{"new"}) {
			t.Fatalf("fallback installed targets = %v, want latest snapshot", req.Installed)
		}
	default:
		t.Fatal("queued historical fallback did not run")
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
			var messagesMu sync.Mutex
			job := fakeJob{name: "core", destination: destination, index: func(_ context.Context, req Request) (Result, error) {
				if req.HistoricalFallback {
					req.Progress(Progress{Commits: 1, Batches: 1})
					return Result{Events: 1}, fallbackErr
				}
				return Result{Cursor: "head"}, nil
			}}
			c := New(Dependencies{
				Store: destination, Installed: fakeInstalled{called: make(chan struct{}), packages: []domain.InstalledPackage{{PackageID: "pkg"}}},
				Sources: []Source{job}, Notify: func(event Event) {
					messagesMu.Lock()
					messages = append(messages, event)
					messagesMu.Unlock()
				},
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
		switch msg := msg.(type) {
		case SyncCommitted:
			if msg.Source != "installed" {
				committed++
			}
		case SyncFailed:
			failed++
		case DatasetChanged:
			changed++
		}
	}
	if committed != 2 || failed != 1 || changed != 3 {
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

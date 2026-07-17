package syncer

import (
	"context"
	"errors"
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
	mu               sync.Mutex
	installed        bool
	starts, finishes []string
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
	return nil
}
func (f *fakeDestination) SyncFinished(_ context.Context, source string, _ time.Time, _ Result, _ error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes = append(f.finishes, source)
	return nil
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

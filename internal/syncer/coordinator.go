// Package syncer coordinates background refresh and history indexing.
package syncer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/anateus/cicerone/internal/domain"
)

const (
	maxErrorText              = 1024
	shutdownPersistenceWindow = 500 * time.Millisecond
)

type CacheReader interface{ LoadCached(context.Context) error }
type InstalledReader interface {
	Installed(context.Context) ([]domain.InstalledPackage, error)
}
type Store interface {
	SetInstalled(context.Context, []domain.InstalledPackage) error
	SyncStarted(context.Context, string, time.Time) error
	SyncFinished(context.Context, string, time.Time, Result, error) error
}

// RunStore preserves the identity of each synchronization attempt. It is
// optional so small test stores and older integrations can keep using Store;
// production stores should implement it whenever source work may overlap.
type RunStore interface {
	SyncStartedRun(context.Context, string, time.Time) (int64, error)
	SyncFinishedRun(context.Context, int64, string, time.Time, Result, error) error
}
type Request struct {
	Since              time.Time
	Installed          []domain.PackageID
	Kinds              map[domain.EventKind]bool
	Progress           func(Progress)
	HistoricalFallback bool
}
type Progress struct{ Commits, Events, Diagnostics, Batches int }
type Result struct {
	Events, Diagnostics int
	Cursor              string
	Since               time.Time
}
type Source interface {
	Name() string
	Refresh(context.Context) error
	Index(context.Context, Request) (Result, error)
}
type Dependencies struct {
	Cache        CacheReader
	Installed    InstalledReader
	Store        Store
	Sources      []Source
	LoadSources  func(context.Context) ([]Source, error)
	Notify       func(Event)
	Now          func() time.Time
	InitialSince time.Time
	// RefreshInterval enables cancellable background repository refreshes when positive.
	RefreshInterval time.Duration
}
type SyncStarted struct {
	Source string
	At     time.Time
}
type SyncCommitted struct {
	Source string
	At     time.Time
	Result Result
}
type SyncProgress struct {
	Source   string
	At       time.Time
	Progress Progress
}
type SyncFailed struct {
	Source string
	At     time.Time
	Err    error
}

// Event reports a state change produced by the coordinator.
// Consumers adapt these events to their own presentation model.
type Event interface{ syncEvent() }

func (SyncStarted) syncEvent()   {}
func (SyncCommitted) syncEvent() {}
func (SyncProgress) syncEvent()  {}
func (SyncFailed) syncEvent()    {}

// DatasetChanged reports that persisted sync data is ready to be read again.
type DatasetChanged struct{}

func (DatasetChanged) syncEvent() {}

type operation struct {
	source  string
	req     Request
	refresh bool
	replace bool
}

type sourceJob struct {
	cancel context.CancelFunc
}

type initialTicket struct {
	coordinator *Coordinator
	once        sync.Once
}

func (t *initialTicket) complete() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		t.coordinator.mu.Lock()
		t.coordinator.initialActive--
		t.coordinator.cond.Broadcast()
		t.coordinator.mu.Unlock()
	})
}

type Coordinator struct {
	deps                          Dependencies
	mu                            sync.Mutex
	cond                          *sync.Cond
	root                          context.Context
	cancel                        context.CancelFunc
	sem                           chan struct{}
	active, initialActive         int
	installed                     []domain.PackageID
	closed, started, sourcesReady bool
	pending                       []operation
	sourceJobs                    map[string]map[*sourceJob]struct{}
}

func New(deps Dependencies) *Coordinator {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	c := &Coordinator{deps: deps, sem: make(chan struct{}, 2), sourceJobs: make(map[string]map[*sourceJob]struct{})}
	c.sourcesReady = deps.LoadSources == nil
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *Coordinator) context(parent context.Context) (context.Context, bool) {
	if parent == nil {
		parent = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false
	}
	if c.root == nil {
		c.root, c.cancel = context.WithCancel(parent)
	}
	return c.root, true
}

// Start loads local state, then refreshes installed state and sources.
func (c *Coordinator) Start(ctx context.Context) {
	root, ok := c.context(ctx)
	if !ok {
		return
	}
	c.mu.Lock()
	if c.started || c.closed {
		c.mu.Unlock()
		return
	}
	c.startLocked(root)
	c.mu.Unlock()
}

func (c *Coordinator) startLocked(root context.Context) {
	c.started = true
	c.active++
	c.initialActive++
	ticket := &initialTicket{coordinator: c}
	go func() {
		defer c.done()
		defer ticket.complete()
		if c.deps.Cache != nil {
			_ = c.deps.Cache.LoadCached(root)
		}
		if err := c.refreshInstalled(root); err != nil {
			c.notify(SyncFailed{Source: "installed", At: c.deps.Now(), Err: bounded(err)})
			if errors.Is(err, context.Canceled) {
				return
			}
		}
		sources := c.deps.Sources
		if c.deps.LoadSources != nil {
			var err error
			sources, err = c.deps.LoadSources(root)
			if err != nil {
				c.mu.Lock()
				if !c.closed {
					c.started = false
				}
				c.mu.Unlock()
				c.notify(SyncFailed{Source: "repositories", At: c.deps.Now(), Err: bounded(err)})
				return
			}
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		c.deps.Sources = append([]Source(nil), sources...)
		c.sourcesReady = true
		for _, source := range sources {
			c.scheduleLocked(root, source, Request{Since: c.deps.InitialSince.UTC()}, true, true)
		}
		pending := c.pending
		c.pending = nil
		for _, op := range pending {
			c.scheduleOperationLocked(root, op)
		}
		c.mu.Unlock()
		if c.deps.RefreshInterval > 0 {
			go c.refreshLoop(root, c.deps.RefreshInterval)
		}
	}()
}

func (c *Coordinator) refreshLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			ready := !c.closed && c.sourcesReady && c.initialActive == 0 && c.active == 0
			c.mu.Unlock()
			if ready {
				c.Refresh(ctx)
			}
		}
	}
}

func (c *Coordinator) refreshInstalled(ctx context.Context) error {
	if c.deps.Installed == nil {
		return nil
	}
	packages, err := c.deps.Installed.Installed(ctx)
	if err != nil {
		return err
	}
	if c.deps.Store != nil {
		if err = c.deps.Store.SetInstalled(ctx, packages); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.installed = make([]domain.PackageID, len(packages))
	for index := range packages {
		c.installed[index] = packages[index].PackageID
	}
	c.mu.Unlock()
	return nil
}

// scheduleLocked accepts work while holding mu, before any goroutine can race Close.
func (c *Coordinator) scheduleLocked(ctx context.Context, source Source, req Request, refresh, initial bool) {
	if source == nil || c.closed {
		return
	}
	jobCtx, cancel := context.WithCancel(ctx)
	job := &sourceJob{cancel: cancel}
	jobs := c.sourceJobs[source.Name()]
	if jobs == nil {
		jobs = make(map[*sourceJob]struct{})
		c.sourceJobs[source.Name()] = jobs
	}
	jobs[job] = struct{}{}
	c.active++
	var ticket *initialTicket
	if initial {
		c.initialActive++
		ticket = &initialTicket{coordinator: c}
	}
	go func() {
		defer c.doneSourceJob(source.Name(), job)
		defer ticket.complete()
		select {
		case c.sem <- struct{}{}:
		case <-jobCtx.Done():
			return
		}
		defer func() { <-c.sem }()
		c.run(jobCtx, source, req, refresh, ticket.complete)
	}()
}

func (c *Coordinator) scheduleOperationLocked(ctx context.Context, op operation) {
	for _, source := range c.deps.Sources {
		if op.source == "" || source.Name() == op.source {
			if op.replace {
				c.cancelSourceLocked(source.Name())
			}
			c.scheduleLocked(ctx, source, op.req, op.refresh, true)
		}
	}
}

func (c *Coordinator) cancelSourceLocked(name string) {
	for job := range c.sourceJobs[name] {
		job.cancel()
	}
}

func (c *Coordinator) doneSourceJob(name string, job *sourceJob) {
	job.cancel()
	c.mu.Lock()
	delete(c.sourceJobs[name], job)
	if len(c.sourceJobs[name]) == 0 {
		delete(c.sourceJobs, name)
	}
	c.active--
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *Coordinator) done() {
	c.mu.Lock()
	c.active--
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *Coordinator) run(ctx context.Context, source Source, req Request, refresh bool, initialReady func()) {
	if req.HistoricalFallback {
		c.runHistoricalFallback(ctx, source, req)
		return
	}
	started := c.deps.Now()
	runStore, hasRunIdentity := c.deps.Store.(RunStore)
	var runID int64
	if c.deps.Store != nil {
		var err error
		if hasRunIdentity {
			runID, err = runStore.SyncStartedRun(ctx, source.Name(), started)
		} else {
			err = c.deps.Store.SyncStarted(ctx, source.Name(), started)
		}
		if err != nil {
			c.notify(SyncFailed{Source: source.Name(), At: c.deps.Now(), Err: bounded(err)})
			return
		}
	}
	c.notify(SyncStarted{Source: source.Name(), At: started})
	var result Result
	var err error
	lastPublishedBatch := 0
	req.Progress = func(progress Progress) {
		c.notify(SyncProgress{Source: source.Name(), At: c.deps.Now(), Progress: progress})
		if progress.Batches > lastPublishedBatch {
			lastPublishedBatch = progress.Batches
			c.notify(DatasetChanged{})
			initialReady()
		}
	}
	if refresh {
		err = source.Refresh(ctx)
	}
	if err == nil {
		result, err = source.Index(ctx, req)
	}
	ended := c.deps.Now()
	canceled := errors.Is(err, context.Canceled)
	err = bounded(err)
	if c.deps.Store != nil {
		persistenceCtx, cancelPersistence := syncPersistenceContext(ctx)
		var statusErr error
		if hasRunIdentity {
			statusErr = runStore.SyncFinishedRun(persistenceCtx, runID, source.Name(), ended, result, err)
		} else {
			statusErr = c.deps.Store.SyncFinished(persistenceCtx, source.Name(), ended, result, err)
		}
		cancelPersistence()
		if err == nil && statusErr != nil {
			err = bounded(statusErr)
		}
	}
	if err != nil {
		if !canceled {
			c.notify(SyncFailed{Source: source.Name(), At: ended, Err: err})
		}
		return
	}
	c.notify(SyncCommitted{Source: source.Name(), At: ended, Result: result})
	c.notify(DatasetChanged{})
	c.scheduleHistoricalFallback(source)
}

func syncPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), shutdownPersistenceWindow)
}

func (c *Coordinator) scheduleHistoricalFallback(source Source) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.root == nil {
		return
	}
	installed := append([]domain.PackageID(nil), c.installed...)
	if len(installed) == 0 {
		return
	}
	c.scheduleLocked(c.root, source, Request{Installed: installed, HistoricalFallback: true}, false, false)
}

func (c *Coordinator) runHistoricalFallback(ctx context.Context, source Source, req Request) {
	lastPublishedBatch := 0
	req.Progress = func(progress Progress) {
		if progress.Batches > lastPublishedBatch {
			lastPublishedBatch = progress.Batches
			c.notify(DatasetChanged{})
		}
	}
	if _, err := source.Index(ctx, req); err == nil {
		c.notify(DatasetChanged{})
	}
}

func (c *Coordinator) Retry(ctx context.Context, name string) {
	root, ok := c.context(ctx)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	op := operation{source: name, refresh: true}
	if !c.sourcesReady {
		c.pending = append(c.pending, op)
		if !c.started {
			c.startLocked(root)
		}
		return
	}
	c.scheduleOperationLocked(root, op)
}

// Refresh preempts in-progress work and fetches every repository before
// resuming indexing from its durable checkpoint.
func (c *Coordinator) Refresh(ctx context.Context) {
	root, ok := c.context(ctx)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	op := operation{refresh: true, replace: true}
	if !c.sourcesReady {
		c.pending = append(c.pending, op)
		if !c.started {
			c.startLocked(root)
		}
		return
	}
	c.scheduleOperationLocked(root, op)
}

func (c *Coordinator) EnsureRange(ctx context.Context, since time.Time) {
	root, ok := c.context(ctx)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	op := operation{req: Request{Since: since.UTC()}}
	if !c.sourcesReady {
		c.pending = append(c.pending, op)
		return
	}
	c.scheduleOperationLocked(root, op)
}
func (c *Coordinator) notify(event Event) {
	if c.deps.Notify != nil {
		c.deps.Notify(event)
	}
}
func (c *Coordinator) Wait() {
	c.mu.Lock()
	for c.active > 0 {
		c.cond.Wait()
	}
	c.mu.Unlock()
}
func (c *Coordinator) WaitInitial() {
	c.mu.Lock()
	for c.initialActive > 0 {
		c.cond.Wait()
	}
	c.mu.Unlock()
}
func (c *Coordinator) Close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.pending = nil
		if c.cancel != nil {
			c.cancel()
		}
	}
	for c.active > 0 {
		c.cond.Wait()
	}
	c.mu.Unlock()
}
func bounded(err error) error {
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(err.Error())
	if len(text) > maxErrorText {
		text = text[:maxErrorText]
	}
	return errors.New(text)
}

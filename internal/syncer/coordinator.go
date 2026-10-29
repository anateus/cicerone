// Package syncer coordinates background refresh and history indexing.
package syncer

import (
	"context"
	"errors"
	"fmt"
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

// RunStore preserves the identity of each synchronization attempt.
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
	// RefreshInterval enables periodic background repository refreshes when positive.
	RefreshInterval time.Duration
}

// Phase identifies the current source operation. Fetching also includes
// repository checks, which do not fetch from a remote for non-owned sources.
type Phase string

const (
	PhaseFetching Phase = "fetching"
	PhaseIndexing Phase = "indexing"
)

type SyncQueued struct {
	Source             string
	ID                 uint64
	At                 time.Time
	HistoricalFallback bool
}

type SyncStarted struct {
	Source             string
	ID                 uint64
	At                 time.Time
	HistoricalFallback bool
}

// SyncPhase reports the active operation for a source job.
type SyncPhase struct {
	Source             string
	ID                 uint64
	At                 time.Time
	Phase              Phase
	HistoricalFallback bool
}
type SyncCommitted struct {
	Source             string
	ID                 uint64
	At                 time.Time
	Result             Result
	HistoricalFallback bool
}
type SyncProgress struct {
	Source             string
	ID                 uint64
	At                 time.Time
	Progress           Progress
	HistoricalFallback bool
}
type SyncFailed struct {
	Source             string
	ID                 uint64
	At                 time.Time
	Err                error
	HistoricalFallback bool
}
type SyncCancelled struct {
	Source             string
	ID                 uint64
	At                 time.Time
	Reason             string
	HistoricalFallback bool
}

// Event reports a state change produced by the coordinator.
// Consumers adapt these events to their own presentation model.
type Event interface{ syncEvent() }

func (SyncQueued) syncEvent()    {}
func (SyncStarted) syncEvent()   {}
func (SyncPhase) syncEvent()     {}
func (SyncCommitted) syncEvent() {}
func (SyncProgress) syncEvent()  {}
func (SyncFailed) syncEvent()    {}
func (SyncCancelled) syncEvent() {}

// DatasetChanged reports that persisted sync data is ready to be read again.
type DatasetChanged struct{}

func (DatasetChanged) syncEvent() {}

var (
	errShutdown = errors.New("shutdown")
	errYield    = errors.New("yield for queued refresh at durable checkpoint")
)

type operation struct {
	source  string
	req     Request
	refresh bool
	initial bool
}

type sourceJob struct {
	id       uint64
	source   Source
	req      Request
	refresh  bool
	ctx      context.Context
	cancel   context.CancelCauseFunc
	ticket   *initialTicket
	reserved bool
	begun    bool
	terminal bool
}

// setupJob can acquire an initial ticket if a manual request arrives while a
// background discovery retry is in flight.
type setupJob struct {
	ticket    *initialTicket
	scheduled bool
}

type installedJob struct {
	id         uint64
	ctx        context.Context
	cancel     context.CancelCauseFunc
	stopCaller func() bool
	stopRoot   func() bool
	previous   <-chan struct{}
	done       chan struct{}
	err        error
	reusable   bool
}

type initialTicket struct {
	coordinator *Coordinator
	done        bool
}

func (t *initialTicket) completeLocked() {
	if t == nil || t.done {
		return
	}
	t.done = true
	t.coordinator.initialActive--
	t.coordinator.cond.Broadcast()
}

type Coordinator struct {
	deps                          Dependencies
	mu                            sync.Mutex
	cond                          *sync.Cond
	refreshLoopOnce               sync.Once
	root                          context.Context
	cancel                        context.CancelCauseFunc
	sem                           chan struct{}
	active, initialActive         int
	installed                     []domain.PackageID
	closed, started, sourcesReady bool
	setup                         *setupJob
	pending                       []operation
	nextJobID                     uint64
	sourceJobs                    map[string][]*sourceJob
	installedJobs                 []*installedJob
	installedValid                bool
	installedChanged              bool
	fallbackSources               map[string]Source
	pendingFallbacks              map[string]Source
}

func New(deps Dependencies) *Coordinator {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	c := &Coordinator{deps: deps, sem: make(chan struct{}, 2), sourceJobs: make(map[string][]*sourceJob)}
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
		c.root, c.cancel = context.WithCancelCause(parent)
		root := c.root
		for _, pending := range c.installedJobs {
			job := pending
			job.stopRoot = context.AfterFunc(root, func() { job.cancel(context.Cause(root)) })
		}
	}
	return c.root, true
}

// Start loads cached state, then refreshes installed state and sources concurrently.
func (c *Coordinator) Start(ctx context.Context) {
	c.start(ctx, true)
}

func (c *Coordinator) start(ctx context.Context, initial bool) {
	root, ok := c.context(ctx)
	if !ok {
		return
	}
	c.mu.Lock()
	if c.started || c.closed {
		c.mu.Unlock()
		return
	}
	c.startLocked(root, initial)
	c.mu.Unlock()
}

func (c *Coordinator) startLocked(root context.Context, initial bool) {
	c.started = true
	if c.deps.RefreshInterval > 0 {
		c.refreshLoopOnce.Do(func() { go c.refreshLoop(root, c.deps.RefreshInterval) })
	}
	c.active++
	setup := &setupJob{}
	if initial {
		c.initialActive++
		setup.ticket = &initialTicket{coordinator: c}
	}
	c.setup = setup
	go func() {
		defer c.doneSetup(setup)
		if c.deps.Cache != nil {
			_ = c.deps.Cache.LoadCached(root)
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		installedJob, reusedInstalled := c.startInstalledLocked(root)
		c.mu.Unlock()
		if installedJob != nil {
			defer c.awaitInstalled(root, installedJob, reusedInstalled)
		}
		sources := c.deps.Sources
		if c.deps.LoadSources != nil {
			c.mu.Lock()
			c.nextJobID++
			id := c.nextJobID
			c.mu.Unlock()
			c.notify(SyncStarted{Source: "repositories", ID: id, At: c.deps.Now()})
			var err error
			sources, err = c.deps.LoadSources(root)
			if err != nil {
				c.mu.Lock()
				if !c.closed {
					c.started = false
				}
				c.mu.Unlock()
				if root.Err() != nil && errors.Is(err, root.Err()) {
					c.notify(SyncCancelled{Source: "repositories", ID: id, At: c.deps.Now(), Reason: cancellationReason(root, err)})
					if hasCancellationCompanion(err) {
						c.notify(SyncFailed{Source: "repositories", ID: id, At: c.deps.Now(), Err: bounded(err)})
					}
				} else {
					c.notify(SyncFailed{Source: "repositories", ID: id, At: c.deps.Now(), Err: bounded(err)})
				}
				return
			}
			c.notify(SyncCommitted{Source: "repositories", ID: id, At: c.deps.Now()})
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		c.deps.Sources = append([]Source(nil), sources...)
		c.sourcesReady = true
		for _, source := range sources {
			c.scheduleLocked(root, source, Request{Since: c.deps.InitialSince.UTC()}, true, setup.ticket != nil)
		}
		pending := c.pending
		c.pending = nil
		for _, op := range pending {
			c.scheduleOperationLocked(root, op)
		}
		setup.scheduled = true
		c.mu.Unlock()
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
			retryDiscovery := !c.closed && !c.started && !c.sourcesReady
			ready := !c.closed && c.sourcesReady && c.initialActive == 0 && (c.setup == nil || c.setup.scheduled)
			c.mu.Unlock()
			if retryDiscovery {
				c.start(ctx, false)
			} else if ready {
				c.refresh(ctx, false)
			}
		}
	}
}

// startInstalledLocked shares only a viable startup-owned lookup with retries.
// A caller-owned lookup can be canceled independently, so startup queues its
// own lookup after it rather than treating it as the startup snapshot.
func (c *Coordinator) startInstalledLocked(ctx context.Context) (*installedJob, bool) {
	if c.deps.Installed == nil {
		return nil, false
	}
	if len(c.installedJobs) != 0 {
		job := c.installedJobs[len(c.installedJobs)-1]
		if job.reusable && job.ctx.Err() == nil {
			return job, true
		}
	}
	return c.enqueueInstalledLocked(ctx, nil), false
}

func (c *Coordinator) awaitInstalled(ctx context.Context, job *installedJob, reused bool) {
	<-job.done
	if !reused || job.err == nil || ctx.Err() != nil {
		return
	}
	// A discovery retry may have joined a lookup that failed after it joined.
	// Give that retry its own snapshot without delaying recent indexing.
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	retry, _ := c.startInstalledLocked(ctx)
	c.mu.Unlock()
	if retry != nil {
		<-retry.done
	}
}

// RefreshInstalled obtains a new snapshot after any earlier installed lookup.
// It is safe to call before Start, and shutdown waits for admitted lookups.
func (c *Coordinator) RefreshInstalled(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errShutdown
	}
	if c.deps.Installed == nil {
		c.mu.Unlock()
		return errors.New("installed-state refresh is unavailable")
	}
	root := c.root
	if root == nil {
		root = context.Background()
	}
	job := c.enqueueInstalledLocked(root, ctx)
	c.mu.Unlock()
	select {
	case <-job.done:
		return job.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InstalledSnapshot returns the latest successfully refreshed installed IDs.
// A queued, running, or failed latest refresh makes the snapshot unavailable.
func (c *Coordinator) InstalledSnapshot() ([]domain.PackageID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.installedValid {
		return nil, false
	}
	return append([]domain.PackageID(nil), c.installed...), true
}

func (c *Coordinator) enqueueInstalledLocked(root, caller context.Context) *installedJob {
	ctx, cancel := context.WithCancelCause(root)
	c.nextJobID++
	job := &installedJob{id: c.nextJobID, ctx: ctx, cancel: cancel, done: make(chan struct{}), reusable: caller == nil}
	if caller != nil {
		job.stopCaller = context.AfterFunc(caller, func() { cancel(context.Cause(caller)) })
	}
	if len(c.installedJobs) != 0 {
		job.previous = c.installedJobs[len(c.installedJobs)-1].done
	}
	c.installedJobs = append(c.installedJobs, job)
	c.installedValid = false
	c.cond.Broadcast()
	c.active++
	go c.runInstalled(job)
	return job
}

func (c *Coordinator) runInstalled(job *installedJob) {
	if job.previous != nil {
		<-job.previous
	}
	ctx := job.ctx
	c.notify(SyncStarted{Source: "installed", ID: job.id, At: c.deps.Now()})
	err := ctx.Err()
	if err == nil {
		err = c.refreshInstalled(ctx)
	}
	c.mu.Lock()
	if err != nil {
		job.reusable = false
	}
	if len(c.installedJobs) == 1 {
		c.installedValid = err == nil
	}
	c.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			c.notify(SyncCancelled{Source: "installed", ID: job.id, At: c.deps.Now(), Reason: cancellationReason(ctx, err)})
			if hasCancellationCompanion(err) {
				c.notify(SyncFailed{Source: "installed", ID: job.id, At: c.deps.Now(), Err: bounded(err)})
			}
		} else {
			c.notify(SyncFailed{Source: "installed", ID: job.id, At: c.deps.Now(), Err: bounded(err)})
		}
	} else {
		c.notify(SyncCommitted{Source: "installed", ID: job.id, At: c.deps.Now()})
		if c.deps.Store != nil {
			c.notify(DatasetChanged{})
		}
	}
	if job.stopCaller != nil {
		job.stopCaller()
	}
	job.cancel(nil)
	c.mu.Lock()
	if job.stopRoot != nil {
		job.stopRoot()
	}
	job.err = err
	if err == nil {
		c.installedChanged = true
	}
	c.installedJobs[0] = nil
	c.installedJobs = c.installedJobs[1:]
	if len(c.installedJobs) == 0 {
		if c.installedChanged {
			if c.pendingFallbacks == nil && len(c.fallbackSources) != 0 {
				c.pendingFallbacks = make(map[string]Source)
			}
			for name, source := range c.fallbackSources {
				c.pendingFallbacks[name] = source
			}
		}
		c.installedChanged = false
		pending := c.pendingFallbacks
		c.pendingFallbacks = nil
		for _, source := range pending {
			c.scheduleHistoricalFallbackLocked(source)
		}
	}
	c.active--
	close(job.done)
	c.cond.Broadcast()
	c.mu.Unlock()
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

// scheduleLocked admits one source job while holding mu. Refreshes coalesce,
// and take priority over fallback work that has not reserved the source.
func (c *Coordinator) scheduleLocked(ctx context.Context, source Source, req Request, refresh, initial bool) {
	if source == nil || c.closed {
		return
	}
	name := source.Name()
	for _, job := range c.sourceJobs[name] {
		if !job.begun && !job.terminal && job.ctx.Err() == nil && ((refresh && job.refresh) || (req.HistoricalFallback && job.req.HistoricalFallback)) {
			// A manual request may reuse a periodic job that has not started.
			if initial && job.ticket == nil {
				c.initialActive++
				job.ticket = &initialTicket{coordinator: c}
			}
			return
		}
	}
	jobCtx, cancel := context.WithCancelCause(ctx)
	c.nextJobID++
	job := &sourceJob{id: c.nextJobID, source: source, req: req, refresh: refresh, ctx: jobCtx, cancel: cancel}
	jobs := c.sourceJobs[name]
	position := len(jobs)
	if refresh {
		for i, queued := range jobs {
			if queued.req.HistoricalFallback && !queued.reserved {
				position = i
				break
			}
		}
	}
	jobs = append(jobs, nil)
	copy(jobs[position+1:], jobs[position:])
	jobs[position] = job
	c.sourceJobs[name] = jobs
	c.active++
	if initial {
		c.initialActive++
		job.ticket = &initialTicket{coordinator: c}
	}
	go c.work(job)
}

func (c *Coordinator) work(job *sourceJob) {
	name := job.source.Name()
	defer c.doneSourceJob(name, job)
	c.notify(SyncQueued{Source: name, ID: job.id, At: c.deps.Now(), HistoricalFallback: job.req.HistoricalFallback})
	for {
		c.mu.Lock()
		for !c.closed && job.ctx.Err() == nil &&
			(c.sourceJobs[name][0] != job || (job.req.HistoricalFallback && len(c.installedJobs) != 0)) {
			c.cond.Wait()
		}
		if job.ctx.Err() != nil {
			c.mu.Unlock()
			c.notify(SyncCancelled{Source: name, ID: job.id, At: c.deps.Now(), Reason: cancellationReason(job.ctx, job.ctx.Err()), HistoricalFallback: job.req.HistoricalFallback})
			return
		}
		job.reserved = true
		c.mu.Unlock()
		select {
		case c.sem <- struct{}{}:
		case <-job.ctx.Done():
			c.notify(SyncCancelled{Source: name, ID: job.id, At: c.deps.Now(), Reason: cancellationReason(job.ctx, job.ctx.Err()), HistoricalFallback: job.req.HistoricalFallback})
			return
		}
		c.mu.Lock()
		if job.ctx.Err() != nil {
			c.mu.Unlock()
			<-c.sem
			c.notify(SyncCancelled{Source: name, ID: job.id, At: c.deps.Now(), Reason: cancellationReason(job.ctx, job.ctx.Err()), HistoricalFallback: job.req.HistoricalFallback})
			return
		}
		if job.req.HistoricalFallback && len(c.installedJobs) != 0 {
			// A new installed refresh arrived while this job waited for a slot.
			// Release the slot and let any already queued refresh take priority.
			job.reserved = false
			jobs := c.sourceJobs[name]
			for i := 1; i < len(jobs); i++ {
				if jobs[i].refresh {
					refresh := jobs[i]
					copy(jobs[1:i+1], jobs[:i])
					jobs[0] = refresh
					break
				}
			}
			c.cond.Broadcast()
			c.mu.Unlock()
			<-c.sem
			continue
		}
		if job.req.HistoricalFallback {
			if len(c.installed) == 0 {
				job.terminal = true
				c.mu.Unlock()
				<-c.sem
				c.notify(SyncCancelled{Source: name, ID: job.id, At: c.deps.Now(), Reason: "installed snapshot is empty", HistoricalFallback: true})
				return
			}
			job.req.Installed = append([]domain.PackageID(nil), c.installed...)
		}
		job.begun = true
		c.mu.Unlock()
		defer func() { <-c.sem }()
		c.run(job)
		return
	}
}

func (c *Coordinator) scheduleOperationLocked(ctx context.Context, op operation) {
	for _, source := range c.deps.Sources {
		if op.source == "" || source.Name() == op.source {
			c.scheduleLocked(ctx, source, op.req, op.refresh, op.initial)
		}
	}
}

func (c *Coordinator) doneSourceJob(name string, job *sourceJob) {
	job.cancel(nil)
	c.mu.Lock()
	job.ticket.completeLocked()
	jobs := c.sourceJobs[name]
	for i, candidate := range jobs {
		if candidate == job {
			copy(jobs[i:], jobs[i+1:])
			jobs[len(jobs)-1] = nil
			jobs = jobs[:len(jobs)-1]
			break
		}
	}
	if len(jobs) == 0 {
		delete(c.sourceJobs, name)
	} else {
		c.sourceJobs[name] = jobs
	}
	c.active--
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *Coordinator) doneSetup(setup *setupJob) {
	c.mu.Lock()
	setup.ticket.completeLocked()
	if c.setup == setup {
		c.setup = nil
	}
	c.active--
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *Coordinator) run(job *sourceJob) {
	ctx, source, req := job.ctx, job.source, job.req
	name := source.Name()
	started := c.deps.Now()
	runStore, hasRunIdentity := c.deps.Store.(RunStore)
	var runID int64
	if c.deps.Store != nil && !req.HistoricalFallback {
		var err error
		if hasRunIdentity {
			runID, err = runStore.SyncStartedRun(ctx, name, started)
		} else {
			err = c.deps.Store.SyncStarted(ctx, name, started)
		}
		if err != nil {
			if ctx.Err() != nil {
				c.notify(SyncCancelled{Source: name, ID: job.id, At: c.deps.Now(), Reason: cancellationReason(ctx, err), HistoricalFallback: req.HistoricalFallback})
			} else {
				c.notify(SyncFailed{Source: name, ID: job.id, At: c.deps.Now(), Err: bounded(err), HistoricalFallback: req.HistoricalFallback})
			}
			return
		}
	}
	c.notify(SyncStarted{Source: name, ID: job.id, At: started, HistoricalFallback: req.HistoricalFallback})
	var result Result
	var err error
	lastPublishedBatch := 0
	req.Progress = func(progress Progress) {
		c.notify(SyncProgress{Source: name, ID: job.id, At: c.deps.Now(), Progress: progress, HistoricalFallback: req.HistoricalFallback})
		if progress.Batches > lastPublishedBatch {
			lastPublishedBatch = progress.Batches
			c.notify(DatasetChanged{})
			if req.HistoricalFallback {
				c.yieldFallbackAtCheckpoint(job)
			} else {
				c.mu.Lock()
				job.ticket.completeLocked()
				c.mu.Unlock()
			}
		}
	}
	if job.refresh {
		c.notify(SyncPhase{Source: name, ID: job.id, At: c.deps.Now(), Phase: PhaseFetching, HistoricalFallback: req.HistoricalFallback})
		err = source.Refresh(ctx)
	}
	if err == nil {
		c.notify(SyncPhase{Source: name, ID: job.id, At: c.deps.Now(), Phase: PhaseIndexing, HistoricalFallback: req.HistoricalFallback})
		result, err = source.Index(ctx, req)
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	sourceErr := err
	ended := c.deps.Now()
	canceled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	companionFailure := canceled && hasCancellationCompanion(sourceErr)
	reason := ""
	if canceled {
		reason = cancellationReason(ctx, err)
		if companionFailure {
			err = fmt.Errorf("failed during cancellation (%s): %w", reason, err)
		} else {
			err = fmt.Errorf("canceled (%s): %w", reason, err)
		}
	}
	err = bounded(err)
	var statusErr error
	if c.deps.Store != nil && (!req.HistoricalFallback || err != nil) {
		persistenceCtx, cancelPersistence := syncPersistenceContext(ctx)
		storedErr := err
		if req.HistoricalFallback {
			storedErr = bounded(fmt.Errorf("historical fallback: %w", err))
			if hasRunIdentity {
				runID, statusErr = runStore.SyncStartedRun(persistenceCtx, name, started)
			} else {
				statusErr = c.deps.Store.SyncStarted(persistenceCtx, name, started)
			}
		}
		if statusErr == nil {
			if hasRunIdentity {
				statusErr = runStore.SyncFinishedRun(persistenceCtx, runID, name, ended, result, storedErr)
			} else {
				statusErr = c.deps.Store.SyncFinished(persistenceCtx, name, ended, result, storedErr)
			}
		}
		cancelPersistence()
	}
	if statusErr != nil && err != nil && !canceled {
		err = bounded(errors.Join(err, fmt.Errorf("recording sync completion: %w", statusErr)))
		statusErr = nil
	}
	if canceled {
		c.notify(SyncCancelled{Source: name, ID: job.id, At: ended, Reason: reason, HistoricalFallback: req.HistoricalFallback})
		if companionFailure {
			if statusErr != nil {
				sourceErr = errors.Join(sourceErr, fmt.Errorf("recording sync completion: %w", statusErr))
				statusErr = nil
			}
			c.notify(SyncFailed{Source: name, ID: job.id, At: ended, Err: bounded(sourceErr), HistoricalFallback: req.HistoricalFallback})
		}
		if errors.Is(context.Cause(ctx), errYield) {
			c.scheduleHistoricalFallback(source)
		}
	} else if err != nil {
		c.notify(SyncFailed{Source: name, ID: job.id, At: ended, Err: err, HistoricalFallback: req.HistoricalFallback})
	} else if statusErr == nil {
		c.notify(SyncCommitted{Source: name, ID: job.id, At: ended, Result: result, HistoricalFallback: req.HistoricalFallback})
		c.notify(DatasetChanged{})
		if !req.HistoricalFallback {
			c.scheduleHistoricalFallback(source)
		}
	}
	if statusErr != nil {
		c.notify(SyncFailed{Source: name, ID: job.id, At: ended, Err: bounded(fmt.Errorf("recording sync completion: %w", statusErr)), HistoricalFallback: req.HistoricalFallback})
	}
}

func hasCancellationCompanion(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, part := range joined.Unwrap() {
			if hasCancellationCompanion(part) {
				return true
			}
		}
		return false
	}
	if nested := errors.Unwrap(err); nested != nil {
		return hasCancellationCompanion(nested)
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func cancellationReason(ctx context.Context, err error) string {
	if cause := context.Cause(ctx); cause != nil {
		return cause.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded.Error()
	}
	return context.Canceled.Error()
}

func (c *Coordinator) yieldFallbackAtCheckpoint(job *sourceJob) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, queued := range c.sourceJobs[job.source.Name()] {
		if queued != job && queued.refresh && !queued.begun {
			job.cancel(errYield)
			return
		}
	}
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
	if c.fallbackSources == nil {
		c.fallbackSources = make(map[string]Source)
	}
	c.fallbackSources[source.Name()] = source
	c.scheduleHistoricalFallbackLocked(source)
}

func (c *Coordinator) scheduleHistoricalFallbackLocked(source Source) {
	if c.closed || c.root == nil {
		return
	}
	if len(c.installedJobs) != 0 {
		if c.pendingFallbacks == nil {
			c.pendingFallbacks = make(map[string]Source)
		}
		c.pendingFallbacks[source.Name()] = source
		return
	}
	if len(c.installed) != 0 {
		c.scheduleLocked(c.root, source, Request{HistoricalFallback: true}, false, false)
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
	op := operation{source: name, refresh: true, initial: true}
	if c.awaitingSourcesLocked() {
		c.queuePendingLocked(op)
		if !c.started {
			c.startLocked(root, true)
		} else {
			c.includeSetupInInitialLocked()
		}
		return
	}
	c.scheduleOperationLocked(root, op)
}

// Refresh queues one refresh per source without interrupting active indexing.
// Historical fallback yields only after committing a batch and then resumes.
func (c *Coordinator) Refresh(ctx context.Context) {
	c.refresh(ctx, true)
}

func (c *Coordinator) refresh(ctx context.Context, initial bool) {
	root, ok := c.context(ctx)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	op := operation{refresh: true, initial: initial}
	if c.awaitingSourcesLocked() {
		c.queuePendingLocked(op)
		if !c.started {
			c.startLocked(root, initial)
		} else if initial {
			c.includeSetupInInitialLocked()
		}
		return
	}
	c.scheduleOperationLocked(root, op)
}

func (c *Coordinator) awaitingSourcesLocked() bool {
	return !c.sourcesReady || (c.setup != nil && !c.setup.scheduled)
}

func (c *Coordinator) includeSetupInInitialLocked() {
	if c.setup != nil && c.setup.ticket == nil {
		c.initialActive++
		c.setup.ticket = &initialTicket{coordinator: c}
	}
}

func (c *Coordinator) queuePendingLocked(op operation) {
	if op.refresh {
		for i := range c.pending {
			pending := &c.pending[i]
			if !pending.refresh {
				continue
			}
			if pending.source == op.source {
				pending.initial = pending.initial || op.initial
				return
			}
			if pending.source == "" && (!op.initial || pending.initial) {
				return
			}
		}
		if op.source == "" {
			pending := c.pending[:0]
			for _, queued := range c.pending {
				if !queued.refresh || (!op.initial && queued.initial) {
					pending = append(pending, queued)
				}
			}
			c.pending = pending
		}
	}
	c.pending = append(c.pending, op)
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
	op := operation{req: Request{Since: since.UTC()}, initial: true}
	if c.awaitingSourcesLocked() {
		c.pending = append(c.pending, op)
		c.includeSetupInInitialLocked()
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

// Stop initiates shutdown without waiting for workers to finish.
func (c *Coordinator) Stop() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.pending = nil
		if c.cancel != nil {
			c.cancel(errShutdown)
		}
		for _, job := range c.installedJobs {
			job.cancel(errShutdown)
		}
		c.cond.Broadcast()
	}
	c.mu.Unlock()
}

func (c *Coordinator) Close() {
	c.Stop()
	c.Wait()
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

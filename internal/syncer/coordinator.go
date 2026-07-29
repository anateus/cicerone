// Package syncer coordinates background refresh and history indexing.
package syncer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/tui"
)

const maxErrorText = 1024

type CacheReader interface{ LoadCached(context.Context) error }
type InstalledReader interface {
	Installed(context.Context) ([]domain.InstalledPackage, error)
}
type Store interface {
	SetInstalled(context.Context, []domain.InstalledPackage) error
	SyncStarted(context.Context, string, time.Time) error
	SyncFinished(context.Context, string, time.Time, Result, error) error
}
type Request struct {
	Since     time.Time
	Installed []domain.PackageID
	Kinds     map[domain.EventKind]bool
	Progress  func(Progress)
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
type CachedSource interface {
	IndexCached(context.Context, Request) (Result, bool, error)
}
type Dependencies struct {
	Cache        CacheReader
	Installed    InstalledReader
	Store        Store
	Sources      []Source
	LoadSources  func(context.Context) ([]Source, error)
	Notify       func(tea.Msg)
	Now          func() time.Time
	InitialSince time.Time
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
type operation struct {
	source  string
	req     Request
	refresh bool
}

type Coordinator struct {
	deps                          Dependencies
	mu                            sync.Mutex
	cond                          *sync.Cond
	root                          context.Context
	cancel                        context.CancelFunc
	sem                           chan struct{}
	active                        int
	installed                     []domain.PackageID
	closed, started, sourcesReady bool
	pending                       []operation
}

func New(deps Dependencies) *Coordinator {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	c := &Coordinator{deps: deps, sem: make(chan struct{}, 2)}
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

// Start first exposes the cached dataset, then refreshes installed state and sources.
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
	go func() {
		defer c.done()
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
			c.scheduleLocked(root, source, Request{Since: c.deps.InitialSince.UTC()}, true)
		}
		pending := c.pending
		c.pending = nil
		for _, op := range pending {
			c.scheduleOperationLocked(root, op)
		}
		c.mu.Unlock()
	}()
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
	ids := make([]domain.PackageID, len(packages))
	for i := range packages {
		ids[i] = packages[i].PackageID
	}
	c.mu.Lock()
	c.installed = ids
	c.mu.Unlock()
	return nil
}

// scheduleLocked accepts work while holding mu, before any goroutine can race Close.
func (c *Coordinator) scheduleLocked(ctx context.Context, source Source, req Request, refresh bool) {
	if source == nil || c.closed {
		return
	}
	c.active++
	go func() {
		defer c.done()
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-c.sem }()
		c.run(ctx, source, req, refresh)
	}()
}

func (c *Coordinator) scheduleOperationLocked(ctx context.Context, op operation) {
	for _, source := range c.deps.Sources {
		if op.source == "" || source.Name() == op.source {
			c.scheduleLocked(ctx, source, op.req, op.refresh)
		}
	}
}
func (c *Coordinator) done() { c.mu.Lock(); c.active--; c.cond.Broadcast(); c.mu.Unlock() }

func (c *Coordinator) run(ctx context.Context, source Source, req Request, refresh bool) {
	started := c.deps.Now()
	if c.deps.Store != nil {
		if err := c.deps.Store.SyncStarted(ctx, source.Name(), started); err != nil {
			c.notify(SyncFailed{Source: source.Name(), At: c.deps.Now(), Err: bounded(err)})
			return
		}
	}
	c.notify(SyncStarted{Source: source.Name(), At: started})
	var result Result
	var err error
	c.mu.Lock()
	req.Installed = append([]domain.PackageID(nil), c.installed...)
	c.mu.Unlock()
	var completed Progress
	var current Progress
	req.Progress = func(progress Progress) {
		current = progress
		progress.Commits += completed.Commits
		progress.Events += completed.Events
		progress.Diagnostics += completed.Diagnostics
		progress.Batches += completed.Batches
		c.notify(SyncProgress{Source: source.Name(), At: c.deps.Now(), Progress: progress})
		c.notify(tui.DatasetChanged{})
	}
	if refresh {
		if cached, ok := source.(CachedSource); ok {
			var used bool
			result, used, err = cached.IndexCached(ctx, req)
			if used {
				completed = current
			}
		}
		if err == nil {
			err = source.Refresh(ctx)
		}
	}
	if err == nil {
		current = Progress{}
		fresh, indexErr := source.Index(ctx, req)
		if refresh {
			result.Events += fresh.Events
			result.Diagnostics += fresh.Diagnostics
			result.Cursor = fresh.Cursor
			result.Since = fresh.Since
		} else {
			result = fresh
		}
		err = indexErr
	}
	ended := c.deps.Now()
	err = bounded(err)
	if c.deps.Store != nil {
		if statusErr := c.deps.Store.SyncFinished(ctx, source.Name(), ended, result, err); err == nil && statusErr != nil {
			err = bounded(statusErr)
		}
	}
	if err != nil {
		c.notify(SyncFailed{Source: source.Name(), At: ended, Err: err})
		return
	}
	c.notify(SyncCommitted{Source: source.Name(), At: ended, Result: result})
	c.notify(tui.DatasetChanged{})
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
func (c *Coordinator) notify(msg tea.Msg) {
	if c.deps.Notify != nil {
		c.deps.Notify(msg)
	}
}
func (c *Coordinator) Wait() {
	c.mu.Lock()
	for c.active > 0 {
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

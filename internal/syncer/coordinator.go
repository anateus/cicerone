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
}
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
type SyncFailed struct {
	Source string
	At     time.Time
	Err    error
}

type Coordinator struct {
	deps      Dependencies
	mu        sync.Mutex
	root      context.Context
	cancel    context.CancelFunc
	sem       chan struct{}
	wg        sync.WaitGroup
	installed []domain.PackageID
	closed    bool
}

func New(deps Dependencies) *Coordinator {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Coordinator{deps: deps, sem: make(chan struct{}, 2)}
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
	if c.deps.Cache != nil {
		_ = c.deps.Cache.LoadCached(root)
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
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
				c.notify(SyncFailed{Source: "repositories", At: c.deps.Now(), Err: bounded(err)})
				return
			}
			c.mu.Lock()
			c.deps.Sources = sources
			c.mu.Unlock()
		}
		for _, source := range sources {
			c.schedule(root, source, Request{Since: c.deps.InitialSince.UTC()}, true)
		}
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

func (c *Coordinator) schedule(ctx context.Context, source Source, req Request, refresh bool) {
	if source == nil {
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-c.sem }()
		c.run(ctx, source, req, refresh)
	}()
}

func (c *Coordinator) run(ctx context.Context, source Source, req Request, refresh bool) {
	started := c.deps.Now()
	if c.deps.Store != nil {
		_ = c.deps.Store.SyncStarted(ctx, source.Name(), started)
	}
	c.notify(SyncStarted{Source: source.Name(), At: started})
	var result Result
	var err error
	if refresh {
		err = source.Refresh(ctx)
	}
	if err == nil {
		c.mu.Lock()
		req.Installed = append([]domain.PackageID(nil), c.installed...)
		c.mu.Unlock()
		result, err = source.Index(ctx, req)
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
	for _, source := range c.deps.Sources {
		if source.Name() == name {
			c.schedule(root, source, Request{}, true)
			return
		}
	}
}

func (c *Coordinator) EnsureRange(ctx context.Context, since time.Time) {
	root, ok := c.context(ctx)
	if !ok {
		return
	}
	for _, source := range c.deps.Sources {
		c.schedule(root, source, Request{Since: since.UTC()}, false)
	}
}

func (c *Coordinator) notify(msg tea.Msg) {
	if c.deps.Notify != nil {
		c.deps.Notify(msg)
	}
}
func (c *Coordinator) Wait() { c.wg.Wait() }
func (c *Coordinator) Close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		if c.cancel != nil {
			c.cancel()
		}
	}
	c.mu.Unlock()
	c.wg.Wait()
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

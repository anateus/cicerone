package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/domain"
	"cicerone/internal/syncer"
)

type plainRuntime interface {
	QueryFeed(context.Context, domain.FeedFilter) ([]domain.FeedGroup, error)
	Preferences(context.Context) (domain.FeedFilter, error)
	StartSync(context.Context)
	WaitSync()
}

type plainNotificationSource interface {
	plainNotifications() []tea.Msg
}

type productionPlainRuntime struct {
	services *runtimeServices
	mu       sync.Mutex
	events   []tea.Msg
}

func (r *productionPlainRuntime) notify(msg tea.Msg) {
	switch msg.(type) {
	case syncer.SyncStarted, syncer.SyncCommitted, syncer.SyncFailed:
		r.mu.Lock()
		r.events = append(r.events, msg)
		r.mu.Unlock()
	}
}

func (r *productionPlainRuntime) QueryFeed(ctx context.Context, filter domain.FeedFilter) ([]domain.FeedGroup, error) {
	return r.services.store.QueryFeed(ctx, filter)
}

func (r *productionPlainRuntime) Preferences(ctx context.Context) (domain.FeedFilter, error) {
	return r.services.store.Preferences(ctx)
}

func (r *productionPlainRuntime) StartSync(ctx context.Context) { r.services.coordinator.Start(ctx) }
func (r *productionPlainRuntime) WaitSync()                     { r.services.coordinator.Wait() }

func (r *productionPlainRuntime) plainNotifications() []tea.Msg {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tea.Msg(nil), r.events...)
}

func runPlain(ctx context.Context, runtime plainRuntime, out io.Writer) error {
	if err := writePlainFeed(ctx, runtime, out, "Cached feed"); err != nil {
		return err
	}
	runtime.StartSync(ctx)
	runtime.WaitSync()

	var failures []error
	if source, ok := runtime.(plainNotificationSource); ok {
		type sourceNotifications struct {
			started bool
			result  tea.Msg
		}
		bySource := make(map[string]sourceNotifications)
		for _, msg := range source.plainNotifications() {
			switch event := msg.(type) {
			case syncer.SyncStarted:
				record := bySource[event.Source]
				record.started = true
				bySource[event.Source] = record
			case syncer.SyncCommitted:
				record := bySource[event.Source]
				record.result = event
				bySource[event.Source] = record
			case syncer.SyncFailed:
				record := bySource[event.Source]
				record.result = event
				bySource[event.Source] = record
			}
		}
		sources := make([]string, 0, len(bySource))
		for name := range bySource {
			sources = append(sources, name)
		}
		sort.Strings(sources)
		for _, name := range sources {
			record := bySource[name]
			if record.started {
				fmt.Fprintf(out, "Synchronizing %s…\n", name)
			}
			switch event := record.result.(type) {
			case syncer.SyncCommitted:
				fmt.Fprintf(out, "%s synchronized · %d updates\n", name, event.Result.Events)
			case syncer.SyncFailed:
				fmt.Fprintf(out, "%s synchronization failed · %v\n", name, event.Err)
				failures = append(failures, event.Err)
			}
		}
	}
	if err := writePlainFeed(ctx, runtime, out, "Refreshed feed"); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func writePlainFeed(ctx context.Context, runtime plainRuntime, out io.Writer, heading string) error {
	filter, err := runtime.Preferences(ctx)
	if err != nil {
		return err
	}
	groups, err := runtime.QueryFeed(ctx, filter)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, heading); err != nil {
		return err
	}
	for _, group := range groups {
		for _, event := range group.Events {
			value := event.NewVersion
			if value == "" {
				value = event.NewRevision
			}
			if _, err := fmt.Fprintf(out, "%s %s · %s · %s\n", event.Name, value, event.Kind, event.Time.Format("2006-01-02")); err != nil {
				return err
			}
		}
	}
	return nil
}

var executePlain = executePlainProduction

func executePlainProduction(stdout, stderr io.Writer) (code int) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	plain := &productionPlainRuntime{}
	services, err := newRuntime(home, plain.notify)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	plain.services = services
	defer func() {
		if closeErr := services.Close(); closeErr != nil && code == 0 {
			fmt.Fprintln(stderr, closeErr)
			code = 1
		}
	}()
	if err := runPlain(services.ctx, plain, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

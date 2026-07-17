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
type plainNotificationStream interface{ plainNotificationChannel() <-chan tea.Msg }

type productionPlainRuntime struct {
	services *runtimeServices
	mu       sync.Mutex
	events   []tea.Msg
	stream   chan tea.Msg
}

func (r *productionPlainRuntime) notify(msg tea.Msg) {
	switch msg.(type) {
	case syncer.SyncStarted, syncer.SyncProgress, syncer.SyncCommitted, syncer.SyncFailed:
		r.mu.Lock()
		r.events = append(r.events, msg)
		r.mu.Unlock()
		if r.stream != nil {
			r.stream <- msg
		}
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
func (r *productionPlainRuntime) plainNotificationChannel() <-chan tea.Msg { return r.stream }

func runPlain(ctx context.Context, runtime plainRuntime, out io.Writer) error {
	seen := make(map[domain.EventID]bool)
	if err := writePlainFeedSeen(ctx, runtime, out, "Cached feed", seen); err != nil {
		return err
	}
	runtime.StartSync(ctx)
	var liveErrors []error
	if source, ok := runtime.(plainNotificationStream); ok && source.plainNotificationChannel() != nil {
		done := make(chan struct{})
		go func() { runtime.WaitSync(); close(done) }()
		for running := true; running; {
			select {
			case msg := <-source.plainNotificationChannel():
				if event, ok := msg.(syncer.SyncProgress); ok {
					if _, err := fmt.Fprintf(out, "%s · %d commits scanned · %d updates · %d batches\n", event.Source, event.Progress.Commits, event.Progress.Events, event.Progress.Batches); err != nil {
						liveErrors = append(liveErrors, err)
					}
					if err := writePlainFeedSeen(ctx, runtime, out, "", seen); err != nil {
						liveErrors = append(liveErrors, err)
					}
				}
			case <-done:
				running = false
			}
		}
	} else {
		runtime.WaitSync()
	}

	failures := liveErrors
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
				if _, err := fmt.Fprintf(out, "Synchronizing %s…\n", name); err != nil {
					failures = append(failures, err)
				}
			}
			switch event := record.result.(type) {
			case syncer.SyncCommitted:
				if _, err := fmt.Fprintf(out, "%s synchronized · %d updates\n", name, event.Result.Events); err != nil {
					failures = append(failures, err)
				}
			case syncer.SyncFailed:
				if _, err := fmt.Fprintf(out, "%s synchronization failed · %v\n", name, event.Err); err != nil {
					failures = append(failures, err)
				}
				failures = append(failures, event.Err)
			}
		}
	}
	if err := writePlainFeedSeen(ctx, runtime, out, "Refreshed feed", seen); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func writePlainFeed(ctx context.Context, runtime plainRuntime, out io.Writer, heading string) error {
	return writePlainFeedSeen(ctx, runtime, out, heading, make(map[domain.EventID]bool))
}

func writePlainFeedSeen(ctx context.Context, runtime plainRuntime, out io.Writer, heading string, seen map[domain.EventID]bool) error {
	filter, err := runtime.Preferences(ctx)
	if err != nil {
		return err
	}
	groups, err := runtime.QueryFeed(ctx, filter)
	if err != nil {
		return err
	}
	if heading != "" {
		if _, err := fmt.Fprintln(out, heading); err != nil {
			return err
		}
	}
	for _, group := range groups {
		for _, event := range group.Events {
			if seen[event.ID] {
				continue
			}
			seen[event.ID] = true
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
	plain := &productionPlainRuntime{stream: make(chan tea.Msg, 32)}
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

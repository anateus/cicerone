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
	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/syncer"
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
	case syncer.SyncQueued, syncer.SyncStarted, syncer.SyncProgress, syncer.SyncCommitted, syncer.SyncFailed, syncer.SyncCancelled:
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
	type fallbackKey struct {
		source string
		id     uint64
	}
	liveFallbackResults := make(map[fallbackKey]bool)
	liveFallbackStarts := make(map[string]bool)
	if source, ok := runtime.(plainNotificationStream); ok && source.plainNotificationChannel() != nil {
		stream := source.plainNotificationChannel()
		handleProgress := func(msg tea.Msg) {
			switch event := msg.(type) {
			case syncer.SyncQueued:
				activity := "synchronization"
				if event.HistoricalFallback {
					activity = "historical indexing"
				}
				if _, err := fmt.Fprintf(out, "%s %s queued\n", event.Source, activity); err != nil {
					liveErrors = append(liveErrors, err)
				}
			case syncer.SyncStarted:
				if event.HistoricalFallback {
					liveFallbackStarts[event.Source] = true
					if _, err := fmt.Fprintf(out, "Indexing %s history…\n", event.Source); err != nil {
						liveErrors = append(liveErrors, err)
					}
				}
			case syncer.SyncProgress:
				label := event.Source
				if event.HistoricalFallback {
					label += " history"
				}
				if _, err := fmt.Fprintf(out, "%s · %d commits scanned · %d updates · %d batches\n", label, event.Progress.Commits, event.Progress.Events, event.Progress.Batches); err != nil {
					liveErrors = append(liveErrors, err)
				}
				if err := writePlainFeedSeen(ctx, runtime, out, "", seen); err != nil {
					liveErrors = append(liveErrors, err)
				}
			case syncer.SyncCommitted:
				if event.HistoricalFallback {
					liveFallbackResults[fallbackKey{event.Source, event.ID}] = true
					if _, err := fmt.Fprintf(out, "%s historical indexing complete · %d updates\n", event.Source, event.Result.Events); err != nil {
						liveErrors = append(liveErrors, err)
					}
				}
			case syncer.SyncFailed:
				if event.HistoricalFallback {
					liveFallbackResults[fallbackKey{event.Source, event.ID}] = true
					if _, err := fmt.Fprintf(out, "%s historical indexing failed · %v\n", event.Source, event.Err); err != nil {
						liveErrors = append(liveErrors, err)
					}
					liveErrors = append(liveErrors, event.Err)
				}
			case syncer.SyncCancelled:
				if event.HistoricalFallback {
					liveFallbackResults[fallbackKey{event.Source, event.ID}] = true
					if _, err := fmt.Fprintf(out, "%s historical indexing canceled · %s\n", event.Source, event.Reason); err != nil {
						liveErrors = append(liveErrors, err)
					}
				}
			}
		}
		done := make(chan struct{})
		go func() { runtime.WaitSync(); close(done) }()
		for running := true; running; {
			select {
			case msg := <-stream:
				handleProgress(msg)
			case <-done:
				running = false
			}
		}
		for draining := true; draining; {
			select {
			case msg := <-stream:
				handleProgress(msg)
			default:
				draining = false
			}
		}
	} else {
		runtime.WaitSync()
	}

	failures := liveErrors
	if source, ok := runtime.(plainNotificationSource); ok {
		type stageNotifications struct {
			started bool
			result  tea.Msg
		}
		type sourceNotifications struct {
			normal, fallback stageNotifications
		}
		bySource := make(map[string]sourceNotifications)
		for _, msg := range source.plainNotifications() {
			var name string
			var fallback, started bool
			switch event := msg.(type) {
			case syncer.SyncStarted:
				name, fallback, started = event.Source, event.HistoricalFallback, true
			case syncer.SyncCommitted:
				name, fallback = event.Source, event.HistoricalFallback
			case syncer.SyncFailed:
				name, fallback = event.Source, event.HistoricalFallback
			case syncer.SyncCancelled:
				name, fallback = event.Source, event.HistoricalFallback
			default:
				continue
			}
			record := bySource[name]
			stage := &record.normal
			if fallback {
				stage = &record.fallback
			}
			if started {
				stage.started = true
			} else {
				stage.result = msg
			}
			bySource[name] = record
		}
		sources := make([]string, 0, len(bySource))
		for name := range bySource {
			sources = append(sources, name)
		}
		sort.Strings(sources)
		for _, name := range sources {
			record := bySource[name]
			if record.normal.started {
				if _, err := fmt.Fprintf(out, "Synchronizing %s…\n", name); err != nil {
					failures = append(failures, err)
				}
			}
			switch event := record.normal.result.(type) {
			case syncer.SyncCommitted:
				if _, err := fmt.Fprintf(out, "%s synchronized · %d updates\n", name, event.Result.Events); err != nil {
					failures = append(failures, err)
				}
			case syncer.SyncFailed:
				if _, err := fmt.Fprintf(out, "%s synchronization failed · %v\n", name, event.Err); err != nil {
					failures = append(failures, err)
				}
				failures = append(failures, event.Err)
			case syncer.SyncCancelled:
				if _, err := fmt.Fprintf(out, "%s synchronization canceled · %s\n", name, event.Reason); err != nil {
					failures = append(failures, err)
				}
			}
			if record.fallback.started && !liveFallbackStarts[name] {
				if _, err := fmt.Fprintf(out, "Indexing %s history…\n", name); err != nil {
					failures = append(failures, err)
				}
			}
			switch event := record.fallback.result.(type) {
			case syncer.SyncCommitted:
				if liveFallbackResults[fallbackKey{event.Source, event.ID}] {
					continue
				}
				if _, err := fmt.Fprintf(out, "%s historical indexing complete · %d updates\n", name, event.Result.Events); err != nil {
					failures = append(failures, err)
				}
			case syncer.SyncFailed:
				if liveFallbackResults[fallbackKey{event.Source, event.ID}] {
					continue
				}
				if _, err := fmt.Fprintf(out, "%s historical indexing failed · %v\n", name, event.Err); err != nil {
					failures = append(failures, err)
				}
				failures = append(failures, event.Err)
			case syncer.SyncCancelled:
				if liveFallbackResults[fallbackKey{event.Source, event.ID}] {
					continue
				}
				if _, err := fmt.Fprintf(out, "%s historical indexing canceled · %s\n", name, event.Reason); err != nil {
					failures = append(failures, err)
				}
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
	services, err := newRuntime(home, plain.notify, 0)
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

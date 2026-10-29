package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
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
	case syncer.SyncQueued, syncer.SyncStarted, syncer.SyncPhase, syncer.SyncProgress, syncer.SyncCommitted, syncer.SyncFailed, syncer.SyncCancelled:
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
	type notificationKey struct {
		source   string
		id       uint64
		fallback bool
		kind     string
	}
	liveNotifications := make(map[notificationKey]bool)
	report := func(msg tea.Msg) {
		var line string
		var failure error
		switch event := msg.(type) {
		case syncer.SyncStarted:
			if event.HistoricalFallback {
				line = fmt.Sprintf("Indexing %s history…", event.Source)
			} else if event.Source == "installed" || event.Source == "repositories" {
				line = fmt.Sprintf("Preparing %s…", event.Source)
			} else {
				line = fmt.Sprintf("Synchronizing %s…", event.Source)
			}
		case syncer.SyncPhase:
			label := event.Source
			if event.HistoricalFallback {
				label += " history"
			}
			switch event.Phase {
			case syncer.PhaseFetching:
				line = fmt.Sprintf("%s · fetching/checking…", label)
			case syncer.PhaseIndexing:
				line = fmt.Sprintf("%s · indexing…", label)
			}
		case syncer.SyncCommitted:
			if event.HistoricalFallback {
				line = fmt.Sprintf("%s historical indexing complete · %d updates", event.Source, event.Result.Events)
			} else if event.Source == "installed" || event.Source == "repositories" {
				line = fmt.Sprintf("%s preparation complete", event.Source)
			} else {
				line = fmt.Sprintf("%s synchronized · %d updates", event.Source, event.Result.Events)
			}
		case syncer.SyncFailed:
			activity := "synchronization"
			if event.HistoricalFallback {
				activity = "historical indexing"
			} else if event.Source == "installed" || event.Source == "repositories" {
				activity = "preparation"
			}
			line = fmt.Sprintf("%s %s failed · %v", event.Source, activity, event.Err)
			failure = event.Err
		case syncer.SyncCancelled:
			activity := "synchronization"
			if event.HistoricalFallback {
				activity = "historical indexing"
			} else if event.Source == "installed" || event.Source == "repositories" {
				activity = "preparation"
			}
			line = fmt.Sprintf("%s %s canceled · %s", event.Source, activity, event.Reason)
		}
		if line != "" {
			if _, err := fmt.Fprintln(out, line); err != nil {
				liveErrors = append(liveErrors, err)
			}
		}
		if failure != nil {
			liveErrors = append(liveErrors, failure)
		}
	}
	keyFor := func(msg tea.Msg) (notificationKey, bool) {
		switch event := msg.(type) {
		case syncer.SyncStarted:
			return notificationKey{event.Source, event.ID, event.HistoricalFallback, "started"}, true
		case syncer.SyncPhase:
			return notificationKey{event.Source, event.ID, event.HistoricalFallback, "phase:" + string(event.Phase)}, true
		case syncer.SyncCommitted:
			return notificationKey{event.Source, event.ID, event.HistoricalFallback, "committed"}, true
		case syncer.SyncFailed:
			return notificationKey{event.Source, event.ID, event.HistoricalFallback, "failed"}, true
		case syncer.SyncCancelled:
			return notificationKey{event.Source, event.ID, event.HistoricalFallback, "cancelled"}, true
		default:
			return notificationKey{}, false
		}
	}
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
			case syncer.SyncStarted, syncer.SyncPhase, syncer.SyncCommitted, syncer.SyncFailed, syncer.SyncCancelled:
				key, _ := keyFor(msg)
				liveNotifications[key] = true
				report(msg)
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

	if source, ok := runtime.(plainNotificationSource); ok {
		type sourceNotifications struct {
			normal, fallback []tea.Msg
		}
		bySource := make(map[string]sourceNotifications)
		for _, msg := range source.plainNotifications() {
			key, ok := keyFor(msg)
			if !ok || liveNotifications[key] {
				continue
			}
			record := bySource[key.source]
			if key.fallback {
				record.fallback = append(record.fallback, msg)
			} else {
				record.normal = append(record.normal, msg)
			}
			bySource[key.source] = record
		}
		sources := make([]string, 0, len(bySource))
		for name := range bySource {
			sources = append(sources, name)
		}
		sort.Strings(sources)
		for _, name := range sources {
			record := bySource[name]
			for _, msg := range record.normal {
				report(msg)
			}
			for _, msg := range record.fallback {
				report(msg)
			}
		}
	}
	failures := liveErrors
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
		if summary := plainFilterSummary(filter); summary != "" {
			heading += " · " + summary
		}
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

func plainFilterSummary(filter domain.FeedFilter) string {
	var parts []string
	if len(filter.Types) > 0 {
		var values []string
		for typ, enabled := range filter.Types {
			if enabled {
				values = append(values, string(typ))
			}
		}
		sort.Strings(values)
		if len(values) == 0 {
			values = append(values, "none")
		}
		parts = append(parts, "type: "+strings.Join(values, ", "))
	}
	switch filter.GroupScope {
	case domain.GroupScopeAll:
		if len(filter.Types) > 0 || len(filter.Kinds) > 0 || filter.Query != "" || filter.Horizon > 0 {
			parts = append(parts, "group: all (excluding hidden)")
		}
	case domain.GroupScopeUngrouped:
		parts = append(parts, "group: ungrouped")
	case domain.GroupScopeUser:
		parts = append(parts, fmt.Sprintf("group: %d", filter.GroupTarget))
	}
	if len(filter.Kinds) > 0 {
		var values []string
		for kind, enabled := range filter.Kinds {
			if enabled {
				values = append(values, string(kind))
			}
		}
		sort.Strings(values)
		if len(values) == 0 {
			values = append(values, "none")
		}
		parts = append(parts, "kind: "+strings.Join(values, ", "))
	}
	if filter.Query != "" {
		scope := filter.Search
		if scope == "" {
			scope = domain.SearchNames
		}
		parts = append(parts, fmt.Sprintf("search: %q (%s)", filter.Query, scope))
	}
	if filter.Horizon > 0 {
		parts = append(parts, "horizon: "+filter.Horizon.String())
	}
	return strings.Join(parts, " · ")
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

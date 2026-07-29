package domain

import (
	"slices"
	"strings"
	"time"
)

// SearchScope is the broadest cached content category included in a feed search.
type SearchScope string

const (
	SearchNames        SearchScope = "names"
	SearchDescriptions SearchScope = "descriptions"
	SearchChangelogs   SearchScope = "changelogs"
	SearchREADMEs      SearchScope = "readmes"
)

// FeedFilter controls which update events appear and how they are grouped.
type FeedFilter struct {
	Now     time.Time
	Horizon time.Duration
	Kinds   map[EventKind]bool
	Types   map[PackageType]bool
	Query   string
	Search  SearchScope
	RollUp  bool
}

// FeedGroup is one feed row. Events contains the newest event first.
type FeedGroup struct {
	ID     EventID
	Events []UpdateEvent
}

// BuildFeed filters, sorts, and optionally rolls up update events.
func BuildFeed(events []UpdateEvent, installed map[PackageID]bool, f FeedFilter) []FeedGroup {
	type packageKind struct {
		packageID PackageID
		kind      EventKind
	}

	candidates := make([]UpdateEvent, 0, len(events))
	for _, event := range events {
		if !matchesFeedFilter(event, f) {
			continue
		}
		candidates = append(candidates, event)
	}

	insideHorizon := make(map[packageKind]bool)
	for _, event := range candidates {
		if !beforeHorizon(event, f) {
			insideHorizon[packageKind{event.PackageID, event.Kind}] = true
		}
	}

	newestFallback := make(map[packageKind]UpdateEvent)
	filtered := make([]UpdateEvent, 0, len(candidates))
	for _, event := range candidates {
		if !beforeHorizon(event, f) {
			filtered = append(filtered, event)
			continue
		}

		key := packageKind{event.PackageID, event.Kind}
		if !installed[event.PackageID] || insideHorizon[key] {
			continue
		}
		current, ok := newestFallback[key]
		if !ok || eventLess(event, current) {
			newestFallback[key] = event
		}
	}
	for _, event := range newestFallback {
		filtered = append(filtered, event)
	}

	slices.SortFunc(filtered, func(a, b UpdateEvent) int {
		if eventLess(a, b) {
			return -1
		}
		if eventLess(b, a) {
			return 1
		}
		return 0
	})

	groups := make([]FeedGroup, 0, len(filtered))
	for _, event := range filtered {
		if f.RollUp && len(groups) > 0 && groups[len(groups)-1].Events[0].PackageID == event.PackageID {
			groups[len(groups)-1].Events = append(groups[len(groups)-1].Events, event)
			continue
		}
		groups = append(groups, FeedGroup{ID: event.ID, Events: []UpdateEvent{event}})
	}
	return groups
}

func matchesFeedFilter(event UpdateEvent, f FeedFilter) bool {
	if len(f.Kinds) > 0 && !f.Kinds[event.Kind] {
		return false
	}
	if len(f.Types) > 0 && !f.Types[event.Type] {
		return false
	}
	query := strings.ToLower(strings.TrimSpace(f.Query))
	return query == "" || strings.Contains(strings.ToLower(event.Name), query) || strings.Contains(strings.ToLower(string(event.PackageID)), query)
}

func beforeHorizon(event UpdateEvent, f FeedFilter) bool {
	return f.Horizon > 0 && event.Time.Before(f.Now.Add(-f.Horizon))
}

func eventLess(a, b UpdateEvent) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	return a.ID < b.ID
}

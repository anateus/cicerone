package tui

import (
	"cicerone/internal/domain"
	"cicerone/internal/store"
)

type FeedLoaded struct {
	RequestID uint64
	Groups    []domain.FeedGroup
	Err       error
}

type DatasetChanged struct{ Stale bool }
type WindowSize struct{ Width, Height int }

type ChangelogLoaded struct {
	RequestID   uint64
	SelectionID uint64
	EventID     domain.EventID
	PackageID   domain.PackageID
	Sections    []store.ChangelogSection
	Err         error
}

type Notify struct {
	RequestID   uint64
	SelectionID uint64
	Text        string
	Err         error
}

type PreferencesLoaded struct {
	Filter domain.FeedFilter
	Err    error
}
type preferencesSaved struct{ Err error }
type ChangelogDebounced struct{ SelectionID uint64 }
type ToggleFilter struct{ Kind domain.EventKind }
type ToggleTypeFilter struct{ Type domain.PackageType }
type ToggleRollUp struct{}
type ToggleExpanded struct{}
type SearchChanged struct{ Text string }
type SetLightMode struct{ Light bool }

package domain

import "time"

// EventID uniquely identifies an immutable package update event.
type EventID string

// EventKind classifies the type of package change.
type EventKind string

const (
	EventVersion  EventKind = "version"
	EventRevision EventKind = "revision"
	EventMetadata EventKind = "metadata"
)

// UpdateCadence describes the observed interval between a package's version updates.
type UpdateCadence string

const (
	UpdateCadenceUnknown  UpdateCadence = ""
	UpdateCadenceRare     UpdateCadence = "rare"
	UpdateCadenceFrequent UpdateCadence = "frequent"
)

// ClassifyUpdateCadence returns a signal only when at least two version updates
// have been observed. The quiet middle range keeps the feed from labelling
// ordinary release rhythms.
func ClassifyUpdateCadence(count int, first, last time.Time) UpdateCadence {
	averageInterval := AverageUpdateInterval(count, first, last)
	if averageInterval == 0 {
		return UpdateCadenceUnknown
	}
	switch {
	case averageInterval <= 7*24*time.Hour/2:
		return UpdateCadenceFrequent
	case averageInterval >= 120*24*time.Hour:
		return UpdateCadenceRare
	default:
		return UpdateCadenceUnknown
	}
}

// AverageUpdateInterval returns the observed mean time between version updates.
func AverageUpdateInterval(count int, first, last time.Time) time.Duration {
	if count < 2 || first.IsZero() || !last.After(first) {
		return 0
	}
	return last.Sub(first) / time.Duration(count-1)
}

// NewEventID constructs the stable event identity used across repositories.
func NewEventID(repository, commit string, packageID PackageID, kind EventKind) EventID {
	return EventID(repository + ":" + commit + ":" + string(packageID) + ":" + string(kind))
}

// UpdateEvent records an immutable change to a package definition.
type UpdateEvent struct {
	ID             EventID
	PackageID      PackageID
	Name           string
	Type           PackageType
	Kind           EventKind
	OldVersion     string
	NewVersion     string
	OldRevision    string
	NewRevision    string
	Repository     string
	DefinitionPath string
	Commit         string
	Time           time.Time
	Diagnostic     string
	Installed      bool
	Seen           bool
	Cadence        UpdateCadence
	UpdateInterval time.Duration
}

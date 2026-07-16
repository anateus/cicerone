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
}

package history

import "cicerone/internal/domain"

type Classification struct {
	Kind       domain.EventKind
	Ambiguous  bool
	Diagnostic string
}

// Classify applies version, revision, then metadata priority.
func Classify(before, after *Definition) Classification {
	identity := after
	if identity == nil {
		identity = before
	}
	if identity == nil || identity.Name == "" || identity.Type == "" {
		return Classification{Ambiguous: true, Diagnostic: "definition identity could not be derived"}
	}
	if before == nil {
		if after.Version == "" {
			return Classification{Ambiguous: true, Diagnostic: "definition version could not be derived"}
		}
		return Classification{Kind: domain.EventVersion}
	}
	if after == nil {
		return Classification{Kind: domain.EventMetadata}
	}
	if before.Version == "" || after.Version == "" {
		return Classification{Ambiguous: true, Diagnostic: "definition version could not be derived"}
	}
	if before.Version != after.Version {
		return Classification{Kind: domain.EventVersion}
	}
	if before.Revision != after.Revision {
		return Classification{Kind: domain.EventRevision}
	}
	return Classification{Kind: domain.EventMetadata}
}

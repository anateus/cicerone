package history

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/gitrepo"
)

const packageHistoryCommitLimit = 100

// IndexPackage looks up the newest unambiguous version or revision change for
// one definition in the already available repository. It never advances the
// repository-wide history cursor or marks missing history as exhausted.
func (i *Indexer) IndexPackage(ctx context.Context, source gitrepo.Source, packageID domain.PackageID, definitionPath string) (bool, error) {
	if i == nil || i.store == nil {
		return false, fmt.Errorf("history indexer requires a store")
	}
	configured := i.repository.Source()
	if source.Name == "" || source.Path == "" || configured.Name != source.Name || configured.Path != source.Path || configured.Kind != source.Kind {
		return false, fmt.Errorf("history source does not match repository")
	}
	var packageType domain.PackageType
	var tap string
	switch source.Kind {
	case string(domain.PackageFormula):
		packageType, tap = domain.PackageFormula, "core"
	case string(domain.PackageCask):
		packageType, tap = domain.PackageCask, "cask"
	default:
		return false, fmt.Errorf("unsupported history source kind %q", source.Kind)
	}
	if source.Name != tap && source.Name != "homebrew-"+tap {
		return false, fmt.Errorf("unsupported history source %q", source.Name)
	}
	prefix := "Formula/"
	if packageType == domain.PackageCask {
		prefix = "Casks/"
	}
	if !strings.HasPrefix(definitionPath, prefix) || path.Ext(definitionPath) != ".rb" ||
		path.Base(definitionPath) == ".rb" || path.Clean(definitionPath) != definitionPath ||
		strings.ContainsAny(definitionPath, "\\:\x00") || strings.HasPrefix(definitionPath, "-") {
		return false, fmt.Errorf("invalid %s definition path %q", packageType, definitionPath)
	}
	name := strings.TrimSuffix(path.Base(definitionPath), ".rb")
	if name == "" || (packageID != domain.PackageID(name) && packageID != domain.PackageID("homebrew/"+tap+"/"+name)) {
		return false, fmt.Errorf("package %q does not match definition %q", packageID, definitionPath)
	}
	if storedType, found, err := i.store.HistoryPackageType(ctx, packageID); err != nil {
		return false, err
	} else if found && storedType != packageType {
		return false, fmt.Errorf("package %q has type %q, not %q", packageID, storedType, packageType)
	}
	commits, err := i.repository.PathCommits(ctx, definitionPath, packageHistoryCommitLimit)
	if err != nil {
		return false, err
	}
	var selected *domain.UpdateEvent
scan:
	for _, commit := range commits {
		for _, change := range commit.Changes {
			// A rename into or out of this path is not evidence of this
			// package's update. Never attribute the other path's history.
			if change.Path != definitionPath || change.OldPath != "" ||
				(change.Status != "A" && change.Status != "M") {
				continue
			}
			if change.Status == "A" {
				ambiguous, err := i.repository.AmbiguousAddition(ctx, commit.Hash, definitionPath)
				if err != nil {
					return false, err
				}
				if ambiguous {
					continue
				}
			}
			before, _, err := i.definition(ctx, commit.Hash+"^", definitionPath, change.Status == "A")
			if err != nil {
				return false, err
			}
			after, diagnostics, err := i.definition(ctx, commit.Hash, definitionPath, false)
			if err != nil {
				return false, err
			}
			if after == nil || after.Name != name || after.Type != packageType || after.FullName != name ||
				(before != nil && (before.Name != name || before.FullName != name || before.Type != packageType)) {
				continue
			}
			classification := Classify(before, after)
			if classification.Ambiguous || (classification.Kind != domain.EventVersion && classification.Kind != domain.EventRevision) {
				continue
			}
			event := domain.UpdateEvent{
				ID:        domain.NewEventID(source.Name, commit.Hash, packageID, classification.Kind),
				PackageID: packageID, Name: name, Type: packageType, Kind: classification.Kind,
				Repository: source.Name, DefinitionPath: definitionPath, Commit: commit.Hash,
				Time: commit.AuthorTime, Diagnostic: strings.Join(diagnostics, "; "),
				NewVersion: after.Version, NewRevision: after.Revision,
			}
			if before != nil {
				event.OldVersion, event.OldRevision = before.Version, before.Revision
			}
			if classification.Kind == domain.EventVersion {
				selected = &event
				break scan
			}
			if selected == nil {
				selected = &event // Fall back to the newest revision if no version is found.
			}
		}
	}
	if selected == nil {
		return false, nil
	}
	if err := i.store.UpsertEvents(ctx, []domain.UpdateEvent{*selected}); err != nil {
		return false, err
	}
	return true, nil
}

package history

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"cicerone/internal/domain"
	"cicerone/internal/gitrepo"
	"cicerone/internal/store"
)

type Request struct {
	Since     time.Time
	Installed []domain.PackageID
	Kinds     map[domain.EventKind]bool
	Progress  func(Progress)
}
type Progress struct{ Commits, Events, Diagnostics, Batches int }

const historyBatchCommits = 100

type Result struct {
	Events, Diagnostics int
	Head                string
	Since               time.Time
}
type Indexer struct {
	repository gitrepo.Repository
	store      *store.Store
}

func NewIndexer(repository gitrepo.Repository, destination *store.Store) *Indexer {
	return &Indexer{repository: repository, store: destination}
}

func (i *Indexer) Index(ctx context.Context, source gitrepo.Source, req Request) (Result, error) {
	if i == nil || i.store == nil {
		return Result{}, fmt.Errorf("history indexer requires a store")
	}
	head, err := i.repository.Head(ctx)
	if err != nil {
		return Result{}, err
	}
	state, exists, err := i.store.HistoryState(ctx, source.Name)
	if err != nil {
		return Result{}, err
	}
	since := req.Since.UTC()
	if exists && (since.IsZero() || (!state.Since.IsZero() && state.Since.Before(since))) {
		since = state.Since
	}
	var ranges []gitrepo.Range
	var remove []string
	if !exists {
		ranges = append(ranges, gitrepo.Range{Revision: head, Since: since})
	} else if state.Head != head {
		base, e := i.repository.MergeBase(ctx, state.Head, head)
		if e != nil {
			return Result{}, e
		}
		if base != state.Head {
			old, e := i.repository.Commits(ctx, gitrepo.Range{Revision: base + ".." + state.Head})
			if e != nil {
				return Result{}, e
			}
			for _, c := range old {
				remove = append(remove, c.Hash)
			}
		}
		ranges = append(ranges, gitrepo.Range{Revision: base + ".." + head})
	}
	if exists && !req.Since.IsZero() && req.Since.Before(state.Since) {
		ranges = append(ranges, gitrepo.Range{Revision: head, Since: req.Since, Until: state.Since})
	}
	seen := map[string]bool{}
	missing := map[string]bool{}
	for _, installedID := range req.Installed {
		packageID, e := i.store.ResolveHistoryPackageID(ctx, source.Name, installedID)
		if e != nil {
			return Result{}, e
		}
		for _, kind := range []domain.EventKind{domain.EventVersion, domain.EventRevision, domain.EventMetadata} {
			if len(req.Kinds) > 0 && !req.Kinds[kind] {
				continue
			}
			has, e := i.store.HasHistoryEvent(ctx, source.Name, packageID, kind)
			if e != nil {
				return Result{}, e
			}
			if !has {
				missing[string(packageID)+"\x00"+string(kind)] = true
			}
		}
	}
	fallbackIndex := -1
	if len(missing) > 0 {
		fallbackIndex = len(ranges)
		ranges = append(ranges, gitrepo.Range{Revision: head})
	}
	var events []domain.UpdateEvent
	var aliases []store.HistoryAlias
	var persistedDiagnostics []store.HistoryDiagnostic
	progress := Progress{}
	batchCommits := 0
	flush := func() error {
		if batchCommits == 0 {
			return nil
		}
		if err := i.store.ApplyHistoryBatch(ctx, store.HistoryBatch{Repository: source.Name, Events: events, Aliases: aliases, Diagnostics: persistedDiagnostics}); err != nil {
			return err
		}
		progress.Batches++
		if req.Progress != nil {
			req.Progress(progress)
		}
		events = nil
		aliases = nil
		persistedDiagnostics = nil
		batchCommits = 0
		return nil
	}
	for rangeIndex, r := range ranges {
		e := i.repository.WalkCommits(ctx, r, func(commit gitrepo.Commit) error {
			if seen[commit.Hash] {
				return nil
			}
			if rangeIndex == fallbackIndex && !since.IsZero() && !commit.AuthorTime.Before(since) {
				return nil
			}
			seen[commit.Hash] = true
			progress.Commits++
			batchCommits++
			for _, change := range commit.Changes {
				if filepath.Ext(change.Path) != ".rb" && filepath.Ext(change.OldPath) != ".rb" {
					continue
				}
				beforePath := change.Path
				if change.OldPath != "" {
					beforePath = change.OldPath
				}
				before, bd, e := i.definition(ctx, commit.Hash+"^", beforePath, change.Status == "A")
				if e != nil {
					return e
				}
				after, ad, e := i.definition(ctx, commit.Hash, change.Path, change.Status == "D")
				if e != nil {
					return e
				}
				progress.Diagnostics += len(bd) + len(ad)
				classification := Classify(before, after)
				if classification.Ambiguous {
					progress.Diagnostics++
					classification.Kind = domain.EventMetadata
				}
				for _, message := range append(append([]string{}, bd...), ad...) {
					persistedDiagnostics = append(persistedDiagnostics, store.HistoryDiagnostic{Repository: source.Name, Commit: commit.Hash, Path: change.Path, Message: message})
				}
				if classification.Diagnostic != "" {
					persistedDiagnostics = append(persistedDiagnostics, store.HistoryDiagnostic{Repository: source.Name, Commit: commit.Hash, Path: change.Path, Message: classification.Diagnostic})
				}
				identity := after
				if identity == nil {
					identity = before
				}
				if identity == nil {
					continue
				}
				pkgID := domain.PackageID(identity.FullName)
				if pkgID == "" {
					pkgID = domain.PackageID(identity.Name)
				}
				if change.Status == "R" && before != nil && before.FullName != identity.FullName {
					aliases = append(aliases, store.HistoryAlias{Alias: before.FullName, PackageID: pkgID, Repository: source.Name, Commit: commit.Hash})
				}
				if len(req.Kinds) > 0 && !req.Kinds[classification.Kind] {
					continue
				}
				key := string(pkgID) + "\x00" + string(classification.Kind)
				if rangeIndex == fallbackIndex && !missing[key] {
					continue
				}
				diagnostic := strings.Join(append(append(bd, ad...), classification.Diagnostic), "; ")
				event := domain.UpdateEvent{ID: domain.NewEventID(source.Name, commit.Hash, pkgID, classification.Kind), PackageID: pkgID, Name: identity.Name, Type: identity.Type, Kind: classification.Kind, Repository: source.Name, DefinitionPath: change.Path, Commit: commit.Hash, Time: commit.AuthorTime, Diagnostic: diagnostic}
				if before != nil {
					event.OldVersion = before.Version
					event.OldRevision = before.Revision
				}
				if after != nil {
					event.NewVersion = after.Version
					event.NewRevision = after.Revision
				}
				events = append(events, event)
				progress.Events++
				delete(missing, key)
			}
			if batchCommits == historyBatchCommits {
				return flush()
			}
			return nil
		})
		if e != nil {
			return Result{}, e
		}
	}
	if err := flush(); err != nil {
		return Result{}, err
	}
	if err := i.store.FinalizeHistory(ctx, store.HistoryBatch{Repository: source.Name, Path: source.Path, Head: head, Since: since, RemoveCommits: remove}); err != nil {
		return Result{}, err
	}
	return Result{Events: progress.Events, Diagnostics: progress.Diagnostics, Head: head, Since: since}, nil
}

func (i *Indexer) definition(ctx context.Context, revision, path string, absent bool) (*Definition, []string, error) {
	if absent || path == "" {
		return nil, nil, nil
	}
	body, err := i.repository.Blob(ctx, revision, path)
	if err != nil {
		return nil, nil, err
	}
	d, diagnostics := ParseDefinition(path, body)
	return d, diagnostics, nil
}

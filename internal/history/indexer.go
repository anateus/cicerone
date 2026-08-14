package history

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
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

const (
	historyInitialBatchCommits = 10
	historyBatchCommits        = 100
	historyProgressCommits     = 10
	historyCancelFlushWindow   = 500 * time.Millisecond
)

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
	var rangeKeys []string
	var remove []string
	if !exists {
		ranges = append(ranges, gitrepo.Range{Revision: head, Since: since})
		rangeKeys = append(rangeKeys, historyScanKey("initial", time.Time{}, time.Time{}, req, false))
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
		rangeKeys = append(rangeKeys, historyScanKey("forward:"+state.Head, time.Time{}, time.Time{}, req, false))
	}
	if exists && !req.Since.IsZero() && req.Since.Before(state.Since) {
		ranges = append(ranges, gitrepo.Range{Revision: head, Since: req.Since, Until: state.Since})
		rangeKeys = append(rangeKeys, historyScanKey("backward", req.Since, state.Since, req, false))
	}
	seen := map[string]bool{}
	missing := map[string]store.HistoryCoverage{}
	for _, installedID := range req.Installed {
		if strings.Contains(string(installedID), "/") {
			continue
		}
		if source.Kind != "" {
			packageType, found, e := i.store.HistoryPackageType(ctx, installedID)
			if e != nil {
				return Result{}, e
			}
			if found && string(packageType) != source.Kind {
				continue
			}
		}
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
				covered, e := i.store.HasHistoryFallbackCoverage(ctx, source.Name, packageID, kind)
				if e != nil {
					return Result{}, e
				}
				if !covered {
					missing[string(packageID)+"\x00"+string(kind)] = store.HistoryCoverage{PackageID: packageID, Kind: kind}
				}
			}
		}
	}
	fallbackIndex := -1
	if len(missing) > 0 {
		fallbackIndex = len(ranges)
		ranges = append(ranges, gitrepo.Range{Revision: head})
		rangeKeys = append(rangeKeys, historyScanKey("fallback", time.Time{}, time.Time{}, req, true))
	}
	var events []domain.UpdateEvent
	var aliases []store.HistoryAlias
	var persistedDiagnostics []store.HistoryDiagnostic
	var processed []store.HistoryProgress
	progress := Progress{}
	countedProgress := map[string]bool{}
	loadedCheckpoints := map[string]bool{}
	encountered := map[string]bool{}
	batchCommits := 0
	batchLimit := historyInitialBatchCommits
	flush := func(flushCtx context.Context, scanKey string) error {
		if batchCommits == 0 {
			return nil
		}
		if err := i.store.ApplyHistoryBatch(flushCtx, store.HistoryBatch{Repository: source.Name, ScanKey: scanKey, Events: events, Aliases: aliases, Diagnostics: persistedDiagnostics, Processed: processed}); err != nil {
			return err
		}
		progress.Batches++
		if req.Progress != nil {
			req.Progress(progress)
		}
		events = nil
		aliases = nil
		persistedDiagnostics = nil
		processed = nil
		batchCommits = 0
		batchLimit = historyBatchCommits
		return nil
	}
	for rangeIndex, r := range ranges {
		scanKey := rangeKeys[rangeIndex]
		checkpointRows, e := i.store.HistoryScanProgress(ctx, source.Name, scanKey)
		if e != nil {
			return Result{}, e
		}
		checkpoint := make(map[string]bool, len(checkpointRows))
		for _, item := range checkpointRows {
			checkpoint[item.Commit] = true
			loadedCheckpoints[item.Commit] = true
			if !countedProgress[item.Commit] {
				countedProgress[item.Commit] = true
				progress.Commits++
				progress.Events += item.Events
				progress.Diagnostics += item.Diagnostics
			}
		}
		if len(checkpointRows) > 0 {
			if req.Progress != nil {
				req.Progress(progress)
			}
		}
		e = i.repository.WalkCommits(ctx, r, func(commit gitrepo.Commit) error {
			encountered[commit.Hash] = true
			if seen[commit.Hash] {
				return nil
			}
			if checkpoint[commit.Hash] {
				seen[commit.Hash] = true
				return nil
			}
			if rangeIndex == fallbackIndex && !since.IsZero() && !commit.AuthorTime.Before(since) {
				return nil
			}
			seen[commit.Hash] = true
			eventsBefore := progress.Events
			diagnosticsBefore := progress.Diagnostics
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
				if rangeIndex == fallbackIndex {
					if _, wanted := missing[key]; !wanted {
						continue
					}
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
			processed = append(processed, store.HistoryProgress{Commit: commit.Hash, Events: progress.Events - eventsBefore, Diagnostics: progress.Diagnostics - diagnosticsBefore})
			if batchCommits == batchLimit {
				return flush(ctx, scanKey)
			}
			if batchCommits%historyProgressCommits == 0 && req.Progress != nil {
				req.Progress(progress)
			}
			return nil
		})
		if e != nil {
			if batchCommits > 0 && ctx.Err() != nil {
				flushCtx, cancelFlush := context.WithTimeout(context.WithoutCancel(ctx), historyCancelFlushWindow)
				flushErr := flush(flushCtx, scanKey)
				cancelFlush()
				if flushErr != nil {
					return Result{}, errors.Join(e, flushErr)
				}
			}
			return Result{}, e
		}
		if err := flush(ctx, scanKey); err != nil {
			return Result{}, err
		}
		if fallbackIndex > 0 && rangeIndex == fallbackIndex-1 {
			if err := i.store.FinalizeHistory(ctx, store.HistoryBatch{
				Repository:        source.Name,
				Path:              source.Path,
				Head:              head,
				Since:             since,
				RemoveCommits:     remove,
				CompletedScanKeys: append([]string(nil), rangeKeys[:fallbackIndex]...),
			}); err != nil {
				return Result{}, err
			}
		}
	}
	removeSet := make(map[string]bool, len(remove))
	for _, commit := range remove {
		removeSet[commit] = true
	}
	for commit := range loadedCheckpoints {
		if !encountered[commit] && !removeSet[commit] {
			remove = append(remove, commit)
		}
	}
	var exhausted []store.HistoryCoverage
	if fallbackIndex >= 0 {
		exhausted = make([]store.HistoryCoverage, 0, len(missing))
		for _, coverage := range missing {
			exhausted = append(exhausted, coverage)
		}
	}
	if err := i.store.FinalizeHistory(ctx, store.HistoryBatch{Repository: source.Name, Path: source.Path, Head: head, Since: since, RemoveCommits: remove, Exhausted: exhausted}); err != nil {
		return Result{}, err
	}
	return Result{Events: progress.Events, Diagnostics: progress.Diagnostics, Head: head, Since: since}, nil
}

func historyScanKey(label string, since, until time.Time, req Request, includeInstalled bool) string {
	var installed []string
	if includeInstalled {
		installed = make([]string, len(req.Installed))
		for index, id := range req.Installed {
			installed[index] = string(id)
		}
		sort.Strings(installed)
	}
	var kinds []string
	for kind, enabled := range req.Kinds {
		if enabled {
			kinds = append(kinds, string(kind))
		}
	}
	sort.Strings(kinds)
	identity := strings.Join([]string{
		label,
		since.UTC().Format(time.RFC3339Nano),
		until.UTC().Format(time.RFC3339Nano),
		strings.Join(installed, "\x00"),
		strings.Join(kinds, "\x00"),
	}, "\x01")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
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

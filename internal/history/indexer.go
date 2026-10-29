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

	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/gitrepo"
	"github.com/anateus/cicerone/internal/store"
)

type Request struct {
	Since     time.Time
	Installed []domain.PackageID
	Kinds     map[domain.EventKind]bool
	Progress  func(Progress)
}
type Progress struct{ Commits, Events, Diagnostics, Batches int }

const (
	historyScanFormatVersion   = "mainline-merges-v1"
	historyInitialBatchCommits = 1
	historyBatchCommits        = 10
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

func (i *Indexer) Index(ctx context.Context, source gitrepo.Source, req Request) (result Result, err error) {
	if i == nil || i.store == nil {
		return Result{}, fmt.Errorf("history indexer requires a store")
	}
	var blobs *gitrepo.BlobReader
	defer func() {
		if blobs != nil {
			err = errors.Join(err, blobs.Close())
		}
	}()
	definition := func(revision, path string) (*Definition, []string, error) {
		if path == "" {
			return nil, nil, nil
		}
		if blobs == nil {
			var openErr error
			blobs, openErr = i.repository.OpenBlobReader(ctx)
			if openErr != nil {
				return nil, nil, openErr
			}
		}
		body, readErr := blobs.Blob(revision, path)
		if readErr != nil {
			return nil, nil, readErr
		}
		parsed, diagnostics := ParseDefinition(path, body)
		return parsed, diagnostics, nil
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
	prefetchOwned := i.repository.Source().Owned
	var prefetcher *gitrepo.BlobPrefetcher
	var scanCursor *store.HistoryScanCursor
	flush := func(flushCtx context.Context, scanKey string) error {
		if batchCommits == 0 {
			return nil
		}
		if err := i.store.ApplyHistoryBatch(flushCtx, store.HistoryBatch{Repository: source.Name, ScanKey: scanKey, Cursor: scanCursor, Events: events, Aliases: aliases, Diagnostics: persistedDiagnostics, Processed: processed}); err != nil {
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
		return flushCtx.Err()
	}
	for rangeIndex, r := range ranges {
		scanCursor = nil
		scanKey := rangeKeys[rangeIndex]
		checkpointRows, e := i.store.HistoryScanProgress(ctx, source.Name, scanKey)
		if e != nil {
			return Result{}, e
		}
		if rangeIndex == fallbackIndex && len(missing) == 0 && len(checkpointRows) == 0 {
			continue
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
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		var lastWalkCommit string
		processCommit := func(commit gitrepo.Commit) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			lastWalkCommit = commit.Hash
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
			savedProgress := progress
			eventStart, aliasStart, diagnosticStart := len(events), len(aliases), len(persistedDiagnostics)
			var foundMissing []store.HistoryCoverage
			complete := false
			defer func() {
				if !complete {
					events = events[:eventStart]
					aliases = aliases[:aliasStart]
					persistedDiagnostics = persistedDiagnostics[:diagnosticStart]
					progress = savedProgress
					for _, coverage := range foundMissing {
						missing[string(coverage.PackageID)+"\x00"+string(coverage.Kind)] = coverage
					}
				}
			}()
			eventsBefore := progress.Events
			diagnosticsBefore := progress.Diagnostics
			progress.Commits++
			for _, change := range commit.Changes {
				beforePath, afterPath := historyDefinitionPaths(change)
				if beforePath == "" && afterPath == "" {
					continue
				}
				before, bd, e := definition(commit.Hash+"^", beforePath)
				if e != nil {
					return e
				}
				after, ad, e := definition(commit.Hash, afterPath)
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
				if coverage, ok := missing[key]; ok {
					foundMissing = append(foundMissing, coverage)
					delete(missing, key)
				}
			}
			processed = append(processed, store.HistoryProgress{Commit: commit.Hash, Events: progress.Events - eventsBefore, Diagnostics: progress.Diagnostics - diagnosticsBefore})
			batchCommits++
			if scanCursor != nil {
				scanCursor.Frontier = commit.Hash
			}
			complete = true
			if batchCommits == batchLimit {
				return flush(ctx, scanKey)
			}
			return nil
		}
		walk := func(requested gitrepo.Range) error {
			lastWalkCommit = ""
			// Keep the first commit immediate and bound later lookahead to the
			// next publication batch. Resolve exactly the same before/after
			// definitions as processCommit, without changing Git's rename diff.
			var pending []gitrepo.Commit
			if prefetchOwned {
				pending = make([]gitrepo.Commit, 0, historyBatchCommits)
			}
			consume := func() error {
				if len(pending) == 0 {
					return nil
				}
				var refs []gitrepo.BlobRef
				if prefetchOwned && (prefetcher == nil || prefetcher.Enabled()) {
					for _, commit := range pending {
						if seen[commit.Hash] || checkpoint[commit.Hash] ||
							(rangeIndex == fallbackIndex && !since.IsZero() && !commit.AuthorTime.Before(since)) {
							continue
						}
						for _, change := range commit.Changes {
							beforePath, afterPath := historyDefinitionPaths(change)
							if beforePath != "" {
								refs = append(refs, gitrepo.BlobRef{Revision: commit.Hash + "^", Path: beforePath})
							}
							if afterPath != "" {
								refs = append(refs, gitrepo.BlobRef{Revision: commit.Hash, Path: afterPath})
							}
						}
					}
				}
				if len(refs) > 0 {
					if prefetcher == nil {
						var openErr error
						prefetcher, openErr = i.repository.NewBlobPrefetcher(ctx)
						if openErr != nil {
							return openErr
						}
					}
					if err := prefetcher.Prefetch(ctx, refs); err != nil {
						return err
					}
				}
				for _, commit := range pending {
					if err := processCommit(commit); err != nil {
						return err
					}
				}
				clear(pending)
				pending = pending[:0]
				return nil
			}
			e := i.repository.WalkCommits(ctx, requested, func(commit gitrepo.Commit) error {
				if !prefetchOwned || (prefetcher != nil && !prefetcher.Enabled()) {
					return processCommit(commit)
				}
				pending = append(pending, commit)
				if len(pending) >= batchLimit-batchCommits {
					return consume()
				}
				return nil
			})
			if e == nil {
				e = consume()
			}
			if e != nil {
				if batchCommits > 0 && ctx.Err() != nil {
					flushCtx, cancelFlush := context.WithTimeout(context.WithoutCancel(ctx), historyCancelFlushWindow)
					flushErr := flush(flushCtx, scanKey)
					cancelFlush()
					if flushErr != nil {
						return errors.Join(e, flushErr)
					}
				}
				return e
			}
			return nil
		}
		floor := ""
		if before, after, ok := strings.Cut(r.Revision, ".."); ok && after == head {
			floor = before
		}
		fallbackSince := time.Time{}
		if rangeIndex == fallbackIndex {
			fallbackSince = since
		}
		savedCursor, hasCursor, e := i.store.HistoryScanCursor(ctx, source.Name, scanKey)
		if e != nil {
			return Result{}, e
		}
		validCursor := hasCursor && checkpoint[savedCursor.Frontier] &&
			savedCursor.Floor == floor && savedCursor.Since.Equal(r.Since) &&
			savedCursor.Until.Equal(r.Until) && savedCursor.FallbackSince.Equal(fallbackSince)
		// A pruned saved head is not an ancestor; retain its checkpoint rows
		// for reconciliation while clearing the unusable frontier below.
		if validCursor && savedCursor.Head != head {
			validCursor, e = i.repository.IsFirstParentAncestor(ctx, savedCursor.Head, head)
			if e != nil {
				return Result{}, e
			}
		}
		if hasCursor && (!validCursor || savedCursor.Head != head) {
			// A changed prefix can be interrupted or rewritten before it is complete.
			// Invalidate the old frontier before publishing any of that prefix.
			if err := i.store.ApplyHistoryBatch(ctx, store.HistoryBatch{Repository: source.Name, ScanKey: scanKey, ClearScanCursor: true}); err != nil {
				return Result{}, err
			}
		}
		if validCursor && savedCursor.Head != head {
			prefix := r
			prefix.Revision = savedCursor.Head + ".." + head
			if err := walk(prefix); err != nil {
				return Result{}, err
			}
			if err := flush(ctx, scanKey); err != nil {
				return Result{}, err
			}
			// Git's --since can stop traversal before reaching the old head.
			// Resume from the frontier only if the filtered prefix reached it.
			reachedOldHead := r.Since.IsZero()
			if lastWalkCommit != "" {
				parent, err := i.repository.FirstParent(ctx, lastWalkCommit)
				if err != nil {
					return Result{}, err
				}
				reachedOldHead = reachedOldHead || parent == savedCursor.Head
			}
			if reachedOldHead {
				savedCursor.Head = head
				if err := i.store.ApplyHistoryBatch(ctx, store.HistoryBatch{Repository: source.Name, ScanKey: scanKey, Cursor: &savedCursor}); err != nil {
					return Result{}, err
				}
			} else {
				validCursor = false
			}
		}
		if validCursor {
			// These rows were committed together with a contiguous frontier on
			// this exact first-parent history and with these exact scan bounds.
			for _, item := range checkpointRows {
				encountered[item.Commit] = true
				seen[item.Commit] = true
			}
			scanCursor = &savedCursor
			parent, err := i.repository.FirstParent(ctx, savedCursor.Frontier)
			if err != nil {
				return Result{}, err
			}
			if parent != "" {
				tail := r
				tail.Revision = parent
				if floor != "" {
					tail.Revision = floor + ".." + parent
				}
				if err := walk(tail); err != nil {
					return Result{}, err
				}
			}
		} else {
			if len(checkpointRows) == 0 {
				scanCursor = &store.HistoryScanCursor{Head: head, Floor: floor, Since: r.Since, Until: r.Until, FallbackSince: fallbackSince}
			}
			if err := walk(r); err != nil {
				return Result{}, err
			}
		}
		if err := flush(ctx, scanKey); err != nil {
			return Result{}, err
		}
		if fallbackIndex > 0 && rangeIndex == fallbackIndex-1 {
			allCheckpointsEncountered := true
			for commit := range loadedCheckpoints {
				if !encountered[commit] {
					allCheckpointsEncountered = false
					break
				}
			}
			// Keep primary checkpoints until fallback finishes if they still
			// need reconciliation against the remaining history.
			if allCheckpointsEncountered {
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
	if err := i.store.FinalizeHistory(ctx, store.HistoryBatch{Repository: source.Name, Path: source.Path, Head: head, Since: since, RemoveCommits: remove, Exhausted: exhausted, CompletedScanKeys: rangeKeys}); err != nil {
		return Result{}, err
	}
	return Result{Events: progress.Events, Diagnostics: progress.Diagnostics, Head: head, Since: since}, nil
}

func historyDefinitionPaths(change gitrepo.Change) (before, after string) {
	if filepath.Ext(change.Path) != ".rb" && filepath.Ext(change.OldPath) != ".rb" {
		return "", ""
	}
	before, after = change.Path, change.Path
	if change.OldPath != "" {
		before = change.OldPath
	}
	if change.Status == "A" {
		before = ""
	}
	if change.Status == "D" {
		after = ""
	}
	return before, after
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
		historyScanFormatVersion,
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

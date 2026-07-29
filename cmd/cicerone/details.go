package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"cicerone/internal/changelog"
	"cicerone/internal/domain"
	"cicerone/internal/download"
	"cicerone/internal/history"
	"cicerone/internal/homebrew"
	"cicerone/internal/store"
	"cicerone/internal/tui"
)

type packageDetailLoader struct {
	store      *store.Store
	brew       *homebrew.Client
	changelogs changelogLoader
	queue      *download.Queue
	fetcher    *changelog.Fetcher
	send       func(tea.Msg)
	infoMu     sync.Mutex
	infoCalls  map[domain.PackageID]*packageInfoCall
	progress   *detailProgressTracker
}

type packageInfoCall struct {
	done chan struct{}
	info homebrew.PackageInfo
	err  error
}

type detailProgressTracker struct {
	mu                      sync.Mutex
	downloadActive, pending int
	infoActive              int
	sequence                uint64
	send                    func(tea.Msg)
}

func (t *detailProgressTracker) downloads(progress download.Progress) {
	t.mu.Lock()
	t.downloadActive, t.pending = progress.Active, progress.Pending
	t.emitLocked()
	t.mu.Unlock()
}

func (t *detailProgressTracker) info(delta int) {
	t.mu.Lock()
	t.infoActive += delta
	t.emitLocked()
	t.mu.Unlock()
}

func (t *detailProgressTracker) emitLocked() {
	if t.send == nil {
		return
	}
	t.sequence++
	message := tui.DetailProgress{Active: t.downloadActive + t.infoActive, Pending: t.pending, Sequence: t.sequence}
	go t.send(message)
}

func (l *packageDetailLoader) LoadPackageInfo(ctx context.Context, packageID domain.PackageID) (homebrew.PackageInfo, error) {
	info, ok, err := l.LoadCachedPackageInfo(ctx, packageID)
	if err != nil {
		return homebrew.PackageInfo{}, err
	}
	if ok {
		go func() {
			_, refreshErr := l.refreshPackageInfo(ctx, packageID)
			if refreshErr != nil && l.send != nil {
				l.send(tui.PackageInfoLoaded{PackageID: packageID, Err: refreshErr})
			}
		}()
		return info, nil
	}
	return l.refreshPackageInfo(ctx, packageID)
}

func (l *packageDetailLoader) LoadCachedPackageInfo(ctx context.Context, packageID domain.PackageID) (homebrew.PackageInfo, bool, error) {
	cached, ok, err := l.store.PackageInfo(ctx, string(packageID))
	if err != nil || !ok {
		return homebrew.PackageInfo{}, ok, err
	}
	var info homebrew.PackageInfo
	if err := json.Unmarshal(cached.Normalized, &info); err != nil {
		return homebrew.PackageInfo{}, false, err
	}
	return info, true, nil
}

func (l *packageDetailLoader) refreshPackageInfo(ctx context.Context, packageID domain.PackageID) (homebrew.PackageInfo, error) {
	l.infoMu.Lock()
	if l.infoCalls == nil {
		l.infoCalls = make(map[domain.PackageID]*packageInfoCall)
	}
	if active := l.infoCalls[packageID]; active != nil {
		l.infoMu.Unlock()
		select {
		case <-active.done:
			return active.info, active.err
		case <-ctx.Done():
			return homebrew.PackageInfo{}, ctx.Err()
		}
	}
	call := &packageInfoCall{done: make(chan struct{})}
	l.infoCalls[packageID] = call
	l.infoMu.Unlock()
	if l.progress != nil {
		l.progress.info(1)
	}
	defer func() {
		l.infoMu.Lock()
		delete(l.infoCalls, packageID)
		close(call.done)
		l.infoMu.Unlock()
		if l.progress != nil {
			l.progress.info(-1)
		}
	}()

	info, raw, err := l.brew.Info(ctx, string(packageID))
	if err != nil {
		call.err = err
		return homebrew.PackageInfo{}, err
	}
	normalized, err := json.Marshal(info)
	if err != nil {
		call.err = err
		return homebrew.PackageInfo{}, err
	}
	if err := l.store.SavePackageInfo(ctx, store.PackageInfoRecord{
		PackageID: string(packageID), FetchedAt: time.Now().UTC(), Raw: raw, Normalized: normalized,
	}); err != nil {
		call.err = err
		return homebrew.PackageInfo{}, err
	}
	call.info = info
	if l.send != nil {
		l.send(tui.PackageInfoLoaded{PackageID: packageID, Info: info})
	}
	return info, nil
}

func (l *packageDetailLoader) LoadREADME(ctx context.Context, packageID domain.PackageID, eventID domain.EventID) (store.PackageDocument, error) {
	cachedDocument, found, err := l.LoadCachedREADME(ctx, packageID)
	if err != nil {
		return store.PackageDocument{}, err
	}
	if found {
		go func() {
			_, refreshErr := l.refreshREADMEWithCached(ctx, packageID, eventID, cachedDocument)
			if refreshErr != nil && l.send != nil {
				l.send(tui.READMELoaded{PackageID: packageID, Err: refreshErr})
			}
		}()
		return cachedDocument, nil
	}
	return l.refreshREADMEWithCached(ctx, packageID, eventID, store.PackageDocument{})
}

func (l *packageDetailLoader) LoadCachedREADME(ctx context.Context, packageID domain.PackageID) (store.PackageDocument, bool, error) {
	cached, err := l.store.PackageDocuments(ctx, string(packageID), store.DocumentREADME)
	if err != nil || len(cached) == 0 {
		return store.PackageDocument{}, false, err
	}
	return cached[0], true, nil
}

func (l *packageDetailLoader) LoadCachedRepositoryTags(ctx context.Context, packageID domain.PackageID) (store.PackageRepositoryTags, bool, error) {
	return l.store.PackageRepositoryTags(ctx, string(packageID))
}

func (l *packageDetailLoader) refreshREADMEWithCached(ctx context.Context, packageID domain.PackageID, eventID domain.EventID, cached store.PackageDocument) (store.PackageDocument, error) {
	target, err := l.changelogs.cache.ChangelogTarget(ctx, packageID, eventID)
	if err != nil {
		return store.PackageDocument{}, err
	}
	repository, err := l.changelogs.repository(ctx, target.Repository)
	if err != nil {
		return store.PackageDocument{}, err
	}
	body, err := repository.Blob(ctx, target.Commit, target.DefinitionPath)
	if err != nil {
		return store.PackageDocument{}, err
	}
	definition, _ := history.ParseDefinition(target.DefinitionPath, body)
	if definition == nil {
		return store.PackageDocument{}, fmt.Errorf("parse README metadata for %s", target.Name)
	}
	repositoryURL := githubRepositoryURL(definition.Homepage, definition.URL)
	var repositoryErr error
	if l.changelogs.locator != nil {
		resolved, resolveErr := l.changelogs.locator.Resolve(ctx, string(packageID), target.Name, definition.Homepage, definition.URL)
		if resolveErr == nil {
			repositoryURL = resolved
		} else {
			repositoryErr = resolveErr
		}
	}
	defer l.enqueueRepositoryTags(ctx, packageID, repositoryURL)
	candidates := readmeCandidateURLs(repositoryURL)
	if len(candidates) == 0 {
		if repositoryErr != nil {
			return store.PackageDocument{}, fmt.Errorf("discover upstream repository: %w", repositoryErr)
		}
		return store.PackageDocument{}, errors.New("upstream repository is unavailable")
	}
	var fetched changelog.Fetched
	var fetchErr error
	for _, rawURL := range candidates {
		validators := changelog.Validators{}
		if cached.URL == rawURL {
			validators = changelog.Validators{ETag: cached.ETag, LastModified: cached.LastModified}
		}
		result, err := l.queue.Enqueue(download.Request{
			URL: rawURL, Profile: "document", Priority: download.Current, Context: ctx,
			Fetch: func(fetchCtx context.Context) (any, error) {
				return l.fetcher.FetchConditional(fetchCtx, rawURL, validators)
			},
		})
		if err != nil {
			fetchErr = err
			continue
		}
		downloaded := <-result
		if downloaded.Err != nil {
			fetchErr = downloaded.Err
			continue
		}
		var ok bool
		fetched, ok = downloaded.Value.(changelog.Fetched)
		if !ok {
			fetchErr = errors.New("unexpected README download result")
			continue
		}
		fetchErr = nil
		break
	}
	if fetchErr != nil {
		return store.PackageDocument{}, fetchErr
	}
	if fetched.NotModified && cached.ID != "" {
		if err := l.store.TouchPackageDocument(ctx, cached.ID, fetched.FetchedAt, fetched.ETag, fetched.LastModified); err != nil {
			return store.PackageDocument{}, err
		}
		cached.FetchedAt = fetched.FetchedAt
		if l.send != nil {
			l.send(tui.READMELoaded{PackageID: packageID, Document: cached})
		}
		return cached, nil
	}
	sum := sha256.Sum256(fetched.Body)
	document, err := l.store.SavePackageDocument(ctx, string(packageID), store.PackageDocument{
		Kind: store.DocumentREADME, URL: fetched.FinalURL.String(), SourceURL: fetched.FinalURL.String(),
		MediaType: fetched.MediaType, ETag: fetched.ETag, LastModified: fetched.LastModified,
		Hash: hex.EncodeToString(sum[:]), FetchedAt: fetched.FetchedAt, Raw: fetched.Body,
		Extracted: fetched.Body, ExtractionStatus: "ok",
	})
	if err == nil && l.send != nil {
		l.send(tui.READMELoaded{PackageID: packageID, Document: document})
	}
	return document, err
}

func (l *packageDetailLoader) enqueueRepositoryTags(ctx context.Context, packageID domain.PackageID, repositoryURL string) {
	if l.queue == nil || l.changelogs.resolver == nil || repositoryURL == "" {
		return
	}
	result, err := l.queue.Enqueue(download.Request{
		URL: repositoryURL, Profile: "repository-tags", Priority: download.Speculative, Context: ctx,
		Fetch: func(fetchCtx context.Context) (any, error) {
			return l.changelogs.resolver.RepositoryMetadataTags(fetchCtx, repositoryURL)
		},
	})
	if err != nil {
		if l.send != nil {
			l.send(tui.RepositoryTagsLoaded{PackageID: packageID, Err: err})
		}
		return
	}
	go func() {
		completed := <-result
		if completed.Err != nil {
			if l.send != nil {
				l.send(tui.RepositoryTagsLoaded{PackageID: packageID, Err: completed.Err})
			}
			return
		}
		tags, ok := completed.Value.([]string)
		if !ok {
			if l.send != nil {
				l.send(tui.RepositoryTagsLoaded{PackageID: packageID, Err: errors.New("unexpected repository tags result")})
			}
			return
		}
		record := store.PackageRepositoryTags{
			PackageID: string(packageID), Tags: tags, FetchedAt: time.Now().UTC(),
		}
		if err := l.store.SavePackageRepositoryTags(ctx, record); err != nil {
			if l.send != nil {
				l.send(tui.RepositoryTagsLoaded{PackageID: packageID, Err: err})
			}
			return
		}
		if l.send != nil {
			l.send(tui.RepositoryTagsLoaded{PackageID: packageID, Record: record})
		}
	}()
}

func githubOwnerRepo(raw string) (string, string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}

func readmeCandidateURLs(repositoryURL string) []string {
	parsed, err := url.Parse(repositoryURL)
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return nil
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	var base string
	switch strings.ToLower(parsed.Hostname()) {
	case "github.com":
		base = "https://raw.githubusercontent.com/" + owner + "/" + repo + "/HEAD/"
	case "codeberg.org":
		base = "https://codeberg.org/" + owner + "/" + repo + "/raw/branch/HEAD/"
	default:
		return nil
	}
	names := []string{"README.md", "README.markdown", "README.rst", "README.txt", "README"}
	result := make([]string, len(names))
	for index, name := range names {
		result[index] = base + name
	}
	return result
}

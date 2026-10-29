package history

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/gitrepo"
	"github.com/anateus/cicerone/internal/store"
	"github.com/anateus/cicerone/internal/testutil"
)

const (
	progressiveHistoryWait         = 10 * time.Second
	progressiveHistoryStillBlocked = 50 * time.Millisecond
	maxHistoryBlobFetchObjects     = 64
)

func TestIndexerPublishesProgressiveHistoryBeforeCompletion(t *testing.T) {
	const commitCount = 22
	repo, commits, versions, firstAt := makeProgressiveFormulaHistory(t, commitCount)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	destination, err := store.Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destination.Close() })

	source := gitrepo.Source{Name: "core", Kind: "formula", Path: repo.Path}
	request := Request{Since: firstAt.Add(-time.Minute)}
	scanKey := historyScanKey("initial", time.Time{}, time.Time{}, request, false)
	publications := make(chan progressivePublication, 2)
	done := make(chan indexOutcome, 1)
	release := make(chan struct{})
	indexStarted := false
	indexFinished := false
	defer func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
		if indexStarted && !indexFinished {
			select {
			case <-done:
			case <-time.After(progressiveHistoryWait):
			}
		}
	}()

	request.Progress = func(progress Progress) {
		if progress.Batches != 1 && progress.Batches != 2 {
			return
		}
		publications <- readProgressivePublication(destination, source.Name, scanKey, progress)
		if progress.Batches == 2 {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	go func() {
		result, indexErr := NewIndexer(gitrepo.New(source, execx.NewRunner()), destination).Index(ctx, source, request)
		done <- indexOutcome{result: result, err: indexErr}
	}()
	indexStarted = true

	newestToOldest := make([]expectedHistoryEvent, len(commits))
	for offset := range commits {
		index := len(commits) - offset - 1
		oldVersion := ""
		if index > 0 {
			oldVersion = versions[index-1]
		}
		newestToOldest[offset] = expectedHistoryEvent{
			commit: commits[index], packageID: "foo", path: "Formula/foo.rb",
			oldVersion: oldVersion, newVersion: versions[index],
		}
	}

	first := awaitProgressivePublication(t, publications)
	second := awaitProgressivePublication(t, publications)
	assertProgressivePublication(t, first, Progress{Commits: 1, Events: 1, Batches: 1}, newestToOldest[:1], scanKey, commits[len(commits)-1], commits[len(commits)-1], request.Since)
	secondFrontier := commits[commitCount-11]
	assertProgressivePublication(t, second, Progress{Commits: 11, Events: 11, Batches: 2}, newestToOldest[:11], scanKey, commits[len(commits)-1], secondFrontier, request.Since)

	select {
	case outcome := <-done:
		indexFinished = true
		t.Fatalf("index completed while the second durable publication was held: result=%#v err=%v", outcome.result, outcome.err)
	case <-time.After(progressiveHistoryStillBlocked):
	}
	if state, ok, err := destination.HistoryState(ctx, source.Name); err != nil || ok {
		t.Fatalf("history state became final before release: state=%#v ok=%v err=%v", state, ok, err)
	}

	close(release)
	outcome := awaitIndexOutcome(t, done)
	indexFinished = true
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if outcome.result.Head != commits[len(commits)-1] || outcome.result.Events != commitCount {
		t.Fatalf("completed index result=%#v, want head %s and %d events", outcome.result, commits[len(commits)-1], commitCount)
	}
	groups, err := destination.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryEventSequence(t, flattenHistoryEvents(groups), newestToOldest)
	state, ok, err := destination.HistoryState(ctx, source.Name)
	if err != nil || !ok || state.Head != commits[len(commits)-1] {
		t.Fatalf("final history state=%#v ok=%v err=%v", state, ok, err)
	}
}

func TestIndexerPrefetchesPartialCloneAcrossRenameAndFirstParentMerge(t *testing.T) {
	isolateGitConfig(t)
	requireNoLazyFetchSupport(t)
	t.Setenv("GIT_NO_LAZY_FETCH", "")
	fixture := makeRenameMergeHistory(t, 65)
	fixture.repo.Run("-C", fixture.repo.Path, "config", "uploadpack.allowFilter", "true")
	fixture.repo.Run("-C", fixture.repo.Path, "config", "uploadpack.allowAnySHA1InWant", "true")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	remoteURL := (&url.URL{Scheme: "file", Path: fixture.repo.Path}).String()
	mirrorPath := filepath.Join(t.TempDir(), "mirror.git")
	cloneOutput, err := runGitCommand(ctx, "clone", "--mirror", "--filter=blob:none", "--", remoteURL, mirrorPath)
	if err != nil {
		t.Fatalf("clone file:// filtered mirror: %v\n%s", err, cloneOutput)
	}
	assertGitConfig(t, ctx, mirrorPath, "remote.origin.url", remoteURL)
	assertGitConfig(t, ctx, mirrorPath, "remote.origin.promisor", "true")
	assertGitConfig(t, ctx, mirrorPath, "remote.origin.partialclonefilter", "blob:none")

	wantEvents := []expectedHistoryEvent{
		{commit: fixture.mainline, packageID: "foo", path: "Formula/f/foo.rb", oldVersion: "1.2", newVersion: "1.3"},
		{commit: fixture.update, packageID: "foo", path: "Formula/f/foo.rb", oldVersion: "1.1", newVersion: "1.2"},
		{commit: fixture.rename, packageID: "foo", path: "Formula/f/foo.rb", oldVersion: "1.0", newVersion: "1.1"},
		{commit: fixture.initial, packageID: "foo", path: "Formula/foo.rb", newVersion: "1.0"},
	}
	wantObjects := []string{
		sourceBlobID(t, fixture.repo, fixture.initial, "Formula/foo.rb"),
		sourceBlobID(t, fixture.repo, fixture.rename, "Formula/f/foo.rb"),
		sourceBlobID(t, fixture.repo, fixture.update, "Formula/f/foo.rb"),
		sourceBlobID(t, fixture.repo, fixture.mainline, "Formula/f/foo.rb"),
	}
	mergeObjects := make([]string, 0, len(fixture.extraPaths))
	for index := range fixture.extraPaths {
		path := fixture.extraPaths[index]
		wantEvents = append(wantEvents, expectedHistoryEvent{
			commit: fixture.merge, packageID: domain.PackageID(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))),
			path: path, newVersion: fmt.Sprintf("3.%d", index),
		})
		object := sourceBlobID(t, fixture.repo, fixture.merge, path)
		wantObjects = append(wantObjects, object)
		mergeObjects = append(mergeObjects, object)
	}
	if sideBlob := sourceBlobID(t, fixture.repo, fixture.side, fixture.extraPaths[0]); sideBlob != wantObjects[4] {
		t.Fatalf("merge did not preserve the side-branch blob: %s != %s", sideBlob, wantObjects[4])
	}
	assertUniqueObjects(t, wantObjects)
	assertBlobStates(t, ctx, mirrorPath, wantObjects, "missing")

	destination, err := store.Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destination.Close() })
	source := gitrepo.Source{Kind: "formula", Name: "core", Path: mirrorPath, RemoteURL: remoteURL, Owned: true}
	runner := &fetchRecordingRunner{Runner: execx.NewRunner()}
	request := Request{Since: fixture.firstAt.Add(-time.Minute)}
	result, err := NewIndexer(gitrepo.New(source, runner), destination).Index(ctx, source, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Head != fixture.merge || result.Events != len(wantEvents) {
		t.Fatalf("partial-clone index result=%#v, want merge head %s and %d events", result, fixture.merge, len(wantEvents))
	}
	groups, err := destination.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryEventSet(t, flattenHistoryEvents(groups), wantEvents)
	assertRecordedBlobFetches(t, runner.fetches, wantObjects, mergeObjects)
	assertBlobStates(t, ctx, mirrorPath, wantObjects, "blob")
}

func TestIndexerResumesPartialCloneAfterRemoteReturns(t *testing.T) {
	isolateGitConfig(t)
	requireNoLazyFetchSupport(t)
	t.Setenv("GIT_NO_LAZY_FETCH", "")

	repo, commits, versions, firstAt := makeProgressiveFormulaHistory(t, 12)
	repo.Run("-C", repo.Path, "config", "uploadpack.allowFilter", "true")
	repo.Run("-C", repo.Path, "config", "uploadpack.allowAnySHA1InWant", "true")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	remoteURL := (&url.URL{Scheme: "file", Path: repo.Path}).String()
	mirrorPath := filepath.Join(t.TempDir(), "mirror.git")
	cloneOutput, err := runGitCommand(ctx, "clone", "--mirror", "--filter=blob:none", "--", remoteURL, mirrorPath)
	if err != nil {
		t.Fatalf("clone file:// filtered mirror: %v\n%s", err, cloneOutput)
	}
	assertGitConfig(t, ctx, mirrorPath, "remote.origin.url", remoteURL)
	assertGitConfig(t, ctx, mirrorPath, "remote.origin.promisor", "true")
	assertGitConfig(t, ctx, mirrorPath, "remote.origin.partialclonefilter", "blob:none")

	wantEvents := make([]expectedHistoryEvent, len(commits))
	blobObjects := make([]string, len(commits))
	for offset := range commits {
		index := len(commits) - offset - 1
		oldVersion := ""
		if index > 0 {
			oldVersion = versions[index-1]
		}
		wantEvents[offset] = expectedHistoryEvent{
			commit: commits[index], packageID: "foo", path: "Formula/foo.rb",
			oldVersion: oldVersion, newVersion: versions[index],
		}
		blobObjects[index] = sourceBlobID(t, repo, commits[index], "Formula/foo.rb")
	}
	assertUniqueObjects(t, blobObjects)
	assertBlobStates(t, ctx, mirrorPath, blobObjects, "missing")

	source := gitrepo.Source{Kind: "formula", Name: "core", Path: mirrorPath, RemoteURL: remoteURL, Owned: true}
	destination, err := store.Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destination.Close() })
	runner := &fetchRecordingRunner{Runner: execx.NewRunner()}
	indexer := NewIndexer(gitrepo.New(source, runner), destination)
	request := Request{Since: firstAt.Add(-time.Minute)}
	scanKey := historyScanKey("initial", time.Time{}, time.Time{}, request, false)
	offlinePath := repo.Path + ".offline"
	sourceRestored := false
	t.Cleanup(func() {
		if sourceRestored {
			return
		}
		if err := os.Rename(offlinePath, repo.Path); err != nil && !os.IsNotExist(err) {
			t.Errorf("restore file:// source during cleanup: %v", err)
		}
	})
	type offlineSourceSignal struct {
		progress Progress
		err      error
	}
	offline := make(chan offlineSourceSignal, 1)
	offlineArmed := true
	request.Progress = func(progress Progress) {
		if progress.Batches != 1 || !offlineArmed {
			return
		}
		offlineArmed = false
		offline <- offlineSourceSignal{progress: progress, err: os.Rename(repo.Path, offlinePath)}
	}

	done := make(chan indexOutcome, 1)
	indexStarted := false
	indexFinished := false
	defer func() {
		cancel()
		if indexStarted && !indexFinished {
			select {
			case <-done:
			case <-time.After(progressiveHistoryWait):
			}
		}
	}()
	go func() {
		result, indexErr := indexer.Index(ctx, source, request)
		done <- indexOutcome{result: result, err: indexErr}
	}()
	indexStarted = true

	outcome := awaitIndexOutcome(t, done)
	indexFinished = true
	var offlineResult offlineSourceSignal
	select {
	case offlineResult = <-offline:
	case <-time.After(progressiveHistoryWait):
		t.Fatal("timed out waiting for the first-batch source move")
	}
	if offlineResult.err != nil {
		t.Fatalf("move file:// source offline after first publication: %v", offlineResult.err)
	}
	if offlineResult.progress != (Progress{Commits: 1, Events: 1, Batches: 1}) {
		t.Fatalf("source moved after progress=%#v, want first one-commit publication", offlineResult.progress)
	}
	if outcome.err == nil {
		t.Fatal("Index succeeded although its next batch's partial-clone blob fetch should fail offline")
	}
	if len(runner.fetches) == 0 || len(runner.fetchErrors) != len(runner.fetches) || runner.fetchErrors[len(runner.fetchErrors)-1] == nil {
		t.Fatalf("no failed bulk Git fetch was recorded: calls=%q errors=%v Index err=%v", runner.fetches, runner.fetchErrors, outcome.err)
	}
	failedFetch := runner.fetches[len(runner.fetches)-1]
	failedObjects := fetchObjectIDs(failedFetch)
	if len(failedObjects) < 2 || len(failedObjects) > maxHistoryBlobFetchObjects {
		t.Fatalf("offline blob fetch has %d object IDs, want a bounded multi-OID request: %q", len(failedObjects), failedFetch)
	}
	if !containsArgument(failedFetch, "origin") || !containsArgument(failedFetch, "--filter=blob:none") {
		t.Fatalf("offline request was not a filtered fetch from origin: %q", failedFetch)
	}
	knownObjects := make(map[string]bool, len(blobObjects))
	for _, object := range blobObjects {
		knownObjects[object] = true
	}
	seenFailedObjects := make(map[string]bool, len(failedObjects))
	for _, object := range failedObjects {
		if !knownObjects[object] || seenFailedObjects[object] {
			t.Fatalf("offline fetch requested unexpected or duplicate object %s: %q", object, failedFetch)
		}
		seenFailedObjects[object] = true
	}

	latest := commits[len(commits)-1]
	if state, ok, err := destination.HistoryState(ctx, source.Name); err != nil || ok {
		t.Fatalf("partial history became final after failed fetch: state=%#v ok=%v err=%v", state, ok, err)
	}
	groups, err := destination.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryEventSequence(t, flattenHistoryEvents(groups), wantEvents[:1])
	progressRows, err := destination.HistoryScanProgress(ctx, source.Name, scanKey)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryProgressRows(t, progressRows, []string{latest})
	cursor, hasCursor, err := destination.HistoryScanCursor(ctx, source.Name, scanKey)
	if err != nil || !hasCursor || cursor.Head != latest || cursor.Frontier != latest ||
		cursor.Floor != "" || !cursor.Since.Equal(request.Since) || !cursor.Until.IsZero() || !cursor.FallbackSince.IsZero() {
		t.Fatalf("partial scan cursor=%#v exists=%v err=%v, want head/frontier %s", cursor, hasCursor, err, latest)
	}

	if err := os.Rename(offlinePath, repo.Path); err != nil {
		t.Fatalf("restore file:// source: %v", err)
	}
	sourceRestored = true
	request.Progress = nil
	result, err := indexer.Index(ctx, source, request)
	if err != nil {
		t.Fatalf("retry after restoring file:// source: %v", err)
	}
	if result.Head != latest || result.Events != len(wantEvents) {
		t.Fatalf("retried index result=%#v, want head %s and %d events", result, latest, len(wantEvents))
	}
	groups, err = destination.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryEventSequence(t, flattenHistoryEvents(groups), wantEvents)
	assertBlobStates(t, ctx, mirrorPath, blobObjects, "blob")
	state, ok, err := destination.HistoryState(ctx, source.Name)
	if err != nil || !ok || state.Head != latest || !state.Since.Equal(request.Since) {
		t.Fatalf("completed retry history state=%#v ok=%v err=%v", state, ok, err)
	}
}

func TestIndexerCancellationDuringSecondPartialCloneFetchResumes(t *testing.T) {
	isolateGitConfig(t)
	requireNoLazyFetchSupport(t)
	t.Setenv("GIT_NO_LAZY_FETCH", "")

	repo, commits, versions, firstAt := makeProgressiveFormulaHistory(t, 12)
	repo.Run("-C", repo.Path, "config", "uploadpack.allowFilter", "true")
	repo.Run("-C", repo.Path, "config", "uploadpack.allowAnySHA1InWant", "true")
	remoteURL := (&url.URL{Scheme: "file", Path: repo.Path}).String()
	mirrorPath := filepath.Join(t.TempDir(), "mirror.git")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cloneOutput, err := runGitCommand(ctx, "clone", "--mirror", "--filter=blob:none", "--", remoteURL, mirrorPath)
	if err != nil {
		t.Fatalf("clone file:// filtered mirror: %v\n%s", err, cloneOutput)
	}
	assertGitConfig(t, ctx, mirrorPath, "remote.origin.url", remoteURL)
	assertGitConfig(t, ctx, mirrorPath, "remote.origin.promisor", "true")
	assertGitConfig(t, ctx, mirrorPath, "remote.origin.partialclonefilter", "blob:none")

	wantEvents := make([]expectedHistoryEvent, len(commits))
	blobObjects := make([]string, len(commits))
	for offset := range commits {
		index := len(commits) - offset - 1
		oldVersion := ""
		if index > 0 {
			oldVersion = versions[index-1]
		}
		wantEvents[offset] = expectedHistoryEvent{
			commit: commits[index], packageID: "foo", path: "Formula/foo.rb",
			oldVersion: oldVersion, newVersion: versions[index],
		}
		blobObjects[index] = sourceBlobID(t, repo, commits[index], "Formula/foo.rb")
	}
	assertUniqueObjects(t, blobObjects)
	assertBlobStates(t, ctx, mirrorPath, blobObjects, "missing")

	source := gitrepo.Source{Kind: "formula", Name: "core", Path: mirrorPath, RemoteURL: remoteURL, Owned: true}
	destination, err := store.Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destination.Close() })
	runner := &secondFetchGateRunner{Runner: execx.NewRunner(), secondStarted: make(chan struct{})}
	indexer := NewIndexer(gitrepo.New(source, runner), destination)
	request := Request{Since: firstAt.Add(-time.Minute)}
	scanKey := historyScanKey("initial", time.Time{}, time.Time{}, request, false)
	firstProgress := make(chan Progress, 1)
	request.Progress = func(progress Progress) {
		if progress.Batches == 1 {
			firstProgress <- progress
		}
	}

	done := make(chan indexOutcome, 1)
	indexStarted := false
	indexFinished := false
	defer func() {
		cancel()
		if indexStarted && !indexFinished {
			select {
			case <-done:
			case <-time.After(progressiveHistoryWait):
			}
		}
	}()
	go func() {
		result, indexErr := indexer.Index(ctx, source, request)
		done <- indexOutcome{result: result, err: indexErr}
	}()
	indexStarted = true

	var progress Progress
	select {
	case progress = <-firstProgress:
	case <-time.After(progressiveHistoryWait):
		t.Fatal("timed out waiting for the first durable publication")
	}
	if progress != (Progress{Commits: 1, Events: 1, Batches: 1}) {
		t.Fatalf("first progress=%#v, want one durable newest commit", progress)
	}
	select {
	case <-runner.secondStarted:
	case <-time.After(progressiveHistoryWait):
		t.Fatal("timed out waiting for the second bounded blob fetch")
	}
	select {
	case outcome := <-done:
		indexFinished = true
		t.Fatalf("index completed while the second fetch was gated: result=%#v err=%v", outcome.result, outcome.err)
	case <-time.After(progressiveHistoryStillBlocked):
	}

	latest := commits[len(commits)-1]
	groups, err := destination.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryEventSequence(t, flattenHistoryEvents(groups), wantEvents[:1])
	progressRows, err := destination.HistoryScanProgress(ctx, source.Name, scanKey)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryProgressRows(t, progressRows, []string{latest})
	cursor, hasCursor, err := destination.HistoryScanCursor(ctx, source.Name, scanKey)
	if err != nil || !hasCursor || cursor.Head != latest || cursor.Frontier != latest ||
		cursor.Floor != "" || !cursor.Since.Equal(request.Since) || !cursor.Until.IsZero() || !cursor.FallbackSince.IsZero() {
		t.Fatalf("gated scan cursor=%#v exists=%v err=%v, want head/frontier %s", cursor, hasCursor, err, latest)
	}
	if state, ok, err := destination.HistoryState(ctx, source.Name); err != nil || ok {
		t.Fatalf("gated partial history became final: state=%#v ok=%v err=%v", state, ok, err)
	}
	if len(runner.fetches) != 2 {
		t.Fatalf("gated fetch calls=%d, want initial and second-batch fetches: %q", len(runner.fetches), runner.fetches)
	}
	failedBatchObjects := fetchObjectIDs(runner.fetches[1])
	if len(failedBatchObjects) < 2 || len(failedBatchObjects) > maxHistoryBlobFetchObjects {
		t.Fatalf("second fetch has %d object IDs, want bounded multi-OID fetch: %q", len(failedBatchObjects), runner.fetches[1])
	}
	knownObjects := make(map[string]bool, len(blobObjects))
	for _, object := range blobObjects {
		knownObjects[object] = true
	}
	seenObjects := make(map[string]bool, len(failedBatchObjects))
	for _, object := range failedBatchObjects {
		if !knownObjects[object] || seenObjects[object] {
			t.Fatalf("second fetch requested unexpected or duplicate object %s: %q", object, runner.fetches[1])
		}
		seenObjects[object] = true
	}

	cancel()
	outcome := awaitIndexOutcome(t, done)
	indexFinished = true
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("Index error=%v, want context cancellation", outcome.err)
	}

	verificationCtx, verifyCancel := context.WithTimeout(context.Background(), progressiveHistoryWait)
	defer verifyCancel()
	groups, err = destination.QueryFeed(verificationCtx, domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryEventSequence(t, flattenHistoryEvents(groups), wantEvents[:1])
	progressRows, err = destination.HistoryScanProgress(verificationCtx, source.Name, scanKey)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryProgressRows(t, progressRows, []string{latest})
	cursor, hasCursor, err = destination.HistoryScanCursor(verificationCtx, source.Name, scanKey)
	if err != nil || !hasCursor || cursor.Head != latest || cursor.Frontier != latest {
		t.Fatalf("cursor after cancellation=%#v exists=%v err=%v, want first-batch frontier %s", cursor, hasCursor, err, latest)
	}
	if state, ok, err := destination.HistoryState(verificationCtx, source.Name); err != nil || ok {
		t.Fatalf("canceled partial history became final: state=%#v ok=%v err=%v", state, ok, err)
	}

	request.Progress = nil
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer retryCancel()
	result, err := NewIndexer(gitrepo.New(source, execx.NewRunner()), destination).Index(retryCtx, source, request)
	if err != nil {
		t.Fatalf("retry with an ungated runner: %v", err)
	}
	if result.Head != latest || result.Events != len(wantEvents) {
		t.Fatalf("retried index result=%#v, want head %s and %d events", result, latest, len(wantEvents))
	}
	groups, err = destination.QueryFeed(retryCtx, domain.FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryEventSequence(t, flattenHistoryEvents(groups), wantEvents)
	assertBlobStates(t, retryCtx, mirrorPath, blobObjects, "blob")
	state, ok, err := destination.HistoryState(retryCtx, source.Name)
	if err != nil || !ok || state.Head != latest || !state.Since.Equal(request.Since) {
		t.Fatalf("completed retry history state=%#v ok=%v err=%v", state, ok, err)
	}
}

func makeProgressiveFormulaHistory(t *testing.T, count int) (*testutil.GitRepo, []string, []string, time.Time) {
	t.Helper()
	repo := testutil.NewGitRepo(t)
	firstAt := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	commits := make([]string, 0, count)
	versions := make([]string, 0, count)
	for index := range count {
		version := fmt.Sprintf("%d.0", index+1)
		versions = append(versions, version)
		commits = append(commits, repo.Commit("Formula/foo.rb", formula(version), fmt.Sprintf("version %s", version), firstAt.Add(time.Duration(index)*time.Minute)))
	}
	return repo, commits, versions, firstAt
}

type renameMergeHistory struct {
	repo       *testutil.GitRepo
	firstAt    time.Time
	initial    string
	side       string
	rename     string
	update     string
	mainline   string
	merge      string
	extraPaths []string
}

func makeRenameMergeHistory(t *testing.T, extraFormulaCount int) renameMergeHistory {
	t.Helper()
	repo := testutil.NewGitRepo(t)
	firstAt := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	initial := repo.Commit("Formula/foo.rb", formula("1.0"), "initial foo", firstAt)
	repo.Run("-C", repo.Path, "branch", "side")
	repo.Run("-C", repo.Path, "checkout", "side")
	extraPaths := make([]string, 0, extraFormulaCount)
	for index := range extraFormulaCount {
		path := fmt.Sprintf("Formula/extra-%02d.rb", index)
		if err := os.WriteFile(filepath.Join(repo.Path, path), []byte(formula(fmt.Sprintf("3.%d", index))), 0o644); err != nil {
			t.Fatal(err)
		}
		extraPaths = append(extraPaths, path)
	}
	repo.Run("-C", repo.Path, "add", "-A")
	runGitAt(t, repo, firstAt.Add(time.Minute), "commit", "-m", "add side formulas")
	side := gitHead(t, repo)
	repo.Run("-C", repo.Path, "checkout", "main")
	if err := os.MkdirAll(filepath.Join(repo.Path, "Formula", "f"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.Run("-C", repo.Path, "mv", "Formula/foo.rb", "Formula/f/foo.rb")
	if err := os.WriteFile(filepath.Join(repo.Path, "Formula/f/foo.rb"), []byte(formula("1.1")), 0o644); err != nil {
		t.Fatal(err)
	}
	repo.Run("-C", repo.Path, "add", "-A")
	runGitAt(t, repo, firstAt.Add(2*time.Minute), "commit", "-m", "rename and update foo")
	rename := gitHead(t, repo)
	update := repo.Commit("Formula/f/foo.rb", formula("1.2"), "update foo 1.2", firstAt.Add(3*time.Minute))
	mainline := repo.Commit("Formula/f/foo.rb", formula("1.3"), "update foo 1.3", firstAt.Add(4*time.Minute))
	runGitAt(t, repo, firstAt.Add(5*time.Minute), "merge", "--no-ff", "--no-edit", "side")
	merge := gitHead(t, repo)
	return renameMergeHistory{
		repo: repo, firstAt: firstAt, initial: initial, side: side,
		rename: rename, update: update, mainline: mainline, merge: merge,
		extraPaths: extraPaths,
	}
}

func runGitAt(t *testing.T, repo *testutil.GitRepo, at time.Time, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", repo.Path}, args...)
	command := exec.Command("git", commandArgs...)
	date := at.Format(time.RFC3339)
	command.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", commandArgs, err, output)
	}
}

func gitHead(t *testing.T, repo *testutil.GitRepo) string {
	t.Helper()
	head := strings.TrimSpace(repo.Run("-C", repo.Path, "rev-parse", "HEAD"))
	if !isFullGitObjectID(head) {
		t.Fatalf("git HEAD=%q is not a full object ID", head)
	}
	return head
}

type expectedHistoryEvent struct {
	commit, path           string
	packageID              domain.PackageID
	oldVersion, newVersion string
}

type progressivePublication struct {
	progress  Progress
	events    []domain.UpdateEvent
	rows      []store.HistoryProgress
	cursor    store.HistoryScanCursor
	hasCursor bool
	err       error
}

type indexOutcome struct {
	result Result
	err    error
}

func readProgressivePublication(destination *store.Store, repository, scanKey string, progress Progress) progressivePublication {
	ctx, cancel := context.WithTimeout(context.Background(), progressiveHistoryWait)
	defer cancel()
	publication := progressivePublication{progress: progress}
	groups, err := destination.QueryFeed(ctx, domain.FeedFilter{})
	if err != nil {
		publication.err = err
		return publication
	}
	publication.events = flattenHistoryEvents(groups)
	publication.rows, err = destination.HistoryScanProgress(ctx, repository, scanKey)
	if err != nil {
		publication.err = err
		return publication
	}
	publication.cursor, publication.hasCursor, publication.err = destination.HistoryScanCursor(ctx, repository, scanKey)
	return publication
}

func awaitProgressivePublication(t *testing.T, publications <-chan progressivePublication) progressivePublication {
	t.Helper()
	select {
	case publication := <-publications:
		return publication
	case <-time.After(progressiveHistoryWait):
		t.Fatal("timed out waiting for a durable history publication")
		return progressivePublication{}
	}
}

func awaitIndexOutcome(t *testing.T, outcomes <-chan indexOutcome) indexOutcome {
	t.Helper()
	select {
	case outcome := <-outcomes:
		return outcome
	case <-time.After(progressiveHistoryWait):
		t.Fatal("timed out waiting for history indexing to complete")
		return indexOutcome{}
	}
}

func assertProgressivePublication(t *testing.T, publication progressivePublication, wantProgress Progress, wantEvents []expectedHistoryEvent, scanKey, head, frontier string, since time.Time) {
	t.Helper()
	if publication.err != nil {
		t.Fatalf("read durable publication: %v", publication.err)
	}
	if publication.progress != wantProgress {
		t.Fatalf("progress=%#v, want %#v", publication.progress, wantProgress)
	}
	assertHistoryEventSequence(t, publication.events, wantEvents)
	wantCommits := make([]string, len(wantEvents))
	for index, event := range wantEvents {
		wantCommits[index] = event.commit
	}
	assertHistoryProgressRows(t, publication.rows, wantCommits)
	if !publication.hasCursor {
		t.Fatal("durable publication has no scan cursor")
	}
	cursor := publication.cursor
	if cursor.Head != head || cursor.Frontier != frontier || cursor.Floor != "" ||
		!cursor.Since.Equal(since) || !cursor.Until.IsZero() || !cursor.FallbackSince.IsZero() {
		t.Fatalf("published cursor=%#v for %s, want head %s frontier %s and since %s", cursor, scanKey, head, frontier, since)
	}
}

func assertHistoryProgressRows(t *testing.T, rows []store.HistoryProgress, wantCommits []string) {
	t.Helper()
	want := make(map[string]bool, len(wantCommits))
	for _, commit := range wantCommits {
		want[commit] = true
	}
	if len(rows) != len(wantCommits) {
		t.Fatalf("durable progress rows=%d, want %d; rows=%#v", len(rows), len(wantCommits), rows)
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if !want[row.Commit] || seen[row.Commit] {
			t.Fatalf("unexpected or duplicate durable progress row: %#v", row)
		}
		if row.Events != 1 || row.Diagnostics != 0 {
			t.Fatalf("durable progress row=%#v, want one event and no diagnostics", row)
		}
		seen[row.Commit] = true
	}
}

func flattenHistoryEvents(groups []domain.FeedGroup) []domain.UpdateEvent {
	var events []domain.UpdateEvent
	for _, group := range groups {
		events = append(events, group.Events...)
	}
	return events
}

func assertHistoryEventSequence(t *testing.T, got []domain.UpdateEvent, want []expectedHistoryEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("visible events=%d, want %d", len(got), len(want))
	}
	for index := range want {
		assertHistoryEvent(t, got[index], want[index])
	}
}

func assertHistoryEventSet(t *testing.T, got []domain.UpdateEvent, want []expectedHistoryEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("visible events=%d, want %d", len(got), len(want))
	}
	remaining := make(map[domain.EventID]expectedHistoryEvent, len(want))
	for _, expected := range want {
		id := domain.NewEventID("core", expected.commit, expected.packageID, domain.EventVersion)
		if _, exists := remaining[id]; exists {
			t.Fatalf("duplicate expected event identity %s", id)
		}
		remaining[id] = expected
	}
	for _, event := range got {
		expected, ok := remaining[event.ID]
		if !ok {
			t.Fatalf("unexpected or duplicate event identity %s (commit %s)", event.ID, event.Commit)
		}
		assertHistoryEvent(t, event, expected)
		delete(remaining, event.ID)
	}
	if len(remaining) != 0 {
		t.Fatalf("missing event identities: %v", remaining)
	}
}

func assertHistoryEvent(t *testing.T, got domain.UpdateEvent, want expectedHistoryEvent) {
	t.Helper()
	wantID := domain.NewEventID("core", want.commit, want.packageID, domain.EventVersion)
	if got.ID != wantID || got.Commit != want.commit || got.PackageID != want.packageID ||
		got.Repository != "core" || got.Kind != domain.EventVersion || got.Type != domain.PackageFormula ||
		got.DefinitionPath != want.path || got.OldVersion != want.oldVersion || got.NewVersion != want.newVersion {
		t.Fatalf("event=%#v, want identity %s commit=%s package=%s path=%s version %s -> %s", got, wantID, want.commit, want.packageID, want.path, want.oldVersion, want.newVersion)
	}
}

func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))
}

func requireNoLazyFetchSupport(t *testing.T) {
	t.Helper()
	output, err := runGitCommand(context.Background(), "--no-lazy-fetch", "version")
	if err == nil {
		return
	}
	if strings.Contains(strings.ToLower(output), "unknown option") && strings.Contains(strings.ToLower(output), "no-lazy-fetch") {
		t.Skipf("host Git does not support --no-lazy-fetch: %s", strings.TrimSpace(output))
	}
	t.Fatalf("check host Git --no-lazy-fetch support: %v\n%s", err, output)
}

func runGitCommand(ctx context.Context, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	return string(output), err
}

func sourceBlobID(t *testing.T, repo *testutil.GitRepo, revision, path string) string {
	t.Helper()
	id := strings.TrimSpace(repo.Run("-C", repo.Path, "rev-parse", "--verify", revision+":"+path))
	if !isFullGitObjectID(id) {
		t.Fatalf("blob at %s:%s has invalid object ID %q", revision, path, id)
	}
	return id
}

func assertUniqueObjects(t *testing.T, objects []string) {
	t.Helper()
	seen := make(map[string]bool, len(objects))
	for _, object := range objects {
		if !isFullGitObjectID(object) || seen[object] {
			t.Fatalf("invalid or duplicate fixture blob object %q", object)
		}
		seen[object] = true
	}
}

func assertBlobStates(t *testing.T, ctx context.Context, repository string, objects []string, want string) {
	t.Helper()
	input := strings.Join(objects, "\n") + "\n"
	command := exec.CommandContext(ctx, "git", "--no-lazy-fetch", "-C", repository, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("check partial-clone blobs without lazy fetch: %v\n%s", err, output)
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != len(objects) {
		t.Fatalf("blob check lines=%d, want %d: %q", len(lines), len(objects), output)
	}
	for index, object := range objects {
		if expected := object + " " + want; lines[index] != expected {
			t.Fatalf("blob check[%d]=%q, want %q", index, lines[index], expected)
		}
	}
}

func assertGitConfig(t *testing.T, ctx context.Context, repository, key, want string) {
	t.Helper()
	output, err := runGitCommand(ctx, "-C", repository, "config", "--get", key)
	if err != nil {
		t.Fatalf("read mirror config %s: %v\n%s", key, err, output)
	}
	if got := strings.TrimSpace(output); got != want {
		t.Fatalf("mirror config %s=%q, want %q", key, got, want)
	}
}

func (r *fetchRecordingRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	isFetch := name == "git" && containsArgument(args, "fetch")
	if isFetch {
		r.fetches = append(r.fetches, append([]string(nil), args...))
	}
	result, err := r.Runner.Run(ctx, name, args...)
	if isFetch {
		r.fetchErrors = append(r.fetchErrors, err)
	}
	return result, err
}

func (r *fetchRecordingRunner) Start(ctx context.Context, name string, args ...string) (io.ReadWriteCloser, error) {
	return r.Runner.(execx.DuplexRunner).Start(ctx, name, args...)
}

type fetchRecordingRunner struct {
	execx.Runner
	fetches     [][]string
	fetchErrors []error
}

type secondFetchGateRunner struct {
	execx.Runner
	fetches       [][]string
	secondStarted chan struct{}
}

func (r *secondFetchGateRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	if name == "git" && containsArgument(args, "fetch") {
		r.fetches = append(r.fetches, append([]string(nil), args...))
		if len(r.fetches) == 2 {
			close(r.secondStarted)
			<-ctx.Done()
			return execx.Result{}, ctx.Err()
		}
	}
	return r.Runner.Run(ctx, name, args...)
}

func (r *secondFetchGateRunner) Start(ctx context.Context, name string, args ...string) (io.ReadWriteCloser, error) {
	return r.Runner.(execx.DuplexRunner).Start(ctx, name, args...)
}

func assertRecordedBlobFetches(t *testing.T, fetches [][]string, wantObjects, requiredObjects []string) {
	t.Helper()
	if len(requiredObjects) <= maxHistoryBlobFetchObjects {
		t.Fatalf("fixture has %d required merge blobs, want more than %d to exercise the fetch bound", len(requiredObjects), maxHistoryBlobFetchObjects)
	}
	if len(fetches) < 2 {
		t.Fatalf("recorded %d blob fetches for %d required merge blobs; want bounded multi-fetch groups", len(fetches), len(requiredObjects))
	}
	want := make(map[string]bool, len(wantObjects))
	for _, object := range wantObjects {
		want[object] = true
	}
	required := make(map[string]bool, len(requiredObjects))
	for _, object := range requiredObjects {
		if !want[object] || required[object] {
			t.Fatalf("required blob fetch object %s is unexpected or duplicated", object)
		}
		required[object] = true
	}
	seen := make(map[string]bool, len(wantObjects))
	multiObjectFetch := false
	for _, args := range fetches {
		if !containsArgument(args, "origin") || !containsArgument(args, "--no-tags") || !containsArgument(args, "--no-write-fetch-head") ||
			!containsArgument(args, "--recurse-submodules=no") || !containsArgument(args, "--filter=blob:none") ||
			containsArgument(args, "--stdin") {
			t.Fatalf("blob fetch arguments=%q, missing expected bounded fetch options", args)
		}
		objects := fetchObjectIDs(args)
		if len(objects) == 0 || len(objects) > maxHistoryBlobFetchObjects {
			t.Fatalf("blob fetch has %d object IDs, want 1..%d: %q", len(objects), maxHistoryBlobFetchObjects, args)
		}
		if len(objects) > 1 {
			multiObjectFetch = true
		}
		for _, object := range objects {
			if !want[object] || seen[object] {
				t.Fatalf("blob fetch requested unexpected or duplicate object %s: %q", object, args)
			}
			seen[object] = true
		}
	}
	if !multiObjectFetch {
		t.Fatal("no Git fetch requested multiple missing blob object IDs")
	}
	for object := range required {
		if !seen[object] {
			t.Fatalf("required merge blob %s was not explicitly fetched; fetched=%v", object, seen)
		}
	}
}

func fetchObjectIDs(args []string) []string {
	inFetch := false
	var objects []string
	for _, arg := range args {
		if !inFetch {
			if arg == "fetch" {
				inFetch = true
			}
			continue
		}
		if isFullGitObjectID(arg) {
			objects = append(objects, arg)
		}
	}
	return objects
}

func containsArgument(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}

func isFullGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

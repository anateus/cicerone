package gitrepo_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/gitrepo"
	"github.com/anateus/cicerone/internal/testutil"
)

// These tests need a Git version that can inspect a partial clone without
// implicitly downloading each missing object.
func requirePrefetchGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "no-global-gitconfig"))
	t.Setenv("GIT_NO_LAZY_FETCH", "0")

	cmd := exec.Command("git", "--no-lazy-fetch", "version")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	if strings.Contains(string(out), "unknown option") && strings.Contains(string(out), "no-lazy-fetch") {
		t.Skipf("Git does not support --no-lazy-fetch: %s", out)
	}
	t.Fatalf("checking Git --no-lazy-fetch support: %v\n%s", err, out)
}

type prefetchTestBlob struct {
	ref     gitrepo.BlobRef
	oid     string
	content string
}

type prefetchFixture struct {
	source    *testutil.GitRepo
	remoteURL string
	mirror    string
	old       prefetchTestBlob
	copy      prefetchTestBlob
	updated   prefetchTestBlob
	unusual   prefetchTestBlob
	local     prefetchTestBlob
	unrelated prefetchTestBlob
	bulk      []prefetchTestBlob
}

func newPrefetchFixture(t *testing.T, bulkCount int) *prefetchFixture {
	t.Helper()
	source := testutil.NewGitRepo(t)
	source.Run("-C", source.Path, "config", "uploadpack.allowFilter", "true")
	source.Run("-C", source.Path, "config", "uploadpack.allowAnySHA1InWant", "true")
	f := &prefetchFixture{source: source, remoteURL: (&url.URL{Scheme: "file", Path: source.Path}).String()}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	commit := func(path, content, message string) prefetchTestBlob {
		revision := source.Commit(path, content, message, at)
		at = at.Add(time.Second)
		oid := strings.TrimSpace(source.Run("-C", source.Path, "rev-parse", "--verify", revision+":"+path))
		return prefetchTestBlob{ref: gitrepo.BlobRef{Revision: revision, Path: path}, oid: oid, content: content}
	}
	f.old = commit("Formula/recipe.rb", "old recipe\n", "old recipe")
	f.copy = commit("Formula/copied recipe.rb", f.old.content, "copy recipe")
	f.updated = commit("Formula/recipe.rb", "updated recipe\n", "update recipe")
	f.unusual = commit("Formula/space and\nnewline.rb", "unusual path\n", "unusual path")
	f.local = commit("Formula/already local.rb", "already local\n", "local blob")
	f.unrelated = commit("Formula/unrelated.rb", "unrelated blob\n", "unrelated blob")
	if f.old.oid != f.copy.oid || f.old.oid == f.updated.oid || f.old.oid == f.unusual.oid || f.updated.oid == f.unusual.oid {
		t.Fatal("fixture does not have the intended distinct and shared blob IDs")
	}

	if bulkCount > 0 {
		dir := filepath.Join(source.Path, "Formula", "prefetch-bulk")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range bulkCount {
			name := fmt.Sprintf("item-%03d.rb", i)
			content := fmt.Sprintf("bulk content %03d\n", i)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		source.Run("-C", source.Path, "add", "--", "Formula/prefetch-bulk")
		source.Run("-C", source.Path, "commit", "-m", "add bulk definitions")
		revision := strings.TrimSpace(source.Run("-C", source.Path, "rev-parse", "HEAD"))
		tree := source.Run("-C", source.Path, "ls-tree", "-r", "-z", revision, "--", "Formula/prefetch-bulk")
		ids := make(map[string]string, bulkCount)
		for _, record := range strings.Split(tree, "\x00") {
			if record == "" {
				continue
			}
			header, path, ok := strings.Cut(record, "\t")
			fields := strings.Fields(header)
			if !ok || len(fields) != 3 || fields[1] != "blob" {
				t.Fatalf("unexpected ls-tree record %q", record)
			}
			ids[path] = fields[2]
		}
		if len(ids) != bulkCount {
			t.Fatalf("bulk tree contains %d blobs, want %d", len(ids), bulkCount)
		}
		for i := range bulkCount {
			path := fmt.Sprintf("Formula/prefetch-bulk/item-%03d.rb", i)
			oid := ids[path]
			if oid == "" {
				t.Fatalf("bulk tree missing %q", path)
			}
			f.bulk = append(f.bulk, prefetchTestBlob{
				ref:     gitrepo.BlobRef{Revision: revision, Path: path},
				oid:     oid,
				content: fmt.Sprintf("bulk content %03d\n", i),
			})
		}
	}
	f.mirror = f.cloneMirror(t)
	return f
}

func (f *prefetchFixture) cloneMirror(t *testing.T) string {
	t.Helper()
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	f.source.Run("clone", "--mirror", "--filter=blob:none", "--", f.remoteURL, mirror)
	return mirror
}

func (f *prefetchFixture) repositorySource() gitrepo.Source {
	return gitrepo.Source{Name: "prefetch-test", Path: f.mirror, RemoteURL: f.remoteURL, Owned: true}
}

// Probe object presence without allowing Git to hide a missing fixture object
// by fetching it. A successful batch response also distinguishes absence from a
// broken repository or a failed subprocess.
func (f *prefetchFixture) requireObjects(t *testing.T, present bool, blobs ...prefetchTestBlob) {
	t.Helper()
	var input, want strings.Builder
	for _, blob := range blobs {
		fmt.Fprintln(&input, blob.oid)
		if present {
			fmt.Fprintf(&want, "%s blob\n", blob.oid)
		} else {
			fmt.Fprintf(&want, "%s missing\n", blob.oid)
		}
	}
	cmd := exec.Command("git", "--no-lazy-fetch", "-C", f.mirror, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	cmd.Stdin = strings.NewReader(input.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("checking local objects: %v\n%s", err, out)
	}
	if string(out) != want.String() {
		t.Fatalf("local object status = %q, want %q", out, want.String())
	}
}

type prefetchRecordingRunner struct {
	execx.Runner
	calls  [][]string
	starts int
}

func (r *prefetchRecordingRunner) record(name string, args []string) {
	r.calls = append(r.calls, append([]string{name}, args...))
}

func (r *prefetchRecordingRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	r.record(name, args)
	return r.Runner.Run(ctx, name, args...)
}

func (r *prefetchRecordingRunner) Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	r.record(name, args)
	return r.Runner.Stream(ctx, name, args...)
}

func (r *prefetchRecordingRunner) Start(ctx context.Context, name string, args ...string) (io.ReadWriteCloser, error) {
	r.record(name, args)
	r.starts++
	return r.Runner.(execx.DuplexRunner).Start(ctx, name, args...)
}

func (r *prefetchRecordingRunner) fetches() [][]string {
	var fetches [][]string
	for _, call := range r.calls {
		for _, arg := range call {
			if arg == "fetch" {
				fetches = append(fetches, call)
				break
			}
		}
	}
	return fetches
}

type gatedPrefetchRunner struct {
	*prefetchRecordingRunner
	entered chan struct{}
}

func (r *gatedPrefetchRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	for _, arg := range args {
		if name == "git" && arg == "fetch" {
			r.record(name, args)
			r.entered <- struct{}{}
			<-ctx.Done()
			return execx.Result{}, ctx.Err()
		}
	}
	return r.prefetchRecordingRunner.Run(ctx, name, args...)
}

type unsupportedPrefetchRunner struct {
	*prefetchRecordingRunner
	probes int
}

func (r *unsupportedPrefetchRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	if name == "git" && len(args) == 2 && args[0] == "--no-lazy-fetch" && args[1] == "version" {
		r.record(name, args)
		r.probes++
		return execx.Result{Stderr: []byte("unknown option: --no-lazy-fetch")}, errors.New("git does not support --no-lazy-fetch")
	}
	return r.prefetchRecordingRunner.Run(ctx, name, args...)
}

func requireReadablePrefetchBlob(t *testing.T, repo gitrepo.Repository, blob prefetchTestBlob) {
	t.Helper()
	reader, err := repo.OpenBlobReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("closing blob reader: %v", err)
		}
	}()
	body, err := reader.Blob(blob.ref.Revision, blob.ref.Path)
	if err != nil || string(body) != blob.content {
		t.Fatalf("Blob(%q, %q) = %q, %v; want %q", blob.ref.Revision, blob.ref.Path, body, err, blob.content)
	}
}

// Fetch operands must be full object IDs, not revision:path expressions, a
// default refspec, or an --stdin request. Return them for exact set comparison.
func fetchObjectIDs(t *testing.T, call []string, remoteURL string) []string {
	t.Helper()
	fetch := -1
	for i, arg := range call {
		if arg == "fetch" {
			fetch = i
			break
		}
	}
	if fetch < 0 {
		t.Fatalf("not a fetch command: %q", call)
	}
	options := make(map[string]bool)
	remoteCount := 0
	var ids []string
	for _, arg := range call[fetch+1:] {
		switch {
		case arg == "origin" || arg == remoteURL:
			remoteCount++
		case arg == "--":
		case strings.HasPrefix(arg, "--"):
			options[arg] = true
		default:
			ids = append(ids, arg)
		}
	}
	for _, option := range []string{"--no-tags", "--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none"} {
		if !options[option] {
			t.Errorf("fetch missing %s: %q", option, call)
		}
	}
	if options["--stdin"] || remoteCount != 1 || len(ids) == 0 {
		t.Fatalf("fetch must have one remote and nonempty OID refspecs without --stdin: %q", call)
	}
	return ids
}

func requireFetchedIDs(t *testing.T, fetches [][]string, remoteURL string, want []string) {
	t.Helper()
	var got []string
	for _, fetch := range fetches {
		ids := fetchObjectIDs(t, fetch, remoteURL)
		if len(ids) > 64 {
			t.Fatalf("fetch has %d object IDs, exceeds the bound of 64: %q", len(ids), fetch)
		}
		got = append(got, ids...)
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fetched OIDs = %q, want %q", got, want)
	}
}

func TestBlobPrefetchFetchesOnlyMissingHistoricalObjects(t *testing.T) {
	requirePrefetchGit(t)
	f := newPrefetchFixture(t, 0)
	f.requireObjects(t, false, f.old, f.copy, f.updated, f.unusual, f.local, f.unrelated)
	// Seed one requested object before recording the prefetcher's commands.
	f.source.Run("-C", f.mirror, "fetch", "origin", "--no-tags", "--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none", f.local.oid)
	f.requireObjects(t, true, f.local)
	f.requireObjects(t, false, f.old, f.copy, f.updated, f.unusual, f.unrelated)

	ctx := context.Background()
	runner := &prefetchRecordingRunner{Runner: execx.NewRunner()}
	repo := gitrepo.New(f.repositorySource(), runner)
	prefetcher, err := repo.NewBlobPrefetcher(ctx)
	if err != nil {
		t.Fatal(err)
	}
	readerRunner := &prefetchRecordingRunner{Runner: execx.NewRunner()}
	reader, err := gitrepo.New(f.repositorySource(), readerRunner).OpenBlobReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("closing blob reader: %v", err)
		}
	})
	// A successful request establishes that the same cat-file process is already
	// serving objects before new packs are fetched.
	body, err := reader.Blob(f.local.ref.Revision, f.local.ref.Path)
	if err != nil || string(body) != f.local.content || readerRunner.starts != 1 {
		t.Fatalf("initial persistent read = %q, %v; starts=%d", body, err, readerRunner.starts)
	}

	refs := []gitrepo.BlobRef{f.old.ref, f.updated.ref, f.unusual.ref, f.copy.ref, f.old.ref, f.local.ref}
	if err := prefetcher.Prefetch(ctx, refs); err != nil {
		t.Fatalf("Prefetch: %v", err)
	}
	fetches := runner.fetches()
	if len(fetches) != 1 {
		t.Fatalf("explicit fetches = %q, want one bulk fetch", fetches)
	}
	requireFetchedIDs(t, fetches, f.remoteURL, []string{f.old.oid, f.updated.oid, f.unusual.oid})
	f.requireObjects(t, true, f.old, f.copy, f.updated, f.unusual, f.local)
	f.requireObjects(t, false, f.unrelated)

	// If the existing reader cannot see the fetched pack, it must not be able
	// to mask that bug by lazily downloading the object from the source.
	if err := os.Rename(f.source.Path, f.source.Path+"-offline"); err != nil {
		t.Fatal(err)
	}
	for _, blob := range []prefetchTestBlob{f.old, f.updated, f.unusual, f.copy} {
		body, err := reader.Blob(blob.ref.Revision, blob.ref.Path)
		if err != nil || string(body) != blob.content {
			t.Fatalf("persistent Blob(%q, %q) = %q, %v; want %q", blob.ref.Revision, blob.ref.Path, body, err, blob.content)
		}
	}
	if readerRunner.starts != 1 {
		t.Fatalf("blob reader started %d cat-file processes, want the original process", readerRunner.starts)
	}
	if err := prefetcher.Prefetch(ctx, refs); err != nil {
		t.Fatalf("Prefetch of present objects with offline origin: %v", err)
	}
	if err := prefetcher.Prefetch(ctx, nil); err != nil {
		t.Fatalf("empty Prefetch: %v", err)
	}
	if got := runner.fetches(); len(got) != 1 {
		t.Fatalf("present or empty requests triggered another fetch: %q", got)
	}
	f.requireObjects(t, false, f.unrelated)
}

func TestBlobPrefetchBoundsFetchGroups(t *testing.T) {
	requirePrefetchGit(t)
	f := newPrefetchFixture(t, 65)
	f.requireObjects(t, false, f.bulk...)
	f.requireObjects(t, false, f.unrelated)
	runner := &prefetchRecordingRunner{Runner: execx.NewRunner()}
	prefetcher, err := gitrepo.New(f.repositorySource(), runner).NewBlobPrefetcher(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var refs []gitrepo.BlobRef
	var want []string
	for _, blob := range f.bulk {
		refs = append(refs, blob.ref)
		want = append(want, blob.oid)
	}
	// The duplicate is across the 64-object boundary.
	refs = append(refs, f.bulk[0].ref)
	if err := prefetcher.Prefetch(context.Background(), refs); err != nil {
		t.Fatalf("Prefetch: %v", err)
	}
	fetches := runner.fetches()
	if len(fetches) < 2 {
		t.Fatalf("fetches = %q, want more than one bounded group for 65 missing objects", fetches)
	}
	requireFetchedIDs(t, fetches, f.remoteURL, want)
	f.requireObjects(t, true, f.bulk...)
	f.requireObjects(t, false, f.unrelated)
}

func TestBlobPrefetchDisabledForUnownedOrDifferentPromisors(t *testing.T) {
	requirePrefetchGit(t)
	base := newPrefetchFixture(t, 0)
	tests := []struct {
		name  string
		setup func(*prefetchFixture, *gitrepo.Source)
	}{
		{name: "unowned", setup: func(_ *prefetchFixture, source *gitrepo.Source) { source.Owned = false }},
		{name: "mismatched origin", setup: func(_ *prefetchFixture, source *gitrepo.Source) { source.RemoteURL += "-other" }},
		{name: "missing origin filter", setup: func(f *prefetchFixture, _ *gitrepo.Source) {
			f.source.Run("-C", f.mirror, "config", "--unset", "remote.origin.partialCloneFilter")
		}},
		{name: "different origin filter", setup: func(f *prefetchFixture, _ *gitrepo.Source) {
			f.source.Run("-C", f.mirror, "config", "remote.origin.partialCloneFilter", "tree:0")
		}},
		{name: "additional promisor", setup: func(f *prefetchFixture, _ *gitrepo.Source) {
			f.source.Run("-C", f.mirror, "remote", "add", "other", f.remoteURL)
			f.source.Run("-C", f.mirror, "config", "remote.other.promisor", "true")
		}},
		{name: "only non-origin promisor", setup: func(f *prefetchFixture, _ *gitrepo.Source) {
			f.source.Run("-C", f.mirror, "remote", "add", "other", f.remoteURL)
			f.source.Run("-C", f.mirror, "config", "remote.origin.promisor", "false")
			f.source.Run("-C", f.mirror, "config", "remote.other.promisor", "true")
		}},
		{name: "legacy alternative promisor", setup: func(f *prefetchFixture, _ *gitrepo.Source) {
			f.source.Run("-C", f.mirror, "remote", "add", "other", f.remoteURL)
			f.source.Run("-C", f.mirror, "config", "extensions.partialClone", "other")
		}},
		{name: "alternative partial clone filter", setup: func(f *prefetchFixture, _ *gitrepo.Source) {
			f.source.Run("-C", f.mirror, "remote", "add", "other", f.remoteURL)
			f.source.Run("-C", f.mirror, "config", "remote.other.partialCloneFilter", "blob:none")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := *base
			fixture.source = &testutil.GitRepo{T: t, Path: base.source.Path}
			fixture.mirror = fixture.cloneMirror(t)
			source := fixture.repositorySource()
			tc.setup(&fixture, &source)
			fixture.requireObjects(t, false, fixture.old, fixture.unrelated)
			runner := &prefetchRecordingRunner{Runner: execx.NewRunner()}
			prefetcher, err := gitrepo.New(source, runner).NewBlobPrefetcher(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := prefetcher.Prefetch(context.Background(), []gitrepo.BlobRef{fixture.old.ref}); err != nil {
				t.Fatalf("disabled Prefetch: %v", err)
			}
			if got := runner.fetches(); len(got) != 0 {
				t.Fatalf("disabled prefetch issued fetches: %q", got)
			}
			fixture.requireObjects(t, false, fixture.old, fixture.unrelated)
		})
	}
}

func TestBlobPrefetchReportsInaccessiblePromisor(t *testing.T) {
	requirePrefetchGit(t)
	f := newPrefetchFixture(t, 0)
	f.requireObjects(t, false, f.old)
	runner := &prefetchRecordingRunner{Runner: execx.NewRunner()}
	prefetcher, err := gitrepo.New(f.repositorySource(), runner).NewBlobPrefetcher(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Leave the configured origin unchanged, but make the real remote unavailable.
	if err := os.Rename(f.source.Path, f.source.Path+"-offline"); err != nil {
		t.Fatal(err)
	}
	if err := prefetcher.Prefetch(context.Background(), []gitrepo.BlobRef{f.old.ref}); err == nil {
		t.Fatal("Prefetch succeeded with an inaccessible promisor")
	}
	fetches := runner.fetches()
	if len(fetches) != 1 {
		t.Fatalf("fetch attempts = %q, want one", fetches)
	}
	requireFetchedIDs(t, fetches, f.remoteURL, []string{f.old.oid})
	f.requireObjects(t, false, f.old)
}

func TestBlobPrefetchReportsCancellationAndMissingRefs(t *testing.T) {
	requirePrefetchGit(t)
	f := newPrefetchFixture(t, 0)
	f.requireObjects(t, false, f.old)
	runner := &prefetchRecordingRunner{Runner: execx.NewRunner()}
	repo := gitrepo.New(f.repositorySource(), runner)
	prefetcher, err := repo.NewBlobPrefetcher(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.NewBlobPrefetcher(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewBlobPrefetcher with canceled context = %v, want context.Canceled", err)
	}
	if err := prefetcher.Prefetch(ctx, []gitrepo.BlobRef{f.old.ref}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prefetch with canceled context = %v, want context.Canceled", err)
	}
	missing := gitrepo.BlobRef{Revision: f.old.ref.Revision, Path: "Formula/not-in-this-commit.rb"}
	if err := prefetcher.Prefetch(context.Background(), []gitrepo.BlobRef{missing}); err == nil {
		t.Fatal("Prefetch succeeded for a missing revision:path")
	}
	if got := runner.fetches(); len(got) != 0 {
		t.Fatalf("canceled or unresolved requests issued fetches: %q", got)
	}
	f.requireObjects(t, false, f.old)
}

func TestBlobPrefetchCancellationDuringFetch(t *testing.T) {
	requirePrefetchGit(t)
	f := newPrefetchFixture(t, 0)
	f.requireObjects(t, false, f.old, f.local, f.unrelated)
	f.source.Run("-C", f.mirror, "fetch", "origin", "--no-tags", "--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none", f.local.oid)
	f.requireObjects(t, true, f.local)
	f.requireObjects(t, false, f.old, f.unrelated)

	runner := &gatedPrefetchRunner{
		prefetchRecordingRunner: &prefetchRecordingRunner{Runner: execx.NewRunner()},
		entered:                 make(chan struct{}, 1),
	}
	repo := gitrepo.New(f.repositorySource(), runner)
	prefetcher, err := repo.NewBlobPrefetcher(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- prefetcher.Prefetch(ctx, []gitrepo.BlobRef{f.old.ref, f.local.ref})
	}()
	select {
	case <-runner.entered:
	case err := <-done:
		t.Fatalf("Prefetch returned before entering fetch: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Prefetch to enter fetch")
	}
	// The runner holds the fetch at its boundary and cannot report successful
	// hydration; cancellation releases it without contacting the remote.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prefetch after cancellation during fetch = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for canceled Prefetch")
	}
	fetches := runner.fetches()
	if len(fetches) != 1 {
		t.Fatalf("fetch attempts = %q, want one canceled attempt", fetches)
	}
	requireFetchedIDs(t, fetches, f.remoteURL, []string{f.old.oid})
	f.requireObjects(t, false, f.old, f.unrelated)
	f.requireObjects(t, true, f.local)
	requireReadablePrefetchBlob(t, repo, f.local)
	f.requireObjects(t, false, f.old, f.unrelated)
}

func TestBlobPrefetchDisabledWhenGitLacksNoLazyFetch(t *testing.T) {
	requirePrefetchGit(t)
	f := newPrefetchFixture(t, 0)
	f.requireObjects(t, false, f.old, f.local, f.unrelated)
	f.source.Run("-C", f.mirror, "fetch", "origin", "--no-tags", "--no-write-fetch-head", "--recurse-submodules=no", "--filter=blob:none", f.local.oid)
	f.requireObjects(t, true, f.local)
	f.requireObjects(t, false, f.old, f.unrelated)

	runner := &unsupportedPrefetchRunner{prefetchRecordingRunner: &prefetchRecordingRunner{Runner: execx.NewRunner()}}
	repo := gitrepo.New(f.repositorySource(), runner)
	prefetcher, err := repo.NewBlobPrefetcher(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if runner.probes != 1 {
		t.Fatalf("unsupported Git capability probes = %d, want one", runner.probes)
	}
	if err := prefetcher.Prefetch(context.Background(), []gitrepo.BlobRef{f.old.ref, f.local.ref}); err != nil {
		t.Fatalf("Prefetch with unsupported Git: %v", err)
	}
	if got := runner.fetches(); len(got) != 0 {
		t.Fatalf("unsupported Git issued fetches: %q", got)
	}
	f.requireObjects(t, false, f.old, f.unrelated)
	f.requireObjects(t, true, f.local)
	requireReadablePrefetchBlob(t, repo, f.local)
	f.requireObjects(t, false, f.old, f.unrelated)
}

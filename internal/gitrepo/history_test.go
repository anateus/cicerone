package gitrepo_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cicerone/internal/execx"
	"cicerone/internal/gitrepo"
	"cicerone/internal/testutil"
)

func TestOwnedMirrorLifecycleAndLocalFetchGuard(t *testing.T) {
	runner := &testutil.Runner{}
	path := filepath.Join(t.TempDir(), "mirrors", "core.git")
	owned := gitrepo.New(gitrepo.Source{Name: "homebrew-core", Path: path, RemoteURL: "https://example.test/core.git", Owned: true}, runner)
	if err := owned.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := owned.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"clone", "--mirror", "--filter=blob:none", "--", "https://example.test/core.git", path},
		{"-C", path, "fetch", "--prune"},
	}
	if len(runner.RunCalls) != len(want) {
		t.Fatalf("calls = %#v", runner.RunCalls)
	}
	for i, call := range runner.RunCalls {
		if call.Name != "git" || !reflect.DeepEqual(call.Args, want[i]) {
			t.Errorf("call %d = %#v, want git %#v", i, call, want[i])
		}
	}
	local := gitrepo.New(gitrepo.Source{Path: t.TempDir()}, runner)
	if err := local.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := local.Fetch(context.Background()); err == nil {
		t.Fatal("local Fetch error = nil")
	}
	if len(runner.RunCalls) != 2 {
		t.Fatalf("local Fetch executed Git: %#v", runner.RunCalls[2:])
	}
}

func TestEnsureRejectsInterruptedOwnedCacheWithoutModifyingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.git")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "interrupted-clone")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	repository := gitrepo.New(gitrepo.Source{Name: "homebrew-core", Path: path, RemoteURL: "https://example.test/core.git", Owned: true}, execx.NewRunner())
	err := repository.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "remove") || !strings.Contains(err.Error(), path) {
		t.Fatalf("Ensure error = %v, want explicit cache recovery instruction", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
		t.Fatalf("interrupted cache was modified: %q, %v", got, err)
	}
}

func TestEnsureRejectsBareCloneThatIsNotAMirror(t *testing.T) {
	source := testutil.NewGitRepo(t)
	source.Commit("Formula/a.rb", "contents", "initial", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cache := filepath.Join(t.TempDir(), "core.git")
	source.Run("clone", "--bare", "--", source.Path, cache)
	repository := gitrepo.New(gitrepo.Source{Name: "homebrew-core", Path: cache, RemoteURL: source.Path, Owned: true}, execx.NewRunner())
	err := repository.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mirror") || !strings.Contains(err.Error(), "move it aside") {
		t.Fatalf("Ensure error = %v, want non-mirror recovery guidance", err)
	}
	if got := source.Run("-C", cache, "rev-parse", "HEAD"); strings.TrimSpace(got) == "" {
		t.Fatal("bare cache was modified")
	}
}

func TestEnsureAcceptsUsableMirror(t *testing.T) {
	source := testutil.NewGitRepo(t)
	source.Commit("Formula/a.rb", "contents", "initial", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cache := filepath.Join(t.TempDir(), "core.git")
	source.Run("clone", "--mirror", "--", source.Path, cache)
	repository := gitrepo.New(gitrepo.Source{Name: "homebrew-core", Path: cache, RemoteURL: source.Path, Owned: true}, execx.NewRunner())
	if err := repository.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure error = %v", err)
	}
}

func TestRepositoryHistoryProtocol(t *testing.T) {
	r := testutil.NewGitRepo(t)
	jan1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	first := r.Commit("Formula/a file.rb", "one", "first", jan1)
	second := r.Commit("Formula/a file.rb", "two", "second", jan1.Add(24*time.Hour))
	r.Run("-C", r.Path, "mv", "Formula/a file.rb", "Formula/renamed file.rb")
	third := r.Commit("Formula/renamed file.rb", "two", "rename", jan1.Add(48*time.Hour))
	repository := gitrepo.New(gitrepo.Source{Path: r.Path}, execx.NewRunner())
	head, err := repository.Head(context.Background())
	if err != nil || head != third {
		t.Fatalf("Head = %q, %v, want %q", head, err, third)
	}

	commits, err := repository.Commits(context.Background(), gitrepo.Range{Revision: "HEAD", Since: jan1.Add(12 * time.Hour), Until: jan1.Add(72 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 || commits[0].Hash != third || commits[1].Hash != second {
		t.Fatalf("commits = %#v", commits)
	}
	if got := commits[0].Changes; len(got) != 1 || got[0].Status != "R" || got[0].OldPath != "Formula/a file.rb" || got[0].Path != "Formula/renamed file.rb" {
		t.Fatalf("rename = %#v", got)
	}
	blob, err := repository.Blob(context.Background(), first, "Formula/a file.rb")
	if err != nil || string(blob) != "one" {
		t.Fatalf("Blob = %q, %v", blob, err)
	}
}

func TestMergeBaseAcrossDivergentBranches(t *testing.T) {
	r := testutil.NewGitRepo(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := r.Commit("file", "base", "base", at)
	r.Run("-C", r.Path, "branch", "old")
	r.Commit("file", "main", "main", at.Add(time.Hour))
	r.Run("-C", r.Path, "switch", "old")
	r.Commit("file", "old", "old", at.Add(2*time.Hour))
	repository := gitrepo.New(gitrepo.Source{Path: r.Path}, execx.NewRunner())
	got, err := repository.MergeBase(context.Background(), "main", "old")
	if err != nil || got != base {
		t.Fatalf("MergeBase = %q, %v, want %q", got, err, base)
	}
	if _, err := repository.Blob(context.Background(), "HEAD", filepath.Join("missing", "file")); err == nil {
		t.Fatal("Blob missing error = nil")
	}
}

func TestCommitsAllowsRecordSeparatorInFilename(t *testing.T) {
	r := testutil.NewGitRepo(t)
	at := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	path := "Formula/control" + string([]byte{0x1e}) + "byte.rb"
	hash := r.Commit(path, "contents", "control-byte path", at)
	repository := gitrepo.New(gitrepo.Source{Path: r.Path}, execx.NewRunner())
	commits, err := repository.Commits(context.Background(), gitrepo.Range{})
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || commits[0].Hash != hash || len(commits[0].Changes) != 1 || commits[0].Changes[0].Path != path {
		t.Fatalf("commits = %#v", commits)
	}
}

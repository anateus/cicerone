package gitrepo_test

import (
	"context"
	"path/filepath"
	"reflect"
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
	if err := local.Fetch(context.Background()); err == nil {
		t.Fatal("local Fetch error = nil")
	}
	if len(runner.RunCalls) != 2 {
		t.Fatalf("local Fetch executed Git: %#v", runner.RunCalls[2:])
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

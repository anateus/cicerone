package gitrepo_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/gitrepo"
	"github.com/anateus/cicerone/internal/testutil"
)

func TestFirstParentAndAncestryAcrossMerge(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	root := repo.Commit("root", "root", "root", at)
	repo.Run("-C", repo.Path, "branch", "side")
	main := repo.Commit("main", "main", "main", at.Add(time.Minute))
	repo.Run("-C", repo.Path, "switch", "side")
	side := repo.Commit("side", "side", "side", at.Add(2*time.Minute))
	repo.Run("-C", repo.Path, "switch", "main")
	repo.Run("-C", repo.Path, "merge", "--no-ff", "side", "-m", "merge side")
	merge := strings.TrimSpace(repo.Run("-C", repo.Path, "rev-parse", "HEAD"))
	head := repo.Commit("after", "after", "after merge", at.Add(3*time.Minute))
	repository := gitrepo.New(gitrepo.Source{Path: repo.Path}, execx.NewRunner())
	ctx := context.Background()

	for _, tc := range []struct {
		hash, want string
	}{{root, ""}, {main, root}, {merge, main}, {head, merge}} {
		got, err := repository.FirstParent(ctx, tc.hash)
		if err != nil || got != tc.want {
			t.Errorf("FirstParent(%s) = %q, %v; want %q", tc.hash, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		ancestor, descendant string
		want                 bool
	}{
		{root, root, true}, {root, head, true}, {main, head, true},
		{merge, head, true}, {side, head, false}, {head, main, false},
	} {
		got, err := repository.IsFirstParentAncestor(ctx, tc.ancestor, tc.descendant)
		if err != nil || got != tc.want {
			t.Errorf("IsFirstParentAncestor(%s, %s) = %v, %v; want %v", tc.ancestor, tc.descendant, got, err, tc.want)
		}
	}
	missing := strings.Repeat("0", 40)
	if _, err := repository.FirstParent(ctx, missing); err == nil {
		t.Fatal("FirstParent accepted nonexistent commit")
	}
	if got, err := repository.IsFirstParentAncestor(ctx, missing, head); err != nil || got {
		t.Fatalf("missing ancestor = %v, %v; want false, nil", got, err)
	}
	if got, err := repository.IsFirstParentAncestor(ctx, missing, missing); err != nil || got {
		t.Fatalf("missing identical ancestor = %v, %v; want false, nil", got, err)
	}
	if _, err := repository.IsFirstParentAncestor(ctx, root, missing); err == nil {
		t.Fatal("missing descendant did not report a Git error")
	}
	_, lookupErr := execx.NewRunner().Run(ctx, "git", "-C", repo.Path, "cat-file", "-e", missing)
	if lookupErr == nil {
		t.Fatal("expected missing-object command failure")
	}
	failed := gitrepo.New(gitrepo.Source{Path: repo.Path}, &ancestryFailureRunner{result: execx.Result{Stderr: []byte("object fetch failed")}, err: lookupErr})
	if got, err := failed.IsFirstParentAncestor(ctx, missing, head); err == nil || got || !errors.Is(err, lookupErr) {
		t.Fatalf("stderr-bearing missing-object failure = %v, %v", got, err)
	}
	cancelledLookup, cancelLookup := context.WithCancel(ctx)
	failed = gitrepo.New(gitrepo.Source{Path: repo.Path}, &ancestryFailureRunner{err: lookupErr, cancel: cancelLookup})
	if _, err := failed.IsFirstParentAncestor(cancelledLookup, missing, head); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled object lookup = %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repository.IsFirstParentAncestor(cancelled, root, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled same-hash check = %v", err)
	}
}

func TestFirstParentAncestryOnlyRequestsNewPrefix(t *testing.T) {
	ancestor := strings.Repeat("a", 40)
	descendant := strings.Repeat("b", 40)
	otherParent := strings.Repeat("c", 40)
	runner := &historyRecordingRunner{reader: io.NopCloser(strings.NewReader(otherParent + " " + ancestor + "\n" + ancestor + "\n"))}
	repository := gitrepo.New(gitrepo.Source{Path: "/repo"}, runner)
	got, err := repository.IsFirstParentAncestor(context.Background(), ancestor, descendant)
	if err != nil || !got {
		t.Fatalf("IsFirstParentAncestor = %v, %v", got, err)
	}
	for _, arg := range []string{"log", "--first-parent", "--format=%P", ancestor + ".." + descendant} {
		if !slices.Contains(runner.args, arg) {
			t.Fatalf("ancestry command %v missing %q", runner.args, arg)
		}
	}
}

type ancestryFailureRunner struct {
	result execx.Result
	err    error
	cancel context.CancelFunc
}

func (r *ancestryFailureRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	if r.cancel != nil {
		r.cancel()
	}
	return r.result, r.err
}

func (*ancestryFailureRunner) Stream(context.Context, string, ...string) (io.ReadCloser, error) {
	panic("unexpected ancestry stream")
}

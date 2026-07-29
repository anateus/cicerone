package gitrepo_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cicerone/internal/execx"
	"cicerone/internal/gitrepo"
	"cicerone/internal/testutil"
)

func TestDiscoverPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := gitrepo.Discover(ctx, t.TempDir(), t.TempDir(), execx.NewRunner())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Discover error = %v, want context.Canceled", err)
	}
}

func TestDiscoverPrefersConventionalLocalTapWithoutMutation(t *testing.T) {
	prefix := t.TempDir()
	local := filepath.Join(prefix, "Homebrew", "Library", "Taps", "homebrew", "homebrew-core")
	repo := testutil.NewGitRepo(t)
	// Moving preserves the repository while placing it at Homebrew's conventional path.
	if err := moveDir(repo.Path, local); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{Runner: execx.NewRunner()}

	sources, err := gitrepo.Discover(context.Background(), prefix, t.TempDir(), runner)
	if err != nil {
		t.Fatal(err)
	}
	if got := sources[0]; got.Path != local || got.Owned || got.Name != "homebrew-core" || got.Kind != "formula" {
		t.Fatalf("core source = %#v", got)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		for _, mutating := range []string{" clone ", " fetch ", " checkout ", " reset "} {
			if strings.Contains(" "+joined+" ", mutating) {
				t.Fatalf("mutating discovery command: %q", joined)
			}
		}
	}
}

func TestDiscoverUsesBrewRepositoryThenOwnedFallback(t *testing.T) {
	prefix, cache := t.TempDir(), t.TempDir()
	valid := testutil.NewGitRepo(t).Path
	runner := &scriptedRunner{responses: map[string]execx.Result{
		"brew --repository homebrew/core": {Stdout: []byte(valid + "\n")},
		"brew --repository homebrew/cask": {Stdout: []byte(filepath.Join(t.TempDir(), "invalid") + "\n")},
	}}
	sources, err := gitrepo.Discover(context.Background(), prefix, cache, runner)
	if err != nil {
		t.Fatal(err)
	}
	want := []gitrepo.Source{
		{Kind: "formula", Name: "homebrew-core", Path: valid, RemoteURL: "https://github.com/Homebrew/homebrew-core.git", Branch: "main"},
		{Kind: "cask", Name: "homebrew-cask", Path: filepath.Join(cache, "homebrew-cask.git"), RemoteURL: "https://github.com/Homebrew/homebrew-cask.git", Branch: "main", Owned: true},
	}
	if !reflect.DeepEqual(sources, want) {
		t.Fatalf("sources = %#v, want %#v", sources, want)
	}
}

type recordingRunner struct {
	execx.Runner
	calls [][]string
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.Runner.Run(ctx, name, args...)
}

type scriptedRunner struct{ responses map[string]execx.Result }

func (r *scriptedRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	if name == "git" {
		return execx.NewRunner().Run(context.Background(), name, args...)
	}
	return r.responses[strings.Join(append([]string{name}, args...), " ")], nil
}
func (r *scriptedRunner) Stream(context.Context, string, ...string) (io.ReadCloser, error) {
	panic("unused")
}

func moveDir(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}

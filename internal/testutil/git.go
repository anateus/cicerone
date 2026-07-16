package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type GitRepo struct {
	T    *testing.T
	Path string
}

func NewGitRepo(t *testing.T) *GitRepo {
	t.Helper()
	r := &GitRepo{T: t, Path: filepath.Join(t.TempDir(), "repo")}
	r.Run("init", "-b", "main", r.Path)
	r.Run("-C", r.Path, "config", "user.name", "Test User")
	r.Run("-C", r.Path, "config", "user.email", "test@example.com")
	return r
}

func (r *GitRepo) Run(args ...string) string {
	r.T.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		r.T.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (r *GitRepo) Commit(path, contents, message string, at time.Time) string {
	r.T.Helper()
	fullPath := filepath.Join(r.Path, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		r.T.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(contents), 0o644); err != nil {
		r.T.Fatal(err)
	}
	r.Run("-C", r.Path, "add", "--", path)
	date := at.Format(time.RFC3339)
	cmd := exec.Command("git", "-C", r.Path, "commit", "-m", message)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.T.Fatalf("git commit: %v\n%s", err, out)
	}
	return r.Run("-C", r.Path, "rev-parse", "HEAD")[:40]
}

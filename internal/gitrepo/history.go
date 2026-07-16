package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
)

type Range struct {
	Revision string
	Since    time.Time
	Until    time.Time
}

type Commit struct {
	Hash       string
	AuthorTime time.Time
	Subject    string
	Changes    []Change
}

type Change struct {
	Status  string
	OldPath string
	Path    string
}

func (r Repository) MergeBase(ctx context.Context, a, b string) (string, error) {
	result, err := r.runner.Run(ctx, "git", "-C", r.source.Path, "merge-base", a, b)
	if err != nil {
		return "", fmt.Errorf("merge base %s %s: %w", a, b, err)
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

func (r Repository) Commits(ctx context.Context, requested Range) ([]Commit, error) {
	revision := requested.Revision
	if revision == "" {
		revision = "HEAD"
	}
	args := []string{"-C", r.source.Path, "log", "--format=%H%x00%aI%x00%s%x00", "--name-status", "-z", "-M"}
	if !requested.Since.IsZero() {
		args = append(args, "--since="+requested.Since.Format(time.RFC3339Nano))
	}
	if !requested.Until.IsZero() {
		args = append(args, "--until="+requested.Until.Format(time.RFC3339Nano))
	}
	args = append(args, revision, "--")
	result, err := r.runner.Run(ctx, "git", args...)
	if err != nil {
		return nil, fmt.Errorf("read commits: %w", err)
	}
	commits, err := parseCommits(result.Stdout)
	if err != nil {
		return nil, fmt.Errorf("parse commits: %w", err)
	}
	return commits, nil
}

func (r Repository) Blob(ctx context.Context, revision, path string) ([]byte, error) {
	result, err := r.runner.Run(ctx, "git", "-C", r.source.Path, "show", revision+":"+path)
	if err != nil {
		return nil, fmt.Errorf("read blob %s:%s: %w", revision, path, err)
	}
	return result.Stdout, nil
}

func parseCommits(output []byte) ([]Commit, error) {
	parts := bytes.Split(output, []byte{0})
	commits := make([]Commit, 0)
	for i := 0; i < len(parts); {
		for i < len(parts) && len(parts[i]) == 0 {
			i++
		}
		if i == len(parts) {
			break
		}
		hash := strings.TrimPrefix(string(parts[i]), "\n")
		if !isObjectID(hash) || i+2 >= len(parts) {
			return nil, fmt.Errorf("malformed header")
		}
		authorTime, err := time.Parse(time.RFC3339, string(parts[i+1]))
		if err != nil {
			return nil, fmt.Errorf("author time: %w", err)
		}
		commit := Commit{Hash: hash, AuthorTime: authorTime, Subject: string(parts[i+2])}
		i += 3
		for i < len(parts) {
			if len(parts[i]) == 0 {
				i++
				continue
			}
			status := strings.TrimPrefix(string(parts[i]), "\n")
			if isObjectID(status) {
				break
			}
			i++
			if status == "" {
				return nil, fmt.Errorf("empty change status")
			}
			if i == len(parts) {
				return nil, fmt.Errorf("missing path for %s", status)
			}
			change := Change{Status: status[:1]}
			if change.Status == "R" || change.Status == "C" {
				if i+1 >= len(parts) {
					return nil, fmt.Errorf("missing rename path")
				}
				change.OldPath, change.Path = string(parts[i]), string(parts[i+1])
				i += 2
			} else {
				change.Path = string(parts[i])
				i++
			}
			commit.Changes = append(commit.Changes, change)
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

func isObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

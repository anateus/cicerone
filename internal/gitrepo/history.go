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
	args := []string{"-C", r.source.Path, "log", "--format=%x1e%H%x00%aI%x00%s", "--name-status", "-z", "-M"}
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
	records := bytes.Split(output, []byte{0x1e})
	commits := make([]Commit, 0, len(records)-1)
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		fields := bytes.SplitN(record, []byte{0}, 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed header")
		}
		authorTime, err := time.Parse(time.RFC3339, string(fields[1]))
		if err != nil {
			return nil, fmt.Errorf("author time: %w", err)
		}
		rest := bytes.TrimPrefix(fields[2], []byte{'\n'})
		parts := bytes.Split(rest, []byte{0})
		commit := Commit{Hash: string(fields[0]), AuthorTime: authorTime}
		if len(parts) > 0 {
			commit.Subject = strings.TrimSuffix(string(parts[0]), "\n")
		}
		parts = parts[1:]
		for len(parts) > 0 {
			if len(parts[0]) == 0 {
				parts = parts[1:]
				continue
			}
			status := strings.TrimPrefix(string(parts[0]), "\n")
			parts = parts[1:]
			if status == "" {
				return nil, fmt.Errorf("empty change status")
			}
			if len(parts) == 0 {
				return nil, fmt.Errorf("missing path for %s", status)
			}
			change := Change{Status: status[:1]}
			if change.Status == "R" || change.Status == "C" {
				if len(parts) < 2 {
					return nil, fmt.Errorf("missing rename path")
				}
				change.OldPath, change.Path = string(parts[0]), string(parts[1])
				parts = parts[2:]
			} else {
				change.Path = string(parts[0])
				parts = parts[1:]
			}
			commit.Changes = append(commit.Changes, change)
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

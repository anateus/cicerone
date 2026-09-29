package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
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
	var commits []Commit
	err := r.WalkCommits(ctx, requested, func(commit Commit) error {
		commits = append(commits, commit)
		return nil
	})
	return commits, err
}

func commitArgs(path string, requested Range) []string {
	revision := requested.Revision
	if revision == "" {
		revision = "HEAD"
	}
	// Homebrew records formula changes on the second parent of merge commits.
	// Walk the mainline and diff each merge against its first parent so those
	// changes are visible without traversing every pull-request branch commit.
	args := []string{"-C", path, "log", "--first-parent", "-m", "--format=%H%x00%aI%x00%s%x00", "--name-status", "-z", "-M"}
	if !requested.Since.IsZero() {
		args = append(args, "--since="+requested.Since.Format(time.RFC3339Nano))
	}
	if !requested.Until.IsZero() {
		args = append(args, "--until="+requested.Until.Format(time.RFC3339Nano))
	}
	args = append(args, revision, "--")
	return args
}

func (r Repository) WalkCommits(ctx context.Context, requested Range, yield func(Commit) error) (err error) {
	return r.walkCommitArgs(ctx, commitArgs(r.source.Path, requested), yield)
}

// PathCommits returns up to limit mainline commits changing one definition.
// The limit bounds returned matches, not how much history Git traverses when
// fewer matches exist. It does not follow renames into other paths.
func (r Repository) PathCommits(ctx context.Context, definitionPath string, limit int) ([]Commit, error) {
	if definitionPath == "" || path.IsAbs(definitionPath) || path.Clean(definitionPath) != definitionPath ||
		strings.ContainsAny(definitionPath, "\\:\x00") || strings.HasPrefix(definitionPath, "../") || definitionPath == ".." ||
		strings.HasPrefix(definitionPath, "-") || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid definition path or commit limit")
	}
	args := commitArgs(r.source.Path, Range{})
	// The pathspec follows the separator, after the revision.
	args = append(args[:len(args)-2], "--max-count="+strconv.Itoa(limit), "HEAD", "--", definitionPath)
	var commits []Commit
	err := r.walkCommitArgs(ctx, args, func(commit Commit) error {
		commits = append(commits, commit)
		return nil
	})
	return commits, err
}

// AmbiguousAddition checks a single commit's changed-path metadata when a
// path-limited log describes a change as an addition. Git's pathspec can hide
// the old side of a rename, so do not infer a new package from such a record.
func (r Repository) AmbiguousAddition(ctx context.Context, hash, definitionPath string) (bool, error) {
	if !isObjectID(hash) {
		return false, fmt.Errorf("invalid commit hash")
	}
	result, err := r.runner.Run(ctx, "git", "-C", r.source.Path, "show", "--format=", "--name-status", "-z", "-M", hash, "--")
	if err != nil {
		return false, fmt.Errorf("inspect addition %s: %w", hash, err)
	}
	fields := strings.Split(string(result.Stdout), "\x00")
	for index := 0; index < len(fields); index++ {
		status := strings.TrimPrefix(fields[index], "\n")
		if status == "" {
			continue
		}
		switch status[0] {
		case 'R', 'C':
			if index+2 >= len(fields) {
				return false, fmt.Errorf("malformed rename in %s", hash)
			}
			index += 2
			if fields[index] == definitionPath {
				return true, nil
			}
		case 'D':
			if index+1 >= len(fields) {
				return false, fmt.Errorf("malformed deletion in %s", hash)
			}
			index++
			if path.Ext(fields[index]) == path.Ext(definitionPath) {
				return true, nil
			}
		default:
			if index+1 >= len(fields) {
				return false, fmt.Errorf("malformed change in %s", hash)
			}
			index++
		}
	}
	return false, nil
}

func (r Repository) walkCommitArgs(ctx context.Context, args []string, yield func(Commit) error) (err error) {
	if yield == nil {
		return errors.New("commit callback is nil")
	}
	stream, err := r.runner.Stream(ctx, "git", args...)
	if err != nil {
		return fmt.Errorf("read commits: %w", err)
	}
	defer func() { err = errors.Join(err, stream.Close()) }()
	reader := bufio.NewReader(stream)
	next := func() (string, error) {
		for {
			token, readErr := reader.ReadString(0)
			token = strings.TrimSuffix(token, "\x00")
			if token != "" {
				return token, readErr
			}
			if readErr != nil {
				return "", readErr
			}
		}
	}
	readHeader := func(hash string) (Commit, error) {
		encodedTime, readErr := next()
		if readErr != nil {
			return Commit{}, fmt.Errorf("malformed header")
		}
		authorTime, parseErr := time.Parse(time.RFC3339, encodedTime)
		if parseErr != nil {
			return Commit{}, fmt.Errorf("author time: %w", parseErr)
		}
		subject, readErr := next()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return Commit{}, readErr
		}
		return Commit{Hash: hash, AuthorTime: authorTime, Subject: subject}, nil
	}
	hash, readErr := next()
	if errors.Is(readErr, io.EOF) && hash == "" {
		return nil
	}
	hash = strings.TrimPrefix(hash, "\n")
	if !isObjectID(hash) {
		return fmt.Errorf("parse commits: malformed header")
	}
	commit, err := readHeader(hash)
	if err != nil {
		return fmt.Errorf("parse commits: %w", err)
	}
	for {
		token, tokenErr := next()
		if errors.Is(tokenErr, io.EOF) && token == "" {
			return yield(commit)
		}
		token = strings.TrimPrefix(token, "\n")
		if isObjectID(token) {
			if err := yield(commit); err != nil {
				return err
			}
			commit, err = readHeader(token)
			if err != nil {
				return fmt.Errorf("parse commits: %w", err)
			}
			continue
		}
		if token == "" {
			return fmt.Errorf("parse commits: empty change status")
		}
		path, pathErr := next()
		if path == "" || (pathErr != nil && !errors.Is(pathErr, io.EOF)) {
			return fmt.Errorf("parse commits: missing path for %s", token)
		}
		change := Change{Status: token[:1]}
		if change.Status == "R" || change.Status == "C" {
			newPath, newPathErr := next()
			if newPath == "" || (newPathErr != nil && !errors.Is(newPathErr, io.EOF)) {
				return fmt.Errorf("parse commits: missing rename path")
			}
			change.OldPath, change.Path = path, newPath
		} else {
			change.Path = path
		}
		commit.Changes = append(commit.Changes, change)
	}
}

func (r Repository) Blob(ctx context.Context, revision, path string) ([]byte, error) {
	result, err := r.runner.Run(ctx, "git", "-C", r.source.Path, "show", revision+":"+path)
	if err != nil && ctx.Err() == nil {
		result, err = r.runner.Run(ctx, "git", "-C", r.source.Path, "show", revision+":"+path)
	}
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

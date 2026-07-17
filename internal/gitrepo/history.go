package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	args := []string{"-C", path, "log", "--format=%H%x00%aI%x00%s%x00", "--name-status", "-z", "-M"}
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
	if yield == nil {
		return errors.New("commit callback is nil")
	}
	stream, err := r.runner.Stream(ctx, "git", commitArgs(r.source.Path, requested)...)
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

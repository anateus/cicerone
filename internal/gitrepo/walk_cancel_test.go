package gitrepo_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/anateus/cicerone/internal/gitrepo"
)

type interruptedCommitStream struct {
	data   []byte
	cancel context.CancelFunc
	err    error
}

func (s *interruptedCommitStream) Read(p []byte) (int, error) {
	if len(s.data) != 0 {
		n := copy(p, s.data)
		s.data = s.data[n:]
		return n, nil
	}
	s.cancel()
	return 0, s.err
}

func (*interruptedCommitStream) Close() error { return nil }

func TestWalkCommitsNormalizesCanceledReadFailures(t *testing.T) {
	hash := strings.Repeat("a", 40)
	header := hash + "\x002026-01-01T00:00:00Z\x00subject\x00"
	for _, tc := range []struct {
		name, prefix string
	}{
		{"partial hash", "partial"},
		{"missing timestamp", hash + "\x00"},
		{"missing subject", hash + "\x002026-01-01T00:00:00Z\x00"},
		{"missing status", header},
		{"partial path", header + "M\x00Formula/partial"},
		{"missing rename destination", header + "R100\x00Formula/old.rb\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := &interruptedCommitStream{data: []byte(tc.prefix), cancel: cancel, err: errors.Join(io.ErrClosedPipe, io.ErrUnexpectedEOF)}
			repository := gitrepo.New(gitrepo.Source{Path: "/repo"}, &pipeRunner{reader: stream})
			err := repository.WalkCommits(ctx, gitrepo.Range{}, func(gitrepo.Commit) error {
				t.Fatal("yielded incomplete commit")
				return nil
			})
			if !errors.Is(err, context.Canceled) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("WalkCommits error = %v, want cancellation without pipe artifacts", err)
			}
			if strings.Contains(err.Error(), "malformed") || strings.Contains(err.Error(), "missing") {
				t.Fatalf("WalkCommits fabricated a parse failure: %v", err)
			}
		})
	}
}

func TestWalkCommitsCancellationPreservesIndependentReadError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	realErr := errors.New("independent read failure")
	stream := &interruptedCommitStream{cancel: cancel, err: errors.Join(io.ErrClosedPipe, realErr)}
	repository := gitrepo.New(gitrepo.Source{Path: "/repo"}, &pipeRunner{reader: stream})
	err := repository.WalkCommits(ctx, gitrepo.Range{}, func(gitrepo.Commit) error { return nil })
	if !errors.Is(err, context.Canceled) || !errors.Is(err, realErr) || errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("WalkCommits error = %v, want cancellation and independent read failure", err)
	}
}

func TestWalkCommitsPreservesMalformedRecordsWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repository := gitrepo.New(gitrepo.Source{Path: "/repo"}, &pipeRunner{reader: io.NopCloser(strings.NewReader("not-a-hash\x00"))})
	err := repository.WalkCommits(ctx, gitrepo.Range{}, func(gitrepo.Commit) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "malformed header") {
		t.Fatalf("WalkCommits error = %v, want independently malformed header", err)
	}
}

func TestWalkCommitsPreservesCallbackErrorAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hash := strings.Repeat("a", 40)
	data := hash + "\x002026-01-01T00:00:00Z\x00subject\x00M\x00Formula/a.rb\x00"
	repository := gitrepo.New(gitrepo.Source{Path: "/repo"}, &pipeRunner{reader: io.NopCloser(strings.NewReader(data))})
	checkpointErr := errors.New("checkpoint failed")
	err := repository.WalkCommits(ctx, gitrepo.Range{}, func(gitrepo.Commit) error {
		cancel()
		return checkpointErr
	})
	if !errors.Is(err, checkpointErr) {
		t.Fatalf("WalkCommits error = %v, want callback failure", err)
	}
}

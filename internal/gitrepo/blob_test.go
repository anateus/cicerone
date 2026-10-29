package gitrepo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/gitrepo"
	"github.com/anateus/cicerone/internal/testutil"
)

type batchRecordingRunner struct {
	execx.Runner
	starts int
	args   []string
	runs   [][]string
	writes []string
}

func (r *batchRecordingRunner) Run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	r.runs = append(r.runs, append([]string{name}, args...))
	return r.Runner.Run(ctx, name, args...)
}

func (r *batchRecordingRunner) Start(ctx context.Context, name string, args ...string) (io.ReadWriteCloser, error) {
	r.starts++
	r.args = append([]string{name}, args...)
	stream, err := r.Runner.(execx.DuplexRunner).Start(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	return &batchRecordingStream{ReadWriteCloser: stream, runner: r}, nil
}

type batchRecordingStream struct {
	io.ReadWriteCloser
	runner *batchRecordingRunner
}

func (s *batchRecordingStream) Write(p []byte) (int, error) {
	s.runner.writes = append(s.runner.writes, string(p))
	return s.ReadWriteCloser.Write(p)
}

func TestBlobReaderPreservesBytesAndArbitraryPaths(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	big := bytes.Repeat([]byte{0, 1, '\n', 0xff, '\r', 0}, 50_000)
	files := []struct {
		path string
		body []byte
	}{
		{"Formula/space and : colon.rb", []byte{0xff, 0, '\n', '\r', 1, 0, '\n'}},
		{"Formula/newline\nname\t;$(not-a-command).rb", []byte("lines\n\n")},
		{"Formula/carriage\r", []byte("carriage")},
		{"Formula/trailing space ", []byte("space")},
		{"Formula/control\x1ebyte.rb", []byte("control")},
		{"Formula/-leading.rb", nil},
		{"Formula/large.rb", big},
	}
	for _, file := range files {
		path := filepath.Join(repo.Path, file.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, file.body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo.Run("-C", repo.Path, "add", "-A")
	repo.Run("-C", repo.Path, "commit", "-m", "add unusual paths")
	hash := strings.TrimSpace(repo.Run("-C", repo.Path, "rev-parse", "HEAD"))
	runner := &batchRecordingRunner{Runner: execx.NewRunner()}
	reader, err := gitrepo.New(gitrepo.Source{Path: repo.Path}, runner).OpenBlobReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	for _, file := range files {
		got, err := reader.Blob(hash, file.path)
		if err != nil || !bytes.Equal(got, file.body) {
			t.Fatalf("Blob(%q) returned %d bytes, err=%v; want %d exact bytes", file.path, len(got), err, len(file.body))
		}
	}
	if runner.starts != 1 {
		t.Fatalf("started %d cat-file processes, want one", runner.starts)
	}
	if !strings.Contains(strings.Join(runner.args, " "), "cat-file") || strings.Contains(strings.Join(runner.args, " "), " -z") {
		t.Fatalf("unexpected batch command: %v", runner.args)
	}
	var wantWrites []string
	var wantRuns [][]string
	for _, file := range files {
		expression := hash + ":" + file.path
		query := expression
		if strings.ContainsAny(file.path, "\r\n") {
			query = strings.TrimSpace(repo.Run("-C", repo.Path, "rev-parse", "--verify", expression))
			wantRuns = append(wantRuns, []string{"git", "-C", repo.Path, "rev-parse", "--verify", expression})
		}
		wantWrites = append(wantWrites, query+"\n")
	}
	if !reflect.DeepEqual(runner.runs, wantRuns) {
		t.Fatalf("path resolution calls = %q, want %q", runner.runs, wantRuns)
	}
	if !reflect.DeepEqual(runner.writes, wantWrites) {
		t.Fatalf("batch requests = %q, want %q", runner.writes, wantWrites)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := reader.Blob(hash, files[0].path); err == nil {
		t.Fatal("Blob after Close succeeded")
	}
}

func TestBlobReaderReportsMissingAndNonBlobAfterRetry(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	hash := repo.Commit("Formula/good.rb", "contents", "add", time.Now())
	runner := &batchRecordingRunner{Runner: execx.NewRunner()}
	reader, err := gitrepo.New(gitrepo.Source{Path: repo.Path}, runner).OpenBlobReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	missing := "Formula/missing\nname.rb"
	if _, err := reader.Blob(hash, missing); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing blob error = %v", err)
	}
	if _, err := reader.Blob(hash, "Formula"); err == nil || !strings.Contains(err.Error(), "not blob") {
		t.Fatalf("tree object error = %v", err)
	}
	got, err := reader.Blob(hash, "Formula/good.rb")
	if err != nil || string(got) != "contents" {
		t.Fatalf("Blob after failures = %q, %v", got, err)
	}
	wantWrites := []string{hash + ":Formula\n", hash + ":Formula\n", hash + ":Formula/good.rb\n"}
	if runner.starts != 3 || !reflect.DeepEqual(runner.writes, wantWrites) {
		t.Fatalf("starts = %d, writes = %q; want 3 and %q", runner.starts, runner.writes, wantWrites)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("missing-path resolution calls = %q, want two attempts", runner.runs)
	}
}

type scriptedBatchRunner struct {
	stream   io.ReadWriteCloser
	streams  []io.ReadWriteCloser
	starts   int
	startErr error
}

func (r *scriptedBatchRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	return execx.Result{}, errors.New("unexpected Run")
}
func (r *scriptedBatchRunner) Stream(context.Context, string, ...string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected Stream")
}
func (r *scriptedBatchRunner) Start(context.Context, string, ...string) (io.ReadWriteCloser, error) {
	r.starts++
	if len(r.streams) != 0 {
		index := r.starts - 1
		if index >= len(r.streams) {
			return nil, errors.New("unexpected process restart")
		}
		return r.streams[index], nil
	}
	return r.stream, r.startErr
}

type scriptedBatchStream struct {
	reader   io.Reader
	written  []byte
	closeErr error
	closed   int
}

func (s *scriptedBatchStream) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *scriptedBatchStream) Write(p []byte) (int, error) {
	s.written = append(s.written, p...)
	return len(p), nil
}
func (s *scriptedBatchStream) Close() error {
	s.closed++
	return s.closeErr
}

type gatedBatchStream struct {
	response []byte
	pending  []byte
	requests int
}

func (s *gatedBatchStream) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		return 0, errors.New("response read without a request")
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *gatedBatchStream) Write(p []byte) (int, error) {
	if len(s.pending) != 0 {
		return 0, errors.New("second request sent before reading the first response")
	}
	if len(p) == 0 || p[len(p)-1] != '\n' {
		return 0, errors.New("request is not newline terminated")
	}
	s.requests++
	s.pending = s.response
	return len(p), nil
}

func (*gatedBatchStream) Close() error { return nil }

func TestBlobReaderKeepsOnlyOneResponseInFlight(t *testing.T) {
	body := bytes.Repeat([]byte("body\n"), 2_000)
	header := fmt.Sprintf("ok %s blob %d\n", strings.Repeat("a", 40), len(body))
	response := append([]byte(header), body...)
	response = append(response, '\n')
	stream := &gatedBatchStream{response: response}
	runner := &scriptedBatchRunner{stream: stream}
	reader, err := gitrepo.New(gitrepo.Source{}, runner).OpenBlobReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for n := range 100 {
		got, err := reader.Blob("HEAD", fmt.Sprintf("file-%d", n))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("request %d returned %d bytes, %v", n, len(got), err)
		}
	}
	if runner.starts != 1 || stream.requests != 100 {
		t.Fatalf("starts=%d requests=%d", runner.starts, stream.requests)
	}
}

func TestBlobReaderRejectsMalformedOrTruncatedResponses(t *testing.T) {
	oid := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name     string
		response string
	}{
		{"unexpected expression", "other missing\n"},
		{"invalid object ID", "ok nope blob 1\nx\n"},
		{"invalid size", "ok " + oid + " blob -1\nx\n"},
		{"truncated body", "ok " + oid + " blob 4\nabc"},
		{"missing terminator", "ok " + oid + " blob 1\nx"},
		{"invalid terminator", "ok " + oid + " blob 1\nx!"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &scriptedBatchStream{reader: strings.NewReader(tc.response)}
			reader, err := gitrepo.New(gitrepo.Source{Path: "/repo"}, &scriptedBatchRunner{stream: stream}).OpenBlobReader(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.Blob("HEAD", "x"); err == nil {
				t.Fatal("malformed response accepted")
			} else if tc.name == "truncated body" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("truncation error = %v, want unexpected EOF without cancellation", err)
			}
			if _, err := reader.Blob("HEAD", "x"); err == nil || !strings.Contains(err.Error(), "unusable") {
				t.Fatalf("corrupted reader reused: %v", err)
			}
			if err := reader.Close(); err != nil || stream.closed != 2 {
				t.Fatalf("Close = %v, called %d times", err, stream.closed)
			}
		})
	}
}

func TestBlobReaderRetriesTransientFailureWithFreshProcess(t *testing.T) {
	oid := strings.Repeat("a", 40)
	_, processErr := execx.NewRunner().Run(context.Background(), "git", "-C", filepath.Join(t.TempDir(), "missing"), "rev-parse", "HEAD")
	var gitFailure *execx.ProcessError
	if !errors.As(processErr, &gitFailure) {
		t.Fatalf("expected command failure, got %v", processErr)
	}
	first := &scriptedBatchStream{reader: strings.NewReader("HEAD:file missing\n"), closeErr: processErr}
	second := &scriptedBatchStream{reader: strings.NewReader("ok " + oid + " blob 4\nx\x00y\n\n")}
	runner := &scriptedBatchRunner{streams: []io.ReadWriteCloser{first, second}}
	reader, err := gitrepo.New(gitrepo.Source{}, runner).OpenBlobReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Blob("HEAD", "file")
	if err != nil || !bytes.Equal(got, []byte{'x', 0, 'y', '\n'}) {
		t.Fatalf("retried Blob = %q, %v", got, err)
	}
	if runner.starts != 2 || first.closed != 1 || string(first.written) != "HEAD:file\n" || string(second.written) != "HEAD:file\n" {
		t.Fatalf("retry starts=%d closes=%d writes=%q / %q", runner.starts, first.closed, first.written, second.written)
	}
	if err := reader.Close(); err != nil || second.closed != 1 {
		t.Fatalf("final Close = %v, called %d times", err, second.closed)
	}
}

func TestBlobReaderDoesNotHideCleanupFailureOnRetry(t *testing.T) {
	closeErr := errors.New("failed to clean up process")
	_, processErr := execx.NewRunner().Run(context.Background(), "git", "-C", filepath.Join(t.TempDir(), "missing"), "rev-parse", "HEAD")
	first := &scriptedBatchStream{reader: strings.NewReader("HEAD:file missing\n"), closeErr: errors.Join(processErr, closeErr)}
	runner := &scriptedBatchRunner{streams: []io.ReadWriteCloser{first}}
	reader, err := gitrepo.New(gitrepo.Source{}, runner).OpenBlobReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Blob("HEAD", "file"); !errors.Is(err, closeErr) {
		t.Fatalf("Blob error = %v, want cleanup failure", err)
	}
	if runner.starts != 1 || first.closed != 1 {
		t.Fatalf("unsafe retry after cleanup error: starts=%d closes=%d", runner.starts, first.closed)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func TestBlobReaderReturnsStartAndCloseErrors(t *testing.T) {
	startErr := errors.New("start failure")
	if _, err := gitrepo.New(gitrepo.Source{}, &scriptedBatchRunner{startErr: startErr}).OpenBlobReader(context.Background()); !errors.Is(err, startErr) {
		t.Fatalf("start error = %v", err)
	}
	closeErr := errors.New("close failure")
	stream := &scriptedBatchStream{reader: strings.NewReader(""), closeErr: closeErr}
	reader, err := gitrepo.New(gitrepo.Source{}, &scriptedBatchRunner{stream: stream}).OpenBlobReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close error = %v", err)
	}
	if err := reader.Close(); !errors.Is(err, closeErr) || stream.closed != 1 {
		t.Fatalf("repeated Close error = %v; closed %d times", err, stream.closed)
	}
}

type blockedBatchStream struct {
	ctx     context.Context
	reading chan struct{}
}

func (s *blockedBatchStream) Read([]byte) (int, error) {
	close(s.reading)
	<-s.ctx.Done()
	return 0, io.EOF
}
func (*blockedBatchStream) Write(p []byte) (int, error) { return len(p), nil }
func (*blockedBatchStream) Close() error                { return nil }

func TestBlobReaderCancellationInterruptsRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &blockedBatchStream{ctx: ctx, reading: make(chan struct{})}
	reader, err := gitrepo.New(gitrepo.Source{}, &scriptedBatchRunner{stream: stream}).OpenBlobReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		_, err := reader.Blob("HEAD", "file")
		done <- err
	}()
	select {
	case <-stream.reading:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
			t.Fatalf("Blob error = %v, want cancellation without EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Blob did not return after cancellation")
	}
}

type cancelOnResponseStream struct {
	*scriptedBatchStream
	cancel context.CancelFunc
}

func (s *cancelOnResponseStream) Read(p []byte) (int, error) {
	n, err := s.scriptedBatchStream.Read(p)
	s.cancel()
	return n, err
}

func TestBlobReaderCancellationDoesNotRetryMissingResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &cancelOnResponseStream{
		scriptedBatchStream: &scriptedBatchStream{reader: strings.NewReader("HEAD:file missing\n")},
		cancel:              cancel,
	}
	runner := &scriptedBatchRunner{stream: stream}
	reader, err := gitrepo.New(gitrepo.Source{}, runner).OpenBlobReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.Blob("HEAD", "file"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Blob error = %v, want cancellation", err)
	}
	if runner.starts != 1 {
		t.Fatalf("started %d processes after cancellation", runner.starts)
	}
}

type cancelOnIOStream struct {
	*scriptedBatchStream
	cancel   context.CancelFunc
	readErr  error
	writeErr error
}

func (s *cancelOnIOStream) Read(p []byte) (int, error) {
	if s.readErr != nil {
		s.cancel()
		return 0, s.readErr
	}
	return s.scriptedBatchStream.Read(p)
}

func (s *cancelOnIOStream) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		s.cancel()
		return 0, s.writeErr
	}
	return s.scriptedBatchStream.Write(p)
}

func TestBlobReaderCancellationPreservesMixedIOErrors(t *testing.T) {
	realErr := errors.New("independent read or write failure")
	for _, tc := range []struct {
		name     string
		readErr  error
		writeErr error
		wantReal bool
	}{
		{"read EOF", io.EOF, nil, false},
		{"read mixed", errors.Join(io.ErrUnexpectedEOF, os.ErrClosed, realErr), nil, true},
		{"write mixed", nil, errors.Join(io.ErrClosedPipe, syscall.EPIPE, realErr), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := &cancelOnIOStream{
				scriptedBatchStream: &scriptedBatchStream{reader: strings.NewReader("")},
				cancel:              cancel, readErr: tc.readErr, writeErr: tc.writeErr,
			}
			runner := &scriptedBatchRunner{stream: stream}
			reader, err := gitrepo.New(gitrepo.Source{}, runner).OpenBlobReader(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			_, err = reader.Blob("HEAD", "file")
			if !errors.Is(err, context.Canceled) || errors.Is(err, realErr) != tc.wantReal {
				t.Fatalf("Blob error = %v, want cancellation and real error=%v", err, tc.wantReal)
			}
			for _, pipeErr := range []error{io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, os.ErrClosed, syscall.EPIPE} {
				if errors.Is(err, pipeErr) {
					t.Errorf("Blob error retained pipe artifact %v: %v", pipeErr, err)
				}
			}
			if runner.starts != 1 {
				t.Fatalf("started %d processes after cancellation", runner.starts)
			}
		})
	}
}

func TestBlobReaderRetainsEarlierFailureWhenRetryIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oid := strings.Repeat("a", 40)
	first := &scriptedBatchStream{reader: strings.NewReader("ok " + oid + " blob 4\nabc")}
	second := &cancelOnIOStream{
		scriptedBatchStream: &scriptedBatchStream{reader: strings.NewReader("")},
		cancel:              cancel, readErr: io.EOF,
	}
	runner := &scriptedBatchRunner{streams: []io.ReadWriteCloser{first, second}}
	reader, err := gitrepo.New(gitrepo.Source{}, runner).OpenBlobReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_, err = reader.Blob("HEAD", "file")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		t.Fatalf("retry error = %v, want cancellation and earlier truncation only", err)
	}
}

func TestBlobReaderClosePreservesMixedCleanupError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	realErr := errors.New("independent cleanup failure")
	stream := &scriptedBatchStream{reader: strings.NewReader(""), closeErr: errors.Join(io.ErrClosedPipe, os.ErrClosed, syscall.EPIPE, realErr)}
	reader, err := gitrepo.New(gitrepo.Source{}, &scriptedBatchRunner{stream: stream}).OpenBlobReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	err = reader.Close()
	if !errors.Is(err, context.Canceled) || !errors.Is(err, realErr) {
		t.Fatalf("Close error = %v, want cancellation and real failure", err)
	}
	for _, pipeErr := range []error{io.ErrClosedPipe, os.ErrClosed, syscall.EPIPE} {
		if errors.Is(err, pipeErr) {
			t.Errorf("Close error retained pipe artifact %v: %v", pipeErr, err)
		}
	}
}

func TestBlobReaderReportsGitProcessFailureOnClose(t *testing.T) {
	reader, err := gitrepo.New(gitrepo.Source{Path: filepath.Join(t.TempDir(), "missing")}, execx.NewRunner()).OpenBlobReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := reader.Blob("HEAD", "file")
	closeErr := reader.Close()
	if readErr == nil || closeErr == nil || !strings.Contains(closeErr.Error(), "git") {
		t.Fatalf("read error = %v, close error = %v", readErr, closeErr)
	}
}

func TestBlobReaderRejectsInvalidRequestWithoutWriting(t *testing.T) {
	stream := &scriptedBatchStream{reader: strings.NewReader("")}
	reader, err := gitrepo.New(gitrepo.Source{}, &scriptedBatchRunner{stream: stream}).OpenBlobReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, input := range [][2]string{{"HEAD\nother", "file"}, {"-option", "file"}, {"HEAD", "file\x00other"}, {"", "file"}, {"HEAD", ""}} {
		if _, err := reader.Blob(input[0], input[1]); err == nil {
			t.Errorf("accepted invalid request %q", fmt.Sprint(input))
		}
	}
	if len(stream.written) != 0 {
		t.Fatalf("invalid requests wrote %q", stream.written)
	}
}

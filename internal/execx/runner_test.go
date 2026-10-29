package execx_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/execx"
)

func TestRunCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := execx.NewRunner().Run(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", "block")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestRunCancellationDoesNotWaitForInheritedPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := execx.NewRunner().Run(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", "spawn-pipe-holder")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Run() cancellation took %v while a descendant held its pipes", elapsed)
	}
}

func TestRunFailureIncludesCommandExitCodeAndBoundedStderr(t *testing.T) {
	args := []string{"-test.run=TestHelperProcess", "--", "fail", "literal;not-a-shell-command"}
	_, err := execx.NewRunner().Run(context.Background(), os.Args[0], args...)
	if err == nil {
		t.Fatal("Run() error = nil, want process failure")
	}
	message := err.Error()
	for _, want := range []string{os.Args[0], strings.Join(args, " "), "exit code 17", "stderr-start"} {
		if !strings.Contains(message, want) {
			t.Errorf("Run() error = %q, want substring %q", message, want)
		}
	}
	if len(message) > 5000 {
		t.Errorf("Run() error length = %d, want bounded stderr", len(message))
	}
	if strings.Contains(message, "stderr-end") {
		t.Errorf("Run() error contains unbounded stderr tail: %q", message)
	}
}

func TestStreamStartsProcessAndReturnsOutput(t *testing.T) {
	stream, err := execx.NewRunner().Stream(context.Background(), os.Args[0], "-test.run=TestHelperProcess", "--", "output")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if string(got) != "stream-output" {
		t.Errorf("stream output = %q, want %q", got, "stream-output")
	}
}

func TestStreamCloseReportsNonzeroExitAndBoundedStderr(t *testing.T) {
	args := []string{"-test.run=TestHelperProcess", "--", "fail", "literal;not-a-shell-command"}
	stream, err := execx.NewRunner().Stream(context.Background(), os.Args[0], args...)
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	err = stream.Close()
	if err == nil {
		t.Fatal("Close() error = nil, want process failure")
	}
	message := err.Error()
	for _, want := range []string{os.Args[0], strings.Join(args, " "), "exit code 17", "stderr-start"} {
		if !strings.Contains(message, want) {
			t.Errorf("Close() error = %q, want substring %q", message, want)
		}
	}
	if len(message) > 5000 || strings.Contains(message, "stderr-end") {
		t.Errorf("Close() error contains unbounded stderr: %q", message)
	}
}

func TestStreamCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := execx.NewRunner().Stream(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", "ready-block")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	reader := bufio.NewReader(stream)
	if ready, err := reader.ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("read readiness = %q, %v", ready, err)
	}
	cancel()
	_, _ = io.ReadAll(reader)
	if err := stream.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want context.Canceled", err)
	}
}

func TestStreamCancellationUnblocksInheritedPipe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := execx.NewRunner().Stream(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", "ready-spawn-pipe-holder")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stream)
	if ready, err := reader.ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("read readiness = %q, %v", ready, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := reader.ReadByte()
		done <- err
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read remained blocked after cancellation")
	}
	if err := stream.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context.Canceled", err)
	}
}

func TestStreamCancellationPreservesIndependentProcessFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := execx.NewRunner().Stream(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", "fail")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(stream)
	cancel()
	err = stream.Close()
	var exitErr *exec.ExitError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
		t.Fatalf("Close error = %v, want cancellation and exit code 17", err)
	}
}

func TestStreamCloseWaitsForProcess(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "exited")
	stream, err := execx.NewRunner().Stream(context.Background(), os.Args[0], "-test.run=TestHelperProcess", "--", "mark-exit", marker)
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	reader := bufio.NewReader(stream)
	if ready, err := reader.ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("read readiness = %q, %v", ready, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker after Close() = %v, want process exit marker", err)
	}
}

func TestDuplexExchangesRequestsAndWaitsForExit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "duplex-exited")
	stream, err := execx.NewRunner().(execx.DuplexRunner).Start(context.Background(), os.Args[0], "-test.run=TestHelperProcess", "--", "duplex", marker)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stream)
	for _, request := range []string{"one\n", "two\n"} {
		if _, err := io.WriteString(stream, request); err != nil {
			t.Fatal(err)
		}
		got, err := reader.ReadString('\n')
		if err != nil || got != "reply:"+request {
			t.Fatalf("response = %q, %v", got, err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("child was not reaped after EOF: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("repeated Close = %v", err)
	}
}

func TestDuplexCancellationUnblocksInheritedPipe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := execx.NewRunner().(execx.DuplexRunner).Start(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", "ready-spawn-pipe-holder")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stream)
	if ready, err := reader.ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("read readiness = %q, %v", ready, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := reader.ReadByte()
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
			t.Fatalf("read error = %v, want cancellation without pipe artifacts", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read remained blocked after cancellation")
	}
	if err := stream.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context.Canceled", err)
	}
}

func TestDuplexCancellationUnblocksWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := execx.NewRunner().(execx.DuplexRunner).Start(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", "ready-block")
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := bufio.NewReader(stream).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("read readiness = %q, %v", ready, err)
	}
	payload := bytes.Repeat([]byte("x"), 2<<20)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := stream.Write(payload)
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("write returned before cancellation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE) {
			t.Fatalf("write error = %v, want cancellation without pipe artifacts", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write remained blocked after cancellation")
	}
	if err := stream.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context.Canceled", err)
	}
}

func TestCancellationErrorFiltersOnlyCanceledPipeLeaves(t *testing.T) {
	realErr := errors.New("independent failure")
	mixed := fmt.Errorf("read: %w", errors.Join(io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, os.ErrClosed, syscall.EPIPE, realErr))
	if got := execx.CancellationError(context.Background(), mixed); got != mixed {
		t.Fatalf("uncanceled error changed: %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := execx.CancellationError(ctx, mixed)
	if !errors.Is(got, context.Canceled) || !errors.Is(got, realErr) {
		t.Fatalf("canceled mixed error = %v, want cancellation and real failure", got)
	}
	for _, pipeErr := range []error{io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, os.ErrClosed, syscall.EPIPE} {
		if errors.Is(got, pipeErr) {
			t.Errorf("canceled mixed error retained %v: %v", pipeErr, got)
		}
	}
}

func TestDuplexStartReturnsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := execx.NewRunner().(execx.DuplexRunner).Start(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", "block")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v, want context.Canceled", err)
	}
}

func TestDuplexCloseStopsUncooperativeProcess(t *testing.T) {
	stream, err := execx.NewRunner().(execx.DuplexRunner).Start(context.Background(), os.Args[0], "-test.run=TestHelperProcess", "--", "ready-block")
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := bufio.NewReader(stream).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("read readiness = %q, %v", ready, err)
	}
	start := time.Now()
	if err := stream.Close(); err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("Close error = %v, want forced exit", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
}

func TestDuplexCloseReportsFailureAndBoundedStderr(t *testing.T) {
	stream, err := execx.NewRunner().(execx.DuplexRunner).Start(context.Background(), os.Args[0], "-test.run=TestHelperProcess", "--", "fail")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(stream)
	err = stream.Close()
	if err == nil || !strings.Contains(err.Error(), "exit code 17") || !strings.Contains(err.Error(), "stderr-start") {
		t.Fatalf("Close error = %v", err)
	}
	if len(err.Error()) > 5000 || strings.Contains(err.Error(), "stderr-end") {
		t.Fatalf("Close error has unbounded stderr: %v", err)
	}
}

func TestDuplexCancellationPreservesIndependentProcessFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := execx.NewRunner().(execx.DuplexRunner).Start(ctx, os.Args[0], "-test.run=TestHelperProcess", "--", "fail")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(stream)
	cancel()
	err = stream.Close()
	var exitErr *exec.ExitError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
		t.Fatalf("Close error = %v, want cancellation and exit code 17", err)
	}
}

func TestHelperProcess(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == -1 || separator+1 >= len(os.Args) {
		return
	}

	switch os.Args[separator+1] {
	case "block":
		time.Sleep(time.Hour)
	case "ready-block":
		fmt.Fprintln(os.Stdout, "ready")
		time.Sleep(time.Hour)
	case "spawn-pipe-holder":
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "short-block")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		time.Sleep(time.Hour)
	case "ready-spawn-pipe-holder":
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "short-block")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		fmt.Fprintln(os.Stdout, "ready")
		time.Sleep(time.Hour)
	case "duplex":
		reader := bufio.NewReader(os.Stdin)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if !errors.Is(err, io.EOF) {
					os.Exit(5)
				}
				break
			}
			fmt.Fprint(os.Stdout, "reply:", line)
		}
		// A successful child may legitimately finish later than WaitDelay.
		time.Sleep(400 * time.Millisecond)
		if err := os.WriteFile(os.Args[separator+2], []byte("exited"), 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	case "short-block":
		time.Sleep(2 * time.Second)
	case "output":
		fmt.Fprint(os.Stdout, "stream-output")
		os.Exit(0)
	case "mark-exit":
		fmt.Fprintln(os.Stdout, "ready")
		time.Sleep(20 * time.Millisecond)
		if err := os.WriteFile(os.Args[separator+2], []byte("exited"), 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	case "fail":
		fmt.Fprint(os.Stderr, "stderr-start:")
		fmt.Fprint(os.Stderr, strings.Repeat("x", 8*1024))
		fmt.Fprint(os.Stderr, ":stderr-end")
		os.Exit(17)
	default:
		os.Exit(2)
	}
}

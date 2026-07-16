package execx_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cicerone/internal/execx"
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

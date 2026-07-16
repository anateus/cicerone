package execx_test

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	case "fail":
		fmt.Fprint(os.Stderr, "stderr-start:")
		fmt.Fprint(os.Stderr, strings.Repeat("x", 8*1024))
		fmt.Fprint(os.Stderr, ":stderr-end")
		os.Exit(17)
	default:
		os.Exit(2)
	}
}

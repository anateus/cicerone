package homebrew

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anateus/cicerone/internal/domain"
)

func TestActionArgumentsAndValidation(t *testing.T) {
	tests := []struct {
		name    string
		action  Action
		want    []string
		wantErr bool
	}{
		{"formula install", Action{Kind: Install, Packages: []domain.PackageID{"homebrew/core/ripgrep"}, Type: domain.PackageFormula}, []string{"install", "--formula", "homebrew/core/ripgrep"}, false},
		{"cask install", Action{Kind: Install, Packages: []domain.PackageID{"firefox@developer-edition"}, Type: domain.PackageCask}, []string{"install", "--cask", "firefox@developer-edition"}, false},
		{"formula upgrade", Action{Kind: Upgrade, Packages: []domain.PackageID{"gcc+lib_1.2"}, Type: domain.PackageFormula}, []string{"upgrade", "--formula", "gcc+lib_1.2"}, false},
		{"cask upgrade", Action{Kind: Upgrade, Packages: []domain.PackageID{"font.test"}, Type: domain.PackageCask}, []string{"upgrade", "--cask", "font.test"}, false},
		{"leading dash", Action{Kind: Install, Packages: []domain.PackageID{"--help"}, Type: domain.PackageFormula}, nil, true},
		{"space", Action{Kind: Install, Packages: []domain.PackageID{"bad name"}, Type: domain.PackageFormula}, nil, true},
		{"semicolon", Action{Kind: Install, Packages: []domain.PackageID{"bad;name"}, Type: domain.PackageFormula}, nil, true},
		{"empty", Action{Kind: Install, Packages: []domain.PackageID{""}, Type: domain.PackageFormula}, nil, true},
		{"no packages", Action{Kind: Install, Type: domain.PackageFormula}, nil, true},
		{"several formulae", Action{Kind: Install, Packages: []domain.PackageID{"ripgrep", "fd", "bat"}, Type: domain.PackageFormula}, []string{"install", "--formula", "ripgrep", "fd", "bat"}, false},
		{"one bad name poisons the batch", Action{Kind: Upgrade, Packages: []domain.PackageID{"ok", "--force"}, Type: domain.PackageFormula}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotName string
			var gotArgs []string
			client := NewClient(nil)
			client.commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				gotName, gotArgs = name, append([]string(nil), args...)
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestActionHelperProcess", "--", "success")
				cmd.Env = append(os.Environ(), "GO_WANT_ACTION_HELPER=1")
				return cmd
			}
			err := client.RunAction(context.Background(), tt.action, io.Discard)
			if tt.wantErr {
				if err == nil {
					t.Fatal("RunAction error = nil")
				}
				if gotName != "" {
					t.Fatalf("invalid action executed %q", gotName)
				}
				return
			}
			if err != nil {
				t.Fatalf("RunAction error = %v", err)
			}
			if gotName != "brew" || !reflect.DeepEqual(gotArgs, tt.want) {
				t.Fatalf("command = %q %#v, want brew %#v", gotName, gotArgs, tt.want)
			}
		})
	}
}

func TestPlanActionsGroupsByVerbAndTypeInStableOrder(t *testing.T) {
	got := PlanActions([]ActionTarget{
		{Package: "firefox", Type: domain.PackageCask},
		{Package: "git", Type: domain.PackageFormula, Installed: true},
		{Package: "ripgrep", Type: domain.PackageFormula},
		{Package: "iterm2", Type: domain.PackageCask, Installed: true},
		{Package: "fd", Type: domain.PackageFormula},
		{Package: "ripgrep", Type: domain.PackageFormula},
	})
	want := []Action{
		{Kind: Install, Type: domain.PackageFormula, Packages: []domain.PackageID{"ripgrep", "fd"}},
		{Kind: Upgrade, Type: domain.PackageFormula, Packages: []domain.PackageID{"git"}},
		{Kind: Install, Type: domain.PackageCask, Packages: []domain.PackageID{"firefox"}},
		{Kind: Upgrade, Type: domain.PackageCask, Packages: []domain.PackageID{"iterm2"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %#v\nwant %#v", got, want)
	}
	if PlanActions(nil) != nil {
		t.Fatal("empty targets produced actions")
	}
}

func TestRunActionStreamsBothOutputsIntoBoundedRetainedOutput(t *testing.T) {
	client := NewClient(nil)
	client.commandContext = helperCommand("large-output")
	output := NewRetainedOutput()
	if err := client.RunAction(context.Background(), Action{Kind: Install, Packages: []domain.PackageID{"ok"}, Type: domain.PackageFormula}, output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if len(got) > 1<<20 {
		t.Fatalf("retained %d bytes, want <= 1 MiB", len(got))
	}
	if !strings.Contains(got, "stdout-tail") || !strings.Contains(got, "stderr-tail") {
		t.Fatalf("missing stream tails from retained output: length %d", len(got))
	}
}

func TestRetainedOutputKeepsOnlyNewestMiB(t *testing.T) {
	output := NewRetainedOutput()
	_, _ = output.Write([]byte(strings.Repeat("x", (1<<20)+100)))
	if got := len(output.Bytes()); got != 1<<20 {
		t.Fatalf("retained %d bytes", got)
	}
}

func TestRunActionCancellationInterruptsChild(t *testing.T) {
	client := NewClient(nil)
	client.commandContext = helperCommand("interrupt")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	err := client.RunAction(ctx, Action{Kind: Upgrade, Packages: []domain.PackageID{"ok"}, Type: domain.PackageCask}, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
}

func TestRunActionKillsAndReapsChildAfterGracePeriod(t *testing.T) {
	client := NewClient(nil)
	client.cancelGrace = 10 * time.Millisecond
	var cmd *exec.Cmd
	client.commandContext = func(context.Context, string, ...string) *exec.Cmd {
		cmd = helperCommand("ignore-interrupt")(context.Background(), "brew")
		return cmd
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	err := client.RunAction(ctx, Action{Kind: Upgrade, Packages: []domain.PackageID{"ok"}, Type: domain.PackageCask}, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("kill fallback took %s", elapsed)
	}
	if cmd.ProcessState == nil {
		t.Fatal("killed child was not reaped")
	}
}

func helperCommand(mode string) func(context.Context, string, ...string) *exec.Cmd {
	return func(context.Context, string, ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestActionHelperProcess", "--", mode)
		cmd.Env = append(os.Environ(), "GO_WANT_ACTION_HELPER=1")
		return cmd
	}
}

func TestActionHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ACTION_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "success":
		os.Exit(0)
	case "large-output":
		var writes sync.WaitGroup
		writes.Add(2)
		go func() { defer writes.Done(); _, _ = io.WriteString(os.Stdout, strings.Repeat("o", 700<<10)) }()
		go func() { defer writes.Done(); _, _ = io.WriteString(os.Stderr, strings.Repeat("e", 700<<10)) }()
		writes.Wait()
		_, _ = io.WriteString(os.Stdout, "stdout-tail")
		_, _ = io.WriteString(os.Stderr, "stderr-tail")
		os.Exit(0)
	case "interrupt":
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		<-interrupt
		os.Exit(0)
	case "ignore-interrupt":
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		for {
			<-interrupt
		}
	}
	os.Exit(2)
}

package homebrew

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/anateus/cicerone/internal/domain"
)

type ActionKind string

const (
	Install ActionKind = "install"
	Upgrade ActionKind = "upgrade"
)

// Action is one brew invocation. Every package shares the kind and type so a
// single `brew install --formula a b c` covers them.
type Action struct {
	Kind     ActionKind
	Packages []domain.PackageID
	Type     domain.PackageType
}

// ActionTarget is a package the user asked to install or upgrade.
type ActionTarget struct {
	Package   domain.PackageID
	Type      domain.PackageType
	Installed bool
}

// PlanActions splits targets into the fewest brew invocations: brew takes one
// verb and one --formula/--cask flag per call, so installs and upgrades of
// formulae and casks each get their own action. Order is stable (formula
// installs, formula upgrades, cask installs, cask upgrades) and duplicates
// are dropped.
func PlanActions(targets []ActionTarget) []Action {
	order := []struct {
		kind ActionKind
		typ  domain.PackageType
	}{
		{Install, domain.PackageFormula}, {Upgrade, domain.PackageFormula},
		{Install, domain.PackageCask}, {Upgrade, domain.PackageCask},
	}
	seen := make(map[domain.PackageID]bool, len(targets))
	var actions []Action
	for _, slot := range order {
		action := Action{Kind: slot.kind, Type: slot.typ}
		for _, target := range targets {
			kind := Install
			if target.Installed {
				kind = Upgrade
			}
			if kind != slot.kind || target.Type != slot.typ || seen[target.Package] {
				continue
			}
			seen[target.Package] = true
			action.Packages = append(action.Packages, target.Package)
		}
		if len(action.Packages) > 0 {
			actions = append(actions, action)
		}
	}
	return actions
}

var actionPackageNamePattern = regexp.MustCompile(`^[A-Za-z0-9@+_.\-/]+$`)

func actionArgs(action Action) ([]string, error) {
	if len(action.Packages) == 0 {
		return nil, fmt.Errorf("Homebrew action has no packages")
	}
	names := make([]string, 0, len(action.Packages))
	for _, packageID := range action.Packages {
		name := string(packageID)
		if name == "" || name[0] == '-' || !actionPackageNamePattern.MatchString(name) {
			return nil, fmt.Errorf("invalid Homebrew package name %q", name)
		}
		names = append(names, name)
	}
	if action.Kind != Install && action.Kind != Upgrade {
		return nil, fmt.Errorf("invalid Homebrew action %q", action.Kind)
	}
	var flag string
	switch action.Type {
	case domain.PackageFormula:
		flag = "--formula"
	case domain.PackageCask:
		flag = "--cask"
	default:
		return nil, fmt.Errorf("invalid Homebrew package type %q", action.Type)
	}
	return append([]string{string(action.Kind), flag}, names...), nil
}

func (c *Client) RunAction(ctx context.Context, action Action, output io.Writer) error {
	args, err := actionArgs(action)
	if err != nil {
		return err
	}
	cmd := c.commandContext(ctx, "brew", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("capture brew stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("capture brew stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start brew: %w", err)
	}

	w := &synchronizedWriter{writer: output}
	var copies sync.WaitGroup
	copies.Add(2)
	go func() { defer copies.Done(); _, _ = io.Copy(w, stdout) }()
	go func() { defer copies.Done(); _, _ = io.Copy(w, stderr) }()
	done := make(chan error, 1)
	go func() { copies.Wait(); done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		if waitErr != nil {
			return fmt.Errorf("brew %s failed: %w", action.Kind, waitErr)
		}
		return nil
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt)
		}
		timer := time.NewTimer(c.cancelGrace)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		}
		return ctx.Err()
	}
}

type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writer == nil {
		return len(p), nil
	}
	return w.writer.Write(p)
}

const retainedOutputLimit = 1 << 20

type RetainedOutput struct {
	mu   sync.RWMutex
	data []byte
}

func NewRetainedOutput() *RetainedOutput {
	return &RetainedOutput{data: make([]byte, 0, retainedOutputLimit)}
}
func (r *RetainedOutput) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if len(p) >= retainedOutputLimit {
		r.data = append(r.data[:0], p[len(p)-retainedOutputLimit:]...)
		return n, nil
	}
	overflow := len(r.data) + len(p) - retainedOutputLimit
	if overflow > 0 {
		copy(r.data, r.data[overflow:])
		r.data = r.data[:len(r.data)-overflow]
	}
	r.data = append(r.data, p...)
	return n, nil
}
func (r *RetainedOutput) String() string { r.mu.RLock(); defer r.mu.RUnlock(); return string(r.data) }
func (r *RetainedOutput) Bytes() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]byte(nil), r.data...)
}

var _ io.Writer = (*RetainedOutput)(nil)

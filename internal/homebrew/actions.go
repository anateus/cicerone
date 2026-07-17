package homebrew

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"time"

	"cicerone/internal/domain"
)

type ActionKind string

const (
	Install ActionKind = "install"
	Upgrade ActionKind = "upgrade"
)

type Action struct {
	Kind    ActionKind
	Package domain.PackageID
	Type    domain.PackageType
}

var actionPackageNamePattern = regexp.MustCompile(`^[A-Za-z0-9@+_.\-/]+$`)

func actionArgs(action Action) ([]string, error) {
	name := string(action.Package)
	if name == "" || name[0] == '-' || !actionPackageNamePattern.MatchString(name) {
		return nil, fmt.Errorf("invalid Homebrew package name %q", name)
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
	return []string{string(action.Kind), flag, name}, nil
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

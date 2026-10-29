package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Result struct {
	Stdout []byte
	Stderr []byte
}

type Runner interface {
	Run(ctx context.Context, name string, args ...string) (Result, error)
	Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error)
}

// DuplexRunner starts a process whose stdin and stdout remain available until
// the returned stream is closed. Close closes both pipes and reaps the child.
type DuplexRunner interface {
	Start(ctx context.Context, name string, args ...string) (io.ReadWriteCloser, error)
}

const (
	maxStderrBytes      = 4 * 1024
	processWaitDelay    = 250 * time.Millisecond
	processCloseTimeout = 5 * time.Second
)

type runner struct{}

func NewRunner() Runner {
	return runner{}
}

func (runner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = processWaitDelay
	var stdout bytes.Buffer
	stderr := &limitedBuffer{remaining: maxStderrBytes}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	result := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, fmt.Errorf("run %s %s: %w", name, strings.Join(args, " "), ctxErr)
	}
	return result, processError(name, args, err, stderr.String())
}

func (runner) Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = processWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe stdout for %s: %w", name, err)
	}
	stderr := &limitedBuffer{remaining: maxStderrBytes}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(processError(name, args, err, stderr.String()), closePipe(stdout))
	}
	reader := &processReader{ReadCloser: stdout, cmd: cmd, ctx: ctx, name: name, args: append([]string(nil), args...), stderr: stderr}
	reader.stopCancel = context.AfterFunc(ctx, func() { _ = stdout.Close() })
	return reader, nil
}

func (runner) Start(ctx context.Context, name string, args ...string) (io.ReadWriteCloser, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = processWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe stdin for %s: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("pipe stdout for %s: %w", name, err), closePipe(stdin), closePipe(cmd.Stdin.(io.Closer)))
	}
	stderr := &limitedBuffer{remaining: maxStderrBytes}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		stdinErr := closePipe(stdin)
		stdoutErr := closePipe(stdout)
		return nil, errors.Join(processError(name, args, err, stderr.String()), stdinErr, stdoutErr)
	}
	p := &processDuplex{
		stdin: stdin, stdout: stdout, cmd: cmd, ctx: ctx,
		name: name, args: append([]string(nil), args...), stderr: stderr,
	}
	// A descendant may inherit the pipes even after CommandContext kills the
	// child. Close our ends as well so a blocked Read or Write can return.
	p.stopCancel = context.AfterFunc(ctx, func() {
		_ = stdin.Close()
		_ = stdout.Close()
	})
	return p, nil
}

// ProcessError reports a failed command separately from pipe cleanup errors.
type ProcessError struct {
	message string
	err     error
}

func (e *ProcessError) Error() string { return e.message }
func (e *ProcessError) Unwrap() error { return e.err }

func processError(name string, args []string, err error, stderr string) error {
	exitCode := -1
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}
	return &ProcessError{message: fmt.Sprintf("run %s %s: exit code %d: %v; stderr: %s", name, strings.Join(args, " "), exitCode, err, stderr), err: err}
}

func canceledProcessExit(err error) bool {
	if err == nil || err == context.Canceled || err == context.DeadlineExceeded {
		return true
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// CancellationError classifies an error from context-owned I/O. If the context
// was canceled, it removes pipe-closure artifacts while preserving every other
// error, including non-pipe errors joined with those artifacts.
func CancellationError(ctx context.Context, err error) error {
	ctxErr := ctx.Err()
	if ctxErr == nil || err == nil {
		return err
	}
	filtered, _ := stripPipeErrors(err, true)
	if filtered == nil {
		return ctxErr
	}
	if errors.Is(filtered, ctxErr) {
		return filtered
	}
	return errors.Join(ctxErr, filtered)
}

func stripPipeErrors(err error, canceled bool) (error, bool) {
	if err == nil {
		return nil, false
	}
	if err == os.ErrClosed || (canceled && (err == io.EOF || err == io.ErrUnexpectedEOF || err == io.ErrClosedPipe || err == syscall.EPIPE)) {
		return nil, true
	}
	// Process failures are not pipe artifacts, even when they wrap an I/O
	// error. Preserve them rather than interpreting their cause in isolation.
	if _, ok := err.(*ProcessError); ok {
		return err, false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var parts []error
		changed := false
		for _, part := range joined.Unwrap() {
			filtered, removed := stripPipeErrors(part, canceled)
			changed = changed || removed
			if filtered != nil {
				parts = append(parts, filtered)
			}
		}
		if changed {
			return errors.Join(parts...), true
		}
		return err, false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		filtered, changed := stripPipeErrors(wrapped.Unwrap(), canceled)
		if changed {
			return filtered, true
		}
	}
	return err, false
}

func closePipe(pipe io.Closer) error {
	err, _ := stripPipeErrors(pipe.Close(), false)
	return err
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	remaining int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	originalLen := len(p)
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	_, _ = b.buffer.Write(p)
	b.remaining -= len(p)
	return originalLen, nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buffer.Bytes() }

func (b *limitedBuffer) String() string { return b.buffer.String() }

type processReader struct {
	io.ReadCloser
	cmd        *exec.Cmd
	ctx        context.Context
	name       string
	args       []string
	stderr     *limitedBuffer
	stopCancel func() bool
}

func (r *processReader) Close() error {
	r.stopCancel()
	closeErr := closePipe(r.ReadCloser)
	waitErr := r.cmd.Wait()
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		closeErr, _ = stripPipeErrors(closeErr, true)
		if !canceledProcessExit(waitErr) {
			waitErr = processError(r.name, r.args, waitErr, r.stderr.String())
		} else {
			waitErr = nil
		}
		return errors.Join(fmt.Errorf("stream %s %s: %w", r.name, strings.Join(r.args, " "), ctxErr), closeErr, waitErr)
	}
	if waitErr != nil {
		return errors.Join(processError(r.name, r.args, waitErr, r.stderr.String()), closeErr)
	}
	return closeErr
}

type processDuplex struct {
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	cmd        *exec.Cmd
	ctx        context.Context
	name       string
	args       []string
	stderr     *limitedBuffer
	stopCancel func() bool
	closeOnce  sync.Once
	closeErr   error
}

func (p *processDuplex) Read(b []byte) (int, error) {
	n, err := p.stdout.Read(b)
	return n, CancellationError(p.ctx, err)
}

func (p *processDuplex) Write(b []byte) (int, error) {
	n, err := p.stdin.Write(b)
	return n, CancellationError(p.ctx, err)
}

func (p *processDuplex) Close() error {
	p.closeOnce.Do(func() {
		p.stopCancel()
		stdinErr := closePipe(p.stdin)
		stdoutErr := closePipe(p.stdout)

		wait := make(chan error, 1)
		go func() { wait <- p.cmd.Wait() }()
		// WaitDelay bounds inherited pipes after exit or cancellation; an
		// otherwise healthy child can need longer to shut down after EOF.
		timer := time.NewTimer(processCloseTimeout)
		defer timer.Stop()
		var waitErr error
		var forcedErr error
		select {
		case waitErr = <-wait:
		case <-timer.C:
			killErr := p.cmd.Process.Kill()
			waitErr = <-wait
			if killErr == nil {
				forcedErr = fmt.Errorf("process %s did not exit after closing its pipes", p.name)
			} else if !errors.Is(killErr, os.ErrProcessDone) {
				forcedErr = fmt.Errorf("stop process %s: %w", p.name, killErr)
			}
		}
		if ctxErr := p.ctx.Err(); ctxErr != nil {
			stdinErr, _ = stripPipeErrors(stdinErr, true)
			stdoutErr, _ = stripPipeErrors(stdoutErr, true)
			if !canceledProcessExit(waitErr) {
				waitErr = processError(p.name, p.args, waitErr, p.stderr.String())
			} else {
				waitErr = nil
			}
			p.closeErr = errors.Join(ctxErr, stdinErr, stdoutErr, forcedErr, waitErr)
			return
		}
		if waitErr != nil {
			waitErr = processError(p.name, p.args, waitErr, p.stderr.String())
		}
		p.closeErr = errors.Join(stdinErr, stdoutErr, forcedErr, waitErr)
	})
	return p.closeErr
}

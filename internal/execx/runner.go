package execx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

type Result struct {
	Stdout []byte
	Stderr []byte
}

type Runner interface {
	Run(ctx context.Context, name string, args ...string) (Result, error)
	Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error)
}

const maxStderrBytes = 4 * 1024

type runner struct{}

func NewRunner() Runner {
	return runner{}
}

func (runner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe stdout for %s: %w", name, err)
	}
	stderr := &limitedBuffer{remaining: maxStderrBytes}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, processError(name, args, err, stderr.String())
	}
	return &processReader{ReadCloser: stdout, cmd: cmd, ctx: ctx, name: name, args: append([]string(nil), args...), stderr: stderr}, nil
}

func processError(name string, args []string, err error, stderr string) error {
	exitCode := -1
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}
	return fmt.Errorf("run %s %s: exit code %d: %w; stderr: %s", name, strings.Join(args, " "), exitCode, err, stderr)
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
	cmd    *exec.Cmd
	ctx    context.Context
	name   string
	args   []string
	stderr *limitedBuffer
}

func (r *processReader) Close() error {
	closeErr := r.ReadCloser.Close()
	waitErr := r.cmd.Wait()
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return fmt.Errorf("stream %s %s: %w", r.name, strings.Join(r.args, " "), ctxErr)
	}
	if waitErr != nil {
		return processError(r.name, r.args, waitErr, r.stderr.String())
	}
	return closeErr
}

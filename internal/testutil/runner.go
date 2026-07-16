package testutil

import (
	"context"
	"io"

	"cicerone/internal/execx"
)

type Call struct {
	Name string
	Args []string
}

type Runner struct {
	RunResult execx.Result
	RunErr    error
	RunCalls  []Call
}

func (r *Runner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	r.RunCalls = append(r.RunCalls, Call{Name: name, Args: append([]string(nil), args...)})
	return r.RunResult, r.RunErr
}

func (r *Runner) Stream(context.Context, string, ...string) (io.ReadCloser, error) {
	panic("unexpected Stream call")
}

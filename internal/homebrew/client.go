package homebrew

import (
	"context"
	"os/exec"
	"time"

	"cicerone/internal/execx"
)

type Client struct {
	runner         execx.Runner
	commandContext func(context.Context, string, ...string) *exec.Cmd
	cancelGrace    time.Duration
}

func NewClient(runner execx.Runner) *Client {
	return &Client{runner: runner, commandContext: func(_ context.Context, name string, args ...string) *exec.Cmd {
		return exec.Command(name, args...)
	}, cancelGrace: 2 * time.Second}
}

package homebrew

import "cicerone/internal/execx"

type Client struct {
	runner execx.Runner
}

func NewClient(runner execx.Runner) *Client {
	return &Client{runner: runner}
}

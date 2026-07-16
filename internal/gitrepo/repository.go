package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cicerone/internal/execx"
)

type Source struct {
	Kind      string
	Name      string
	Path      string
	RemoteURL string
	Owned     bool
}

type Repository struct {
	source Source
	runner execx.Runner
}

func New(source Source, runner execx.Runner) Repository {
	return Repository{source: source, runner: runner}
}

func (r Repository) Ensure(ctx context.Context) error {
	if !r.source.Owned {
		return nil
	}
	if _, err := os.Stat(r.source.Path); err == nil {
		if err := r.validateOwned(ctx); err != nil {
			return fmt.Errorf("owned cache %s is incomplete or unexpected: %w; move it aside or remove it after inspection, then retry", r.source.Path, err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect repository %s: %w", r.source.Path, err)
	}
	if err := os.MkdirAll(filepath.Dir(r.source.Path), 0o755); err != nil {
		return fmt.Errorf("create repository cache: %w", err)
	}
	_, err := r.runner.Run(ctx, "git", "clone", "--mirror", "--filter=blob:none", "--", r.source.RemoteURL, r.source.Path)
	if err != nil {
		return fmt.Errorf("clone %s: %w", r.source.Name, err)
	}
	return nil
}

func (r Repository) validateOwned(ctx context.Context) error {
	result, err := r.runner.Run(ctx, "git", "-C", r.source.Path, "rev-parse", "--is-bare-repository")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(result.Stdout)) != "true" {
		return errors.New("repository is not a bare mirror")
	}
	result, err = r.runner.Run(ctx, "git", "-C", r.source.Path, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(result.Stdout)) != r.source.RemoteURL {
		return fmt.Errorf("origin does not match %s", r.source.RemoteURL)
	}
	result, err = r.runner.Run(ctx, "git", "-C", r.source.Path, "config", "--get", "remote.origin.mirror")
	if err != nil {
		return fmt.Errorf("read mirror setting: %w", err)
	}
	if strings.TrimSpace(string(result.Stdout)) != "true" {
		return errors.New("repository lacks remote.origin.mirror=true")
	}
	result, err = r.runner.Run(ctx, "git", "-C", r.source.Path, "config", "--get-all", "remote.origin.fetch")
	if err != nil {
		return fmt.Errorf("read mirror fetch refspec: %w", err)
	}
	if strings.TrimSpace(string(result.Stdout)) != "+refs/*:refs/*" {
		return errors.New("repository lacks the mirror fetch refspec")
	}
	if _, err := r.runner.Run(ctx, "git", "-C", r.source.Path, "rev-parse", "--verify", "HEAD"); err != nil {
		return fmt.Errorf("repository has no usable HEAD: %w", err)
	}
	return nil
}

func (r Repository) Fetch(ctx context.Context) error {
	if !r.source.Owned {
		return fmt.Errorf("refuse to fetch user-owned repository %s", r.source.Path)
	}
	_, err := r.runner.Run(ctx, "git", "-C", r.source.Path, "fetch", "--prune")
	if err != nil {
		return fmt.Errorf("fetch %s: %w", r.source.Name, err)
	}
	return nil
}

func (r Repository) Head(ctx context.Context) (string, error) {
	result, err := r.runner.Run(ctx, "git", "-C", r.source.Path, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

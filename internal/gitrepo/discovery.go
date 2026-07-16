package gitrepo

import (
	"context"
	"path/filepath"
	"strings"

	"cicerone/internal/execx"
)

type sourceDefinition struct {
	kind, name, tap, remote string
}

var sourceDefinitions = []sourceDefinition{
	{kind: "formula", name: "homebrew-core", tap: "homebrew/core", remote: "https://github.com/Homebrew/homebrew-core.git"},
	{kind: "cask", name: "homebrew-cask", tap: "homebrew/cask", remote: "https://github.com/Homebrew/homebrew-cask.git"},
}

func Discover(ctx context.Context, brewPrefix, cacheDir string, runner execx.Runner) ([]Source, error) {
	sources := make([]Source, 0, len(sourceDefinitions))
	for _, definition := range sourceDefinitions {
		path := discoverLocal(ctx, brewPrefix, definition, runner)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owned := path == ""
		if owned {
			path = filepath.Join(cacheDir, definition.name+".git")
		}
		sources = append(sources, Source{Kind: definition.kind, Name: definition.name, Path: path, RemoteURL: definition.remote, Owned: owned})
	}
	return sources, nil
}

func discoverLocal(ctx context.Context, prefix string, definition sourceDefinition, runner execx.Runner) string {
	candidates := []string{
		filepath.Join(prefix, "Library", "Taps", "homebrew", definition.name),
		filepath.Join(prefix, "Homebrew", "Library", "Taps", "homebrew", definition.name),
	}
	for _, candidate := range candidates {
		if validRepository(ctx, candidate, runner) {
			return candidate
		}
	}
	result, err := runner.Run(ctx, "brew", "--repository", definition.tap)
	if err != nil {
		return ""
	}
	candidate := strings.TrimSpace(string(result.Stdout))
	if validRepository(ctx, candidate, runner) {
		return candidate
	}
	return ""
}

func validRepository(ctx context.Context, path string, runner execx.Runner) bool {
	if path == "" {
		return false
	}
	_, err := runner.Run(ctx, "git", "-C", path, "rev-parse", "--git-dir")
	return err == nil
}

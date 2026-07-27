package upstream

import (
	"context"
	"errors"
	"sort"
	"strings"

	"cicerone/internal/execx"
)

func RepositoryTags(ctx context.Context, repositoryURL string, runner execx.Runner) ([]string, error) {
	if runner == nil {
		return nil, errors.New("repository tag runner is nil")
	}
	result, err := runner.Run(ctx, "git", "ls-remote", "--tags", "--refs", repositoryURL)
	if err != nil {
		return nil, err
	}
	unique := make(map[string]struct{})
	for line := range strings.Lines(string(result.Stdout)) {
		_, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		tag, ok := strings.CutPrefix(ref, "refs/tags/")
		if ok && tag != "" {
			unique[tag] = struct{}{}
		}
	}
	tags := make([]string, 0, len(unique))
	for tag := range unique {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags, nil
}

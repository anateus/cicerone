package homebrew

import (
	"context"
	"fmt"
	"strings"

	"cicerone/internal/domain"
)

func (c *Client) SearchDescriptions(ctx context.Context, query string) ([]domain.PackageID, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	result, err := c.runner.Run(ctx, "brew", "search", "--desc", query)
	if err != nil {
		return nil, fmt.Errorf("search Homebrew descriptions: %w", err)
	}
	var matches []domain.PackageID
	seen := make(map[domain.PackageID]bool)
	for line := range strings.Lines(string(result.Stdout)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "==>") {
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || !packageNamePattern.MatchString(name) {
			continue
		}
		packageID := domain.PackageID(name)
		if !seen[packageID] {
			seen[packageID] = true
			matches = append(matches, packageID)
		}
	}
	return matches, nil
}

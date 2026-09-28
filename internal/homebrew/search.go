package homebrew

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anateus/cicerone/internal/domain"
)

// CatalogMatch is a Homebrew search hit, including packages outside the update feed.
type CatalogMatch = domain.CatalogPackage

// SearchCatalog supplements the local history with Homebrew's full package catalog.
// Homebrew does not label the two sections of a name search, so search each type
// separately. Description search does label its sections and includes descriptions.
func (c *Client) SearchCatalog(ctx context.Context, query string, descriptions bool) ([]CatalogMatch, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	var matches []CatalogMatch
	seen := make(map[domain.PackageID]int)
	add := func(id domain.PackageID, kind domain.PackageType, description string) {
		if index, ok := seen[id]; ok {
			if description != "" && matches[index].Type == kind {
				matches[index].Description = description
			}
			return
		}
		seen[id] = len(matches)
		matches = append(matches, CatalogMatch{ID: id, Type: kind, Description: description})
	}
	var failures []error
	for _, kind := range []domain.PackageType{domain.PackageFormula, domain.PackageCask} {
		result, err := c.runner.Run(ctx, "brew", "search", "--"+string(kind), query)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for line := range strings.Lines(string(result.Stdout)) {
			name := strings.TrimSpace(line)
			if packageNamePattern.MatchString(name) {
				add(domain.PackageID(name), kind, "")
			}
		}
	}
	if descriptions {
		result, err := c.runner.Run(ctx, "brew", "search", "--desc", query)
		if err != nil {
			failures = append(failures, err)
		} else {
			var kind domain.PackageType
			for line := range strings.Lines(string(result.Stdout)) {
				line = strings.TrimSpace(line)
				switch line {
				case "==> Formulae":
					kind = domain.PackageFormula
					continue
				case "==> Casks":
					kind = domain.PackageCask
					continue
				}
				name, description, ok := strings.Cut(line, ":")
				name = strings.TrimSpace(name)
				if ok && kind != "" && packageNamePattern.MatchString(name) {
					add(domain.PackageID(name), kind, strings.TrimSpace(description))
				}
			}
		}
	}
	// A missing formula or cask is a normal search result (brew exits 1).
	// Return partial matches if another search succeeded.
	if len(matches) > 0 {
		return matches, nil
	}
	if len(failures) > 0 {
		return nil, fmt.Errorf("search Homebrew catalog: %w", errors.Join(failures...))
	}
	return nil, nil
}

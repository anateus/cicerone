package homebrew

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"cicerone/internal/domain"
)

var packageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@+._-]*(/[A-Za-z0-9][A-Za-z0-9@+._-]*)*$`)

type installedInfo struct {
	Formulae []struct {
		Name      string `json:"name"`
		FullName  string `json:"full_name"`
		Installed []struct {
			Version string `json:"version"`
		} `json:"installed"`
		Pinned   bool `json:"pinned"`
		Outdated bool `json:"outdated"`
	} `json:"formulae"`
	Casks []struct {
		Token     string `json:"token"`
		FullToken string `json:"full_token"`
		Installed string `json:"installed"`
		Outdated  bool   `json:"outdated"`
	} `json:"casks"`
}

func (c *Client) Installed(ctx context.Context) ([]domain.InstalledPackage, error) {
	result, err := c.runner.Run(ctx, "brew", "info", "--json=v2", "--installed")
	if err != nil {
		return nil, fmt.Errorf("read Homebrew installed state: %w", err)
	}

	var info installedInfo
	if err := json.Unmarshal(result.Stdout, &info); err != nil {
		return nil, fmt.Errorf("decode Homebrew installed state: %w", err)
	}

	packages := make([]domain.InstalledPackage, 0, len(info.Formulae)+len(info.Casks))
	for _, formula := range info.Formulae {
		if !packageNamePattern.MatchString(formula.FullName) {
			return nil, fmt.Errorf("malformed package name %q", formula.FullName)
		}
		version := ""
		if len(formula.Installed) > 0 {
			version = formula.Installed[len(formula.Installed)-1].Version
		}
		packages = append(packages, domain.InstalledPackage{
			PackageID: domain.PackageID(formula.FullName), Name: formula.FullName,
			Version: version, Type: domain.PackageFormula, Pinned: formula.Pinned,
			UpgradeAvailable: formula.Outdated,
		})
	}
	for _, cask := range info.Casks {
		if !packageNamePattern.MatchString(cask.FullToken) {
			return nil, fmt.Errorf("malformed package name %q", cask.FullToken)
		}
		packages = append(packages, domain.InstalledPackage{
			PackageID: domain.PackageID(cask.FullToken), Name: cask.FullToken,
			Version: cask.Installed, Type: domain.PackageCask,
			UpgradeAvailable: cask.Outdated,
		})
	}
	return packages, nil
}

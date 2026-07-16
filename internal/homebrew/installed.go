package homebrew

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

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
		for _, installed := range formula.Installed {
			if version == "" || compareVersions(installed.Version, version) > 0 {
				version = installed.Version
			}
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

type versionPart struct {
	value   string
	numeric bool
}

func compareVersions(left, right string) int {
	leftParts := splitVersion(left)
	rightParts := splitVersion(right)
	common := min(len(leftParts), len(rightParts))
	for i := range common {
		if comparison := compareVersionParts(leftParts[i], rightParts[i]); comparison != 0 {
			return comparison
		}
	}
	return compareVersionRemainder(leftParts[common:], rightParts[common:])
}

func splitVersion(version string) []versionPart {
	var parts []versionPart
	for start := 0; start < len(version); {
		if !isVersionCharacter(rune(version[start])) {
			start++
			continue
		}
		numeric := unicode.IsDigit(rune(version[start]))
		end := start + 1
		for end < len(version) && isVersionCharacter(rune(version[end])) && unicode.IsDigit(rune(version[end])) == numeric {
			end++
		}
		parts = append(parts, versionPart{value: strings.ToLower(version[start:end]), numeric: numeric})
		start = end
	}
	return parts
}

func isVersionCharacter(character rune) bool {
	return unicode.IsDigit(character) || unicode.IsLetter(character)
}

func compareVersionParts(left, right versionPart) int {
	if left.numeric && right.numeric {
		leftNumber := strings.TrimLeft(left.value, "0")
		rightNumber := strings.TrimLeft(right.value, "0")
		if len(leftNumber) != len(rightNumber) {
			return sign(len(leftNumber) - len(rightNumber))
		}
		return strings.Compare(leftNumber, rightNumber)
	}
	if left.numeric {
		return 1
	}
	if right.numeric {
		return -1
	}
	return strings.Compare(left.value, right.value)
}

func compareVersionRemainder(left, right []versionPart) int {
	if len(left) == 0 && len(right) == 0 {
		return 0
	}
	if len(left) == 0 {
		return -remainderWeight(right)
	}
	return remainderWeight(left)
}

func remainderWeight(parts []versionPart) int {
	for _, part := range parts {
		if !part.numeric {
			switch part.value {
			case "a", "alpha", "b", "beta", "pre", "preview", "rc":
				return -1
			default:
				return 1
			}
		}
		if strings.Trim(part.value, "0") != "" {
			return 1
		}
	}
	return 0
}

func sign(value int) int {
	if value < 0 {
		return -1
	}
	if value > 0 {
		return 1
	}
	return 0
}

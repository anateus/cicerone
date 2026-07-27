package homebrew

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type PackageInfo struct {
	Name, FullName, Description, Homepage, License string
	StableVersion, InstalledVersion, Caveats       string
	Dependencies, BuildDependencies                []string
	Cask                                           bool
}

type packageInfoResponse struct {
	Formulae []struct {
		Name                             string
		FullName                         string `json:"full_name"`
		Desc, Homepage, License, Caveats string
		Versions                         struct {
			Stable string `json:"stable"`
		} `json:"versions"`
		Installed []struct {
			Version string `json:"version"`
		} `json:"installed"`
		Dependencies      []string `json:"dependencies"`
		BuildDependencies []string `json:"build_dependencies"`
	} `json:"formulae"`
	Casks []struct {
		Token                       string
		FullToken                   string `json:"full_token"`
		Name                        stringList
		Desc, Homepage              string
		Version, Installed, Caveats string
	} `json:"casks"`
}

type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	var values []string
	if len(bytes.TrimSpace(data)) > 0 && bytes.TrimSpace(data)[0] == '[' {
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		*s = values
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = []string{value}
	return nil
}

func (c *Client) Info(ctx context.Context, packageName string) (PackageInfo, []byte, error) {
	if !packageNamePattern.MatchString(packageName) {
		return PackageInfo{}, nil, fmt.Errorf("malformed package name %q", packageName)
	}
	result, err := c.runner.Run(ctx, "brew", "info", "--json=v2", packageName)
	if err != nil {
		return PackageInfo{}, nil, fmt.Errorf("read Homebrew package info: %w", err)
	}
	var response packageInfoResponse
	if err := json.Unmarshal(result.Stdout, &response); err != nil {
		return PackageInfo{}, nil, fmt.Errorf("decode Homebrew package info: %w", err)
	}
	if len(response.Formulae) == 1 && len(response.Casks) == 0 {
		formula := response.Formulae[0]
		info := PackageInfo{
			Name: formula.Name, FullName: formula.FullName, Description: formula.Desc,
			Homepage: formula.Homepage, License: formula.License, StableVersion: formula.Versions.Stable,
			Caveats: formula.Caveats, Dependencies: formula.Dependencies, BuildDependencies: formula.BuildDependencies,
		}
		for _, installed := range formula.Installed {
			if info.InstalledVersion == "" || compareVersions(installed.Version, info.InstalledVersion) > 0 {
				info.InstalledVersion = installed.Version
			}
		}
		return info, append([]byte(nil), result.Stdout...), nil
	}
	if len(response.Casks) == 1 && len(response.Formulae) == 0 {
		cask := response.Casks[0]
		name := cask.Token
		if len(cask.Name) > 0 && cask.Name[0] != "" {
			name = cask.Name[0]
		}
		return PackageInfo{Name: name, FullName: cask.FullToken, Description: cask.Desc, Homepage: cask.Homepage,
				StableVersion: cask.Version, InstalledVersion: cask.Installed, Caveats: cask.Caveats, Cask: true},
			append([]byte(nil), result.Stdout...), nil
	}
	return PackageInfo{}, nil, errors.New("Homebrew package info did not contain exactly one matching package")
}

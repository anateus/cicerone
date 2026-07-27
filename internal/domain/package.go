package domain

import "strings"

// PackageID is the stable identity of a Homebrew package.
type PackageID string

// PackageType distinguishes Homebrew formulae from casks.
type PackageType string

const (
	PackageFormula PackageType = "formula"
	PackageCask    PackageType = "cask"
)

// InstalledPackage describes the locally installed state of a package.
type InstalledPackage struct {
	PackageID        PackageID
	Name             string
	Version          string
	Type             PackageType
	Pinned           bool
	UpgradeAvailable bool
}

// CleanVersion removes archive filename extensions that are not part of a
// package's semantic version.
func CleanVersion(version string) string {
	lower := strings.ToLower(version)
	for _, suffix := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tgz", ".tar"} {
		if strings.HasSuffix(lower, suffix) {
			return version[:len(version)-len(suffix)]
		}
	}
	return version
}

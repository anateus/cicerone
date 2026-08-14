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

// PackageStatus is the user-assigned state of a package in the feed.
type PackageStatus string

const (
	PackageStatusDefault PackageStatus = "default"
	PackageStatusStarred PackageStatus = "starred"
	PackageStatusSnoozed PackageStatus = "snoozed"
)

// NextPackageStatus returns the next state in the user-facing status cycle.
func NextPackageStatus(status PackageStatus) PackageStatus {
	switch status {
	case PackageStatusStarred:
		return PackageStatusSnoozed
	case PackageStatusSnoozed:
		return PackageStatusDefault
	default:
		return PackageStatusStarred
	}
}

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

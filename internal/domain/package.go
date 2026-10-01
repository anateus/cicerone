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

// PackageStatus is the user-assigned marker state of a package in the feed.
// Starred packages surface in the built-in Starred group; hidden packages are
// excluded from every group view.
type PackageStatus string

const (
	PackageStatusDefault PackageStatus = "default"
	PackageStatusStarred PackageStatus = "starred"
	PackageStatusHidden  PackageStatus = "hidden"
)

// PackageGroupID is the stable identity of a user-defined package group.
type PackageGroupID int64

// PackageGroup is a user-defined collection of packages with a manual order.
type PackageGroup struct {
	ID    PackageGroupID
	Name  string
	Index int
}

// GroupScope selects which slice of packages a feed view shows.
type GroupScope uint8

const (
	// GroupScopeAll shows every package except hidden ones.
	GroupScopeAll GroupScope = iota
	// GroupScopeUngrouped shows only packages with no group assigned.
	GroupScopeUngrouped
	// GroupScopeStarred shows only starred packages.
	GroupScopeStarred
	// GroupScopeHidden shows only hidden packages.
	GroupScopeHidden
	// GroupScopeUser shows only packages assigned to one user group.
	GroupScopeUser
)

// MatchesGroupScope reports whether a package row belongs to the scope.
func MatchesGroupScope(scope GroupScope, groupID PackageGroupID, status PackageStatus, target PackageGroupID) bool {
	switch scope {
	case GroupScopeUngrouped:
		return groupID == 0 && status != PackageStatusHidden
	case GroupScopeStarred:
		return status == PackageStatusStarred && status != PackageStatusHidden
	case GroupScopeHidden:
		return status == PackageStatusHidden
	case GroupScopeUser:
		return groupID == target && status != PackageStatusHidden
	default:
		return status != PackageStatusHidden
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

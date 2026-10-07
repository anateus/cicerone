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

// PackageGroupID is the stable identity of a user-defined package group.
type PackageGroupID int64

// GroupKind distinguishes the built-in Starred and Hidden groups from groups
// the user creates. Membership works the same for every kind; hidden members
// are additionally left out of the All view.
type GroupKind string

const (
	GroupKindUser    GroupKind = "user"
	GroupKindStarred GroupKind = "starred"
	GroupKindHidden  GroupKind = "hidden"
)

// Builtin reports whether the group ships with Cicerone and cannot be removed.
func (k GroupKind) Builtin() bool { return k == GroupKindStarred || k == GroupKindHidden }

// PackageGroup is a collection of packages. Members counts the packages
// currently assigned to it.
type PackageGroup struct {
	ID      PackageGroupID
	Name    string
	Index   int
	Kind    GroupKind
	Members int
}

// GroupScope selects which slice of packages a feed view shows.
type GroupScope uint8

const (
	// GroupScopeAll shows every package except hidden ones.
	GroupScopeAll GroupScope = iota
	// GroupScopeUngrouped shows only packages with no group assigned.
	GroupScopeUngrouped
	// Values 2 and 3 were the Starred and Hidden scopes before those became
	// groups. They stay reserved so saved preferences keep their meaning.
	_
	_
	// GroupScopeUser shows only packages assigned to one group (GroupTarget).
	GroupScopeUser
)

// MatchesGroupScope reports whether a package row belongs to the scope.
// groupKind is the kind of the package's group, or "" when it has none.
func MatchesGroupScope(scope GroupScope, groupID PackageGroupID, groupKind GroupKind, target PackageGroupID) bool {
	switch scope {
	case GroupScopeUngrouped:
		return groupID == 0
	case GroupScopeUser:
		return groupID != 0 && groupID == target
	default:
		return groupKind != GroupKindHidden
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

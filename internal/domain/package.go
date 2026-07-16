package domain

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

package history

import (
	"path/filepath"
	"regexp"
	"strings"

	"cicerone/internal/domain"
)

// Definition is the conservative, non-executing view of a Homebrew definition.
type Definition struct {
	Name, FullName, Version, Revision string
	Type                              domain.PackageType
	Homepage, URL                     string
}

var (
	quotedToken = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`(?m)^\s*` + name + `\s+(["'])([^"']*)["']\s*(?:#.*)?$`)
	}
	revisionToken      = regexp.MustCompile(`(?m)^\s*revision\s+(\d+)\s*(?:#.*)?$`)
	caskNameToken      = regexp.MustCompile(`(?m)^\s*cask\s+(["'])([^"']+)["']\s+do\s*(?:#.*)?$`)
	latestToken        = regexp.MustCompile(`(?m)^\s*version\s+:latest\s*(?:#.*)?$`)
	headToken          = regexp.MustCompile(`(?m)^\s*head\s+(["'])([^"']+)["'](?:\s*,.*)?$`)
	unsupportedVersion = regexp.MustCompile(`(?m)^\s*version\s+[^"':\s].*$`)
	versionLine        = regexp.MustCompile(`(?m)^\s*version\s+(.+?)\s*(?:#.*)?$`)
	urlVersion         = regexp.MustCompile(`(?i)(?:^|[-_/])v?(\d+(?:\.\d+)+(?:[-._][0-9A-Za-z]+)?)`)
)

// ParseDefinition recognizes only line-anchored literal Ruby tokens. It never evaluates Ruby.
func ParseDefinition(path string, content []byte) (*Definition, []string) {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	typ := domain.PackageFormula
	if strings.Contains(filepath.ToSlash(path), "/Casks/") || strings.HasPrefix(filepath.ToSlash(path), "Casks/") {
		typ = domain.PackageCask
	}
	d := &Definition{Name: name, FullName: name, Type: typ}
	text := string(content)
	if m := caskNameToken.FindStringSubmatch(text); len(m) > 0 {
		d.Name = m[2]
		d.FullName = m[2]
		d.Type = domain.PackageCask
	}
	d.Homepage = quotedValue(quotedToken("homepage"), text)
	d.URL = quotedValue(quotedToken("url"), text)
	d.Version = quotedValue(quotedToken("version"), text)
	if strings.Contains(d.Version, "#{") {
		d.Version = ""
	}
	if d.Version == "" && d.Type == domain.PackageFormula {
		if m := urlVersion.FindStringSubmatch(d.URL); len(m) > 0 {
			d.Version = m[1]
		}
	}
	if d.Version == "" && latestToken.MatchString(text) {
		d.Version = "latest"
	}
	if d.Version == "" && headToken.MatchString(text) {
		d.Version = "HEAD"
	}
	if m := revisionToken.FindStringSubmatch(text); len(m) > 0 {
		d.Revision = m[1]
	}
	var diagnostics []string
	if d.Version == "" && (unsupportedVersion.MatchString(text) || versionLine.MatchString(text)) {
		diagnostics = append(diagnostics, "unsupported computed version expression")
	}
	return d, diagnostics
}

func quotedValue(re *regexp.Regexp, text string) string {
	if m := re.FindStringSubmatch(text); len(m) > 0 {
		return m[2]
	}
	return ""
}

package tui

import (
	"fmt"
	"image/color"
	"strings"

	"cicerone/internal/domain"
	"cicerone/internal/store"
	"github.com/charmbracelet/x/ansi"
)

func (m Model) renderInspector(width int) string {
	var b strings.Builder
	b.WriteString(m.titleLine(" Inspector", width))
	b.WriteByte('\n')
	if len(m.groups) == 0 {
		b.WriteString(m.contentLine(" Select an update to inspect.", width))
		return b.String()
	}
	e := m.selectedEvent()
	name := e.Name
	if m.packageInfo.Name != "" {
		name = m.packageInfo.Name
	}
	p := m.palette()
	b.WriteString(m.inspectorRule("╭", "PACKAGE · "+packageTypeLabel(e.Type), "╮", width, p.raisedBG))
	b.WriteByte('\n')
	b.WriteString(m.inspectorLine(name, width, p.raisedBG))
	b.WriteByte('\n')
	for _, line := range []string{m.packageInfo.Description} {
		if line == "" {
			continue
		}
		b.WriteString(m.inspectorLine(line, width, p.raisedBG))
		b.WriteByte('\n')
	}
	b.WriteString(m.inspectorLine(eventKindTitle(e.Kind)+" update", width, p.raisedBG))
	b.WriteByte('\n')
	b.WriteString(m.inspectorLine(m.versionTransition(e), width, p.raisedBG))
	b.WriteByte('\n')
	if m.packageInfo.StableVersion != "" {
		b.WriteString(m.inspectorLine(fmt.Sprintf("Installed  %s", m.packageInfo.InstalledVersion), width, p.raisedBG))
		b.WriteByte('\n')
		b.WriteString(m.inspectorLine(fmt.Sprintf("Latest     %s", m.packageInfo.StableVersion), width, p.raisedBG))
		b.WriteByte('\n')
	}
	if m.packageInfo.License != "" {
		b.WriteString(m.inspectorLine("License    "+m.packageInfo.License, width, p.raisedBG))
		b.WriteByte('\n')
	}
	if m.packageInfo.Homepage != "" {
		b.WriteString(m.inspectorLine("Homepage   "+m.packageInfo.Homepage, width, p.raisedBG))
		b.WriteByte('\n')
	}
	if len(m.repositoryTags) > 0 {
		tags := ansi.Wordwrap("Tags       "+strings.Join(m.repositoryTags, ", "), max(1, width-2), "")
		lines := strings.Split(tags, "\n")
		if len(lines) > 3 {
			if m.repositoryTagsExpanded {
				lines = append(lines, "           t collapse")
			} else {
				lines = append(lines[:2], "           … more · t expand")
			}
		}
		for _, line := range lines {
			b.WriteString(m.inspectorLine(line, width, p.raisedBG))
			b.WriteByte('\n')
		}
	}
	if m.repositoryTagsErr != nil {
		b.WriteString(m.inspectorLine("Repository tags stale: "+m.repositoryTagsErr.Error(), width, p.raisedBG))
		b.WriteByte('\n')
	}
	if m.packageInfoErr != nil {
		b.WriteString(m.inspectorLine("Package info stale: "+m.packageInfoErr.Error(), width, p.raisedBG))
		b.WriteByte('\n')
	}
	b.WriteString(m.inspectorRule("├", "DOCUMENTS · [ / ] switch", "┤", width, p.tabBG))
	b.WriteByte('\n')
	activeDocument := 0
	if m.document == store.DocumentChangelog {
		activeDocument = 1
	}
	documentTabs := m.tabStrip([]string{"README", "CHANGELOG"}, activeDocument, max(1, width-2), p.tabBG, "")
	for _, row := range documentTabs {
		b.WriteString(m.surfaceLine("│"+row+"│", width, p.tabBG))
		b.WriteByte('\n')
	}
	documentTitle := "README"
	if m.document == store.DocumentChangelog {
		documentTitle = "Changelog"
	}
	b.WriteString(m.inspectorLine(strings.ToUpper(documentTitle), width, p.recessedBG))
	b.WriteByte('\n')
	documentLines := make([]string, 0, 16)
	if m.document == store.DocumentREADME {
		if m.readme.ID == "" {
			if m.readmeErr != nil {
				documentLines = append(documentLines, "README unavailable: "+m.readmeErr.Error())
			} else {
				documentLines = append(documentLines, "Waiting for README…")
			}
		} else {
			if m.readmeErr != nil {
				documentLines = append(documentLines, "README stale: "+m.readmeErr.Error())
			}
			if m.readme.SourceURL != "" {
				documentLines = append(documentLines, "Source: "+m.readme.SourceURL)
			}
			documentLines = append(documentLines, strings.Split(renderMarkdown(string(m.readme.Extracted), max(1, width-2), m.light), "\n")...)
		}
	} else if len(m.changelog) == 0 {
		if m.changelogErr != nil {
			documentLines = append(documentLines, "Changelog unavailable: "+m.changelogErr.Error())
		} else if m.changelogLoading {
			documentLines = append(documentLines, "Loading changelog…")
		} else {
			documentLines = append(documentLines, "No changelog found.")
		}
	} else {
		for _, section := range m.changelog {
			documentLines = append(documentLines, section.Version)
			if section.SourceURL != "" {
				documentLines = append(documentLines, "Source: "+section.SourceURL)
			}
			documentLines = append(documentLines, strings.Split(renderMarkdown(section.Body, max(1, width-2), m.light), "\n")...)
		}
		if m.changelogMoreLoading {
			documentLines = append(documentLines, "Loading 10 more releases…")
		} else if m.changelogNextPage > 0 {
			if m.changelogMoreErr != nil {
				documentLines = append(documentLines, "Older releases unavailable: "+m.changelogMoreErr.Error())
			}
			documentLines = append(documentLines, "m load 10 more releases")
		}
	}

	availableRows := max(4, m.height-statusHeight-strings.Count(b.String(), "\n")-2)
	for len(documentLines) < availableRows {
		documentLines = append(documentLines, "")
	}
	for _, line := range documentLines {
		b.WriteString(m.inspectorLine(line, width, p.recessedBG))
		b.WriteByte('\n')
	}
	b.WriteString(m.inspectorRule("╰", "", "╯", width, p.recessedBG))
	return strings.TrimSuffix(b.String(), "\n")
}

func (m Model) inspectorLine(text string, width int, background color.Color) string {
	return m.surfaceLine("│"+fitANSI(text, max(0, width-2))+"│", width, background)
}

func (m Model) inspectorRule(left, label, right string, width int, background color.Color) string {
	middle := ""
	if label != "" {
		middle = " " + label + " "
	}
	middle += strings.Repeat("─", max(0, width-2-ansi.StringWidth(middle)))
	return m.surfaceLine(left+middle+right, width, background)
}

func packageTypeLabel(packageType domain.PackageType) string {
	if packageType == domain.PackageCask {
		return "CASK"
	}
	return "FORMULA"
}

func eventKindTitle(kind domain.EventKind) string {
	switch kind {
	case domain.EventRevision:
		return "Revision"
	case domain.EventMetadata:
		return "Metadata"
	default:
		return "Version"
	}
}

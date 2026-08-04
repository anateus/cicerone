package tui

import (
	"fmt"
	"image/color"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"
	"cicerone/internal/domain"
	"github.com/charmbracelet/x/ansi"
)

const narrowBreakpoint = 100
const statusHeight = 1
const feedHeaderHeight = 5
const freshnessWarningAfter = 24 * time.Hour

func (m Model) render() string {
	w, h := m.width, m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	status := m.statusText()
	var body string
	p := m.palette()
	deemphasizeCore := w >= narrowBreakpoint && m.focus == inspectorPane
	if w < narrowBreakpoint {
		if m.detailOpen {
			body = paintBackground(viewportContent(m.inspectorViewport, m.renderInspector(w), w, h-statusHeight, m.inspectorViewport.YOffset()), w, p.inspectorBG)
		} else {
			body = m.renderPinnedFeed(w, h-statusHeight)
		}
	} else {
		left, right := m.paneWidths()
		feed := m.renderPinnedFeed(left, h-statusHeight)
		if deemphasizeCore {
			feed = deemphasizeANSI(feed, m.light)
		}
		inspector := viewportContent(m.inspectorViewport, m.renderInspector(right), right, h-statusHeight, m.inspectorViewport.YOffset())
		inspector = paintBackground(inspector, right, p.inspectorBG)
		body = joinColumns(feed, inspector, left, right, lipgloss.NewStyle().Background(p.inspectorBG).Render(" "))
	}
	if modal := m.renderActionModal(); modal != "" {
		body = modal + "\n" + body
	}
	lines := strings.Split(body, "\n")
	maxBody := h - statusHeight
	if len(lines) > maxBody {
		lines = lines[:maxBody]
	}
	for len(lines) < maxBody {
		lines = append(lines, m.surfaceLine("", w, p.canvasBG))
	}
	statusLine := m.statusLine(status, w)
	if deemphasizeCore {
		statusLine = deemphasizeANSI(statusLine, m.light)
	}
	return strings.Join(lines, "\n") + "\n" + statusLine
}

func (m Model) statusText() string {
	status := m.notification
	if status == "" {
		if m.stale {
			status = "Refreshing — showing previous results"
		} else if m.loading {
			status = "Loading…"
		} else {
			status = "Ready"
		}
	}
	if m.detailProgress.Active > 0 || m.detailProgress.Pending > 0 {
		detail := fmt.Sprintf("details: %d active · %d queued", m.detailProgress.Active, m.detailProgress.Pending)
		if status == "Ready" {
			status = detail
		} else {
			status += " · " + detail
		}
	}
	return status
}

func (m Model) renderPinnedFeed(width, height int) string {
	header := paintBackground(m.renderFeedHeader(width), width, m.palette().feedBG)
	listHeight := max(1, height-m.feedHeaderRows())
	total := m.feedLineCount(width)
	offset := min(m.viewportOffset, max(0, total-listHeight))
	rows, total := m.renderVisibleFeedRows(width, offset, listHeight)
	p := m.palette()
	rows = withScrollbar(rows, width, listHeight, offset, total, p.scrollTrack, p.scrollThumb, p.feedBG)
	return header + "\n" + paintBackground(rows, width, m.palette().feedBG)
}

func viewportContent(v viewport.Model, content string, width, height, offset int) string {
	v.SetWidth(width)
	v.SetHeight(height)
	v.SetContent(content)
	v.SetYOffset(offset)
	return v.View()
}

func lineCountForViewport(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

func withScrollbar(content string, width, height, offset, total int, track, thumbColor, background color.Color) string {
	if width < 2 || height < 1 || total <= height {
		return content
	}
	lines := strings.Split(content, "\n")
	thumb := 0
	if total > height {
		thumb = offset * (height - 1) / (total - height)
	}
	for index, line := range lines {
		glyph := lipgloss.NewStyle().Foreground(track).Background(background).Render("▏")
		if index == thumb {
			glyph = lipgloss.NewStyle().Foreground(thumbColor).Background(background).Render("▐")
		}
		lines[index] = fit(ansi.Truncate(line, width-1, ""), width-1) + glyph
	}
	return strings.Join(lines, "\n")
}

func (m *Model) syncViewports() {
	w, h := m.width, m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	feedWidth, inspectorWidth := w, w
	if w >= narrowBreakpoint {
		feedWidth, inspectorWidth = m.paneWidths()
	}
	m.feedViewport.SetWidth(feedWidth)
	m.feedViewport.SetHeight(max(1, h-statusHeight-m.feedHeaderRows()))
	m.feedViewport.SetContent(strings.Repeat("\n", max(0, m.feedLineCount(feedWidth)-1)))
	m.feedViewport.SetYOffset(m.viewportOffset)
	m.viewportOffset = m.feedViewport.YOffset()
	m.inspectorViewport.SetWidth(inspectorWidth)
	m.inspectorViewport.SetHeight(h - statusHeight)
	m.inspectorViewport.SetContent(m.renderInspector(inspectorWidth))
}

func (m Model) paneWidths() (feed, inspector int) {
	width := m.width
	if width <= 0 {
		width = 80
	}
	feed = width * 52 / 100
	if m.focus == inspectorPane {
		feed = width * 42 / 100
	}
	return feed, width - feed - 1
}

func (m *Model) keepSelectionVisible() {
	line := m.selectedFeedLine()
	top, height := m.feedViewport.YOffset(), m.feedViewport.Height()
	if line < top {
		top = line
	}
	rowBottom := line + feedRowHeight(m.feedViewport.Width()) - 1
	if rowBottom >= top+height {
		top = rowBottom - height + 1
	}
	m.feedViewport.SetYOffset(top)
	m.viewportOffset = m.feedViewport.YOffset()
}

func (m Model) selectedFeedLine() int {
	line := m.selected * feedRowHeight(m.feedViewport.Width())
	for index := 0; index < m.selected && index < len(m.groups); index++ {
		group := m.groups[index]
		if m.expanded[group.ID] && len(group.Events) > 1 {
			line += len(group.Events) - 1
		}
	}
	if boundary := m.seenBoundary(); m.hasSeenSeparator() && boundary <= m.selected {
		line++
	}
	return line
}

func feedRowHeight(width int) int {
	if width >= 52 {
		return 2
	}
	return 1
}

func (m Model) feedHeaderRows() int {
	if m.searching {
		return feedHeaderHeight + 1
	}
	return feedHeaderHeight
}

type palette struct {
	primary, selectedFG, selectedBG, statusFG, statusBG color.Color
	canvasBG, feedBG, inspectorBG                       color.Color
	alternateRowBG, raisedBG, recessedBG, tabBG         color.Color
	scrollTrack, scrollThumb                            color.Color
}

func (m Model) palette() palette {
	if m.light {
		return palette{
			primary: lipgloss.Color("#4A2D7A"), selectedFG: lipgloss.Color("#FFFFFF"), selectedBG: lipgloss.Color("#7A668D"),
			statusFG: lipgloss.Color("#2E2040"), statusBG: lipgloss.Color("#E9DFF5"), raisedBG: lipgloss.Color("#EEE5F7"),
			canvasBG: lipgloss.Color("#F2ECF7"), feedBG: lipgloss.Color("#E8E3EF"), inspectorBG: lipgloss.Color("#F2ECF7"),
			alternateRowBG: lipgloss.Color("#DED9E6"), recessedBG: lipgloss.Color("#F7F2FB"), tabBG: lipgloss.Color("#DDD0EB"),
			scrollTrack: lipgloss.Color("#CFC7D8"), scrollThumb: lipgloss.Color("#8D7A9D"),
		}
	}
	return palette{
		primary: lipgloss.Color("#B99AD8"), selectedFG: lipgloss.Color("#F6EFFB"), selectedBG: lipgloss.Color("#765C91"),
		statusFG: lipgloss.Color("#EFE5FA"), statusBG: lipgloss.Color("#38264A"), raisedBG: lipgloss.Color("#3A3144"),
		canvasBG: lipgloss.Color("#302A36"), feedBG: lipgloss.Color("#242A34"), inspectorBG: lipgloss.Color("#302A36"),
		alternateRowBG: lipgloss.Color("#292F39"), recessedBG: lipgloss.Color("#2C303A"), tabBG: lipgloss.Color("#463653"),
		scrollTrack: lipgloss.Color("#343A45"), scrollThumb: lipgloss.Color("#665775"),
	}
}

func (m Model) titleLine(text string, width int) string {
	p := m.palette()
	return lipgloss.NewStyle().Bold(true).Foreground(p.primary).Render(fit(text, width))
}

func (m Model) selectedLine(text string) string {
	p := m.palette()
	return preserveOuterStyle(lipgloss.NewStyle().Bold(true).Foreground(p.selectedFG).Background(p.selectedBG).Render(text))
}

func (m Model) statusLine(text string, width int) string {
	p := m.palette()
	base := lipgloss.NewStyle().Foreground(p.statusFG).Background(p.statusBG)
	hints := m.footerHints(width, text)
	if len(hints) == 0 {
		return base.Render(fit(" "+text, width))
	}
	renderedHints := make([]string, 0, len(hints))
	for _, hint := range hints {
		key := lipgloss.NewStyle().Bold(true).Foreground(p.statusFG).Background(p.raisedBG).Render(" " + hint.key + " ")
		renderedHints = append(renderedHints, key+base.Render(" "+hint.label))
	}
	right := strings.Join(renderedHints, base.Render("  "))
	leftWidth := max(0, width-ansi.StringWidth(right))
	left := base.Render(fit(" "+text, leftWidth))
	return preserveOuterStyle(left + right)
}

type footerHint struct {
	key, label string
	priority   int
}

func (m Model) footerHints(width int, status string) []footerHint {
	var hints []footerHint
	switch {
	case m.searching:
		hints = []footerHint{{"enter", "apply", 0}, {"tab", "broaden", 0}, {"esc", "done", 0}}
	case m.pendingAction != nil:
		hints = []footerHint{{"y/enter", "confirm", 0}, {"n", "cancel", 0}, {"q", "quit", 1}}
	case m.actionRunning || m.actionResult != nil:
		hints = []footerHint{{"q", "quit", 0}}
	case (m.width >= narrowBreakpoint && m.focus == inspectorPane) || (m.width < narrowBreakpoint && m.detailOpen):
		hints = []footerHint{
			{"↑↓", "scroll", 0}, {"←→", "pan", 3}, {"enter", "feed", 0},
			{"[", "readme", 2}, {"]", "changelog", 2}, {"q", "quit", 1},
		}
	default:
		hints = []footerHint{{"/", "search", 0}, {"↑↓", "move", 0}, {"enter", "details", 1}, {"space", "expand", 3}}
		if m.deps.Actions != nil && len(m.groups) > 0 {
			label := "install"
			if m.selectedEvent().Installed {
				label = "upgrade"
			}
			hints = append(hints, footerHint{"a", label, 0})
		}
		hints = append(hints, footerHint{"q", "quit", 2})
	}

	reservedStatus := min(width, ansi.StringWidth(status)+1)
	for footerHintsWidth(hints) > max(0, width-reservedStatus) {
		remove := -1
		for i, hint := range hints {
			if remove < 0 || hint.priority > hints[remove].priority {
				remove = i
			}
		}
		if remove < 0 {
			break
		}
		hints = append(hints[:remove], hints[remove+1:]...)
	}
	return hints
}

func footerHintsWidth(hints []footerHint) int {
	width := 0
	for i, hint := range hints {
		if i > 0 {
			width += 2
		}
		width += ansi.StringWidth(hint.key) + ansi.StringWidth(hint.label) + 3
	}
	return width
}

func (m Model) surfaceLine(text string, width int, background color.Color) string {
	return preserveOuterStyle(lipgloss.NewStyle().Background(background).Render(fit(text, width)))
}

func paintBackground(content string, width int, background color.Color) string {
	style := lipgloss.NewStyle().Background(background)
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		lines[index] = preserveOuterStyle(style.Render(fitANSI(line, width)))
	}
	return strings.Join(lines, "\n")
}

var sgrSequence = regexp.MustCompile(`\x1b\[[0-9;]*m`)
var trueColorSpec = regexp.MustCompile(`(38|48);2;([0-9]{1,3});([0-9]{1,3});([0-9]{1,3})`)

func deemphasizeANSI(content string, light bool) string {
	foreground := [3]int{137, 131, 143}
	background := [3]int{36, 42, 52}
	if light {
		foreground = [3]int{119, 113, 127}
		background = [3]int{232, 227, 239}
	}
	content = sgrSequence.ReplaceAllStringFunc(content, func(sequence string) string {
		return trueColorSpec.ReplaceAllStringFunc(sequence, func(colorSpec string) string {
			parts := trueColorSpec.FindStringSubmatch(colorSpec)
			if parts[1] == "38" {
				return fmt.Sprintf("38;2;%d;%d;%d", foreground[0], foreground[1], foreground[2])
			}
			var muted [3]int
			for index := range muted {
				channel, _ := strconv.Atoi(parts[index+2])
				muted[index] = (channel + 4*background[index]) / 5
			}
			return fmt.Sprintf("48;2;%d;%d;%d", muted[0], muted[1], muted[2])
		})
	})
	mutedForeground := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", foreground[0], foreground[1], foreground[2])
	return mutedForeground + strings.ReplaceAll(content, "\x1b[m", "\x1b[m"+mutedForeground) + "\x1b[m"
}

func (m Model) surfaceRule(prefix, label string, width int, background color.Color) string {
	text := prefix
	if label != "" {
		text += " " + label + " "
	}
	text += strings.Repeat("═", max(0, width-ansi.StringWidth(text)))
	return m.surfaceLine(text, width, background)
}

func (m Model) feedTitle(width int) string                { return m.titleLine(" Cicerone  Package updates", width) }
func (m Model) contentLine(text string, width int) string { return fit(text, width) }
func (m Model) feedListHeader(width int) string {
	if width >= 52 {
		return fit("   PACKAGE                                  VERSION", width)
	}
	return fit("   PACKAGE                  VERSION", width)
}

func (m Model) seenSeparator(width int) string {
	p := m.palette()
	label := "   --- previously seen "
	if m.initialRefreshRunning {
		label = "   --- loading additional packages… "
	}
	label += strings.Repeat("-", max(3, width-ansi.StringWidth(label)))
	return lipgloss.NewStyle().Faint(true).Foreground(p.primary).Background(p.feedBG).
		Render(fit(label, width))
}
func (m Model) feedControls(width int) string {
	formulae, casks := m.filter.Types[domain.PackageFormula], m.filter.Types[domain.PackageCask]
	caskActive, allActive := casks && !formulae, formulae && casks
	active := 0
	if caskActive {
		active = 1
	} else if allActive {
		active = 2
	}
	controls := "   " + m.freshnessText()
	controls += fmt.Sprintf("   roll-up %s", onOff(m.filter.RollUp))
	scope := m.filter.Search
	if !validSearchScope(scope) {
		scope = domain.SearchNames
	}
	if !m.searching && strings.TrimSpace(m.filter.Query) != "" {
		controls += "   search " + string(scope) + ": " + fit(m.filter.Query, 18)
	}
	rows := m.tabStrip([]string{"FORMULAE", "CASKS", "ALL"}, active, width, m.palette().feedBG, controls)
	if m.searching {
		return strings.Join([]string{rows[0], rows[1], rows[2], m.searchInputLine(scope, width)}, "\n")
	}
	return strings.Join(rows[:], "\n")
}

func (m Model) freshnessText() string {
	if m.freshness.LastSync.IsZero() {
		if m.freshnessErr != nil {
			return "Sync unavailable"
		}
		return "Sync never"
	}
	now := m.deps.Now()
	text := "Sync " + m.freshness.LastSync.In(time.Local).Format("Jan 2 2006 15:04")
	if age := now.Sub(m.freshness.LastSync); age > freshnessWarningAfter {
		text += "   ! sync " + compactAge(age) + " old"
	}
	if lag := m.freshness.LastSync.Sub(m.freshness.LastPackageUpdate); !m.freshness.LastPackageUpdate.IsZero() && lag > freshnessWarningAfter {
		text += "   ! updates " + compactAge(lag) + " behind"
	}
	if m.freshnessErr != nil {
		text += "   ! freshness unavailable"
	}
	return text
}

func compactAge(duration time.Duration) string {
	if duration < 48*time.Hour {
		return fmt.Sprintf("%dh", int(duration.Round(time.Hour)/time.Hour))
	}
	return fmt.Sprintf("%dd", int(duration.Round(24*time.Hour)/(24*time.Hour)))
}

func (m Model) searchInputLine(scope domain.SearchScope, width int) string {
	p := m.palette()
	surface := lipgloss.NewStyle().Background(p.feedBG)
	field := lipgloss.NewStyle().Bold(true).Foreground(p.primary).Background(p.raisedBG)
	if width <= 4 {
		return preserveOuterStyle(field.Render(fit("█", width)))
	}
	text := " search " + string(scope) + ": " + m.filter.Query + "█"
	return preserveOuterStyle(surface.Render("  ")) +
		preserveOuterStyle(field.Render(fit(text, width-4))) +
		preserveOuterStyle(surface.Render("  "))
}

// tabStrip follows tui-studio's Tabs geometry: adjacent boxes on the first
// two rows and one continuous baseline interrupted only by the active tab.
func (m Model) tabStrip(labels []string, active, width int, background color.Color, middleSuffix string) [3]string {
	p := m.palette()
	var rows [3]string
	for index, label := range labels {
		inside := ansi.StringWidth(label) + 2
		style := lipgloss.NewStyle().Foreground(p.primary).Background(background)
		labelStyle := style
		if index == active {
			labelStyle = labelStyle.Bold(true).Foreground(p.selectedFG)
		}
		rows[0] += preserveOuterStyle(style.Render("╭" + strings.Repeat("─", inside) + "╮"))
		rows[1] += preserveOuterStyle(style.Render("│ ")) +
			preserveOuterStyle(labelStyle.Render(label)) +
			preserveOuterStyle(style.Render(" │"))
		bottom := "┴" + strings.Repeat("─", inside) + "┴"
		if index == active {
			bottom = "╯" + strings.Repeat(" ", inside) + "╰"
		}
		rows[2] += preserveOuterStyle(style.Render(bottom))
	}
	rows[1] += middleSuffix
	for index := range rows {
		if index == 2 {
			rows[index] += lipgloss.NewStyle().Foreground(p.primary).Background(background).
				Render(strings.Repeat("─", max(0, width-ansi.StringWidth(rows[index]))))
		}
		rows[index] = m.surfaceLine(rows[index], width, background)
	}
	return rows
}

func (m Model) feedGroupRows(marker string, e domain.UpdateEvent, width int) []string {
	cadence := m.updateCadenceLabel(e)
	transition := eventKindBadge(e.Kind) + m.versionTransition(e)
	nameWidth := ansi.StringWidth(e.Name)
	cadenceWidth := ansi.StringWidth(cadence)
	versionWidth := ansi.StringWidth(transition)
	overflow := ansi.StringWidth(marker) + nameWidth + 1 + cadenceWidth + 1 + versionWidth - width
	var versionLine string
	if overflow <= 0 {
		right := cadence + " " + transition
		paddedNameWidth := max(1, width-ansi.StringWidth(marker)-ansi.StringWidth(right)-2)
		versionLine = fit(marker+fit(e.Name, paddedNameWidth)+" "+right, width)
	} else {
		shrinkOptional := func(segmentWidth *int) {
			if overflow <= 0 || *segmentWidth <= 0 {
				return
			}
			reduction := min(overflow, *segmentWidth)
			*segmentWidth -= reduction
			overflow -= reduction
			if *segmentWidth == 0 {
				overflow = max(0, overflow-1)
			}
		}
		shrinkOptional(&cadenceWidth)
		shrinkOptional(&versionWidth)
		nameWidth = max(0, nameWidth-overflow)

		versionLine = marker + ansi.Truncate(e.Name, nameWidth, "")
		if cadenceWidth > 0 {
			versionLine += " " + ansi.Truncate(cadence, cadenceWidth, "")
		}
		if versionWidth > 0 {
			versionLine += " " + ansi.Truncate(transition, versionWidth, "")
		}
		versionLine = fit(versionLine, width)
	}
	if width >= 52 {
		description := m.packageDescriptions[e.PackageID]
		if description != "" {
			description = lipgloss.NewStyle().Faint(true).Foreground(m.palette().primary).
				Render(fit(description, max(0, width-2)))
		}
		return []string{
			versionLine,
			fit("  "+description, width),
		}
	}
	return []string{versionLine}
}

func (m Model) updateCadenceLabel(event domain.UpdateEvent) string {
	interval := event.UpdateInterval
	if interval <= 0 {
		label := "last update unknown"
		if !event.Time.IsZero() {
			now := m.filter.Now
			if now.IsZero() {
				now = time.Now()
			}
			label = "last update: " + relativeAge(now, event.Time)
		}
		foreground, background := lipgloss.Color("#6F7681"), lipgloss.Color("#272D36")
		if m.light {
			foreground, background = lipgloss.Color("#8C8493"), lipgloss.Color("#E6E1EA")
		}
		return lipgloss.NewStyle().Faint(true).Foreground(foreground).Background(background).Render(label)
	}
	animal := "🐢"
	switch {
	case interval <= 24*time.Hour:
		animal = "🐆"
	case interval <= 7*24*time.Hour:
		animal = "🐇"
	case interval <= 90*24*time.Hour:
		animal = "🐄"
	}
	type period struct {
		name     string
		duration time.Duration
	}
	periods := []period{
		{name: "day", duration: 24 * time.Hour},
		{name: "week", duration: 7 * 24 * time.Hour},
		{name: "month", duration: 30 * 24 * time.Hour},
		{name: "year", duration: 365 * 24 * time.Hour},
	}
	bestName, bestCount, bestError := "", 0, math.MaxFloat64
	for _, candidate := range periods {
		exact := float64(candidate.duration) / float64(interval)
		rounded := int(math.Round(exact))
		if rounded < 1 {
			continue
		}
		relativeError := math.Abs(float64(rounded)-exact) / exact
		if relativeError <= 0.20 && (bestCount == 0 || rounded < bestCount || rounded == bestCount && relativeError < bestError) {
			bestName, bestCount, bestError = candidate.name, rounded, relativeError
		}
	}
	if bestCount == 0 {
		for _, candidate := range periods {
			exact := float64(candidate.duration) / float64(interval)
			rounded := max(1, int(math.Round(exact)))
			relativeError := math.Abs(float64(rounded)-exact) / max(exact, 0.000001)
			if relativeError < bestError {
				bestName, bestCount, bestError = candidate.name, rounded, relativeError
			}
		}
	}
	label := fmt.Sprintf("%s %d×%s", animal, bestCount, bestName)
	foreground, background := m.cadenceColors(interval)
	return lipgloss.NewStyle().Foreground(foreground).Background(background).Render(label)
}

func (m Model) cadenceColors(interval time.Duration) (color.Color, color.Color) {
	if m.light {
		switch {
		case interval <= 24*time.Hour:
			return lipgloss.Color("#25607A"), lipgloss.Color("#D6EEF7")
		case interval <= 7*24*time.Hour:
			return lipgloss.Color("#326347"), lipgloss.Color("#DCEEE2")
		case interval <= 90*24*time.Hour:
			return lipgloss.Color("#765A12"), lipgloss.Color("#F2E8C7")
		default:
			return lipgloss.Color("#87475A"), lipgloss.Color("#F1DDE3")
		}
	}
	switch {
	case interval <= 24*time.Hour:
		return lipgloss.Color("#9ADFFF"), lipgloss.Color("#243D4A")
	case interval <= 7*24*time.Hour:
		return lipgloss.Color("#8ED3A8"), lipgloss.Color("#263F35")
	case interval <= 90*24*time.Hour:
		return lipgloss.Color("#E7C66B"), lipgloss.Color("#473D25")
	default:
		return lipgloss.Color("#E5A0AF"), lipgloss.Color("#49333A")
	}
}

func relativeAge(now, then time.Time) string {
	age := now.Sub(then)
	if age < time.Minute {
		return "just now"
	}
	type unit struct {
		singular string
		duration time.Duration
	}
	units := []unit{
		{"year", 365 * 24 * time.Hour},
		{"month", 30 * 24 * time.Hour},
		{"week", 7 * 24 * time.Hour},
		{"day", 24 * time.Hour},
		{"hour", time.Hour},
		{"minute", time.Minute},
	}
	for _, candidate := range units {
		if age >= candidate.duration {
			count := int(age / candidate.duration)
			name := candidate.singular
			if count != 1 {
				name += "s"
			}
			return fmt.Sprintf("%d %s ago", count, name)
		}
	}
	return "just now"
}

func (m Model) feedGroupRow(marker string, e domain.UpdateEvent, width int) string {
	rows := m.feedGroupRows(marker, e, width)
	return rows[0]
}

func (m Model) feedChildRow(e domain.UpdateEvent, width int) string {
	return fit("    └ "+eventKindBadge(e.Kind)+m.versionTransition(e), width)
}

func eventKindBadge(kind domain.EventKind) string {
	switch kind {
	case domain.EventRevision:
		return "[revision] "
	case domain.EventMetadata:
		return "[metadata] "
	default:
		return ""
	}
}

func (m Model) versionTransition(e domain.UpdateEvent) string {
	oldVersion := domain.CleanVersion(e.OldVersion)
	newVersion := domain.CleanVersion(e.NewVersion)
	if oldVersion == "" {
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#4FBF79")).Render(newVersion)
	}
	if newVersion == "" {
		return lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#D75F5F")).Render(oldVersion)
	}
	if oldVersion == newVersion {
		return lipgloss.NewStyle().Bold(true).Render(newVersion)
	}

	oldRunes, newRunes := []rune(oldVersion), []rune(newVersion)
	oldStyle := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#D75F5F"))
	oldCommonStyle := lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("#888888"))
	newStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#4FBF79"))
	prefix := 0
	for prefix < len(oldRunes) && prefix < len(newRunes) && oldRunes[prefix] == newRunes[prefix] {
		prefix++
	}
	common := string(newRunes[:prefix])
	newChanged := string(newRunes[prefix:])
	oldChanged := string(oldRunes[prefix:])
	return common + newStyle.Render(newChanged) + " " + oldCommonStyle.Render(common) + oldStyle.Render(oldChanged)
}

func fit(s string, width int) string {
	if ansi.StringWidth(s) > width {
		return ansi.Truncate(s, width, "")
	}
	return s + strings.Repeat(" ", width-ansi.StringWidth(s))
}

func preserveOuterStyle(styled string) string {
	prefixEnd := strings.IndexByte(styled, 'm')
	if prefixEnd < 0 {
		return styled
	}
	prefix := styled[:prefixEnd+1]
	body := styled[prefixEnd+1:]
	body = strings.TrimSuffix(body, "\x1b[m")
	body = strings.ReplaceAll(body, "\x1b[m", "\x1b[m"+prefix)
	return prefix + body + "\x1b[m"
}

func joinColumns(left, right string, lw, rw int, divider string) string {
	l, r := strings.Split(left, "\n"), strings.Split(right, "\n")
	n := len(l)
	if len(r) > n {
		n = len(r)
	}
	lines := make([]string, n)
	for i := 0; i < n; i++ {
		var a, b string
		if i < len(l) {
			a = l[i]
		}
		if i < len(r) {
			b = r[i]
		}
		lines[i] = fitANSI(a, lw) + divider + fitANSI(b, rw)
	}
	return strings.Join(lines, "\n")
}

func fitANSI(s string, width int) string {
	if ansi.StringWidth(s) > width {
		return ansi.Truncate(s, width, "")
	}
	return s + strings.Repeat(" ", width-ansi.StringWidth(s))
}

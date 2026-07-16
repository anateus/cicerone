package tui

import (
	"image/color"
	"strings"

	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const narrowBreakpoint = 100
const statusHeight = 1

func (m Model) render() string {
	w, h := m.width, m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
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
	var body string
	if w < narrowBreakpoint {
		if m.detailOpen {
			body = viewportContent(m.inspectorViewport, m.renderInspector(w), w, h-statusHeight, 0)
		} else {
			body = viewportContent(m.feedViewport, m.renderFeed(w), w, h-statusHeight, m.viewportOffset)
		}
	} else {
		left := w * 3 / 5
		right := w - left - 1
		feed := viewportContent(m.feedViewport, m.renderFeed(left), left, h-statusHeight, m.viewportOffset)
		inspector := viewportContent(m.inspectorViewport, m.renderInspector(right), right, h-statusHeight, 0)
		body = joinColumns(feed, inspector, left, right)
	}
	lines := strings.Split(body, "\n")
	maxBody := h - statusHeight
	if len(lines) > maxBody {
		lines = lines[:maxBody]
	}
	for len(lines) < maxBody {
		lines = append(lines, strings.Repeat(" ", w))
	}
	return strings.Join(lines, "\n") + "\n" + m.statusLine(status, w)
}

func viewportContent(v viewport.Model, content string, width, height, offset int) string {
	v.SetWidth(width)
	v.SetHeight(height)
	v.SetContent(content)
	v.SetYOffset(offset)
	return v.View()
}

type palette struct{ primary, selectedFG, selectedBG, statusFG, statusBG color.Color }

func (m Model) palette() palette {
	if m.light {
		return palette{primary: lipgloss.Color("#4A2D7A"), selectedFG: lipgloss.Color("#FFFFFF"), selectedBG: lipgloss.Color("#6D45A1"), statusFG: lipgloss.Color("#2E2040"), statusBG: lipgloss.Color("#E9DFF5")}
	}
	return palette{primary: lipgloss.Color("#D0A9FF"), selectedFG: lipgloss.Color("#190F24"), selectedBG: lipgloss.Color("#C79BFF"), statusFG: lipgloss.Color("#EFE5FA"), statusBG: lipgloss.Color("#38264A")}
}

func (m Model) titleLine(text string, width int) string {
	p := m.palette()
	return lipgloss.NewStyle().Bold(true).Foreground(p.primary).Render(fit(text, width))
}

func (m Model) selectedLine(text string) string {
	p := m.palette()
	return lipgloss.NewStyle().Bold(true).Foreground(p.selectedFG).Background(p.selectedBG).Render(text)
}

func (m Model) statusLine(text string, width int) string {
	p := m.palette()
	return lipgloss.NewStyle().Foreground(p.statusFG).Background(p.statusBG).Render(fit(" "+text, width))
}

func fit(s string, width int) string {
	r := []rune(s)
	if len(r) > width {
		return string(r[:width])
	}
	return s + strings.Repeat(" ", width-len(r))
}

func joinColumns(left, right string, lw, rw int) string {
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
		lines[i] = fitANSI(a, lw) + "│" + fitANSI(b, rw)
	}
	return strings.Join(lines, "\n")
}

func fitANSI(s string, width int) string {
	if ansi.StringWidth(s) > width {
		return ansi.Truncate(s, width, "")
	}
	return s + strings.Repeat(" ", width-ansi.StringWidth(s))
}

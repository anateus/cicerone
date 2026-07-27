package tui

import (
	"regexp"
	"strings"

	"charm.land/glamour/v2"
)

var markdownImage = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`)

func renderMarkdown(source string, width int, light bool) string {
	if width < 12 {
		return source
	}
	style := "dark"
	if light {
		style = "light"
	}
	source = markdownImage.ReplaceAllStringFunc(source, func(image string) string {
		match := markdownImage.FindStringSubmatch(image)
		label := strings.TrimSpace(match[1])
		if label == "" {
			label = "Image"
		}
		return "▧ " + label
	})
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(width-2),
	)
	if err != nil {
		return source
	}
	rendered, err := renderer.Render(source)
	if err != nil {
		return source
	}
	return strings.TrimSpace(rendered)
}

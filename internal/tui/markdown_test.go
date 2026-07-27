package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderMarkdownCompactsImageURLs(t *testing.T) {
	rendered := ansi.Strip(renderMarkdown("![Project logo](https://cdn.example.test/assets/a/very/long/logo.png)", 60, false))
	if strings.Contains(rendered, "https://cdn.example.test") || !strings.Contains(rendered, "Project logo") {
		t.Fatalf("rendered image = %q", rendered)
	}
}

package tui

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/anateus/cicerone/internal/store"
	"github.com/anateus/cicerone/internal/webpage"
	"github.com/charmbracelet/x/ansi"
)

func TestHomepageInspectorRendersHTMLAsRichText(t *testing.T) {
	pageURL, _ := url.Parse("https://widget.test/")
	markdown, err := webpage.Markdown(pageURL, []byte(`<main><h1>Widget</h1><p>A <strong>small</strong> tool.</p><h2>Features</h2><ul><li>First feature</li></ul><pre><code>brew install widget</code></pre><a href="guide">Guide</a></main>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, width := range []int{40, 80} {
		for _, light := range []bool{false, true} {
			t.Run(fmt.Sprintf("width_%d_light_%v", width, light), func(t *testing.T) {
				m := NewModel(Dependencies{})
				m.width, m.height, m.light = width, 50, light
				m.groups = groups("a")
				m.document = store.DocumentREADME
				m.readme = store.PackageDocument{ID: "homepage", SourceURL: pageURL.String(), ExtractionStatus: "homepage", Extracted: []byte(markdown)}
				m.readmeErr = errors.New("offline")
				rendered := m.renderInspector(width)
				plain := ansi.Strip(rendered)
				for _, want := range []string{"HOMEPAGE", "Homepage stale: offline", "Source: https://widget.test/", "Widget", "Features", "First feature", "brew install widget", "Guide"} {
					if !strings.Contains(plain, want) {
						t.Errorf("missing %q in:\n%s", want, plain)
					}
				}
				if strings.Contains(plain, "<main>") || strings.Contains(plain, "**small**") || strings.Contains(plain, "```") || rendered == plain {
					t.Fatalf("homepage did not receive Markdown styling:\n%s", rendered)
				}
				for _, line := range strings.Split(rendered, "\n") {
					if ansi.StringWidth(line) > width {
						t.Fatalf("line exceeds %d columns: %s", width, line)
					}
				}
				if testing.Verbose() {
					t.Log("\n" + plain)
				}
			})
		}
	}
}

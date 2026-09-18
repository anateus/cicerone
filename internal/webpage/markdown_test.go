package webpage

import (
	"net/url"
	"strings"
	"testing"
)

func TestMarkdownPreservesReadableHTML(t *testing.T) {
	pageURL, _ := url.Parse("https://widget.test/docs/index.html")
	tests := []struct {
		name, html string
		want, omit []string
	}{
		{
			name: "traditional page",
			html: `<html><head><title>Widget home</title></head><body>
<h1>Widget</h1><p>A <strong>small</strong> and <em>useful</em> tool.</p>
<h2>Features</h2><ul><li>Fast</li><li>Portable<ul><li>macOS</li></ul></li></ul>
<ol><li>Download</li><li>Install</li></ol>
<blockquote>Works offline.</blockquote><pre><code>if ready {
    run()
}</code></pre><p>Use <code>widget --help</code>.</p>
<table><tr><th>System</th><th>Support</th></tr><tr><td>macOS</td><td>Yes</td></tr></table>
<a href="../guide?q=1#start">Guide</a><img src="logo.png" alt="Widget logo">
</body></html>`,
			want: []string{"# Widget", "**small**", "*useful*", "## Features", "- Fast", "macOS", "1. Download", "> Works offline.", "```", "    run()", "`widget --help`", "| System", "Support", "https://widget.test/guide?q=1#start", "Widget logo"},
			omit: []string{"<table", "<h1", "Widget home"},
		},
		{
			name: "modern static main content",
			html: `<head><title>Widget</title></head><body><nav>Navigation noise</nav><main><h1>Widget</h1><section><h2>Simple setup</h2><p>Build great things.</p></section><div hidden>Hidden offer</div><div aria-hidden="true">Duplicate copy</div><p style="display: none !important">Invisible</p><script>tracking()</script><style>.hero{color:red}</style><template>Template text</template><svg><text>Icon text</text></svg><button>Download</button></main><footer>Footer noise</footer></body>`,
			want: []string{"# Widget", "## Simple setup", "Build great things.", "Download"},
			omit: []string{"Navigation noise", "Footer noise", "Hidden offer", "Duplicate copy", "Invisible", "tracking()", ".hero", "Template text", "Icon text", "<button"},
		},
		{
			name: "javascript shell",
			html: `<head><title>Widget Cloud</title><meta name="description" content="Tools for working together."></head><body><div id="app"></div><script>boot()</script></body>`,
			want: []string{"# Widget Cloud", "Tools for working together.", "no readable static content", "JavaScript", "[Open homepage in browser](https://widget.test/docs/index.html)"},
			omit: []string{"boot()", "<div"},
		},
		{
			name: "empty main retains body and noscript",
			html: `<head><title>Widget</title></head><body><main></main><p>Available downloads</p><noscript><p>Use the <a href="basic.html">basic site</a>.</p></noscript></body>`,
			want: []string{"# Widget", "Available downloads", "[basic site](https://widget.test/docs/basic.html)"},
			omit: []string{"<noscript", "<p>", "no readable static content"},
		},
		{
			name: "base URL and unsafe links",
			html: `<head><base href="https://cdn.widget.test/manual/"></head><body><a href="start">Start</a><a href="javascript:alert(1)">Unsafe</a><img src="data:image/png;base64,abc" alt="Logo"><p>&#27;[31mhello&#7;</p></body>`,
			want: []string{"https://cdn.widget.test/manual/start", "Unsafe", "hello"},
			omit: []string{"javascript:", "data:", "\x1b", "\x07"},
		},
		{
			name: "malformed HTML",
			html: `<h1>Widget<p>Useful &amp; small<ul><li>One<li>Two`,
			want: []string{"Widget", "Useful & small", "One", "Two"},
			omit: []string{"<h1", "<li", "&amp;"},
		},
		{
			name: "older page with navigation and substantial prose",
			html: `<html><head><title>Widget</title></head><body><div class="menu"><a href="menu">Navigation clutter</a></div><div class="sidebar">Sidebar noise</div><div class="content"><h2>About Widget</h2><p>Widget is a small command-line application that helps people keep track of their tools. It stores useful information locally, so the documentation remains available when you are working offline. The program focuses on making everyday work easier, with clear commands and readable output. It is designed for people who prefer a terminal but still want enough context to understand what changed.</p><p>The application works with existing projects and does not require a separate service. You can inspect the current configuration, review recent activity, and find instructions for the next step. The guide describes installation and common tasks with examples that can be copied directly into your terminal.</p><p><a href="guide">Read the guide</a></p></div></body></html>`,
			want: []string{"Widget is a small command-line application", "The application works with existing projects", "Read the guide"},
			omit: []string{"Navigation clutter", "Sidebar noise"},
		},
		{
			name: "legacy character encoding",
			html: "<head><meta charset=windows-1252></head><body><p>Caf\xe9</p></body>",
			want: []string{"Café"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Markdown(pageURL, []byte(tt.html))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
			for _, omit := range tt.omit {
				if strings.Contains(got, omit) {
					t.Errorf("unexpected %q in:\n%s", omit, got)
				}
			}
		})
	}
}

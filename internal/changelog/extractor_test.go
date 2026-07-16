package changelog

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractorRetainsStructureRemovesBoilerplateAndFindsLinks(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "pages", "release.html"))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://example.test/changelog")
	x := ReadabilityExtractor{}
	first, err := x.Extract(base, body)
	if err != nil {
		t.Fatal(err)
	}
	second, err := x.Extract(base, body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Widget changelog", "## Version 2.3.0", "- Added deterministic exports.", "- Fixed terminal rendering."} {
		if !strings.Contains(first.Text, want) {
			t.Fatalf("text=%q missing %q", first.Text, want)
		}
	}
	for _, noise := range []string{"Products Pricing", "Cookie policy", "Copyright Acme"} {
		if strings.Contains(first.Text, noise) {
			t.Fatalf("text contains boilerplate %q: %q", noise, first.Text)
		}
	}
	if first.Text != second.Text {
		t.Fatal("extraction is not deterministic")
	}
	if len(first.Links) == 0 || first.Links[0].URL.String() != "https://example.test/releases/2.3.0" {
		t.Fatalf("links=%#v", first.Links)
	}
}

func TestExtractorHandlesMalformedHTMLAndSanitizesTerminalText(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "pages", "malformed.html"))
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("\x1b[31m\x00")...)
	base, _ := url.Parse("https://example.test/")
	got, err := (ReadabilityExtractor{}).Extract(base, body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "# Changes 1.4.0") || !strings.Contains(got.Text, "- Recovered item") {
		t.Fatalf("text=%q", got.Text)
	}
	if strings.ContainsAny(got.Text, "\x00\x1b") {
		t.Fatalf("unsafe text=%q", got.Text)
	}
}

package history

import (
	"testing"

	"cicerone/internal/domain"
)

func TestParseDefinitionExtractsAnchoredFormulaTokens(t *testing.T) {
	body := []byte(`class FooBar < Formula
  desc "ignored"
  homepage "https://example.test/foo"
  url "https://example.test/foo-1.2.3.tar.gz"
  version "1.2.3"
  revision 2
end
`)
	got, diagnostics := ParseDefinition("Formula/f/foo-bar.rb", body)
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	want := Definition{Name: "foo-bar", FullName: "foo-bar", Version: "1.2.3", Revision: "2", Type: domain.PackageFormula, Homepage: "https://example.test/foo", URL: "https://example.test/foo-1.2.3.tar.gz"}
	if got == nil || *got != want {
		t.Fatalf("definition = %#v, want %#v", got, want)
	}
}

func TestParseDefinitionHandlesCaskAndLiveVersions(t *testing.T) {
	for _, tc := range []struct{ path, body, version string }{
		{"Casks/f/foo.rb", "cask \"foo\" do\n  version \"2.0,10\"\nend", "2.0,10"},
		{"Formula/head.rb", "class Head < Formula\n  url \"https://example.test/repo.git\"\n  head \"https://example.test/repo.git\"\nend", "HEAD"},
		{"Casks/latest.rb", "cask \"latest\" do\n  version :latest\nend", "latest"},
	} {
		got, diagnostics := ParseDefinition(tc.path, []byte(tc.body))
		if got == nil || got.Version != tc.version || len(diagnostics) != 0 {
			t.Fatalf("%s: got %#v diagnostics %v", tc.path, got, diagnostics)
		}
	}
}

func TestParseDefinitionRejectsComputedRubyWithoutEvaluation(t *testing.T) {
	got, diagnostics := ParseDefinition("Formula/evil.rb", []byte("class Evil < Formula\n  version ENV.fetch(\"VERSION\")\nend\n"))
	if got == nil || got.Version != "" || len(diagnostics) == 0 {
		t.Fatalf("got %#v diagnostics %v", got, diagnostics)
	}
}

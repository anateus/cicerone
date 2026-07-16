package changelog

import (
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestMatchVersionMarkdownHeadings(t *testing.T) {
	tests := []struct {
		name, version, fixture, want string
		minimum, maximum             float64
	}{
		{"ATX normalized v tag", "1.2.3", "atx.md", "Fixed the exact release", 0.8, 1},
		{"setext package prefix", "2.4.0", "setext.md", "Setext release notes", 0.8, 1},
		{"range", "1.2.1", "atx.md", "Older range", 0.8, 0.99},
		{"ambiguous prose", "1.2.3", "ambiguous.md", "mentions 1.2.3", 0.01, 0.49},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			section, ok := MatchVersion(tt.version, []Artifact{{ID: "a", URL: "https://example.test/CHANGELOG.md", Extracted: fixture(t, tt.fixture)}})
			if !ok {
				t.Fatal("MatchVersion did not match")
			}
			if section.Confidence < tt.minimum || section.Confidence > tt.maximum {
				t.Fatalf("confidence = %v, want [%v,%v]", section.Confidence, tt.minimum, tt.maximum)
			}
			if !contains(section.Body, tt.want) {
				t.Fatalf("body = %q, want %q", section.Body, tt.want)
			}
		})
	}
}

func TestMatchVersionSkipsUnreleasedAndRejectsPartialVersion(t *testing.T) {
	section, ok := MatchVersion("1.2.3", []Artifact{{Extracted: []byte("## Unreleased\nmentions 1.2.3\n\n## 1.2\nwrong partial\n")}})
	if !ok {
		t.Fatal("wanted substring fallback")
	}
	if section.Confidence > .49 {
		t.Fatalf("partial confidence = %v, want <= .49", section.Confidence)
	}
}

func TestMatchVersionComparesNumericRangeComponents(t *testing.T) {
	section, ok := MatchVersion("1.10.0", []Artifact{{Extracted: []byte("## 1.11.0 - 1.9.0\n\nnumeric range\n")}})
	if !ok || section.Confidence < .8 {
		t.Fatalf("numeric range match = %#v, %v; want heading match", section, ok)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

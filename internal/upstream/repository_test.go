package upstream

import (
	"net/url"
	"testing"
)

func TestDiscoverRepositoryPrefersExplicitRepositoryContext(t *testing.T) {
	base, _ := url.Parse("https://vonshednob.cc/pter/")
	body := []byte(`
		<ul><li>Repository: <a href="https://codeberg.org/pter/pter">codeberg.org/pter/pter</a></li></ul>
		<p>Contribute on <a href="https://codeberg.org/vonshednob/pter">codeberg</a>.</p>
		<a href="https://github.com/unrelated/theme">theme source</a>`)
	got, ok := DiscoverRepository(base, body, "pter")
	if !ok {
		t.Fatal("DiscoverRepository found no repository")
	}
	if got != "https://codeberg.org/pter/pter" {
		t.Fatalf("repository = %q", got)
	}
}

func TestDiscoverRepositoryRejectsUnrelatedForgeLinks(t *testing.T) {
	base, _ := url.Parse("https://example.test/tool")
	body := []byte(`
		<h2>Source repository</h2>
		<p>This site uses <a href="https://github.com/unrelated/tool">a theme</a>.</p>`)
	if got, ok := DiscoverRepository(base, body, "tool"); ok {
		t.Fatalf("repository = %q, want no match", got)
	}
}

func TestCanonicalRepositorySupportsKnownForges(t *testing.T) {
	tests := map[string]string{
		"https://github.com/acme/widget/issues":      "https://github.com/acme/widget",
		"https://codeberg.org/acme/widget.git":       "https://codeberg.org/acme/widget",
		"https://gitlab.com/acme/widget/-/releases":  "https://gitlab.com/acme/widget",
		"https://bitbucket.org/acme/widget/src/main": "https://bitbucket.org/acme/widget",
	}
	for raw, want := range tests {
		if got, ok := CanonicalRepository(raw); !ok || got != want {
			t.Errorf("CanonicalRepository(%q) = %q, %v; want %q, true", raw, got, ok, want)
		}
	}
}

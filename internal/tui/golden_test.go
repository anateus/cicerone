package tui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"cicerone/internal/domain"
	"cicerone/internal/store"
)

func TestGoldenViews(t *testing.T) {
	rolled := domain.FeedGroup{ID: "roll", Events: []domain.UpdateEvent{event("roll", "ripgrep"), event("old", "ripgrep")}}
	cases := map[string]Model{}
	base := NewModel(Dependencies{})
	base.width = 120
	base.height = 12
	base.loading = false
	base.groups = []domain.FeedGroup{rolled, {ID: "git", Events: []domain.UpdateEvent{event("git", "git")}}}
	base.expanded["roll"] = true
	base.changelog = []store.ChangelogSection{{Version: "2.0", Body: "Faster search\nImproved diagnostics"}}
	cases["wide_dark_expanded"] = base
	light := base
	light.light = true
	cases["wide_light_expanded"] = light
	narrow := base
	narrow.width = 72
	narrow.height = 10
	cases["narrow_feed"] = narrow
	detail := narrow
	detail.detailOpen = true
	cases["narrow_detail"] = detail
	empty := base
	empty.groups = nil
	empty.changelog = nil
	cases["empty"] = empty
	loading := empty
	loading.loading = true
	cases["loading"] = loading
	stale := base
	stale.stale = true
	stale.loading = true
	cases["stale"] = stale
	failed := empty
	failed.err = errors.New("database unavailable")
	cases["error"] = failed
	notify := base
	notify.notification = "Sync complete · 3 new updates"
	cases["notification"] = notify

	for name, model := range cases {
		t.Run(name, func(t *testing.T) { assertGolden(t, name, model.View().Content) })
	}
}

func TestStatusHasFixedHeight(t *testing.T) {
	m := NewModel(Dependencies{})
	m.width = 72
	m.height = 8
	m.loading = false
	m.groups = groups("a", "b")
	ready := m.View().Content
	m.notification = "Synchronizing repositories (12/200)"
	progress := m.View().Content
	if lineCount(ready) != 8 || lineCount(progress) != 8 {
		t.Fatalf("status changed screen height: %d, %d", lineCount(ready), lineCount(progress))
	}
}

func lineCount(s string) int {
	n := 1
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}

func assertGolden(t *testing.T, name, actual string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(actual), 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if actual != string(want) {
		t.Fatalf("golden mismatch for %s\n--- want ---\n%s\n--- got ---\n%s", name, want, actual)
	}
}

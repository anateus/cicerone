package app

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestDefaultPaths(t *testing.T) {
	got := DefaultPaths("/Users/alice")
	want := Paths{
		DataDir:  "/Users/alice/Library/Application Support/cicerone",
		CacheDir: "/Users/alice/Library/Caches/cicerone",
		DBPath:   "/Users/alice/Library/Application Support/cicerone/cicerone.db",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatal(diff)
	}
}

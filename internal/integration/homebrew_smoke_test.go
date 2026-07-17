//go:build homebrew_smoke && darwin

package integration

import (
	"context"
	"testing"
	"time"

	"cicerone/internal/execx"
	"cicerone/internal/homebrew"
)

// TestRealHomebrewReadOnly only reads installed metadata. It never invokes an
// install, upgrade, uninstall, fetch, checkout, or reset command.
func TestRealHomebrewReadOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	packages, err := homebrew.NewClient(execx.NewRunner()).Installed(ctx)
	if err != nil {
		t.Skipf("Homebrew metadata unavailable: %v", err)
	}
	t.Logf("read %d installed Homebrew packages", len(packages))
}

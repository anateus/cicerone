package homebrew_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"cicerone/internal/domain"
	"cicerone/internal/execx"
	"cicerone/internal/homebrew"
	"cicerone/internal/testutil"
)

func TestInstalled(t *testing.T) {
	const fixture = `{
  "formulae": [{
    "name": "ripgrep",
    "full_name": "homebrew/core/ripgrep",
    "installed": [{"version": "13.0.0"}, {"version": "14.1.1"}],
    "pinned": true,
    "outdated": true
  }],
  "casks": [{
    "token": "firefox",
    "full_token": "homebrew/cask/firefox",
    "installed": "128.0",
    "outdated": false
  }]
}`
	runner := &testutil.Runner{RunResult: execx.Result{Stdout: []byte(fixture)}}
	client := homebrew.NewClient(runner)

	got, err := client.Installed(context.Background())
	if err != nil {
		t.Fatalf("Installed() error = %v", err)
	}
	want := []domain.InstalledPackage{
		{PackageID: "homebrew/core/ripgrep", Name: "homebrew/core/ripgrep", Version: "14.1.1", Type: domain.PackageFormula, Pinned: true, UpgradeAvailable: true},
		{PackageID: "homebrew/cask/firefox", Name: "homebrew/cask/firefox", Version: "128.0", Type: domain.PackageCask},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Installed() mismatch (-want +got):\n%s", diff)
	}

	wantCalls := []testutil.Call{{Name: "brew", Args: []string{"info", "--json=v2", "--installed"}}}
	if diff := cmp.Diff(wantCalls, runner.RunCalls); diff != "" {
		t.Errorf("runner calls mismatch (-want +got):\n%s", diff)
	}
}

func TestInstalledRejectsMalformedPackageName(t *testing.T) {
	runner := &testutil.Runner{RunResult: execx.Result{Stdout: []byte(`{
  "formulae": [{"name":"bad name","full_name":"homebrew/core/bad name","installed":[{"version":"1.0"}]}],
  "casks": []
}`)}}
	client := homebrew.NewClient(runner)

	_, err := client.Installed(context.Background())
	if err == nil || !strings.Contains(err.Error(), "malformed package name") {
		t.Fatalf("Installed() error = %v, want malformed package name", err)
	}
}

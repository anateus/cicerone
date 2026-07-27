package homebrew

import (
	"context"
	"slices"
	"testing"

	"cicerone/internal/execx"
	"cicerone/internal/testutil"
)

func TestInfoReturnsStructuredFormulaMetadata(t *testing.T) {
	runner := &testutil.Runner{RunResult: execx.Result{Stdout: []byte(`{
		"formulae":[{
			"name":"widget","full_name":"acme/tap/widget","desc":"Useful widget",
			"homepage":"https://example.test/widget","license":"MIT",
			"versions":{"stable":"2.0.0","head":"HEAD","bottle":true},
			"installed":[{"version":"1.9.0"}],
			"dependencies":["libfoo"],"build_dependencies":["pkg-config"],
			"caveats":"Restart your shell."
		}],"casks":[]
	}`)}}
	client := NewClient(runner)
	got, raw, err := client.Info(context.Background(), "acme/tap/widget")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "widget" || got.FullName != "acme/tap/widget" || got.Description != "Useful widget" ||
		got.Homepage != "https://example.test/widget" || got.License != "MIT" || got.StableVersion != "2.0.0" ||
		got.InstalledVersion != "1.9.0" || got.Caveats != "Restart your shell." ||
		!slices.Equal(got.Dependencies, []string{"libfoo"}) || len(raw) == 0 {
		t.Fatalf("Info = %#v, raw=%q", got, raw)
	}
	if len(runner.RunCalls) != 1 || runner.RunCalls[0].Name != "brew" ||
		!slices.Equal(runner.RunCalls[0].Args, []string{"info", "--json=v2", "acme/tap/widget"}) {
		t.Fatalf("runner calls = %#v", runner.RunCalls)
	}
}

func TestInfoRejectsMalformedPackageBeforeRunningBrew(t *testing.T) {
	runner := &testutil.Runner{}
	_, _, err := NewClient(runner).Info(context.Background(), "widget; rm")
	if err == nil {
		t.Fatal("Info returned nil error")
	}
	if len(runner.RunCalls) != 0 {
		t.Fatalf("runner calls = %#v", runner.RunCalls)
	}
}

func TestInfoAcceptsCaskDisplayNameArray(t *testing.T) {
	runner := &testutil.Runner{RunResult: execx.Result{Stdout: []byte(`{
		"formulae":[],"casks":[{
			"token":"widget","full_token":"acme/tap/widget","name":["Widget App"],
			"desc":"GUI widget","homepage":"https://example.test","version":"2.0","installed":"1.0"
		}]
	}`)}}
	got, _, err := NewClient(runner).Info(context.Background(), "acme/tap/widget")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Cask || got.Name != "Widget App" || got.FullName != "acme/tap/widget" {
		t.Fatalf("Info = %#v", got)
	}
}

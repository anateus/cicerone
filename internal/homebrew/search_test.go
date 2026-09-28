package homebrew

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/anateus/cicerone/internal/domain"
	"github.com/anateus/cicerone/internal/execx"
	"github.com/anateus/cicerone/internal/testutil"
)

type catalogRunner struct {
	calls   []testutil.Call
	outputs map[string]string
	errors  map[string]error
}

func (r *catalogRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	r.calls = append(r.calls, testutil.Call{Name: name, Args: append([]string(nil), args...)})
	key := strings.Join(args, " ")
	return execx.Result{Stdout: []byte(r.outputs[key])}, r.errors[key]
}

func (*catalogRunner) Stream(context.Context, string, ...string) (io.ReadCloser, error) {
	panic("unexpected Stream")
}

func TestSearchCatalogCombinesNamesAndDescriptionsWithTypes(t *testing.T) {
	runner := &catalogRunner{outputs: map[string]string{
		"search --formula solve": "cbc\nacme/tap/solver\n==> Formulae\n",
		"search --cask solve":    "flow5\n",
		"search --desc solve": `==> Formulae
cbc: Mixed integer solver
acme/tap/solver: Helpful solver: with constraints
new-formula: Newly listed solver

==> Casks
flow5: Flow solver
other-cask: Additional solver
`,
	}}
	got, err := NewClient(runner).SearchCatalog(context.Background(), "solve", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []CatalogMatch{
		{ID: "cbc", Type: domain.PackageFormula, Description: "Mixed integer solver"},
		{ID: "acme/tap/solver", Type: domain.PackageFormula, Description: "Helpful solver: with constraints"},
		{ID: "flow5", Type: domain.PackageCask, Description: "Flow solver"},
		{ID: "new-formula", Type: domain.PackageFormula, Description: "Newly listed solver"},
		{ID: "other-cask", Type: domain.PackageCask, Description: "Additional solver"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("catalog = %#v, want %#v", got, want)
	}
	if len(runner.calls) != 3 || !slices.Equal(runner.calls[2].Args, []string{"search", "--desc", "solve"}) {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestSearchCatalogKeepsPartialResultsWhenOneTypeHasNoMatches(t *testing.T) {
	runner := &catalogRunner{outputs: map[string]string{"search --formula ripgrep": "ripgrep\n"},
		errors: map[string]error{"search --cask ripgrep": errors.New("no casks found")}}
	got, err := NewClient(runner).SearchCatalog(context.Background(), "ripgrep", false)
	if err != nil || len(got) != 1 || got[0].ID != "ripgrep" || len(runner.calls) != 2 {
		t.Fatalf("catalog = %#v, calls = %#v, err = %v", got, runner.calls, err)
	}
}

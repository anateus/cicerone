package upstream

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"cicerone/internal/execx"
)

type tagRunner struct {
	result execx.Result
	name   string
	args   []string
}

func (r *tagRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return r.result, nil
}

func (*tagRunner) Stream(context.Context, string, ...string) (io.ReadCloser, error) {
	panic("unused")
}

func TestRepositoryTagsListsProviderIndependentGitTags(t *testing.T) {
	runner := &tagRunner{result: execx.Result{Stdout: []byte(strings.Join([]string{
		"aaa\trefs/tags/v2.0.0",
		"bbb\trefs/tags/v1.0.0",
		"ccc\trefs/heads/main",
		"ddd\trefs/tags/v2.0.0",
	}, "\n"))}}

	got, err := RepositoryTags(context.Background(), "https://codeberg.org/acme/widget", runner)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"v1.0.0", "v2.0.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RepositoryTags = %v, want %v", got, want)
	}
	if runner.name != "git" || !reflect.DeepEqual(runner.args, []string{
		"ls-remote", "--tags", "--refs", "https://codeberg.org/acme/widget",
	}) {
		t.Fatalf("runner call = %q %v", runner.name, runner.args)
	}
}

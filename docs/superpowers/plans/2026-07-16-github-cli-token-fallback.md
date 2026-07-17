# GitHub CLI Token Fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Use a GitHub CLI authentication token for production GitHub API requests when `GITHUB_TOKEN` is empty.

**Architecture:** The changelog resolver gains an in-memory token and an opt-in constructor option that resolves credentials exactly once. Production wiring supplies the existing direct subprocess runner; tests either omit the option or inject a fake runner, so they never read a developer's GitHub CLI configuration.

**Tech Stack:** Go 1.26, Go standard library HTTP, existing `internal/execx` direct subprocess runner, GitHub CLI.

## Global Constraints

- A nonblank `GITHUB_TOKEN` always takes precedence over GitHub CLI.
- When `GITHUB_TOKEN` is blank, invoke `gh` directly with the exact argument slice `auth`, `token`.
- Missing, failing, unauthenticated, or empty-output GitHub CLI lookup is nonfatal and leaves requests unauthenticated.
- Resolve credentials once during resolver construction; never once per request.
- Tokens remain in memory and are never persisted, printed, or logged.
- Tests must never consult real GitHub CLI configuration.
- Subprocesses use direct argument slices and never invoke a shell.

---

### Task 1: Add Opt-In GitHub CLI Token Resolution

**Files:**
- Modify: `internal/changelog/resolver.go`
- Modify: `internal/changelog/resolver_test.go`
- Modify: `cmd/cicerone/main.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `execx.Runner.Run(context.Context, string, ...string) (execx.Result, error)`.
- Produces: `WithGitHubTokenRunner(execx.Runner) ResolverOption` and an in-memory resolver token used by GitHub requests.
- Preserves: `NewResolver(cache *store.Store, client *http.Client) *Resolver` remains hermetic unless the production-only option is passed.

- [ ] **Step 1: Write failing credential-resolution tests**

Add a recording `execx.Runner` fake whose `Run` captures the command and arguments and whose `Stream` returns an unused-test error. Add focused tests that construct resolvers with `WithGitHubTokenRunner(fake)` and verify:

```go
func TestResolverGitHubTokenPrefersEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", " environment-token ")
	runner := &recordingRunner{result: execx.Result{Stdout: []byte("cli-token\n")}}
	r := NewResolver(nil, nil, WithGitHubTokenRunner(runner))
	if r.githubToken != "environment-token" { t.Fatalf("token=%q", r.githubToken) }
	if len(runner.calls) != 0 { t.Fatalf("calls=%v", runner.calls) }
}

func TestResolverGitHubTokenFallsBackToCLI(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	runner := &recordingRunner{result: execx.Result{Stdout: []byte("cli-token\n")}}
	r := NewResolver(nil, nil, WithGitHubTokenRunner(runner))
	if r.githubToken != "cli-token" { t.Fatalf("token=%q", r.githubToken) }
	if diff := cmp.Diff([]runnerCall{{name: "gh", args: []string{"auth", "token"}}}, runner.calls); diff != "" { t.Fatal(diff) }
}
```

Add table cases for a runner error and whitespace-only stdout; both must leave `githubToken` empty. Construct one resolver, perform two requests against an `httptest.Server`, and assert the runner was called only once and both requests carried `Authorization: Bearer cli-token`.

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/changelog -run 'TestResolverGitHubToken' -count=1 -v`

Expected: FAIL because `WithGitHubTokenRunner`, `ResolverOption`, and `githubToken` do not exist.

- [ ] **Step 3: Implement one-time opt-in resolution**

Extend construction without changing existing hermetic callers:

```go
type ResolverOption func(*Resolver)

func WithGitHubTokenRunner(runner execx.Runner) ResolverOption {
	return func(r *Resolver) {
		r.githubToken = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
		if r.githubToken != "" || runner == nil { return }
		result, err := runner.Run(context.Background(), "gh", "auth", "token")
		if err == nil { r.githubToken = strings.TrimSpace(string(result.Stdout)) }
	}
}

func NewResolver(cache *store.Store, client *http.Client, options ...ResolverOption) *Resolver {
	if client == nil { client = http.DefaultClient }
	r := &Resolver{Store: cache, Client: client, APIBaseURL: "https://api.github.com", Now: func() time.Time { return time.Now().UTC() }}
	for _, option := range options { option(r) }
	return r
}
```

Add unexported `githubToken string` to `Resolver`. Change `request` to use that field rather than reading the environment per request:

```go
if r.githubToken != "" {
	req.Header.Set("Authorization", "Bearer "+r.githubToken)
}
```

In production construction, pass `WithGitHubTokenRunner(execx.NewRunner())` to `NewResolver`. Keep command invocation direct and do not surface lookup errors.

- [ ] **Step 4: Update documentation**

Replace the GitHub access paragraph with text stating that `GITHUB_TOKEN` takes precedence, Cicerone otherwise tries `gh auth token`, unavailable authentication falls back to the public rate-limited API, and tokens are never persisted or printed.

- [ ] **Step 5: Verify GREEN and regressions**

Run:

```bash
go test -race ./internal/changelog ./cmd/cicerone -count=1
go test -race ./... -count=1
test -z "$(gofmt -l internal/changelog/resolver.go internal/changelog/resolver_test.go cmd/cicerone/main.go)"
git diff --check
```

Expected: all commands exit 0, tests do not invoke a real `gh`, and output is pristine.

- [ ] **Step 6: Commit**

```bash
git add internal/changelog/resolver.go internal/changelog/resolver_test.go cmd/cicerone/main.go README.md
git commit -m "feat: use GitHub CLI token fallback"
```

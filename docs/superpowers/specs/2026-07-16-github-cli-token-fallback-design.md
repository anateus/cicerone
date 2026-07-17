# GitHub CLI Token Fallback Design

## Goal

Increase the authenticated GitHub API rate limit for users who already authenticate with GitHub CLI, without overriding `GITHUB_TOKEN`, making authentication mandatory, or exposing credentials.

## Credential Resolution

The changelog resolver resolves one GitHub token when it is constructed. A nonblank `GITHUB_TOKEN` is used first. When that variable is blank or absent, Cicerone invokes `gh` directly with the argument slice `auth token` and trims standard output.

If `gh` is unavailable, the user is not authenticated, the command fails, or its output is blank, construction continues without a token. This fallback is opportunistic: GitHub's public API remains usable without authentication and an unavailable credential source must not prevent Cicerone from starting.

The resolved token is held only in memory. Cicerone must not persist, print, or log it. GitHub API requests use the existing `Authorization: Bearer <token>` header behavior when a token is available.

## Boundaries

Token discovery belongs at resolver construction rather than in individual requests. This avoids spawning `gh` repeatedly and makes request behavior deterministic. The command must use the existing direct subprocess runner or an equivalently injectable direct-execution seam; it must never invoke a shell.

The feature does not add token refresh, interactive authentication, rate-limit-specific retries, new configuration, or an error when no credential can be found.

## Testing

Tests inject command execution and never consult a developer's real GitHub CLI configuration. They cover:

- A nonblank `GITHUB_TOKEN` takes precedence and does not invoke `gh`.
- With no environment token, successful `gh auth token` output becomes the bearer token.
- A missing, failing, unauthenticated, or empty-output `gh` command leaves requests unauthenticated without failing resolver construction.
- Token lookup occurs once per resolver construction rather than once per request.
- Command invocation uses the exact direct argument slice `auth`, `token`.

The existing authenticated and unauthenticated GitHub request tests remain green. README documentation explains precedence, the optional GitHub CLI fallback, and that credentials are neither persisted nor printed.

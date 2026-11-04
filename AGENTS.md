# AGENTS.md

Instructions for coding agents and contributors working in this repository.
Human contributors: read [CONTRIBUTING.md](CONTRIBUTING.md) first; this file
is the short form.

## Project

tokenwarden is a Go library and reference gateway that governs LLM traffic to
the Anthropic Messages API: distributed token budgets, concurrency caps,
default-deny policy, and an audit trail.

## Setup

```sh
make setup   # installs pinned tools with mise and the lefthook git hooks
make check   # lint, test -race, build, tidy, govulncheck, gitleaks
```

## Rules

- Tool versions live in `mise.toml`; library versions live in `go.mod`. Pin
  exact stable releases. Never use `latest` or pre-releases.
- Formatting is `gofumpt` + `goimports` through `golangci-lint fmt`. Linting is
  `golangci-lint run` with `.golangci.yaml`. Do not disable a linter inline
  without a comment that explains why.
- Every exported identifier has a doc comment. Inside functions, comment only to
  explain why, not what.
- Prefer the standard library. A new dependency needs a reason in the PR.
- Keep the public API small. Code that is not part of the API goes in
  `internal/`.
- Tests: table-driven where it helps, `-race` always, `testing/synctest` for
  time-dependent code. No sleeps to wait for state.
- Never log `Authorization`, `x-api-key`, or prompt bodies.
- No secrets anywhere. Test keys use the `tw_test_` or `sk-ant-test-` prefix.

## Git

- Conventional Commits for commits and PR titles. The `commit-msg` hook checks.
- Branches: `feat/`, `fix/`, `docs/`, `test/`, `refactor/`, `perf/`, `build/`,
  `ci/`, `chore/` followed by a short topic.
- One change per PR. Squash merge. Never bypass hooks with `--no-verify`.

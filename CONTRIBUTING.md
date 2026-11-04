# Contributing to tokenwarden

Thank you for your interest. This guide covers setup, conventions, and the
pull request process.

## Prerequisites

- [mise](https://mise.jdx.dev) to install the pinned toolchain (Go,
  golangci-lint, lefthook, gitleaks, and others listed in `mise.toml`).
- Docker, for integration tests and the local demo stack.

## Setup

```sh
git clone https://github.com/NimbleEngineer21/tokenwarden.git
cd tokenwarden
make setup
```

`make setup` installs the tools and the git hooks. The hooks run:

| Hook | Checks |
|---|---|
| `pre-commit` | formatting, lint, secret scan, workflow lint |
| `commit-msg` | Conventional Commit subject |
| `pre-push` | `go mod tidy`, build, tests with `-race` |

Run every check that CI runs with `make check`.

## Code style

- Follow [Effective Go](https://go.dev/doc/effective_go),
  [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments), and the
  [Google Go Style Guide](https://google.github.io/styleguide/go/).
- `golangci-lint` enforces formatting (`gofumpt`, `goimports`) and the linters
  in `.golangci.yaml`.
- Every exported identifier has a doc comment. Other comments explain why, not
  what.
- Prefer the standard library. Explain any new dependency in the PR
  description.
- Return errors with context (`fmt.Errorf("reserve tokens: %w", err)`). Do not
  log and return the same error.

## Tests

- Run `make test`. Tests always use the race detector.
- Use `testing/synctest` for code that depends on time.
- Integration tests that need Postgres read `TOKENWARDEN_TEST_DATABASE_URL` and
  skip when it is not set.

## Commits and branches

- Use [Conventional Commits](https://www.conventionalcommits.org):
  `feat(limit): add lease reaper`.
- Name branches `<type>/<topic>`, for example `feat/lease-reaper`.
- Keep each PR to one change. Aim for under 400 lines of non-test code.

## Pull requests

1. Branch from the latest `main`.
2. Run `make check`.
3. Open a PR with a Conventional Commit title. The PR is squash-merged, so the
   title becomes the commit message on `main`.
4. Fill in the PR template: what, why, how you tested it.
5. All CI checks must pass and all review conversations must be resolved.

## Design decisions

Significant design decisions are recorded as ADRs in [docs/adr](docs/adr).
Add one when a change makes a decision that is hard to reverse.

## Security

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).

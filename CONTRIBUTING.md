# Contributing to Agent Kourier

Thanks for helping. Agent Kourier is prerelease software with one maintainer, so a short issue
before a large change saves both of us time.

## Before you start

- **Bugs and questions:** open an issue with the version or commit, your deployment (Helm or a
  container, SQLite or Postgres), the agent backend, and what you saw. Redact tokens and private
  messages.
- **Security problems:** do not open an issue. Follow [SECURITY.md](SECURITY.md).
- **Features and larger changes:** open an issue first. A change that adds a Go module dependency,
  changes the `v1alpha1` resource format, or touches the chat or agent contracts needs agreement
  before code.

## Build and test

Use the Go version declared in [go.mod](go.mod). The tools the build needs, including
golangci-lint, are pinned in `go.mod` and run through `go tool`, so nothing else needs a global
install.

```sh
make fmt lint test build
```

`make` with no target lists every target. The Helm chart tests skip without `helm` on your `PATH`. The
end-to-end tiers (`make e2e-fast-up`, then `make e2e-fast`) need Docker and kind.

## Pull requests

- One concern per pull request. Split a change that mixes a fix, a refactor and a feature.
- Write the test first where you can. Unit tests use the standard library `testing` package,
  table-driven where it fits.
- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit messages and the
  pull request title, for example `fix(session): keep the reply queue across a reload`.
- Opening a pull request asks the code owner in [.github/CODEOWNERS](.github/CODEOWNERS) for a
  review.
- Never commit a real token, Secret or private message. Test credentials are fake constants.
- Signing your commits is optional. `main` accepts only squash merges and requires signed commits,
  and the squash commit is the one GitHub creates and signs when the pull request merges; the
  commits on your branch are not on `main`. To have your own commits shown as Verified, see
  GitHub's guide to [signing commits](https://docs.github.com/en/authentication/managing-commit-signature-verification/signing-commits).

## Checks

A pull request merges into `main` only when these checks pass and its branch is up to date with
`main`:

- From `ci`: `lint` (`make fmt` leaves no diff, `make lint`, `make docs-check`, `make build`),
  `test` (`make test-race` with helm installed, so the chart tests cannot skip, then
  `make test-scripts`), `test-testhooks` (`make test-testhooks`) and `docs-agent` (the tests and
  lint of the separate module under `examples/recipes/docs-agent/agent`).
- From `security`: `govulncheck (root)`, `govulncheck (ax)` and `govulncheck (docs-agent)` (one
  per Go module), `gitleaks` (a secret scan of the whole git history), and `trivy (linux-amd64)` and
  `trivy (linux-arm64)` (a vulnerability scan of the production image built for each platform).
- From `codeql`: `analyze (go)` (CodeQL code scanning).

The `docs` workflow (`build`, the strict site build, and `links`, an external link check) runs only
when a pull request touches the documentation sources, and its checks are not required; keep them
green anyway.

From a fork, a maintainer approves your pull request's workflow runs before they start: the
repository requires approval for every outside contributor.

The content check (`make docs-check`) has a rule that looks for workspace identifiers, and its
pattern comes from a repository secret. A pull request from a fork cannot read the secret, so on
that pull request the rule does not run and the check prints a notice saying so; the rule runs
again on the push to `main` after the merge. You do not need to set `CONTENTCHECK_WORKSPACE`
locally.

## Code conventions

- `context.Context` is the first parameter of anything that does I/O.
- Wrap errors with `%w` and enough context to find the call site.
- No global mutable state, and no `panic` outside `main` and test helpers.
- Exported identifiers have doc comments.
- Only the Slack adapter (`internal/chat/slack`), the broker, `cmd` and tests may import Slack
  packages. The linter enforces this.

## Documentation

Docs live in `docs/` and publish to [agentkourier.dev](https://agentkourier.dev). Every statement
of behaviour must match the code: open the code path and read it, rather than copying a sentence
from another page. Say what the code does ("denies", "queues", "retries") rather than a near
synonym.

Before pushing a docs change, run:

```sh
make docs-ref docs-check docs-build
```

`docs-ref` regenerates `docs/reference/` from the API types and the chart values, so edit those
sources rather than the generated pages.

## Conduct

Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Contributions are made under the [Apache License 2.0](LICENSE), the project's license.

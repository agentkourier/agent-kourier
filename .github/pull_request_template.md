<!--
Title: a Conventional Commit, for example `fix(session): keep the reply queue across a reload`.
One concern per pull request. A change to the v1alpha1 format, a new Go module dependency, or the chat or agent
contracts needs an agreed issue first. See CONTRIBUTING.md.
-->

## What and why

<!-- What changes, and the problem it solves. Link the issue: "Closes #123". -->

## How it was tested

<!-- The commands you ran and what they showed. Say what you could not test. -->

- [ ] `make fmt lint test build`
- [ ] Docs changed: `make docs-ref docs-check docs-build`
- [ ] Chart changed: chart tests ran with `helm` on the `PATH`
- [ ] End-to-end, if the change needs a cluster: `make e2e-fast` (or the tier it touches)

## Checklist

- [ ] Tests cover the change, written first where possible.
- [ ] Every statement of behaviour the docs gain or change was checked against the code, not copied from another page.
- [ ] No real token, Secret, private message or workspace identifier in code, tests, fixtures or this description.
- [ ] A release pull request also sets `version` in `charts/agent-kourier/Chart.yaml`, and `make docs-check` passes
      (see RELEASING.md).

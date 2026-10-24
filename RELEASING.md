# Releasing

A release is a pushed tag `vX.Y.Z` (or `vX.Y.Z-rc.1`). It publishes the image `ghcr.io/agentkourier/agent-kourier`
(linux/amd64 and linux/arm64) and the Helm chart `oci://ghcr.io/agentkourier/charts/agent-kourier`, whose default image
is that image by digest. Nothing is released by hand.

## Cut a release

1. Merge what ships to `main`.
2. Open a release pull request that sets `version` in `charts/agent-kourier/Chart.yaml` to `X.Y.Z` and every
   release version the public docs name to match. `make docs-check` lists each page that still names the old version
   (`release-version`), so the pull request cannot go green with a page left behind. The chart's golden files carry
   the version in a label: refresh them with `go test ./test/chart -update` (with `helm` installed). For a new minor
   version (`X.Y.0`), also add `"X.Y"` at the top of `hack/docs/versions.yaml`, so the documentation site publishes
   the new release line; until its tag exists the site skips it. Merge it. Skip this for a prerelease tag
   (`vX.Y.Z-rc.1`): while one is tried, the chart and the docs keep naming the last release.
3. Before tagging, confirm that `ci.yml`, `security.yml` and `codeql.yml` have completed successfully for that
   exact commit from a push to `main`. The release gate checks the same commit and the latest attempt again; a
   missing, pending, failed or pull-request-only run blocks the release.
4. `git tag vX.Y.Z <the release pull request's commit on main>` and `git push origin vX.Y.Z`.
5. Watch the `release` workflow. Its job summary and the GitHub Release body name the pushed image and chart
   digests. A GitHub Release is created only after both signatures verify.

If a first release attempt fails before its GitHub Release is created, rerun that tag after fixing the failure.
Once a published or draft GitHub Release exists for the tag, the read-only gate refuses to rebuild it.

The documentation site builds each release line from its newest final tag, whether or not that tag has a GitHub
Release. A docs deploy from `main` while a failed attempt waits for its rerun publishes that tag's docs early. A final
tag that is never released, such as one the gate refused, must be deleted, or the site publishes its docs as that
line's.

## What the workflow does

`.github/workflows/release.yml` runs `hack/release/release.sh`, the script behind `make release-image`:

- First checks the exact tagged commit through the Actions API. `ci.yml` must contain successful `lint`, `test`,
  `test-testhooks` and `docs-agent` jobs. `security.yml` must contain successful `govulncheck (root)`,
  `govulncheck (ax)`, `govulncheck (docs-agent)`, `gitleaks`, `trivy (linux-amd64)` and `trivy (linux-arm64)` jobs.
  `codeql.yml` must contain a successful `analyze (go)` job. These are the status checks the `main` ruleset
  requires. `codeql.yml` skips `analyze` while the repository is private, and a skipped run blocks the release.
  The scheduled released-image rescan is the only permitted skipped job. A pending run fails closed with a message
  to wait; the gate never reruns or changes a workflow run.
- Refuses a final tag `vX.Y.Z` unless `charts/agent-kourier/Chart.yaml` at the tagged commit says `version: X.Y.Z`
  and `hack/docs/versions.yaml` lists `X.Y` (`hack/release/check-version.sh`), so no release goes out before the pull
  request that points the docs at it, or without a place on the documentation site.
- Checks the Releases API before granting the build job access to the registry. A 404 means this is the first
  release attempt; a published or draft Release, an authorization failure or any other API error stops the workflow.
- Refuses a tag that is not on `main`.
- Builds each platform with the Dockerfile, which cross-compiles Go on the runner (no emulation) and sets
  `main.version` from the tag.
- Refuses to release a platform whose binary carries a test hook. The Dockerfile's `verify` stage runs
  `internal/testhooks` (the traces `make image-check` looks for, with a testhooks build of the same
  platform as the control) and checks the binary is an ELF of that architecture with the tag as its
  version; the image is not assembled if it fails. The script then checks each platform's image has no
  `dev.agent-kourier.testhooks` label, runs as 65532:65532, and is that platform.
- Pushes one multi-platform image tagged `vX.Y.Z` with the workflow's `GITHUB_TOKEN` (`packages: write`),
  with an SBOM and a max-mode provenance attestation attached, and the OCI labels source, revision, version
  and created (the commit's time).
- The Buildx action is pinned by commit, while its BuildKit image and Docker's SBOM generator follow the action's
  defaults. Pin those tool references before claiming byte-for-byte build-tool reproducibility.
- Installs the pinned Trivy release from `hack/security/install-trivy.sh` and scans each local platform image before
  the multi-platform push. High and critical vulnerabilities, including unfixed findings, fail the release and the
  SARIF reports remain in the release output.
- Packages `charts/agent-kourier` (`hack/release/package-chart.sh`) with version `X.Y.Z`, appVersion `vX.Y.Z`, and
  `image.repository` and `image.digest` stamped with the pushed image, reads the package back to check them,
  and pushes it to `oci://ghcr.io/agentkourier/charts`.
- In a second job that runs no repository code (the only one with `id-token: write`), signs the image digest
  and the chart digest with cosign v3.0.6 keyless (GitHub's OIDC identity; no key, no secret).
- In a separate read-only verification job, verifies both signatures with the OIDC issuer and the exact identity
  `https://github.com/agentkourier/agent-kourier/.github/workflows/release.yml@refs/tags/<tag>`.
- In a final job with `contents: write` and no checkout, creates the GitHub Release with generated notes and the
  verified digests. `.github/release.yml` groups the notes by the labels `pr-labels.yml` sets from each pull
  request's title, and leaves out release pull requests (`chore(release): ...`). Re-running a completed release leaves matching release information unchanged; conflicting
  tag, prerelease state or digest information fails rather than being overwritten.
- For a final tag, after the Release exists, starts `docs.yml` on `main` (`actions: write`, no checkout), which
  rebuilds the documentation site with the new tag as its release line's version. A prerelease is not published there.

The job summary lists both digests. The chart's `tag` stays empty: the digest wins.

## Build it locally

`make release-image VERSION=v0.0.0-rc.1` builds both platforms, runs the same checks, and writes an OCI
archive and the chart package to `bin/release/`. It pushes nothing, and needs helm and jq. It needs a buildx builder that can make multi-platform
images with attestations, which Docker's default `docker` driver without the containerd image store cannot:

    docker buildx create --name agent-kourier-release --driver docker-container
    BUILDER=agent-kourier-release make release-image VERSION=v0.0.0-rc.1

Set `SCAN_IMAGES=1` to run the same per-platform Trivy policy for a local build. `PUSH=1` always enables these
scans and requires the pinned scanner from `hack/security/install-trivy.sh` (or an executable `TRIVY_BIN`).

`PUSH=1` also pushes, and only with explicit `REGISTRY=<repository>` and `CHART_REGISTRY=oci://<repository>` (no
defaults), a version tag that exists at a clean `HEAD`, and a registry login you made yourself. The workflow is the
normal way.

## Verify a release

    IMAGE=ghcr.io/agentkourier/agent-kourier
    TAG=vX.Y.Z
    DIGEST=sha256:...   # from the job summary, or: docker buildx imagetools inspect $IMAGE:$TAG
    IDENTITY="https://github.com/agentkourier/agent-kourier/.github/workflows/release.yml@refs/tags/${TAG}"

    cosign verify "$IMAGE@$DIGEST" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      --certificate-identity "$IDENTITY"

    docker buildx imagetools inspect "$IMAGE@$DIGEST" --format '{{ json .SBOM }}'
    docker buildx imagetools inspect "$IMAGE@$DIGEST" --format '{{ json .Provenance }}'

Consumers should inspect the BuildKit provenance and build inputs, then compare the reported revision with the
tagged commit. The image's `org.opencontainers.image.revision` label names that same commit. The chart is
signed by the same identity, and its default image is the image:

    CHART=ghcr.io/agentkourier/charts/agent-kourier
    CHART_DIGEST=sha256:...   # from the job summary
    cosign verify "$CHART@$CHART_DIGEST" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      --certificate-identity "$IDENTITY"
    helm show values oci://$CHART --version X.Y.Z | grep -A8 '^image:'   # repository and digest stamped

Each release's GitHub Release body names the verified image and chart digests.

## Roll back

Deploy by digest, never by tag. To roll back, set the Deployment's image (or the chart's image value) back
to the previous release's `@sha256:...`, or install the previous chart version (its default image is that release's
digest); the digests of past releases are in their workflow job summaries. A released chart pins its image by digest,
so `--set image.tag=...` alone changes nothing: set `image.digest` to the other release's digest (or `image.digest=""`
with the tag).

If a release fails after its image is pushed (the chart packaging, signing or verification failed), the image tag is
in GHCR without a verified GitHub Release. An unsigned digest is not a release: deploy only digests that `cosign verify`
accepts, and re-run or cut a new tag.
A bad release is not deleted or retagged: cut a new tag with the fix.

## Who can release: repository settings

The workflow runs the files in the tagged commit, so a workflow cannot police who tags; the repository settings
do. They are the real control, and they are settings, not code: the owner applies them (Settings, Rules). These
are applied now:

1. The `main` ruleset (target: the default branch, enforcement Active):
   - Require a pull request before merging, with 0 approvals: the project has one maintainer, who cannot approve
     their own pull request. Squash is the only allowed merge method.
   - Require signed commits. A squash merge in GitHub is signed by GitHub, so every commit merged this way passes.
   - Require these status checks, with the branch up to date before merging (strict): `lint`, `test`,
     `test-testhooks` and `docs-agent` from `ci`; `govulncheck (root)`, `govulncheck (ax)`,
     `govulncheck (docs-agent)`, `gitleaks`, `trivy (linux-amd64)` and `trivy (linux-arm64)` from `security`; and
     `analyze (go)` from `codeql`. The scheduled released-image rescan is intentionally not a required check.
   - Block force pushes and restrict deletions.
   - Bypass: repository admins, for pull requests only. An admin can merge a pull request whose requirements are
     not met, but cannot push to `main` directly.
2. The `release tags` ruleset (target: tags matching `refs/tags/v*`, enforcement Active): Restrict creations,
   Restrict updates, Restrict deletions and Block force pushes, with repository admins on the bypass list
   (always). Only an admin can create a release tag, and a released tag cannot be moved or deleted.
3. Actions: Workflow permissions "Read repository contents" by default (the workflows ask for more by job).
   Optionally add a `release` environment with required reviewers and move the `build` job into it, which
   puts a human approval between a tag and a push.
4. After the first release: Packages, `agent-kourier` and `charts/agent-kourier`, link them to this repository and set
   visibility (public if the cluster pulls without a pull secret).

The release workflow refuses a tag that is not on `main`, so what ships has passed the `main` ruleset. Cut a
release tag only from a commit that GitHub signed when it squash-merged a pull request (the commit shows as
Verified on GitHub); nothing in the workflow checks this. The commits on `main` from before signed commits were
required, the tip of `main` at that time included, are unsigned, so the first release is cut from a later squash
merge. The release gate (`hack/release/check-ci.sh`) checks the same jobs as the `main` ruleset, on the push run
for the tagged commit, as listed under [What the workflow does](#what-the-workflow-does); a change to either list
belongs with a change to the other.

## CI

`.github/workflows/ci.yml` runs `make fmt` (no diff), `make lint` and `make build` (the `lint` job), and what `make test`
runs as two more jobs side by side, `make test-race` with `make test-scripts` (`test`) and `make test-testhooks`
(`test-testhooks`), on pull requests and pushes to `main`. The `test` job has helm installed and `AGENTKOURIER_REQUIRE_HELM=1`
so the chart tests cannot skip. The `zizmor` job (`make lint-workflows`, `hack/security/lint-workflows.sh`) audits
every workflow with a pinned zizmor under the policy in `.github/zizmor.yml` and fails on any finding; it first
proves the policy still fails a set of unsafe fixtures (`hack/security/testdata/zizmor`), and then that the inline
ignore comments hide only the findings the script lists. It runs with `contents: read` and uploads no SARIF, so it
behaves the same on a fork's pull request. It belongs in the `main` ruleset's required checks: the owner adds
`zizmor` from `ci` there, and the same change adds it to `CI_REQUIRED_JOBS` in `hack/release/check-ci.sh` and to
the lists above. Each Go job in `ci.yml`, `security.yml`, `codeql.yml` and `docs.yml` restores a cache of its own
(the module cache and the build cache, plus golangci-lint's cache in the jobs that lint), keyed on the job, the runner
OS, the Go version and the `go.sum` files it builds from. Only a job on a push to `main` saves one, when its exact key
is not saved yet, whether its steps passed or failed (a cancelled job saves nothing); pull requests read `main`'s caches
and save none, so a pull request runs cold until a push run on `main` has saved them. The jobs that install Python
packages, `echo-agent` in `ci.yml` and `build` in `docs.yml`, keep pip's cache under the same rules, keyed on the job,
the runner OS, the Python version and the requirements files they install from. `docs.yml` runs on a push to `main`
only when the push changes a path in its `push` trigger, so only such a push saves the `build` job's caches. Each job has its own timeout, set from the times seen on a cold cache. The kind
tiers are not in CI. They would need a runner with Docker and about 8 GB free, the `kind`, `kubectl`,
`helm` and `jq` binaries installed at pinned versions (`hack/ci/install-kind-tools.sh` installs them, each checked
against a pinned sha256), and the pinned node and base images pulled (the tiers
take minutes to bring up, so they are best as a separate nightly or label-triggered workflow, not on every PR).

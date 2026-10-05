# Releasing

A release is a pushed tag `vX.Y.Z` (or `vX.Y.Z-rc.1`). It publishes the image `ghcr.io/agentkourier/agent-courier`
(linux/amd64 and linux/arm64) and the Helm chart `oci://ghcr.io/agentkourier/charts/courier`, whose default image
is that image by digest. Nothing is released by hand.

## Cut a release

1. Merge what ships to `main`; wait for `ci` to pass on it.
2. `git tag vX.Y.Z <commit on main>` and `git push origin vX.Y.Z`.
3. Watch the `release` workflow. Its job summary names the pushed digest.

## What the workflow does

`.github/workflows/release.yml` runs `hack/release/release.sh`, the script behind `make release-image`:

- Refuses a tag that is not on `main`.
- Builds each platform with the Dockerfile, which cross-compiles Go on the runner (no emulation) and sets
  `main.version` from the tag.
- Refuses to release a platform whose binary carries a test hook. The Dockerfile's `verify` stage runs
  `internal/testhooks` (the traces `make image-check` looks for, with a testhooks build of the same
  platform as the control) and checks the binary is an ELF of that architecture with the tag as its
  version; the image is not assembled if it fails. The script then checks each platform's image has no
  `dev.agent-courier.testhooks` label, runs as 65532:65532, and is that platform.
- Pushes one multi-platform image tagged `vX.Y.Z` with the workflow's `GITHUB_TOKEN` (`packages: write`),
  with an SBOM and a max-mode provenance attestation attached, and the OCI labels source, revision, version
  and created (the commit's time).
- Packages `charts/courier` (`hack/release/package-chart.sh`) with version `X.Y.Z`, appVersion `vX.Y.Z`, and
  `image.repository` and `image.digest` stamped with the pushed image, reads the package back to check them,
  and pushes it to `oci://ghcr.io/agentkourier/charts`.
- In a second job that runs no repository code (the only one with `id-token: write`), signs the image digest
  and the chart digest with cosign keyless (GitHub's OIDC identity; no key, no secret).

The job summary lists both digests. The chart's `tag` stays empty: the digest wins.

## Build it locally

`make release-image VERSION=v0.0.0-rc.1` builds both platforms, runs the same checks, and writes an OCI
archive and the chart package to `bin/release/`. It pushes nothing, and needs helm and jq. It needs a buildx builder that can make multi-platform
images with attestations, which Docker's default `docker` driver without the containerd image store cannot:

    docker buildx create --name courier-release --driver docker-container
    BUILDER=courier-release make release-image VERSION=v0.0.0-rc.1

`PUSH=1` also pushes, and only with explicit `REGISTRY=<repository>` and `CHART_REGISTRY=oci://<repository>` (no
defaults), a version tag that exists at a clean `HEAD`, and a registry login you made yourself. The workflow is the
normal way.

## Verify a release

    IMAGE=ghcr.io/agentkourier/agent-courier
    DIGEST=sha256:...   # from the job summary, or: docker buildx imagetools inspect $IMAGE:vX.Y.Z

    cosign verify "$IMAGE@$DIGEST" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      --certificate-identity-regexp '^https://github\.com/agentkourier/agent-kourier/\.github/workflows/release\.yml@refs/tags/v'

    docker buildx imagetools inspect "$IMAGE@$DIGEST" --format '{{ json .SBOM }}'
    docker buildx imagetools inspect "$IMAGE@$DIGEST" --format '{{ json .Provenance }}'

The provenance names the commit and the workflow run; the image's `org.opencontainers.image.revision`
label names the same commit. The chart is signed by the same identity, and its default image is the image:

    CHART=ghcr.io/agentkourier/charts/courier
    CHART_DIGEST=sha256:...   # from the job summary
    cosign verify "$CHART@$CHART_DIGEST" <the same two flags>
    helm show values oci://$CHART --version X.Y.Z | grep -A8 '^image:'   # repository and digest stamped

## Roll back

Deploy by digest, never by tag. To roll back, set the Deployment's image (or the chart's image value) back
to the previous release's `@sha256:...`, or install the previous chart version (its default image is that release's
digest); the digests of past releases are in their workflow job summaries. A released chart pins its image by digest,
so `--set image.tag=...` alone changes nothing: set `image.digest` to the other release's digest (or `image.digest=""`
with the tag).

If a release fails after its image is pushed (the chart packaging or signing failed), the image tag is in GHCR
unsigned. An unsigned digest is not a release: deploy only digests that `cosign verify` accepts, and re-run or cut a
new tag.
A bad release is not deleted or retagged: cut a new tag with the fix.

## Who can release: repository settings

The workflow runs the files in the tagged commit, so a workflow cannot police who tags; the repository settings
do. They are the real control, and they are settings, not code: the owner applies them (Settings, Rules).

1. A tag ruleset: Target tags, pattern `v*`, enforcement Active, bypass list limited to the maintainers' team
   (or the owner). Rules: Restrict creations, Restrict updates, Restrict deletions, Block force pushes. Only
   maintainers can create a release tag, and a released tag cannot be moved or deleted.
2. Branch protection (a ruleset or classic rule) on `main`: Require a pull request before merging, with one
   approval; Require status checks to pass (`check` from `ci`) and the branch up to date; Block force pushes;
   Restrict deletions. The release workflow also refuses a tag that is not on `main`, so what ships has
   passed this.
3. Actions: Workflow permissions "Read repository contents" by default (the workflows ask for more by job).
   Optionally add a `release` environment with required reviewers and move the `build` job into it, which
   puts a human approval between a tag and a push.
4. After the first release: Packages, `agent-courier` and `charts/courier`, link them to this repository and set
   visibility (public if the cluster pulls without a pull secret).

## CI

`.github/workflows/ci.yml` runs `make fmt` (no diff), `make lint` and `make build` (the `lint` job), and what `make test`
runs as two more jobs side by side, `make test-race` with `make test-scripts` (`test`) and `make test-testhooks`
(`test-testhooks`), on pull requests and pushes to `main`. The `test` job has helm installed and `COURIER_REQUIRE_HELM=1`
so the chart tests cannot skip. Each job has its own timeout, set from the times seen on a cold cache. The kind
tiers are not in CI. They would need a runner with Docker and about 8 GB free, the `kind`, `kubectl`,
`helm` and `jq` binaries installed at pinned versions, and the pinned node and base images pulled (the tiers
take minutes to bring up, so they are best as a separate nightly or label-triggered workflow, not on every PR).

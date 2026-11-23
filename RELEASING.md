# Releasing

A release is a pushed tag `vX.Y.Z` (or `vX.Y.Z-rc.1`). It publishes the image `ghcr.io/agentkourier/agent-kourier`
(linux/amd64 and linux/arm64) and the Helm chart `oci://ghcr.io/agentkourier/charts/agent-kourier`, whose default image
is that image by digest. It also publishes the images of two recipes' agents, `ghcr.io/agentkourier/agent-kourier-prompt-agent`
and `ghcr.io/agentkourier/agent-kourier-echo-agent` (linux/amd64 and linux/arm64), built from
`examples/recipes/<agent>/agent/Dockerfile`, and the images of the two add-ons, `ghcr.io/agentkourier/agent-kourier-cma`
(the CMA add-on) and `ghcr.io/agentkourier/agent-kourier-ax` (the AX add-on) (linux/amd64 and linux/arm64), built from
`addons/<name>/Dockerfile` on Agent Kourier's pinned bases (`hack/addons.txt` lists the add-ons). The add-ons are versioned with Agent Kourier: each release
tags their images with the same `vX.Y.Z`. The AX add-on's image for the route inside Google AX (`addons/ax/Dockerfile.ax`,
`make ax-runner-image`) is not released. Nothing is released by hand.

## Cut a release

1. Merge what ships to `main`.
2. Open a release pull request that sets `version` in `charts/agent-kourier/Chart.yaml` to `X.Y.Z` and every
   release version the public docs name to match. `make docs-check` lists each page that still names the old version
   (`release-version`), so the pull request cannot go green with a page left behind. The chart's golden files carry
   the version in a label: refresh them with `go test ./test/chart -update` (with `helm` installed). For a new minor
   version (`X.Y.0`), also add `"X.Y"` at the top of `hack/docs/versions.yaml`, so the documentation site publishes
   the new release line's tags; until its first tag exists the site skips it. The same pull request points the
   recipes' agents at the images this release publishes: `image:` in every manifest under `examples/recipes` that runs one of
   them (today `prompt-agent/agent/manifests/deployment.yaml`, `echo-agent/agent/manifests/deployment.yaml` and
   `webhook-github/agent/manifests/deployment.yaml`) names `ghcr.io/agentkourier/agent-kourier-<agent>:vX.Y.Z`,
   and so does every page that names one of those tags (today `examples/recipes/prompt-agent/README.md`). A tag left
   behind deploys the last release's agent; `release-version` checks these tags against the chart's version too, so
   it fails the pull request. Merge it. Skip this for a prerelease tag (`vX.Y.Z-rc.1`): while one is tried, the chart
   and the docs keep naming the last release.
3. Before tagging, confirm that `ci.yml`, `security.yml` and `codeql.yml` have completed successfully for that
   exact commit from a push to `main`. The release gate checks the same commit and the latest attempt again; a
   missing, pending, failed or pull-request-only run blocks the release.
4. Run the kind tiers on that commit by hand, since no workflow does ([CI](#ci) says why). The custom resource
   route has its own runs of the tiers, two scenarios that nothing else runs, and runs of the failover tests; runs 7
   and 8 send the fake Slack's requests through the chat listener (Slack's HTTP mode), and runs 9 and 10 add Microsoft
   Teams and a team's second Slack app. Each tier needs Docker and
   about 8 GB free; take each down before the next comes up:

   ```sh
   make e2e-fast-up e2e-fast e2e-fast-down                                       # the ConfigMap route
   make e2e-full-up e2e-full e2e-full-down
   E2E_CONFIG_SOURCE=crds make e2e-fast-up e2e-fast e2e-crs-scenarios e2e-fast-down
   E2E_CONFIG_SOURCE=crds make e2e-full-up e2e-full e2e-full-down                # custom resources
   E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up e2e-crs-migration e2e-fast-down
   E2E_CONFIG_SOURCE=crds E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up e2e-fast e2e-fast-down   # failover
   E2E_SLACK_MODE=http E2E_CONFIG_SOURCE=crds make e2e-fast-up e2e-fast e2e-fast-down                 # Slack HTTP mode
   E2E_SLACK_MODE=http E2E_CONFIG_SOURCE=crds E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up e2e-fast e2e-fast-down
   E2E_TEAMS=1 E2E_CONFIG_SOURCE=crds E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up e2e-fast e2e-teams e2e-fast-down       # Microsoft Teams
   E2E_TWO_APPS=1 E2E_CONFIG_SOURCE=crds E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up e2e-two-apps e2e-fast-down        # two Slack apps
   ```

   `make e2e-fast` and `make e2e-full` read which route the Deployment is on and run the tests of that route. What the
   runs cover, in the order above:

   1. The fast tier on the ConfigMap route, with one SQLite replica: the suite, with every `CR_` and `WH_` test and S16 skipped.
   2. The full tier on the ConfigMap route: its own suite, with the `CR_` and `WH_` tests and S16 skipped.
   3. The fast tier on the custom resource route, with one replica: the suite, the `CR_` tests, except the failover test,
      which needs two replicas, and the migration test, which runs only in its phases, and the `WH_` tests, which drive
      the webhook recipes that the bring-up serves on this route only.
      `make e2e-crs-scenarios` adds the scenarios that need kubectl or Helm, and needs the tier up on this route.
   4. The full tier on the custom resource route: its own suite against an Agent Kourier that reads custom resources. It
      runs none of the `CR_` or `WH_` tests, which skip off the fast tier (`test/e2e/cr_test.go`, `test/e2e/wh_test.go`:
      they use the stub agent, which the full tier lacks).
   5. `make e2e-crs-migration`, which needs the fast tier up on Postgres with two replicas on the ConfigMap route (the
      default), and runs `TestCR_MigrationKeepsASessionAlive` in its phases.
   6. The fast tier on Postgres with two replicas on the custom resource route, over Socket Mode: the suite, the `WH_`
      tests and the failover tests. S16 (`TestS16_FailoverLosesNoEventAndDuplicatesNone`) needs the fast tier with two replicas and
      runs on either route; `TestCR_FailoverDoesNotBlankStatus` needs the custom resource route as well. Both skip
      anywhere else, so only this run and runs 8 and 9 execute them.
   7. The fast tier on the custom resource route with one replica, with the fake Slack in HTTP mode
      (`E2E_SLACK_MODE=http`): the same suite as run 3, with every event and press of the fake Slack's default app sent
      as a signed request to the chat listener instead of over Socket Mode. The full tier stays on Socket Mode: its
      suite tests the kagent behind Agent Kourier, and how Slack's requests reach Agent Kourier does not depend on it.
   8. The fast tier on Postgres with two replicas on the custom resource route, in HTTP mode: the suite, with both
      failover tests, and Slack's requests going to whichever replica carries the leader label. S16's press cases check
      what differs by mode: in HTTP mode a press sent while the leader is dead or frozen is sent again by the fake Slack
      and decided by the new leader, which must have answered at least one such retry, and the person whose press lost
      is told so once.
   9. The fast tier on Postgres with two replicas on the custom resource route, with Microsoft Teams added (`E2E_TEAMS=1`:
      the fake Teams service and two Teams connections in `http` mode): the Slack suite, to show that Teams changes nothing
      for Slack, then the `TEAMS_` tests as a Job of their own (`make e2e-teams`), whose failover case needs two replicas.
   10. The fast tier on Postgres with two replicas on the custom resource route, with a team's second Slack app
      (`E2E_TWO_APPS=1`): `make e2e-two-apps`, the `MC_` tests and the admission steps that upgrade the release with Helm.
      `make e2e-fast` leaves both the `TEAMS_` and the `MC_` tests out.

   `make e2e-crs-scenarios` and `make e2e-crs-migration` say what they need and stop if the tier is not as they need it. A failure blocks the tag.

   The kind tiers reach only the end-to-end tests' fake Slack. When the release changes Slack's HTTP mode (the Slack
   adapter's HTTP handling, the chat listener, or the chart's chat Service and its values), also deploy that commit's image
   to an install with a ChatConnection in HTTP mode that Slack itself reaches, and run `hack/slack/http-probe.sh` against
   it. It sends requests that Slack did not sign (each must be answered 401) and one to an unknown connection (503), then
   posts an alert and a bot's mention in the channel, and waits for the connection's bot to reply to each. It needs three
   Slack apps: the connection's own, one whose bot the Binding's `botMessage` trigger names, and one that mentions the
   connection's bot, which no `botMessage` trigger of the Binding may name. `--help` says what the Binding must have, and
   names the two checks left to be done by hand: a button press, and a leader killed while a message is posted. With the
   leader's metrics port forwarded, the probe also checks the increments of `agentkourier_slack_http_requests_total`:

   ```sh
   kubectl -n agent-kourier port-forward "$(kubectl -n agent-kourier get pod -l agentkourier.dev/role=leader -o name)" 8080 &
   until curl -fsS -o /dev/null http://127.0.0.1:8080/metrics; do sleep 1; done
   hack/slack/http-probe.sh \
     --url https://chat.example.com/chat/slack/agent-kourier/slack \
     --channel C0123456789 \
     --probe-env ~/.config/agent-kourier/probe.env \
     --poster-env ~/.config/agent-kourier/poster.env \
     --mention-env ~/.config/agent-kourier/mention.env \
     --metrics-url http://127.0.0.1:8080/metrics
   kill %1
   ```

   The three files are mode 600: `probe.env` holds the connection's `botToken=`, and the two others each `SLACK_BOT_TOKEN=`
   of their app. The probe exits non-zero if any check fails, and a failure blocks the tag.
5. `git tag vX.Y.Z <the release pull request's commit on main>` and `git push origin vX.Y.Z`.
6. Watch the `release` workflow. Its job summary and the GitHub Release body name the pushed image and chart
   digests. A GitHub Release is created only after both signatures verify.
7. Re-record the clip, since it types the version: `docs/assets/clip/team-app.gif`, which the README, Install with Helm and
   Let a team bring its own Slack app show. `make docs-check` reads the pages, not the GIF, so nothing else
   catches a clip of an old release. `hack/clip/record.sh` says how (`up`, `take` with the published chart, a capture of
   the Slack thread pane with the answer, `stitch`, `down`); open a pull request with the new GIF, and update the
   sentence under the clip in each of the three, which says how long the cut wait for Ready was in that take. `make docs-site` builds each tag from its own tree, so a clip
   recorded after the tag reaches `main` and the site's `dev/` but never that tag: the tag's docs show the clip of the
   release before it, and a tag cut before the clip existed shows none. For the tag to carry its own clip, record it
   before the final tag: tag a release candidate (`vX.Y.Z-rc.N`) on the release commit, run `take` with
   `CLIP_CHART_VERSION=X.Y.Z-rc.N`, merge the clip's pull request, and tag `vX.Y.Z` on that merge commit once its
   checks pass. The take still types `--version X.Y.Z`; the recorder installs the candidate in its place and hides the
   two lines helm prints naming the pulled chart and its digest. Say so in the clip's pull request.

If a first release attempt fails before its GitHub Release is created, rerun that tag after fixing the failure.
Once a published or draft GitHub Release exists for the tag, the read-only gate refuses to rebuild it.

The documentation site publishes every final tag of a listed release line, whether or not that tag has a GitHub
Release. A docs deploy from `main` while a failed attempt waits for its rerun publishes that tag's docs early. A final
tag that is never released, such as one the gate refused, must be deleted, or the site publishes its docs, as its
line's newest and perhaps as latest.

## What the workflow does

`.github/workflows/release.yml` runs `hack/release/release.sh`, the script behind `make release-image`:

- First checks the exact tagged commit through the Actions API. `ci.yml` must contain successful `lint`, `test`,
  `test-testhooks`, `docs-agent`, `crd-corpus (1.37)`, `crd-corpus (1.33)`, `echo-agent`, `prompt-agent`, `kit`, `cma` and `ax` jobs. `security.yml` must contain successful `govulncheck (root)`,
  `govulncheck (ax)`, `govulncheck (docs-agent)`, `govulncheck (cma)`, `govulncheck (kit)`, `gitleaks`, `trivy (linux-amd64)` and `trivy (linux-arm64)` jobs, the four scans of the agents'
  images: `trivy (prompt-agent-linux-amd64)`, `trivy (prompt-agent-linux-arm64)`, `trivy (echo-agent-linux-amd64)` and
  `trivy (echo-agent-linux-arm64)`, the two of the CMA add-on's: `trivy (cma-linux-amd64)` and `trivy (cma-linux-arm64)`, and
  the two of the AX add-on's: `trivy (ax-linux-amd64)` and `trivy (ax-linux-arm64)`. The gate adds an add-on's
  jobs (its `ci.yml` job, its `govulncheck` and its two `trivy` scans) for each add-on that `hack/addons.txt` lists.
  `codeql.yml` must contain a successful `analyze (go)` job. These are the status checks the `main` ruleset
  requires, `kit` and `govulncheck (kit)` once the owner adds them to it. `codeql.yml` skips `analyze` while the repository is private, and a skipped run blocks the release.
  The scheduled released-image rescan is the only permitted skipped job. A pending run fails closed with a message
  to wait; the gate never reruns or changes a workflow run.
- Refuses a final tag `vX.Y.Z` unless `charts/agent-kourier/Chart.yaml` at the tagged commit says `version: X.Y.Z`
  and `hack/docs/versions.yaml` lists `X.Y` (`hack/release/check-version.sh`), so no release goes out before the pull
  request that points the docs at it, or without a place on the documentation site.
- Checks the Releases API before granting the build job access to the registry. A 404 means this is the first
  release attempt; a published or draft Release, an authorization failure or any other API error stops the workflow.
- Refuses a tag that is not on `main`.
- Builds each platform with the Dockerfile, which cross-compiles Go on the runner (no emulation) and sets
  `main.version` from the tag. Then builds each platform of each agent image from its own Dockerfile, which runs `apk`
  and `pip` in the target platform: the arm64 build runs under QEMU, from the binfmt image the workflow pins by digest.
  Then builds each platform of the image of each add-on of `hack/addons.txt` from `addons/<name>/Dockerfile`, with
  `addons/` as the context for the shared kit each requires, on the same pinned Go and runtime images as Agent
  Kourier's, which cross-compile too.
- Refuses to release a platform whose binary carries a test hook. The Dockerfile's `verify` stage runs
  `internal/testhooks` (the traces `make image-check` looks for, with a testhooks build of the same
  platform as the control) and checks the binary is an ELF of that architecture with the tag as its
  version; the image is not assembled if it fails. The script then checks each platform's image has no
  `dev.agent-kourier.testhooks` label, runs as 65532:65532, and is that platform. It checks each agent image's
  platforms and each add-on's the same way.
- Builds, checks and scans every platform of every image before the first push, so an agent or add-on image that fails
  leaves nothing pushed.
- Pushes one multi-platform image tagged `vX.Y.Z` with the workflow's `GITHUB_TOKEN` (`packages: write`),
  with an SBOM and a max-mode provenance attestation attached, and the OCI labels source, revision, version
  and created (the commit's time). Pushes each agent image and each add-on's the same way, tagged `vX.Y.Z`, to Agent
  Kourier's repository with `-<agent>`, `-cma` or `-ax` appended, then refuses it unless each platform's layers are those of the image it checked and scanned
  (`hack/release/check-pushed.sh`): an agent's Dockerfile resolves Wolfi's packages when it runs, so a push that missed
  the checks' cache would have resolved them again.
- The Buildx action is pinned by commit, while its BuildKit image and Docker's SBOM generator follow the action's
  defaults. Pin those tool references before claiming byte-for-byte build-tool reproducibility.
- Installs the pinned Trivy release from `hack/security/install-trivy.sh` and scans each local platform image before
  the multi-platform push. High and critical vulnerabilities, including unfixed findings, fail the release and the
  SARIF reports remain in the release output.
- Packages `charts/agent-kourier` (`hack/release/package-chart.sh`) with version `X.Y.Z`, appVersion `vX.Y.Z`, and
  `image.repository` and `image.digest` stamped with Agent Kourier's pushed image, and `addons.<name>.image.repository`
  and `addons.<name>.image.digest` with each add-on's (`addons.cma.image` with the CMA add-on's, `addons.ax.image` with
  the AX add-on's), so a released chart pulls every image by digest, reads the package back to check them,
  and pushes it to `oci://ghcr.io/agentkourier/charts`. `package-chart.sh` takes each add-on's digest as `name=digest`,
  and refuses a list of them that is not one for each add-on of `hack/addons.txt`, or a chart without an image block
  for one.
- In a second job that runs no repository code (the only one with `id-token: write`), signs the image digests
  and the chart digest with cosign v3.1.4 keyless (GitHub's OIDC identity; no key, no secret).
- In a separate read-only verification job, verifies every signature with the OIDC issuer and the exact identity
  `https://github.com/agentkourier/agent-kourier/.github/workflows/release.yml@refs/tags/<tag>`.
- In a final job with `contents: write` and no checkout, creates the GitHub Release with generated notes and the
  verified digests. `.github/release.yml` groups the notes by the labels `pr-labels.yml` sets from each pull
  request's title, and leaves out release pull requests (`chore(release): ...`). Re-running a completed release leaves matching release information unchanged; conflicting
  tag, prerelease state or digest information fails rather than being overwritten.
- For a final tag, after the Release exists, starts `docs.yml` on `main` (`actions: write`, no checkout), which
  rebuilds the documentation site with the new tag as a version of its own, its line's newest and, for the newest line,
  latest. A prerelease is not published there.

The job summary and the GitHub Release list every digest. The chart's `tag` stays empty: the digest wins.

## Build it locally

`make release-image VERSION=v0.0.0-rc.1` builds both platforms of each image, runs the same checks, and writes an OCI
archive of each image and the chart package to `bin/release/`. It pushes nothing, and needs helm and jq. It needs a buildx builder that can make multi-platform
images with attestations, which Docker's default `docker` driver without the containerd image store cannot:

    docker buildx create --name agent-kourier-release --driver docker-container
    BUILDER=agent-kourier-release make release-image VERSION=v0.0.0-rc.1

The agent images' arm64 build runs `apk` and `pip` under emulation. Docker Desktop has it; on a Linux host without
binfmt handlers, register arm64 first with the binfmt image that `.github/workflows/release.yml` pins (`docker run
--privileged --rm <that image> --install arm64`), or the build fails at the first `RUN`.

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

The agents' images and the add-ons' are signed by the same identity and carry the same attestations and labels.
Verify each as the image above, with `IMAGE=ghcr.io/agentkourier/agent-kourier-prompt-agent`,
`IMAGE=ghcr.io/agentkourier/agent-kourier-echo-agent`, `IMAGE=ghcr.io/agentkourier/agent-kourier-cma` or
`IMAGE=ghcr.io/agentkourier/agent-kourier-ax` and its digest from the GitHub Release.

Each release's GitHub Release body names the verified image and chart digests.

## Roll back

Deploy by digest, never by tag. To roll back, set the Deployment's image (or the chart's image value) back
to the previous release's `@sha256:...`, or install the previous chart version (its default image is that release's
digest); the digests of past releases are in their workflow job summaries. A released chart pins its image by digest,
so `--set image.tag=...` alone changes nothing: set `image.digest` to the other release's digest (or `image.digest=""`
with the tag).

If a release fails after its image is pushed (an agent image's pushed layers differ from the checked ones, or the chart
packaging, signing or verification failed), the image tag is
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
     `test-testhooks`, `docs-agent`, `crd-corpus (1.37)`, `crd-corpus (1.33)`, `echo-agent`, `prompt-agent`, `kit`, `cma` and `ax` from `ci`; `govulncheck (root)`, `govulncheck (ax)`,
     `govulncheck (docs-agent)`, `govulncheck (cma)`, `govulncheck (kit)`, `gitleaks`, `trivy (linux-amd64)`, `trivy (linux-arm64)`, `trivy (prompt-agent-linux-amd64)`,
     `trivy (prompt-agent-linux-arm64)`, `trivy (echo-agent-linux-amd64)`, `trivy (echo-agent-linux-arm64)`, `trivy (cma-linux-amd64)`,
     `trivy (cma-linux-arm64)`, `trivy (ax-linux-amd64)` and `trivy (ax-linux-arm64)` from `security`; and
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
4. Once, after the release that first publishes a package: Packages, `agent-kourier`, `charts/agent-kourier`,
   `agent-kourier-prompt-agent`, `agent-kourier-echo-agent`, `agent-kourier-cma` and `agent-kourier-ax`, link them to this repository and set visibility
   (public if the cluster pulls without a pull secret).

The release workflow refuses a tag that is not on `main`, so what ships has passed the `main` ruleset. Cut a
release tag only from a commit that GitHub signed when it squash-merged a pull request (the commit shows as
Verified on GitHub); nothing in the workflow checks this. The commits on `main` from before signed commits were
required, the tip of `main` at that time included, are unsigned, so the first release is cut from a later squash
merge. The release gate (`hack/release/check-ci.sh`) checks the same jobs as the `main` ruleset, on the push run
for the tagged commit, as listed under [What the workflow does](#what-the-workflow-does); a change to either list
belongs with a change to the other.

## CI

`.github/workflows/ci.yml` runs `make fmt` (no diff), `make lint`, `make build` and `make lint-workflows` (the `lint` job), and what `make test`
runs, on pull requests and pushes to `main`, in jobs that run side by side. Each race pass is split over the legs of a
matrix job: `test-shard (1)` and `test-shard (2)` each run part of `make test-race`, and `test-testhooks-shard (1)` and
`test-testhooks-shard (2)` part of `make test-testhooks`; `test-scripts` runs `make test-scripts`. A leg runs its
target with `TEST_SHARD=<leg>/<legs>`, its number and the number of legs taken from the strategy context, and
`hack/ci/test-shard.sh` gives it its part of the pass's packages: each package goes to exactly one leg, longest first,
balanced by the seconds in `hack/ci/test-weights` (a package not listed there weighs 10). A leg that would get no
package, when a pass has fewer packages than legs, fails rather than run none. There are two legs per pass because the
slowest package is the floor of its leg's time (`internal/redact`, about 125 s of a race pass whose weights sum to
about 1,220 s, and `internal/broker`, about 110 s of about 585 s in the testhooks pass, at four packages at a time).
Two race legs end about 30 s above their floor and two testhooks legs at theirs, so a third would end at most about
30 s sooner and would cost a runner and a cache. A leg is added by adding its number
to the job's `matrix.leg` and its cache prefix to `cache-prune`; `hack/ci/testshards`' test fails until both are there.
`hack/ci/test-shard.sh` says how to refresh the weights from a push run on `main`; stale weights only unbalance the
legs. The required checks `test` and `test-testhooks` are jobs of their own that run however their legs ended and fail
unless every job they need succeeded (for `test`, both `test-shard` legs and `test-scripts`); they check nothing out.
The `test-shard` legs have helm installed and `AGENTKOURIER_REQUIRE_HELM=1` so the chart tests cannot skip. The
`test-postgres` job (`make test-postgres`, `hack/postgres/test.sh`) runs the Postgres store's tests, the store
conformance suite among them, and the broker's Postgres tests against a throwaway Postgres container; those tests skip
in the race passes' legs, which have no database. It is not a required check. To make
it one, the owner adds `test-postgres` from `ci` to the `main` ruleset, and the same change adds it to `CI_REQUIRED_JOBS` in
`hack/release/check-ci.sh` and to the lists above. The `addon` job has a leg for each add-on that `hack/addons.txt`
lists, named after it (`cma`, `ax`): a leg runs `make addon-test-<name>` through `hack/postgres/run.sh`, which starts
the same throwaway Postgres container (`hack/postgres/lib.sh`) and sets `AGENTKOURIER_POSTGRES_DSN` for it, so the
add-on's Postgres store tests run there, and `make addon-lint-<name>` (`make cma-test`, `make ax-lint` and the others
run the same targets). Each test target also runs the shared kit's packages that the add-on links under the add-on's
module graph, and the offline tests of a module nested in the add-on's directory (the CMA add-on's `livecheck`); the
`kit` job runs `make kit-test` and `make kit-lint` on the kit's own. An add-on is added to CI and the release by a line
in `hack/addons.txt`, a leg in the `addon` job, a row in `security.yml`'s `govulncheck` and two in its `trivy` matrix,
and its cache prefixes in both workflows' `cache-prune`: `hack/ci/addonlist`'s test fails until the workflows hold the
list, and the owner adds the new checks to the `main` ruleset. The `crd-corpus` job (`make crd-corpus`, `hack/kind/crd-corpus.sh`) brings up
a kind cluster with only the three CRDs from `charts/agent-kourier/files/crds`, with no image and no Agent Kourier, and has
`test/crdcorpus` send every entry of its corpus to it as a server-side dry-run create. It fails when the schema refuses
an object the broker's own validation accepts, or when an entry's `schema:` flag no longer says what the schema does with
it. It is the only job that has an API server compile the schemas' CEL rules. It installs kind and kubectl with
`hack/ci/install-kind-tools.sh`, at the pins and checksums the kind tiers use, and runs twice: on the node image of
`hack/kind/lib.sh` (`NODE_IMAGE`, Kubernetes 1.37, the job `crd-corpus (1.37)`) and on the oldest Kubernetes the chart admits
(`NODE_IMAGE_MIN`, 1.33, the job `crd-corpus (1.33)`; `CRD_CORPUS_NODE=min make crd-corpus` runs it locally). Both are required checks of
the `main` ruleset and are in `CI_REQUIRED_JOBS`. Renaming either job, or moving the chart's minimum Kubernetes version, is a change to the
workflow, `NODE_IMAGE_MIN`, the ruleset and `CI_REQUIRED_JOBS` together. A run took about three minutes on a warm cache; the timeout is 20. The `lint` job's last step before its cache upkeep, `make lint-workflows`
(`hack/security/lint-workflows.sh`), audits every workflow with a pinned zizmor under the policy in
`.github/zizmor.yml` and fails on any finding; it first proves the policy still fails a set of unsafe fixtures
(`hack/security/testdata/zizmor`), and then that the inline ignore comments hide only the findings the script lists.
It runs even when an earlier step of `lint` failed. It is the only `run` step with the job's token (`contents: read`)
in its environment, for zizmor's online audits, so `make lint`, `make build` and the other steps that run the pull
request's code never see it; it uploads no SARIF, so it behaves the same on a fork's pull request. Because it is a
step of `lint`, a required check, a finding blocks the merge and the release. The online audits can raise a finding
that no change in the pull request caused: an advisory published against a pinned action fails `lint` on every pull
request, docs-only ones included, until the pin moves. A failed download of zizmor or a GitHub API error during the
online audits also fails `lint`; a re-run clears it. `hack/ci/docsonly`'s test fails if the step leaves `lint`, loses
its token, or gets an `if` other than `!cancelled()`, or if `lint` gets a job-level `if`. The Go jobs in `ci.yml`, `security.yml`, `codeql.yml` and `docs.yml` keep the module cache and the build cache
(plus golangci-lint's cache in the jobs that lint) in Actions caches, one per job and one per leg of `test-shard` and
`test-testhooks-shard` (`go-test-<leg>-…`, `go-test-testhooks-<leg>-…`), except that `test-postgres` restores the
second race leg's, which runs `internal/broker`, and the two `crd-corpus` jobs share one, which `crd-corpus (1.37)` saves. A key is
`go-<cache>-<runner OS>-go<Go version>-<hash of the go.sum files it builds from (and, for govulncheck, its script)>-<commit>`, and a run restores the newest
entry whose key starts with `go-<cache>-<runner OS>-`, so a pull request starts from what the latest push to `main`
saved, test results included. Every push run on `main` saves, whether its steps passed or failed (a cancelled job saves
nothing, nor does a re-run of a job whose entry is already saved); pull requests save none. Before a push saves,
`hack/ci/actions-cache.sh` trims the build caches to the entries that run read or wrote, and starts the module cache
empty when the restored entry is from another `go.sum` or Go version. After the saves, each of the four workflows'
`cache-prune` job deletes all but the newest entry of every cache it saves (`codeql.yml`'s also deletes all but the
newest of CodeQL's overlay-base databases); a listing or delete that fails is a warning, not a failure, so an API error
there does not stop a release, and the next push's prune catches up. One entry of each cache comes to about 5.4 GB of
the repository's 10 GB cache limit, about 2.4 GB of it the four test legs' entries (each holds the module cache and
the race builds of its packages' dependencies, most of which the other leg of its pass holds too). A push to `main`
holds a second entry of each cache it saves until the prune runs: about 9.4 GB while `ci`'s are in flight, up to about
10.8 GB if `security.yml`'s and `codeql.yml`'s land before their prunes. The limit is GitHub's default; a repository
admin or organization owner can raise it, and use past 10 GB is then billed. Past it GitHub evicts the least recently
accessed entries first, so a cache that was accessed longest ago (one the prune would delete anyway, or a `docs.yml`
cache, restored only on a docs change) can go, and the next run that wants it starts cold. The jobs that
install Python packages, `echo-agent` and `prompt-agent` in `ci.yml` and `build` in `docs.yml`, keep pip's cache under an exact key on
the job, the runner OS, the Python version and the requirements files they install from: a push to `main` saves it
only when those files or the Python version change, and `cache-prune` then deletes the old entry. `docs.yml` runs on a push to
`main` only when the push changes a path in its `push` trigger, so only such a push saves the `build` job's caches. Each job has its own timeout, set from the times seen on a cold cache. The kind
tiers are not in CI, which is why "Cut a release" has you run them, with the custom resource scenarios, by hand. They would need a runner with Docker and about 8 GB free, the `kind`, `kubectl`,
`helm` and `jq` binaries installed at pinned versions (`hack/ci/install-kind-tools.sh` installs them, each checked
against a pinned sha256), and the pinned node and base images pulled (the tiers
take minutes to bring up, so they are best as a separate nightly or label-triggered workflow, not on every PR). The `crd-corpus` job is not a tier: its cluster has only the CRDs, and it runs on every PR but one that changes only paths it does not read (below).

A pull request's `test-shard` and `test-testhooks-shard` legs reuse the test results that the latest push to `main`
saved: `go test` reports a package `(cached)`, and does not run it, when its test binary and the environment variables
and files of the module its tests read are as they were in that push's run. `go test` compares such a file's size, mode
and modification time, not its contents, and a checkout gives every file the time of the checkout, so before `make` each
leg runs `go run ./hack/ci/testmtimes`. It sets every tracked file's time to 2001-01-01 plus the first 58 bits of its
blob hash as nanoseconds (a time from 2001 to early 2010), a symbolic link's own time the same way from its blob, and
the time of every directory that holds a tracked path, and of the top directory's `.git`, to 2000-01-01; it leaves
untracked files, and tracked files that differ from the index, alone. The same step puts a directory unique to the run
on `PATH`. `go test` cannot see what a program that a test starts reads (`helm` the chart, `bash` a script, `go list`
the packages), but a test that looks a program up on `PATH` has read `PATH`, so such a package (`test/chart`,
`test/kindmanifests`, `cmd/agent-kourier`, `api/v1alpha1`, the packages whose tests run `go list -deps`, and others)
never reuses a result. A push to `main` reuses none: it runs `go clean -testcache` first, so every test runs on every
push, those are the runs the release gate reads, and their results are the ones pull requests reuse. The legs' make step
sets `GODEBUG=gocachetest=1`, so the log says for each package whether it reused a result and, for one that ran, whether
no result was saved for its test binary (`input list not found`: the binary changed) or one was, for other inputs
(`test output not found`: a file or variable its tests read changed). `go test` keys results on `GODEBUG` too: removing
it from the legs makes pull requests run every package until a push to `main` saves results without it. `test-postgres`,
`crd-corpus`, `docs-agent`, `kit` and the add-on legs run `go test` with `-count=1`, which reuses nothing, and do not run `hack/ci/testmtimes`.
`docs-agent`'s must (`make docs-agent-test`): its smoke test reads `docs/`, outside its module, where `go test` checks
nothing, so a reused result would pass a pull request that changes only the docs. `hack/ci/testshards`' test holds the
legs to these steps and every other job to none of them. The limits of the reuse, none of which a setting changes:

- A changed file that keeps its size reuses a stale result if its new blob draws the same time: the odds are 2^-58
  (about 3.5 × 10^-18) per changed file where the file system keeps times to the nanosecond, as APFS and ext4 do, and
  about 3.5 × 10^-9 (2^58 ns is about 2.9 × 10^8 s) where it keeps whole seconds; `hack/ci/testmtimes` says how many
  times its file system cut short. The pull request then passes on the old result, and the push run on `main` after the
  merge, which runs everything, runs it again. The 58 bits are `hashBits` in `hack/ci/testmtimes`: 59 would end in 2019
  and 60 in 2037, and `go test` saves no result of a test that read a file whose time is less than 2 s before the read,
  a time to come included.
- A program a test starts by its path, the test binary re-run as a child included (the `os.Args[0]` re-runs in
  `internal/outbox`, `internal/broker` and `cmd/agent-kourier`), is keyed on its own file, but not on the files or
  variables it reads. A test that depends on what such a program reads must read those inputs itself, or look the
  program up on `PATH`. A file outside the module is never keyed, which is why `docs-agent` runs with `-count=1`.
- `go test` keys nothing a test reads before `m.Run` starts its log (in an `init` function, a package variable or
  `TestMain` before `m.Run`), nor the environment read through `os.Environ`. The one read before `m.Run` today is
  `internal/testhooks`' `armed` variable, from a variable CI never sets in the process that runs the tests.
- A test that is flaky, or that depends on the date, passes on a pull request when its package reuses a pass; the push
  run on `main` runs it again.

Each gated job passes a class to `hack/ci/docs-only.sh` in its first step after the checkout, and on a pull request
whose changed paths are all, as regular files or deleted, paths its class does not read, it skips its other steps and
still reports success under its own name. Every class skips the docs-only list (maintainer prose such as `spec.md` and
`AGENTS.md`, `docs/brand`, `docs/spikes`, and the site's media under `docs/assets`), whose content no test, image,
scanner or CodeQL reads, but for two tests of the docs checks that `make docs-check` in `lint` repeats. Beyond it:

- `tests` (the `test-shard` and `test-testhooks-shard` legs, `test-scripts`, `test-postgres`): nothing more. Tests read
  `README.md` and pages under `docs/` and elsewhere when they run, which only running them shows.
- `scan` (the five `govulncheck` jobs, the `trivy` jobs, `analyze`) and `crd-corpus` (both jobs): any Markdown
  (`*.md`, in lower case) outside every Go package's tree, such as `README.md`, the pages under `docs/` and the chart's
  README. govulncheck reads Go packages and the files they embed; Agent Kourier's image and each add-on's hold only
  the binary, and an agent's image only the files its `.dockerignore` lets in, none of them Markdown; CodeQL's Go autobuild
  extracts `.go` files; `crd-corpus` builds and runs `test/crdcorpus` and reads only YAML from its corpus.
- `docs-agent`: the same Markdown except under `docs/`, its corpus, and `examples/recipes/docs-agent/`.
- `echo-agent`: the same Markdown except under `examples/recipes/echo-agent/agent/`, the one directory it reads.
- `prompt-agent`: the same Markdown except under `examples/recipes/prompt-agent/agent/`, the one directory it reads.
- `kit`: the same Markdown except under `docs/` (the pages its limits guard reads) and `addons/kit/`.
- `addon` (each leg of the `addon` job): the same Markdown except under `docs/` (the add-ons' pages) and `addons/`
  (each leg's own directory and the kit's, which it tests too). It is one class for every leg, so a leg also runs on
  Markdown of another add-on's directory.

A Go package's tree is a directory of the merge commit with a `.go` file in it, and everything below it; the script
finds them on each run, so no class skips a page under a new package's directory, which the package could embed, with
no list to edit. The rule is wider than what Go embeds: a page in such a tree runs the jobs whether or not anything
embeds it. So a pull request that changes `README.md` and pages under `docs/` skips both `crd-corpus` jobs,
`echo-agent`, `prompt-agent`, the `govulncheck` and `trivy` jobs and `analyze`; one that changes only `README.md` also skips
`docs-agent`, `kit` and the add-on legs; one that changes only the docs-only list skips every gated job, and `test` and `test-testhooks` pass as
their legs did. A pull request that skips `govulncheck` or `trivy` does not see a vulnerability published since its
base was scanned; `main`'s push runs do, and so does the nightly `security` run's `govulncheck`. `lint`, with its zizmor
audit, and `gitleaks` always run in full. A symlink or submodule anywhere in the changes runs every step of every gated job: the
docs agent's tests walk the real `docs/` and refuse any symlink in it. So does a path the class reads, an unknown
class, a failure to list the paths or the merge commit's tree, or a merge commit whose second parent is not the pull
request's head. There is no cap on how many paths it lists. A push to `main` always runs every step, so the push runs
the release gate reads (`hack/release/check-ci.sh` reads only push runs on `main`), and the caches they save, are as
before. The header of `ci.yml` says why the jobs skip steps rather than the whole job, and `hack/ci/docsonly`'s test
keeps the copies of the gate in the jobs alike and holds each job to its class.

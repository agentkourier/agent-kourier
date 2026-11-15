#!/usr/bin/env bash
# Tests for the release scripts. These are offline: the fake gh reads the fixtures in testdata.
# Run as `hack/release/test.sh`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHECK=${HERE}/check-ci.sh
CHECK_RELEASE=${HERE}/check-release.sh
CHECK_VERSION=${HERE}/check-version.sh
CHECK_PUSHED=${HERE}/check-pushed.sh
FAKE_GH=${HERE}/testdata/fake-gh
FIXTURES=${HERE}/testdata
SHA=0123456789abcdef0123456789abcdef01234567
REPOSITORY=agentkourier/agent-kourier

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

check() { # DESCRIPTION WANT GOT
  if [ "$2" = "$3" ]; then
    echo "ok: $1"
  else
    echo "FAIL: $1: want [$2], got [$3]" >&2
    return 1
  fi
}

contains() { # DESCRIPTION NEEDLE FILE
  if grep -F -- "$2" "$3" >/dev/null; then
    echo "ok: $1"
  else
    echo "FAIL: $1: [$2] was not found in $3" >&2
    return 1
  fi
}

run_case() { # FIXTURE WANT_STATUS
  local fixture=$1 want=$2 name=$3 status output
  output=${TMP}/${name}.out
  set +e
  GH_FIXTURE=${FIXTURES}/${fixture} GH="${FAKE_GH}" \
    "${CHECK}" "${REPOSITORY}" "${SHA}" >"${output}" 2>&1
  status=$?
  set -e
  check "${name}: exit status" "${want}" "${status}"
}

run_release_case() { # FIXTURE WANT_STATUS NAME
  local fixture=$1 want=$2 name=$3 status output
  output=${TMP}/${name}.out
  set +e
  GH_FIXTURE=${FIXTURES}/${fixture} GH="${FAKE_GH}" \
    "${CHECK_RELEASE}" "${REPOSITORY}" v1.2.3 >"${output}" 2>&1
  status=$?
  set -e
  check "${name}: exit status" "${want}" "${status}"
}

echo "== exact successful push runs and non-skipped jobs"
run_case success 0 success
contains "success: checks CodeQL by default" "ok: codeql.yml run 301" "${TMP}/success.out"

echo "== failed workflow run fails closed"
run_case failure 1 failure
contains "failure: reports the failed run" "concluded 'failure'" "${TMP}/failure.out"

echo "== pending workflow fails closed without mutation"
run_case pending 1 pending
contains "pending: asks the operator to wait" "wait for the latest attempt to complete" "${TMP}/pending.out"
echo "ok: pending: no workflow mutation is available"

echo "== a successful pull request run is not a release gate"
run_case pr-only 1 pr-only

echo "== a successful workflow with a failed job fails closed"
run_case job-failure 1 job-failure
contains "job-failure: names the failed required job" "required job 'test' is failure" "${TMP}/job-failure.out"

echo "== a failed leg of a split test pass fails closed, though its required check passed"
run_case leg-failure 1 leg-failure
contains "leg-failure: names the failed leg" "non-successful jobs: test-shard (2)=failure" "${TMP}/leg-failure.out"

echo "== the newest attempt overrides an older successful attempt"
run_case latest-failure 1 latest-failure
contains "latest-failure: uses the latest attempt" "latest push run 701" "${TMP}/latest-failure.out"

echo "== a missing required job fails closed"
run_case missing-job 1 missing-job
contains "missing-job: names the missing required job" "required job 'test' is missing" "${TMP}/missing-job.out"

echo "== a corpus run on the minimum Kubernetes that failed fails closed"
run_case corpus-min-failure 1 corpus-min-failure
contains "corpus-min-failure: names the required job" "required job 'crd-corpus (1.33)' is failure" "${TMP}/corpus-min-failure.out"

echo "== a missing corpus run fails closed"
run_case corpus-min-missing 1 corpus-min-missing
contains "corpus-min-missing: names the missing required job" "required job 'crd-corpus (1.33)' is missing" "${TMP}/corpus-min-missing.out"

echo "== a skipped required job fails closed"
run_case skipped-required 1 skipped-required
contains "skipped-required: names the skipped required job" "required job 'test-testhooks' is skipped" "${TMP}/skipped-required.out"

echo "== an unapproved skipped job fails closed"
run_case unexpected-skip 1 unexpected-skip
contains "unexpected-skip: names the skipped job" "unexpected skipped job 'unapproved optional job'" "${TMP}/unexpected-skip.out"

echo "== no matching run fails closed"
run_case missing 1 missing

echo "== a skipped CodeQL run fails closed"
run_case codeql-skipped 1 codeql-skipped
contains "codeql-skipped: reports the skipped run" "codeql.yml: latest push run 302 for ${SHA} concluded 'skipped'" "${TMP}/codeql-skipped.out"

echo "== a CodeQL run without the Go analysis fails closed"
run_case codeql-missing-job 1 codeql-missing-job
contains "codeql-missing-job: names the missing required job" "required job 'analyze (go)' is missing" "${TMP}/codeql-missing-job.out"

echo "== an absent GitHub Release returns 404 and permits the first build"
run_release_case missing 0 release-absent
contains "release-absent: recognizes a 404" "no GitHub Release exists" "${TMP}/release-absent.out"

echo "== a published GitHub Release blocks rebuilding the tag"
run_release_case release-published 1 release-published
contains "release-published: refuses a published release" "already exists; refusing to rebuild the published tag" "${TMP}/release-published.out"

echo "== a draft GitHub Release also blocks rebuilding the tag"
run_release_case release-draft 1 release-draft
contains "release-draft: refuses a draft conflict" "already exists as a draft" "${TMP}/release-draft.out"

echo "== an authorization error fails closed"
run_release_case release-auth-error 1 release-auth-error
contains "release-auth-error: reports an indeterminate release state" "could not determine whether release" "${TMP}/release-auth-error.out"

run_version_case() { # TAG CHART_VERSION WANT_STATUS NAME [VERSIONS_YAML]
  local tag=$1 version=$2 want=$3 name=$4 versions=${5:-${TMP}/versions.yaml} status chart
  chart=${TMP}/${name}-Chart.yaml
  if [ -n "${version}" ]; then
    printf 'apiVersion: v2\nname: agent-kourier\nversion: %s\nappVersion: dev\n' "${version}" >"${chart}"
  else
    printf 'apiVersion: v2\nname: agent-kourier\n' >"${chart}"
  fi
  set +e
  "${CHECK_VERSION}" "${tag}" "${chart}" "${versions}" >"${TMP}/${name}.out" 2>&1
  status=$?
  set -e
  check "${name}: exit status" "${want}" "${status}"
}

printf 'releases:\n  - "1.2"\n  - 1.1 # unquoted\n' >"${TMP}/versions.yaml"

echo "== a release tag must be the chart's version"
run_version_case v1.2.3 1.2.3 0 version-match
run_version_case v1.2.3 '"1.2.3"' 0 version-quoted
run_version_case v1.2.4 1.2.3 1 version-mismatch
contains "version-mismatch: says what to do" "merge a pull request that sets the chart's version to 1.2.4" "${TMP}/version-mismatch.out"
run_version_case v1.2.3 '' 1 version-missing
contains "version-missing: names the missing version" "has no version" "${TMP}/version-missing.out"
run_version_case v1.3.0-rc.1 1.2.3 0 version-prerelease
contains "version-prerelease: skips the check" "is a prerelease" "${TMP}/version-prerelease.out"
run_version_case 1.2.3 1.2.3 1 version-no-v
run_version_case v1.2 1.2.3 1 version-short

echo "== a final release tag's line must be listed for the documentation site"
run_version_case v1.1.4 1.1.4 0 version-line-unquoted
run_version_case v1.3.0 1.3.0 1 version-line-missing
contains "version-line-missing: says what to add" 'does not list the release line 1.3: add "1.3"' "${TMP}/version-line-missing.out"
printf 'releases:\n  - "1x0"\n  - "1.02"\n  - "11.0"\n' >"${TMP}/near-versions.yaml"
run_version_case v1.0.2 1.0.2 1 version-line-exact "${TMP}/near-versions.yaml" # 1.0 matches none of 1x0, 1.02, 11.0
run_version_case v1.3.0-rc.1 1.2.3 0 version-line-prerelease
run_version_case v1.2.3 1.2.3 1 version-line-no-file "${TMP}/no-such-versions.yaml"

echo "== the repository's chart version passes as a release tag"
set +e
"${CHECK_VERSION}" "v$(sed -n 's/^version:[[:space:]]*//p' "${HERE}/../../charts/agent-kourier/Chart.yaml" | head -n 1)" >"${TMP}/version-repo.out" 2>&1
status=$?
set -e
check "version-repo: the chart's own version and the repository's versions.yaml pass" 0 "${status}"

# run_release_build NAME [VAR=VALUE ...]: run release.sh for v1.2.3 with the fake docker, helm and scanner, the extra
# variables set; its output, build log and scan log go to TMP/NAME.out, NAME.builds and NAME.scans, its files to TMP/NAME.
run_release_build() {
  local name=$1 status
  shift
  : >"${TMP}/${name}.builds"
  : >"${TMP}/${name}.state"
  : >"${TMP}/${name}.scans"
  set +e
  env PATH="${FIXTURES}/fakebin:${PATH}" FAKE_ROOT="${ROOT}" FAKE_LOG="${TMP}/${name}.builds" FAKE_STATE="${TMP}/${name}.state" \
    FAKE_SCAN_LOG="${TMP}/${name}.scans" SCAN_IMAGE_SCRIPT="${FIXTURES}/fake-scan-image.sh" SCAN_IMAGES=1 \
    VERSION=v1.2.3 REGISTRY=ghcr.io/example/agent-kourier OUT="${TMP}/${name}" "$@" \
    "${HERE}/release.sh" >"${TMP}/${name}.out" 2>&1
  status=$?
  set -e
  return "${status}"
}

echo "== release.sh builds, checks and scans every platform of every image before it builds one to keep"
ROOT=$(cd "${HERE}/../.." && pwd)
if run_release_build build; then
  echo "ok: build: exit status"
else
  cat "${TMP}/build.out" >&2
  echo "FAIL: build: release.sh failed" >&2
  exit 1
fi
for want in \
  "build load linux/amd64 agent-kourier-release-check:linux-amd64 ." \
  "build load linux/arm64 agent-kourier-release-check:linux-arm64 ." \
  "build load linux/amd64 agent-kourier-release-check:prompt-agent-linux-amd64 examples/recipes/prompt-agent/agent" \
  "build load linux/arm64 agent-kourier-release-check:prompt-agent-linux-arm64 examples/recipes/prompt-agent/agent" \
  "build load linux/amd64 agent-kourier-release-check:echo-agent-linux-amd64 examples/recipes/echo-agent/agent" \
  "build load linux/arm64 agent-kourier-release-check:echo-agent-linux-arm64 examples/recipes/echo-agent/agent" \
  "build output linux/amd64,linux/arm64 ghcr.io/example/agent-kourier:v1.2.3 ." \
  "build output linux/amd64,linux/arm64 ghcr.io/example/agent-kourier-prompt-agent:v1.2.3 examples/recipes/prompt-agent/agent" \
  "build output linux/amd64,linux/arm64 ghcr.io/example/agent-kourier-echo-agent:v1.2.3 examples/recipes/echo-agent/agent"; do
  check "build: ${want}" 1 "$(grep -c -x -F -- "${want}" "${TMP}/build.builds")"
done
check "build: nine builds in all" 9 "$(wc -l <"${TMP}/build.builds" | tr -d ' ')"
last_check=$(grep -n '^build load ' "${TMP}/build.builds" | tail -n 1 | cut -d: -f1)
first_kept=$(grep -n -v '^build load ' "${TMP}/build.builds" | head -n 1 | cut -d: -f1)
check "build: every check precedes the first image kept" 1 "$([ "${last_check}" -lt "${first_kept}" ] && echo 1 || echo 0)"
check "build: six platform images scanned" 6 "$(wc -l <"${TMP}/build.scans" | tr -d ' ')"
for f in trivy-linux-arm64.sarif trivy-prompt-agent-linux-amd64.sarif trivy-echo-agent-linux-arm64.sarif; do
  check "build: wrote ${f}" 1 "$([ -s "${TMP}/build/${f}" ] && echo 1 || echo 0)"
done
for f in digest prompt-agent-digest echo-agent-digest; do
  check "build: ${f} is a digest" 1 "$(grep -c -x -E 'sha256:[0-9a-f]{64}' "${TMP}/build/${f}")"
done
check "build: three distinct image digests" 3 "$(cat "${TMP}/build/digest" "${TMP}/build/prompt-agent-digest" "${TMP}/build/echo-agent-digest" | sort -u | wc -l | tr -d ' ')"
contains "build: the chart's default image is Agent Kourier's" "digest: $(cat "${TMP}/build/digest")" "${TMP}/build/agent-kourier-1.2.3.tgz.values"
contains "build: the chart's default repository is Agent Kourier's" "repository: ghcr.io/example/agent-kourier" "${TMP}/build/agent-kourier-1.2.3.tgz.values"

echo "== a finding in an agent image's last platform leaves no image built to keep"
if run_release_build scan-fails FAKE_SCAN_FAIL=echo-agent-linux-arm64; then
  echo "FAIL: scan-fails: release.sh succeeded" >&2
  exit 1
fi
echo "ok: scan-fails: exit status"
check "scan-fails: no image built to keep" 0 "$(grep -c -v '^build load ' "${TMP}/scan-fails.builds" || true)"
check "scan-fails: no digest written" 0 "$(find "${TMP}/scan-fails" -name '*digest' | wc -l | tr -d ' ')"

echo "== an agent image that runs as root is refused before any image is kept"
if run_release_build root-user FAKE_ROOT_IMAGE=prompt-agent-linux-amd64; then
  echo "FAIL: root-user: release.sh succeeded" >&2
  exit 1
fi
contains "root-user: says which image and why" "prompt-agent linux/amd64: the image does not run as 65532:65532" "${TMP}/root-user.out"
check "root-user: no image built to keep" 0 "$(grep -c -v '^build load ' "${TMP}/root-user.builds" || true)"

# run_pushed_case NAME WANT_STATUS PUSHED_IMAGE LAYERS: check-pushed.sh for linux/arm64 of a pushed reference whose .Image
# is PUSHED_IMAGE, against a local image whose layers are LAYERS.
run_pushed_case() {
  local name=$1 want=$2 status
  set +e
  env PATH="${FIXTURES}/fakebin:${PATH}" FAKE_PUSHED_IMAGE="$3" FAKE_LAYERS="$4" \
    "${CHECK_PUSHED}" ghcr.io/example/agent-kourier-echo-agent@sha256:1 linux/arm64 check:echo-agent-linux-arm64 >"${TMP}/${name}.out" 2>&1
  status=$?
  set -e
  check "${name}: exit status" "${want}" "${status}"
}

echo "== a pushed agent image must have the layers of the image that was checked and scanned"
index='{"linux/amd64":{"rootfs":{"type":"layers","diff_ids":["sha256:a1","sha256:a2"]}},"linux/arm64":{"rootfs":{"type":"layers","diff_ids":["sha256:b1","sha256:b2"]}}}'
run_pushed_case pushed-same 0 "${index}" '["sha256:b1","sha256:b2"]'
run_pushed_case pushed-other-platform 1 "${index}" '["sha256:a1","sha256:a2"]'
contains "pushed-other-platform: says the layers differ" "is not the image that was checked and scanned" "${TMP}/pushed-other-platform.out"
run_pushed_case pushed-extra-layer 1 "${index}" '["sha256:b1"]'
run_pushed_case pushed-missing-platform 1 '{"linux/amd64":{"rootfs":{"diff_ids":["sha256:a1"]}}}' '["sha256:a1"]'
contains "pushed-missing-platform: names the platform" "has no linux/arm64 image" "${TMP}/pushed-missing-platform.out"
run_pushed_case pushed-single 0 '{"os":"linux","architecture":"arm64","rootfs":{"diff_ids":["sha256:b1","sha256:b2"]}}' '["sha256:b1","sha256:b2"]'
run_pushed_case pushed-single-other-platform 1 '{"os":"linux","architecture":"amd64","rootfs":{"diff_ids":["sha256:b1","sha256:b2"]}}' '["sha256:b1","sha256:b2"]'

echo "all passed"

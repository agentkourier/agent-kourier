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
  # shellcheck disable=SC2086 # CHECK_WORKFLOWS, when a case sets it, is a list of workflows
  GH_FIXTURE=${FIXTURES}/${fixture} GH="${FAKE_GH}" \
    "${CHECK}" "${REPOSITORY}" "${SHA}" ${CHECK_WORKFLOWS:-} >"${output}" 2>&1
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

echo "== a missing recipe agent job fails closed"
run_case recipe-agent-missing 1 recipe-agent-missing
contains "recipe-agent-missing: names the missing required job" "required job 'prompt-agent' is missing" "${TMP}/recipe-agent-missing.out"

echo "== a missing scan of an agent image fails closed"
run_case agent-scan-missing 1 agent-scan-missing
contains "agent-scan-missing: names the missing required job" "required job 'trivy (echo-agent-linux-arm64)' is missing" "${TMP}/agent-scan-missing.out"

echo "== a missing job of the add-ons' kit fails closed"
run_case kit-missing 1 kit-missing
contains "kit-missing: names the missing required job" "required job 'kit' is missing" "${TMP}/kit-missing.out"

echo "== a missing govulncheck of the add-ons' kit fails closed"
run_case kit-scan-missing 1 kit-scan-missing
contains "kit-scan-missing: names the missing required job" "required job 'govulncheck (kit)' is missing" "${TMP}/kit-scan-missing.out"

echo "== a missing CMA add-on job fails closed"
run_case cma-missing 1 cma-missing
contains "cma-missing: names the missing required job" "required job 'cma' is missing" "${TMP}/cma-missing.out"

echo "== a missing scan of the CMA add-on's image fails closed"
run_case cma-scan-missing 1 cma-scan-missing
contains "cma-scan-missing: names the missing required job" "required job 'trivy (cma-linux-arm64)' is missing" "${TMP}/cma-scan-missing.out"

echo "== a missing AX add-on job fails closed"
run_case ax-missing 1 ax-missing
contains "ax-missing: names the missing required job" "required job 'ax' is missing" "${TMP}/ax-missing.out"

echo "== a missing scan of the AX add-on's image fails closed"
run_case ax-scan-missing 1 ax-scan-missing
contains "ax-scan-missing: names the missing required job" "required job 'trivy (ax-linux-arm64)' is missing" "${TMP}/ax-scan-missing.out"

echo "== each add-on of hack/addons.txt adds its required jobs"
printf 'cma\nax\nthird\n' >"${TMP}/third-addons.txt"
ADDONS_FILE="${TMP}/third-addons.txt" run_case success 1 third-addon
contains "third-addon: names the third add-on's leg" "required job 'third' is missing" "${TMP}/third-addon.out"
printf 'ax\nthird\ncma\n' >"${TMP}/third-first-addons.txt"
ADDONS_FILE="${TMP}/third-first-addons.txt" CHECK_WORKFLOWS=security.yml run_case success 1 third-addon-scan
contains "third-addon-scan: names the third add-on's govulncheck" "required job 'govulncheck (third)' is missing" "${TMP}/third-addon-scan.out"
printf 'cma\n' >"${TMP}/one-addon.txt"
ADDONS_FILE="${TMP}/one-addon.txt" CHECK_WORKFLOWS=ci.yml run_case ax-missing 0 one-addon
contains "one-addon: an add-on the list lacks is not required" "ok: ci.yml run 813" "${TMP}/one-addon.out"
printf 'Cma\n' >"${TMP}/bad-addons.txt"
ADDONS_FILE="${TMP}/bad-addons.txt" run_case success 1 bad-addons
contains "bad-addons: says the list could not be read" "could not read the add-ons of hack/addons.txt" "${TMP}/bad-addons.out"

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

# run_release_build NAME [VAR=VALUE ...]: run release.sh for v1.2.3 with the fake docker, helm and scanner and the
# fixture chart (testdata/chart, CHART_DIR), the extra variables set; its output, build log, build arguments and scan
# log go to TMP/NAME.out, NAME.builds, NAME.args and NAME.scans, its files to TMP/NAME.
run_release_build() {
  local name=$1 status
  shift
  : >"${TMP}/${name}.builds"
  : >"${TMP}/${name}.args"
  : >"${TMP}/${name}.state"
  : >"${TMP}/${name}.scans"
  set +e
  env PATH="${FIXTURES}/fakebin:${PATH}" FAKE_ROOT="${ROOT}" FAKE_LOG="${TMP}/${name}.builds" FAKE_ARGS_LOG="${TMP}/${name}.args" \
    FAKE_STATE="${TMP}/${name}.state" \
    FAKE_SCAN_LOG="${TMP}/${name}.scans" SCAN_IMAGE_SCRIPT="${FIXTURES}/fake-scan-image.sh" SCAN_IMAGES=1 \
    CHART_DIR="${FIXTURES}/chart" VERSION=v1.2.3 REGISTRY=ghcr.io/example/agent-kourier OUT="${TMP}/${name}" "$@" \
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
# Each line is the build's context and Dockerfile: an add-on is built from addons/, which holds the kit it requires.
PROMPT=examples/recipes/prompt-agent/agent
ECHO=examples/recipes/echo-agent/agent
for want in \
  "build load linux/amd64 agent-kourier-release-check:linux-amd64 . Dockerfile" \
  "build load linux/arm64 agent-kourier-release-check:linux-arm64 . Dockerfile" \
  "build load linux/amd64 agent-kourier-release-check:prompt-agent-linux-amd64 ${PROMPT} ${PROMPT}/Dockerfile" \
  "build load linux/arm64 agent-kourier-release-check:prompt-agent-linux-arm64 ${PROMPT} ${PROMPT}/Dockerfile" \
  "build load linux/amd64 agent-kourier-release-check:echo-agent-linux-amd64 ${ECHO} ${ECHO}/Dockerfile" \
  "build load linux/arm64 agent-kourier-release-check:echo-agent-linux-arm64 ${ECHO} ${ECHO}/Dockerfile" \
  "build load linux/amd64 agent-kourier-release-check:cma-linux-amd64 addons addons/cma/Dockerfile" \
  "build load linux/arm64 agent-kourier-release-check:cma-linux-arm64 addons addons/cma/Dockerfile" \
  "build load linux/amd64 agent-kourier-release-check:ax-linux-amd64 addons addons/ax/Dockerfile" \
  "build load linux/arm64 agent-kourier-release-check:ax-linux-arm64 addons addons/ax/Dockerfile" \
  "build output linux/amd64,linux/arm64 ghcr.io/example/agent-kourier:v1.2.3 . Dockerfile" \
  "build output linux/amd64,linux/arm64 ghcr.io/example/agent-kourier-prompt-agent:v1.2.3 ${PROMPT} ${PROMPT}/Dockerfile" \
  "build output linux/amd64,linux/arm64 ghcr.io/example/agent-kourier-echo-agent:v1.2.3 ${ECHO} ${ECHO}/Dockerfile" \
  "build output linux/amd64,linux/arm64 ghcr.io/example/agent-kourier-cma:v1.2.3 addons addons/cma/Dockerfile" \
  "build output linux/amd64,linux/arm64 ghcr.io/example/agent-kourier-ax:v1.2.3 addons addons/ax/Dockerfile"; do
  check "build: ${want}" 1 "$(grep -c -x -F -- "${want}" "${TMP}/build.builds")"
done
check "build: fifteen builds in all" 15 "$(wc -l <"${TMP}/build.builds" | tr -d ' ')"
last_check=$(grep -n '^build load ' "${TMP}/build.builds" | tail -n 1 | cut -d: -f1)
first_kept=$(grep -n -v '^build load ' "${TMP}/build.builds" | head -n 1 | cut -d: -f1)
check "build: every check precedes the first image kept" 1 "$([ "${last_check}" -lt "${first_kept}" ] && echo 1 || echo 0)"
check "build: ten platform images scanned" 10 "$(wc -l <"${TMP}/build.scans" | tr -d ' ')"
for f in trivy-linux-arm64.sarif trivy-prompt-agent-linux-amd64.sarif trivy-echo-agent-linux-arm64.sarif \
  trivy-cma-linux-amd64.sarif trivy-cma-linux-arm64.sarif trivy-ax-linux-amd64.sarif trivy-ax-linux-arm64.sarif; do
  check "build: wrote ${f}" 1 "$([ -s "${TMP}/build/${f}" ] && echo 1 || echo 0)"
done
for f in digest prompt-agent-digest echo-agent-digest cma-digest ax-digest; do
  check "build: ${f} is a digest" 1 "$(grep -c -x -E 'sha256:[0-9a-f]{64}' "${TMP}/build/${f}")"
done
check "build: five distinct image digests" 5 "$(cat "${TMP}/build/digest" "${TMP}/build/prompt-agent-digest" "${TMP}/build/echo-agent-digest" "${TMP}/build/cma-digest" "${TMP}/build/ax-digest" | sort -u | wc -l | tr -d ' ')"
# Each build's arguments, by its Dockerfile, three builds of each image: an add-on is built on Agent Kourier's pinned
# bases with the version, as Agent Kourier is, an agent with no argument.
GO_IMAGE_PIN=$(sed -n 's/^GO_IMAGE=//p' "${ROOT}/hack/kind/lib.sh")
RUNTIME_IMAGE_PIN=$(sed -n 's/^RUNTIME_IMAGE=//p' "${ROOT}/hack/kind/lib.sh")
for want in \
  "Dockerfile GO_IMAGE=${GO_IMAGE_PIN} RUNTIME_IMAGE=${RUNTIME_IMAGE_PIN} VERSION=v1.2.3" \
  "addons/cma/Dockerfile GO_IMAGE=${GO_IMAGE_PIN} RUNTIME_IMAGE=${RUNTIME_IMAGE_PIN} VERSION=v1.2.3" \
  "addons/ax/Dockerfile GO_IMAGE=${GO_IMAGE_PIN} RUNTIME_IMAGE=${RUNTIME_IMAGE_PIN} VERSION=v1.2.3" \
  "${ECHO}/Dockerfile" \
  "${PROMPT}/Dockerfile"; do
  check "build: the arguments of ${want%% *}" 3 "$(grep -c -x -F -- "${want}" "${TMP}/build.args")"
done
contains "build: the chart's default image is Agent Kourier's" "digest: $(cat "${TMP}/build/digest")" "${TMP}/build/agent-kourier-1.2.3.tgz.values"
contains "build: the chart's default repository is Agent Kourier's" "repository: ghcr.io/example/agent-kourier" "${TMP}/build/agent-kourier-1.2.3.tgz.values"
# Each add-on's block, by its indentation, and nothing else: the look-alike image blocks of the fixture keep their values.
values=${TMP}/build/agent-kourier-1.2.3.tgz.values
check "build: addons.cma.image.repository is the add-on's pushed repository" 1 \
  "$(grep -c -x -F "      repository: ghcr.io/example/agent-kourier-cma" "${values}")"
check "build: addons.cma.image.digest is the add-on's digest" 1 \
  "$(grep -c -x -F "      digest: $(cat "${TMP}/build/cma-digest")" "${values}")"
check "build: addons.ax.image.repository is the add-on's pushed repository" 1 \
  "$(grep -c -x -F "      repository: ghcr.io/example/agent-kourier-ax" "${values}")"
check "build: addons.ax.image.digest is the add-on's digest" 1 \
  "$(grep -c -x -F "      digest: $(cat "${TMP}/build/ax-digest")" "${values}")"
check "build: the other image blocks keep their empty digests" 3 "$(grep -c -E '^ +digest: ""$' "${values}")"
check "build: the other add-on keeps its repository" 1 "$(grep -c -x -F "      repository: ghcr.io/example/other" "${values}")"

# release_chart_case NAME SED WANT: release.sh with the fixture chart edited by SED fails at the chart, with WANT in its
# output.
release_chart_case() {
  local name=$1
  rm -rf "${TMP}/${name}-chart"
  cp -R "${FIXTURES}/chart" "${TMP}/${name}-chart"
  sed -i.bak "$2" "${TMP}/${name}-chart/values.yaml"
  rm -f "${TMP}/${name}-chart/values.yaml.bak"
  if run_release_build "${name}" CHART_DIR="${TMP}/${name}-chart"; then
    echo "FAIL: ${name}: release.sh succeeded" >&2
    exit 1
  fi
  contains "${name}: says what the chart lacks" "$3" "${TMP}/${name}.out"
}

echo "== a chart whose add-on image block is not where the release stamps it is refused"
release_chart_case chart-no-cma 's/^  cma:$/  cmax:/' "no longer has exactly one addons.cma.image.repository"
release_chart_case chart-cma-digest-set 's/^      digest: ""$/      digest: sha256:0/' "no longer has exactly one addons.cma.image.repository"
release_chart_case chart-no-ax 's/^  ax:$/  axe:/' "no longer has exactly one addons.ax.image.repository"
release_chart_case chart-ax-digest-set '/^  ax:$/,/^  cma:$/s/^      digest: ""$/      digest: sha256:0/' "no longer has exactly one addons.ax.image.repository"
release_chart_case chart-no-image-digest 's/^  digest: ""$/  digest: sha256:0/' "no longer has exactly one empty image.repository"

# package_case NAME WANT_STATUS ARG...: package-chart.sh for v1.2.3 of ghcr.io/example/agent-kourier at DIGEST_A, with the
# fake helm and the fixture chart, the ARGs after its output directory; its output goes to TMP/NAME.out.
DIGEST_A=sha256:$(printf 'a%.0s' {1..64})
DIGEST_B=sha256:$(printf 'b%.0s' {1..64})
DIGEST_C=sha256:$(printf 'c%.0s' {1..64})
package_case() {
  local name=$1 want=$2 status
  shift 2
  set +e
  env PATH="${FIXTURES}/fakebin:${PATH}" CHART_DIR="${CHART_DIR:-${FIXTURES}/chart}" \
    "${HERE}/package-chart.sh" v1.2.3 ghcr.io/example/agent-kourier "${DIGEST_A}" "${TMP}/${name}" "$@" >"${TMP}/${name}.out" 2>&1
  status=$?
  set -e
  check "${name}: exit status" "${want}" "${status}"
}

echo "== package-chart.sh stamps each add-on of hack/addons.txt with its NAME=DIGEST, in any order"
package_case package-pairs 0 ax="${DIGEST_C}" cma="${DIGEST_B}"
values=${TMP}/package-pairs/agent-kourier-1.2.3.tgz.values
check "package-pairs: addons.cma.image.digest" 1 "$(grep -c -x -F "      digest: ${DIGEST_B}" "${values}")"
check "package-pairs: addons.ax.image.digest" 1 "$(grep -c -x -F "      digest: ${DIGEST_C}" "${values}")"
check "package-pairs: addons.ax.image.repository" 1 "$(grep -c -x -F "      repository: ghcr.io/example/agent-kourier-ax" "${values}")"
check "package-pairs: prints the package" "${TMP}/package-pairs/agent-kourier-1.2.3.tgz" "$(cat "${TMP}/package-pairs.out")"

echo "== package-chart.sh refuses pairs that are not one per add-on of hack/addons.txt"
package_case package-missing 1 cma="${DIGEST_B}"
contains "package-missing: names the add-on" "no digest for the add-on ax" "${TMP}/package-missing.out"
package_case package-unknown 1 cma="${DIGEST_B}" ax="${DIGEST_C}" other="${DIGEST_A}"
contains "package-unknown: names the add-on" "other is not an add-on of hack/addons.txt" "${TMP}/package-unknown.out"
package_case package-twice 1 cma="${DIGEST_B}" ax="${DIGEST_C}" cma="${DIGEST_C}"
contains "package-twice: names the add-on" "cma has two digests" "${TMP}/package-twice.out"
package_case package-bad-digest 1 cma="${DIGEST_B}" ax=sha256:0
contains "package-bad-digest: names the add-on" "the digest of ax must be a sha256 digest (got 'sha256:0')" "${TMP}/package-bad-digest.out"
package_case package-no-name 1 cma="${DIGEST_B}" "${DIGEST_C}"
contains "package-no-name: says what an argument is" "want NAME=DIGEST" "${TMP}/package-no-name.out"
package_case package-old-order 1 "${DIGEST_B}" "${DIGEST_C}"
contains "package-old-order: says what an argument is" "want NAME=DIGEST" "${TMP}/package-old-order.out"
check "package-old-order: no package written" 0 "$(find "${TMP}/package-old-order" -name '*.tgz' 2>/dev/null | wc -l | tr -d ' ')"

echo "== a third add-on in the list is stamped too, and the chart must have its block"
printf 'cma\nax\nthird\n' >"${TMP}/third-addons.txt"
rm -rf "${TMP}/third-chart"
cp -R "${FIXTURES}/chart" "${TMP}/third-chart"
printf '  third:\n    image:\n      repository: ghcr.io/agentkourier/agent-kourier-third\n      digest: ""\n' >>"${TMP}/third-chart/values.yaml"
ADDONS_FILE="${TMP}/third-addons.txt" CHART_DIR="${TMP}/third-chart" package_case package-third 0 cma="${DIGEST_B}" ax="${DIGEST_C}" third="${DIGEST_A}"
check "package-third: addons.third.image.repository" 1 \
  "$(grep -c -x -F "      repository: ghcr.io/example/agent-kourier-third" "${TMP}/package-third/agent-kourier-1.2.3.tgz.values")"
ADDONS_FILE="${TMP}/third-addons.txt" package_case package-third-no-block 1 cma="${DIGEST_B}" ax="${DIGEST_C}" third="${DIGEST_A}"
contains "package-third-no-block: names the add-on's keys" "no longer has exactly one addons.third.image.repository and empty addons.third.image.digest" \
  "${TMP}/package-third-no-block.out"

echo "== release.sh builds, scans and stamps each add-on of the list"
if ! ADDONS_FILE="${TMP}/third-addons.txt" run_release_build third CHART_DIR="${TMP}/third-chart"; then
  cat "${TMP}/third.out" >&2
  echo "FAIL: third: release.sh failed" >&2
  exit 1
fi
for want in \
  "build load linux/amd64 agent-kourier-release-check:third-linux-amd64 addons addons/third/Dockerfile" \
  "build load linux/arm64 agent-kourier-release-check:third-linux-arm64 addons addons/third/Dockerfile" \
  "build output linux/amd64,linux/arm64 ghcr.io/example/agent-kourier-third:v1.2.3 addons addons/third/Dockerfile"; do
  check "third: ${want}" 1 "$(grep -c -x -F -- "${want}" "${TMP}/third.builds")"
done
check "third: eighteen builds in all" 18 "$(wc -l <"${TMP}/third.builds" | tr -d ' ')"
check "third: twelve platform images scanned" 12 "$(wc -l <"${TMP}/third.scans" | tr -d ' ')"
check "third: addons.third.image.digest is its digest" 1 \
  "$(grep -c -x -F "      digest: $(cat "${TMP}/third/third-digest")" "${TMP}/third/agent-kourier-1.2.3.tgz.values")"

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

echo "== a build that fails is run again, and the release goes on once it succeeds"
PROMPT_CHECK=agent-kourier-release-check:prompt-agent-linux-amd64
if run_release_build retried FAKE_BUILD_FAIL="${PROMPT_CHECK}=2" BUILD_RETRY_WAIT=0; then
  echo "ok: retried: exit status"
else
  cat "${TMP}/retried.out" >&2
  echo "FAIL: retried: release.sh failed" >&2
  exit 1
fi
check "retried: two attempts failed" 2 "$(grep -c -x -F -- "${PROMPT_CHECK}" "${TMP}/retried.state.failed")"
contains "retried: says which build it runs again" "==> prompt-agent linux/amd64: build attempt 2 of 4 failed; trying again in 0s" "${TMP}/retried.out"
check "retried: fifteen builds in all" 15 "$(wc -l <"${TMP}/retried.builds" | tr -d ' ')"
check "retried: ten platform images scanned" 10 "$(wc -l <"${TMP}/retried.scans" | tr -d ' ')"
check "retried: the agent's digest is written" 1 "$(grep -c -x -E 'sha256:[0-9a-f]{64}' "${TMP}/retried/prompt-agent-digest")"
ECHO_KEPT=ghcr.io/example/agent-kourier-echo-agent:v1.2.3
if run_release_build retried-kept FAKE_BUILD_FAIL="${ECHO_KEPT}=1" BUILD_RETRY_WAIT=0; then
  echo "ok: retried-kept: exit status"
else
  cat "${TMP}/retried-kept.out" >&2
  echo "FAIL: retried-kept: release.sh failed" >&2
  exit 1
fi
contains "retried-kept: the multi-platform build is run again too" "==> ${ECHO_KEPT}: build attempt 1 of 4 failed" "${TMP}/retried-kept.out"
check "retried-kept: the echo agent's digest is written" 1 "$(grep -c -x -E 'sha256:[0-9a-f]{64}' "${TMP}/retried-kept/echo-agent-digest")"

echo "== a build that fails every attempt stops the release before any image is kept"
if run_release_build retries-spent FAKE_BUILD_FAIL="${PROMPT_CHECK}=3" BUILD_ATTEMPTS=3 BUILD_RETRY_WAIT=0; then
  echo "FAIL: retries-spent: release.sh succeeded" >&2
  exit 1
fi
check "retries-spent: three attempts" 3 "$(grep -c -x -F -- "${PROMPT_CHECK}" "${TMP}/retries-spent.state.failed")"
contains "retries-spent: says which build and how often" "error: prompt-agent linux/amd64: the build failed 3 time(s); giving up" "${TMP}/retries-spent.out"
check "retries-spent: no image built to keep" 0 "$(grep -c -v '^build load ' "${TMP}/retries-spent.builds" || true)"
check "retries-spent: no digest written" 0 "$(find "${TMP}/retries-spent" -name '*digest' | wc -l | tr -d ' ')"
if run_release_build no-retry FAKE_BUILD_FAIL="${PROMPT_CHECK}=1" BUILD_ATTEMPTS=1; then
  echo "FAIL: no-retry: release.sh succeeded" >&2
  exit 1
fi
check "no-retry: BUILD_ATTEMPTS=1 runs a build once" 1 "$(grep -c -x -F -- "${PROMPT_CHECK}" "${TMP}/no-retry.state.failed")"
for bad in BUILD_ATTEMPTS=0 BUILD_ATTEMPTS=x BUILD_RETRY_WAIT=-1; do
  if run_release_build bad-setting "${bad}"; then
    echo "FAIL: bad-setting: release.sh succeeded with ${bad}" >&2
    exit 1
  fi
  contains "bad-setting: refuses ${bad}" "${bad%%=*} must be a whole number" "${TMP}/bad-setting.out"
  check "bad-setting: ${bad} builds nothing" 0 "$(wc -l <"${TMP}/bad-setting.builds" | tr -d ' ')"
done

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

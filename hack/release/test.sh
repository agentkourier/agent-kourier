#!/usr/bin/env bash
# Tests for the release scripts. These are offline: the fake gh reads the fixtures in testdata.
# Run as `hack/release/test.sh`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
CHECK=${HERE}/check-ci.sh
CHECK_RELEASE=${HERE}/check-release.sh
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

echo "== the newest attempt overrides an older successful attempt"
run_case latest-failure 1 latest-failure
contains "latest-failure: uses the latest attempt" "latest push run 701" "${TMP}/latest-failure.out"

echo "== a missing required job fails closed"
run_case missing-job 1 missing-job
contains "missing-job: names the missing required job" "required job 'test' is missing" "${TMP}/missing-job.out"

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

echo "all passed"

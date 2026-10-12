#!/usr/bin/env bash
# Verify that the exact commit being released passed the repository's protected CI workflows.
#
#   check-ci.sh REPOSITORY SHA [WORKFLOW ...]
#
# Each workflow must have a completed, successful push run on main whose head_sha is SHA. Every
# required job must be present and successful. Only the known schedule/workflow-run rescan job may
# be skipped; any other skipped or non-successful job fails closed. GH may be set to a fake command
# by the script tests.
set -euo pipefail

GH=${GH:-gh}

usage() {
  echo "usage: check-ci.sh REPOSITORY SHA [WORKFLOW ...]" >&2
  exit 2
}

die() {
  echo "error: $*" >&2
  exit 1
}

[ $# -ge 2 ] || usage
REPOSITORY=$1
SHA=$2
shift 2
WORKFLOWS=("$@")
[ ${#WORKFLOWS[@]} -gt 0 ] || WORKFLOWS=(ci.yml security.yml)

# These are the display names returned by the Actions jobs API. Matrix jobs include their matrix value
# in the name, as security.yml defines it. Keep this list in lockstep with the workflows: a renamed or
# missing required job must stop a release until this gate is reviewed with that workflow change.
CI_REQUIRED_JOBS=(lint test test-testhooks)
SECURITY_REQUIRED_JOBS=(
  'govulncheck (root)'
  'govulncheck (ax)'
  gitleaks
  'trivy (linux-amd64)'
  'trivy (linux-arm64)'
)

[[ "${REPOSITORY}" =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]] || die "REPOSITORY must be OWNER/REPOSITORY (got '${REPOSITORY}')"
[[ "${SHA}" =~ ^[0-9a-fA-F]{40}$ ]] || die "SHA must be a 40-character commit SHA (got '${SHA}')"
command -v "${GH}" >/dev/null || die "${GH} is required"
command -v jq >/dev/null || die "jq is required"

api() {
  local endpoint=$1
  "${GH}" api --header 'Accept: application/vnd.github+json' "${endpoint}"
}

allowed_skipped_job() {
  [ "$1" = security.yml ] && [ "$2" = 'rescan released image digests' ]
}

check_workflow() {
  local workflow=$1 runs run id status conclusion jobs count failed skipped expected skipped_job
  local -a required
  [[ "${workflow}" =~ ^[A-Za-z0-9._/-]+$ ]] || die "invalid workflow '${workflow}'"

  runs=$(api "repos/${REPOSITORY}/actions/workflows/${workflow}/runs?event=push&branch=main&head_sha=${SHA}&per_page=100") ||
    die "could not read ${workflow} workflow runs"
  run=$(jq -c --arg sha "${SHA}" '
    [ .workflow_runs[]?
      | select(.event == "push" and .head_branch == "main" and .head_sha == $sha) ]
    # run_number selects the newest run; run_attempt makes a later rerun override an earlier attempt.
    | sort_by((.run_number // 0), (.run_attempt // 0), (.created_at // ""), (.id // 0))
    | .[-1] // empty' <<<"${runs}")
  [ -n "${run}" ] || die "${workflow}: no push run on main exists for ${SHA}"

  id=$(jq -r '.id // empty' <<<"${run}")
  status=$(jq -r '.status // empty' <<<"${run}")
  conclusion=$(jq -r '.conclusion // empty' <<<"${run}")
  [ -n "${id}" ] || die "${workflow}: the matching run has no id"

  if [ "${status}" != completed ]; then
    case "${status}" in
      queued | in_progress | requested | waiting | pending)
        die "${workflow}: run ${id} is ${status}; wait for the latest attempt to complete before releasing"
        ;;
      *)
        die "${workflow}: run ${id} has unexpected status '${status}'"
        ;;
    esac
  fi
  [ "${conclusion}" = success ] ||
    die "${workflow}: latest push run ${id} for ${SHA} concluded '${conclusion:-unknown}'"

  jobs=$(api "repos/${REPOSITORY}/actions/runs/${id}/jobs?filter=latest&per_page=100") ||
    die "${workflow}: could not read jobs for run ${id}"
  count=$(jq '[.jobs[]?] | length' <<<"${jobs}")
  [ "${count}" -gt 0 ] || die "${workflow}: run ${id} reported no jobs"

  case "${workflow}" in
    ci.yml) required=("${CI_REQUIRED_JOBS[@]}") ;;
    security.yml) required=("${SECURITY_REQUIRED_JOBS[@]}") ;;
    *) required=() ;;
  esac
  for expected in "${required[@]}"; do
    conclusion=$(jq -r --arg name "${expected}" '
      [ .jobs[]? | select(.name == $name) ]
      | if length == 0 then "missing"
        elif all(.[]; .conclusion == "success") then "success"
        else (map(.conclusion // .status // "unknown") | join(","))
        end' <<<"${jobs}")
    [ "${conclusion}" = success ] ||
      die "${workflow}: required job '${expected}' is ${conclusion}"
  done

  skipped=$(jq -r '[.jobs[]? | select(.conclusion == "skipped") | .name // "(unnamed)"] | .[]' <<<"${jobs}")
  while IFS= read -r skipped_job; do
    [ -n "${skipped_job}" ] || continue
    if ! allowed_skipped_job "${workflow}" "${skipped_job}"; then
      die "${workflow}: unexpected skipped job '${skipped_job}'"
    fi
  done <<<"${skipped}"

  failed=$(jq -r '
    [ .jobs[]?
      | select((.conclusion // "") != "skipped" and (.conclusion // "") != "success")
      | "\(.name // "(unnamed)")=\(.conclusion // .status // "unknown")" ]
    | join(", ")' <<<"${jobs}")
  [ -z "${failed}" ] || die "${workflow}: run ${id} has non-successful jobs: ${failed}"
  echo "ok: ${workflow} run ${id} for ${SHA}; ${count} jobs, required jobs successful"
}

for workflow in "${WORKFLOWS[@]}"; do
  check_workflow "${workflow}"
done

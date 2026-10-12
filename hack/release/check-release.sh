#!/usr/bin/env bash
# Refuse to rebuild a tag that already has a GitHub Release.
#
#   check-release.sh REPOSITORY TAG
#
# A missing release (the Releases API's 404) is the only allowed result. A published or draft release,
# an API/authentication error, malformed JSON or a mismatched tag fails closed. GH may be set to a fake
# command by the script tests.
set -euo pipefail

GH=${GH:-gh}

usage() {
  echo "usage: check-release.sh REPOSITORY TAG" >&2
  exit 2
}

die() {
  echo "error: $*" >&2
  exit 1
}

[ $# -eq 2 ] || usage
REPOSITORY=$1
TAG=$2
[[ "${REPOSITORY}" =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]] || die "REPOSITORY must be OWNER/REPOSITORY (got '${REPOSITORY}')"
[[ "${TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] ||
  die "TAG must be a version tag such as v1.2.3 (got '${TAG}')"
command -v "${GH}" >/dev/null || die "${GH} is required"
command -v jq >/dev/null || die "jq is required"

response=$(mktemp)
error=$(mktemp)
trap 'rm -f "${response}" "${error}"' EXIT

set +e
"${GH}" api --header 'Accept: application/vnd.github+json' \
  "repos/${REPOSITORY}/releases/tags/${TAG}" >"${response}" 2>"${error}"
status=$?
set -e

if [ "${status}" -eq 0 ]; then
  tag_name=$(jq -er '.tag_name // empty' "${response}") || die "GitHub returned an invalid release response"
  [ "${tag_name}" = "${TAG}" ] ||
    die "GitHub returned release tag '${tag_name}' while looking for '${TAG}'"
  draft=$(jq -er 'if (.draft // false) then "true" else "false" end' "${response}") ||
    die "GitHub returned an invalid release response"
  if [ "${draft}" = true ]; then
    die "release ${TAG} already exists as a draft; refusing to rebuild the tag"
  fi
  die "release ${TAG} already exists; refusing to rebuild the published tag"
fi

# gh reports a tag-less release lookup miss as `HTTP 404`. Every other API failure, including auth and
# rate-limit failures, is a release-state uncertainty and therefore blocks the build.
if grep -F -- 'HTTP 404' "${error}" >/dev/null; then
  echo "ok: no GitHub Release exists for ${TAG}; a first release may proceed"
  exit 0
fi
cat "${error}" >&2
die "could not determine whether release ${TAG} exists"

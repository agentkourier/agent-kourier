#!/usr/bin/env bash
# Refuse a release tag that is not the chart's version.
#
# Usage: check-version.sh TAG [CHART_YAML]
#
# A release tag vX.Y.Z must be the version in charts/agent-kourier/Chart.yaml (X.Y.Z). The public docs name that
# version, and `make docs-check` fails while any page names another (hack/docs/contentcheck, release.go), so a release
# goes out only after a pull request that bumps the chart and the docs together. A prerelease tag (vX.Y.Z-rc.1) is not
# checked: while it is tried, the chart and the docs still name the last release.
set -euo pipefail

die() {
  printf 'check-version: %s\n' "$*" >&2
  exit 1
}

TAG=${1:-}
CHART=${2:-"$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)/charts/agent-kourier/Chart.yaml"}

[[ "${TAG}" =~ ^v([0-9]+\.[0-9]+\.[0-9]+)(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] ||
  die "TAG must be a version tag such as v1.2.3 or v1.2.3-rc.1 (got '${TAG}')"
want=${BASH_REMATCH[1]}
if [ -n "${BASH_REMATCH[2]}" ]; then
  echo "ok: ${TAG} is a prerelease; the chart version is not checked"
  exit 0
fi

[ -f "${CHART}" ] || die "${CHART} does not exist"
got=$(sed -n 's/^version:[[:space:]]*//p' "${CHART}" | head -n 1 | tr -d "\"'[:space:]")
[ -n "${got}" ] || die "${CHART} has no version"
[ "${got}" = "${want}" ] ||
  die "the tag ${TAG} is version ${want}, but the chart's version is ${got}: merge a pull request that sets the chart's version to ${want} and the docs to match (make docs-check), then tag that commit"
echo "ok: ${TAG} is the chart's version ${got}"

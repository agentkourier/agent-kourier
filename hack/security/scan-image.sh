#!/usr/bin/env bash
# Scan one image platform with the repository's release vulnerability policy.
#
# Usage: scan-image.sh IMAGE_REF PLATFORM OUTFILE
#
# IMAGE_REF may be a local tag or a registry reference. PLATFORM is one of the
# release platforms. OUTFILE is a SARIF report path; the report is written even
# when Trivy finds vulnerabilities so CI can retain it as an artifact.
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

[ "$#" -eq 3 ] || die "usage: $0 IMAGE_REF PLATFORM OUTFILE"
IMAGE_REF=$1
PLATFORM=$2
OUTFILE=$3
TRIVY_BIN=${TRIVY_BIN:-trivy}

[ -n "${IMAGE_REF}" ] || die "IMAGE_REF must not be empty"
case "${PLATFORM}" in
  linux/amd64 | linux/arm64) ;;
  *) die "PLATFORM must be linux/amd64 or linux/arm64 (got '${PLATFORM}')" ;;
esac
[ -n "${OUTFILE}" ] || die "OUTFILE must not be empty"

if [[ "${TRIVY_BIN}" == */* ]]; then
  [ -x "${TRIVY_BIN}" ] || die "TRIVY_BIN is not executable: ${TRIVY_BIN}"
else
  command -v "${TRIVY_BIN}" >/dev/null || die "Trivy is required (set TRIVY_BIN or install trivy)"
fi

mkdir -p "$(dirname -- "${OUTFILE}")"
"${TRIVY_BIN}" image \
  --platform "${PLATFORM}" \
  --scanners vuln \
  --severity HIGH,CRITICAL \
  --ignore-unfixed=false \
  --exit-code 1 \
  --format sarif \
  --output "${OUTFILE}" \
  --no-progress \
  "${IMAGE_REF}"

[ -s "${OUTFILE}" ] || die "Trivy did not write a SARIF report to ${OUTFILE}"

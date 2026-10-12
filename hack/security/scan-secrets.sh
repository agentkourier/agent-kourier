#!/usr/bin/env bash
# Scan repository history for leaked credentials with standalone Gitleaks.
#
# Usage: scan-secrets.sh [REPOSITORY] [SARIF_OUTFILE]
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

REPOSITORY=${1:-.}
SARIF_OUTFILE=${2:-}
GITLEAKS_BIN=${GITLEAKS_BIN:-gitleaks}
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
GITLEAKS_CONFIG=${GITLEAKS_CONFIG:-"${SCRIPT_DIR}/gitleaks.toml"}

[ -d "${REPOSITORY}" ] || die "repository does not exist: ${REPOSITORY}"
if [[ "${GITLEAKS_BIN}" == */* ]]; then
  [ -x "${GITLEAKS_BIN}" ] || die "GITLEAKS_BIN is not executable: ${GITLEAKS_BIN}"
else
  command -v "${GITLEAKS_BIN}" >/dev/null || die "Gitleaks is required (set GITLEAKS_BIN or install gitleaks)"
fi
[ -f "${GITLEAKS_CONFIG}" ] || die "Gitleaks config does not exist: ${GITLEAKS_CONFIG}"

ARGS=(git --config "${GITLEAKS_CONFIG}" --gitleaks-ignore-path "${SCRIPT_DIR}/gitleaksignore" --no-banner --no-color --redact --exit-code 1 --log-opts=--all)
if [ -n "${SARIF_OUTFILE}" ]; then
  mkdir -p "$(dirname -- "${SARIF_OUTFILE}")"
  ARGS+=(--report-format sarif --report-path "${SARIF_OUTFILE}")
fi
ARGS+=("${REPOSITORY}")

"${GITLEAKS_BIN}" "${ARGS[@]}"

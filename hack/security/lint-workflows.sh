#!/usr/bin/env bash
# Lint the repository's GitHub Actions workflows with zizmor and fail on any finding. First proves the policy
# (.github/zizmor.yml) still fails each fixture under testdata/zizmor, so a loosened config cannot pass silently.
#
# Usage: lint-workflows.sh
# ZIZMOR_BIN selects a zizmor binary; without it the pinned release is installed (install-zizmor.sh). GH_TOKEN or
# GITHUB_TOKEN, when set, enables zizmor's online audits of the repository's workflows (impostor commits, known
# vulnerable actions); the fixtures are audited offline.
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd -- "${SCRIPT_DIR}/../.." && pwd)
CONFIG="${ROOT}/.github/zizmor.yml"
FIXTURES="${SCRIPT_DIR}/testdata/zizmor"

if [ -z "${ZIZMOR_BIN:-}" ]; then
  ZIZMOR_BIN=$("${SCRIPT_DIR}/install-zizmor.sh")
fi
[ -x "${ZIZMOR_BIN}" ] || die "ZIZMOR_BIN is not executable: ${ZIZMOR_BIN}"
[ -f "${CONFIG}" ] || die "zizmor config does not exist: ${CONFIG}"
command -v jq >/dev/null || die "jq is required"

# --config: zizmor's own discovery looks for the config at the git root, which in a worktree is the main checkout's.
ARGS=(--config "${CONFIG}" --persona regular --no-progress --color never)

# Each fixture is named <audit>--<case>.yml and must produce a finding from <audit>.
found=0
for fixture in "${FIXTURES}"/*--*.yml; do
  [ -f "${fixture}" ] || continue
  found=$((found + 1))
  audit=$(basename -- "${fixture}")
  audit=${audit%%--*}
  output=$("${ZIZMOR_BIN}" "${ARGS[@]}" --offline --quiet "${fixture}" 2>&1) && die "zizmor passed ${fixture}"
  grep -F -- "[${audit}]:" <<<"${output}" >/dev/null || {
    printf '%s\n' "${output}" >&2
    die "zizmor did not report ${audit} for ${fixture}"
  }
done
[ "${found}" -gt 0 ] || die "no zizmor fixtures in ${FIXTURES}"
printf 'zizmor reported the expected finding for %d unsafe fixtures\n' "${found}"

cd "${ROOT}"
"${ZIZMOR_BIN}" "${ARGS[@]}" .

# An inline ignore covers every finding whose span holds the comment, and zizmor reports each trigger of an `on:`
# block as its own finding spanning the whole block: the ignore on security.yml's workflow_run would also hide a
# pull_request_target added beside it. So the findings the ignores hide must be exactly these (audit, file, zizmor's
# annotation); add a line here only with the ignore comment and its reason. This run uses the same mode as the audit
# above (online when a token is set), so an ignore of an online audit is counted too.
expected_ignored=$(sort <<'EXPECTED'
dangerous-triggers	./.github/workflows/security.yml	workflow_run is almost always used insecurely
EXPECTED
)
ignored=$("${ZIZMOR_BIN}" "${ARGS[@]}" --no-ignores --no-exit-codes --quiet --format json . |
  jq -r '.[] | select(.ignored) | [.ident, .locations[0].symbolic.key.Local.verbatim_path, .locations[0].symbolic.annotation] | @tsv' |
  sort) || die "could not list the findings zizmor ignores"
if [ "${ignored}" != "${expected_ignored}" ]; then
  printf 'expected ignored findings:\n%s\nignored findings:\n%s\n' "${expected_ignored}" "${ignored}" >&2
  die "the findings zizmor's ignore comments hide changed; fix the new finding or update expected_ignored"
fi
printf 'zizmor ignore comments hide only the expected findings (%d)\n' "$(grep -c . <<<"${expected_ignored}" || true)"

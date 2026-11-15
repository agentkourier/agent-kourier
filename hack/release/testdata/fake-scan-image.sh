#!/usr/bin/env bash
# The image scanner for the release.sh tests in hack/release/test.sh: appends "<image> <platform>" to FAKE_SCAN_LOG,
# writes the report, and fails for an image whose reference ends with FAKE_SCAN_FAIL, as a finding would.
set -euo pipefail
: "${FAKE_SCAN_LOG:?}"
echo "$1 $2" >>"${FAKE_SCAN_LOG}"
echo '{}' >"$3"
if [ -n "${FAKE_SCAN_FAIL:-}" ] && [[ "$1" == *"${FAKE_SCAN_FAIL}" ]]; then
  exit 1
fi

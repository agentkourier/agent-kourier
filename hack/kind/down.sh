#!/usr/bin/env bash
# Remove what up-full.sh and up-fast.sh created: the courier-full and courier-fast kind clusters
# and the images they built (mockllm, the test doubles, the E2E runner). Nothing else is touched.
# Usage: down.sh [full|fast]; with no argument both tiers go, and one that is not up is skipped.
# One full tier per Docker host: worktrees share the fixed cluster names, so this deletes a
# cluster even when another checkout made it (and says so).
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"

tiers=(full fast)
case "${1:-}" in
  "") ;;
  full | fast) tiers=("$1") ;;
  *) die "usage: down.sh [full|fast]" ;;
esac

for tier in "${tiers[@]}"; do
  use_tier "${tier}"
  if cluster_exists; then
    assert_owned
    owner=$(owner_checkout)
    [ "${owner}" = "${ROOT}" ] ||
      echo "warning: ${CLUSTER} was created from checkout ${owner}, not ${ROOT}; deleting it anyway" >&2
    kind delete cluster --name "${CLUSTER}"
  else
    echo "kind cluster ${CLUSTER} does not exist"
  fi
  # Both tiers build the test doubles and the runner; only the full tier builds mockllm.
  repos=(courier-e2e/fast-doubles courier-e2e/runner courier-e2e/courier-testhooks)
  [ "${tier}" = full ] && repos+=(courier-e2e/mockllm)
  for repo in "${repos[@]}"; do
    for image in $(docker image ls -q "${repo}" | sort -u); do
      docker image rm "${image}" >/dev/null 2>&1 || true
    done
  done
done

#!/usr/bin/env bash
# Remove what up-full.sh and up-fast.sh created: the courier-full and courier-fast kind clusters
# and the images they built (mockllm, the test doubles, the E2E runner). Nothing else is touched.
# Usage: down.sh [full|fast|ax]; with no argument all tiers go, and one that is not up is skipped.
# One full tier per Docker host: worktrees share the fixed cluster names, so this deletes a
# cluster even when another checkout made it (and says so).
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"

tiers=(full fast ax)
case "${1:-}" in
  "") ;;
  full | fast | ax) tiers=("$1") ;;
  *) die "usage: down.sh [full|fast|ax]" ;;
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
  if [ "${tier}" = ax ] && docker inspect courier-ax-registry >/dev/null 2>&1; then
    [ "$(docker inspect -f '{{index .Config.Labels "dev.agent-courier.ax-registry"}}' courier-ax-registry)" = true ] || die "courier-ax-registry is not owned by this tooling"
    docker rm -f courier-ax-registry >/dev/null
  fi

  # Both tiers build the test doubles and the runner; only the full tier builds mockllm.
  repos=(courier-e2e/fast-doubles courier-e2e/runner courier-e2e/courier-testhooks)
  [ "${tier}" = full ] && repos+=(courier-e2e/mockllm)
  [ "${tier}" = ax ] && repos+=(courier-e2e/ax localhost:5002/courier-ax)
  for repo in "${repos[@]}"; do
    for image in $(docker image ls --format '{{.Repository}}:{{.Tag}}' "${repo}" | sort -u); do
      docker image rm "${image}" >/dev/null 2>&1 || true
    done
  done
done

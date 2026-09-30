#!/usr/bin/env bash
# Remove what up-full.sh created: the courier-full kind cluster and the mockllm image it
# built. Nothing else is touched. The fast tier's teardown joins here in WP-13.
# One full tier per Docker host: worktrees share the fixed cluster name, so this deletes
# the cluster even when another checkout made it (and says so).
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"

if cluster_exists; then
  assert_owned
  owner=$(owner_checkout)
  [ "${owner}" = "${ROOT}" ] ||
    echo "warning: ${CLUSTER} was created from checkout ${owner}, not ${ROOT}; deleting it anyway" >&2
  kind delete cluster --name "${CLUSTER}"
else
  echo "kind cluster ${CLUSTER} does not exist"
fi
for image in $(docker image ls -q courier-e2e/mockllm | sort -u); do
  docker image rm "${image}" >/dev/null 2>&1 || true
done

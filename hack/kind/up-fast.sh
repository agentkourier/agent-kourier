#!/usr/bin/env bash
# Bring up the fast E2E tier from nothing: a kind cluster (courier-fast) with prometheus-operator
# and one Alertmanager, the fake Slack, the stub agent (-require-auth) and a sink webhook
# receiver, and courier's image when one exists (spec 12.1). Safe to re-run.
# The fast tier is its own cluster, so it runs beside the full tier; `make e2e-down` removes both
# (hack/kind/down.sh fast removes only this one).
#
# Every kubectl and helm call names --context kind-courier-fast and every kind call names
# --name courier-fast: the host kubeconfig may hold other clusters, production among them, and
# the current context is never relied on. assert_context aborts unless that context is the local
# kind cluster.
#
# The operator's CRDs are installed by helm on first install only; helm never upgrades them. To
# change OPERATOR_CHART_VERSION (in lib.sh), run `hack/kind/down.sh fast` first.
#
# Courier's image (COURIER_IMAGE, default courier:dev) is loaded if Docker has it. The courier
# binary is not a broker until WP-35, which adds the Dockerfile; until then the step says so and
# skips.
set -eEuo pipefail

COURIER_IMAGE=${COURIER_IMAGE:-courier:dev}

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
use_tier fast
FAST=${HERE}/fast
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")

STEP=preflight
step() {
  STEP=$*
  echo "==> $*"
}
on_error() {
  echo "error: step '${STEP}' failed (line $1). Recover with: hack/kind/down.sh fast && make e2e-fast-up" >&2
}
trap 'on_error $LINENO' ERR
BUILD=
trap '[ -z "${BUILD}" ] || rm -rf "${BUILD}"' EXIT

for tool in docker kind kubectl helm go; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"

step "kind cluster ${CLUSTER}"
if cluster_exists; then
  assert_owned
  echo "reusing the cluster created earlier (from $(owner_checkout))"
else
  kind create cluster --name "${CLUSTER}" --config "${FAST}/kind-config.yaml" --image "${NODE_IMAGE}"
  assert_context fast
  "${K[@]}" annotate nodes -l "${OWNER_LABEL}" "${CHECKOUT_KEY}=${ROOT}" >/dev/null
fi
assert_context fast

step "prometheus-operator ${OPERATOR_CHART_VERSION}"
install_operator

step "test doubles image"
build_doubles_image

step "fake Slack, stub agent, sink, Alertmanager"
apply_doubles

step "courier image ${COURIER_IMAGE}"
if docker image inspect "${COURIER_IMAGE}" >/dev/null 2>&1; then
  kind load docker-image "${COURIER_IMAGE}" --name "${CLUSTER}"
else
  echo "skipping: no Docker image ${COURIER_IMAGE}. Courier is not a broker until WP-35, which adds its Dockerfile; build the image and re-run to load it."
fi

step "verify"
"${FAST}/verify.sh"
echo "fast tier up in ${SECONDS}s (context ${CTX})"

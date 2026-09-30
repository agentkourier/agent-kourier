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
# change OPERATOR_CHART_VERSION, run `hack/kind/down.sh fast` first.
#
# Courier's image (COURIER_IMAGE, default courier:dev) is loaded if Docker has it. The courier
# binary is not a broker until WP-35, which adds the Dockerfile; until then the step says so and
# skips.
set -eEuo pipefail

OPERATOR_OCI=oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack
OPERATOR_CHART_VERSION=91.8.2
DOUBLES_IMAGE=courier-e2e/fast-doubles:dev
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

# assert_context: die unless the kubeconfig context this script is about to use is the local
# courier-fast kind cluster. The name is checked literally and the API server must be on
# loopback, which no remote cluster is.
assert_context() {
  local name server
  name=$(kubectl config view --minify --context "${CTX}" -o 'jsonpath={.contexts[0].name}' 2>/dev/null) || name=
  server=$(kubectl config view --minify --context "${CTX}" -o 'jsonpath={.clusters[0].cluster.server}' 2>/dev/null) || server=
  [ "${CTX}" = kind-courier-fast ] && [ "${name}" = kind-courier-fast ] ||
    die "context '${name:-?}' is not kind-courier-fast; refusing to run kubectl or helm"
  case "${server}" in
    https://127.0.0.1:* | https://localhost:*) ;;
    *) die "context ${CTX} points at a non-local API server; refusing to run kubectl or helm" ;;
  esac
}

step "kind cluster ${CLUSTER}"
if cluster_exists; then
  assert_owned
  echo "reusing the cluster created earlier (from $(owner_checkout))"
else
  kind create cluster --name "${CLUSTER}" --config "${FAST}/kind-config.yaml" --image "${NODE_IMAGE}"
  assert_context
  "${K[@]}" annotate nodes -l "${OWNER_LABEL}" "${CHECKOUT_KEY}=${ROOT}" >/dev/null
fi
assert_context

step "prometheus-operator ${OPERATOR_CHART_VERSION}"
"${H[@]}" upgrade --install prometheus-operator "${OPERATOR_OCI}" --version "${OPERATOR_CHART_VERSION}" \
  --namespace monitoring --create-namespace -f "${FAST}/operator-values.yaml" --wait --timeout 4m >/dev/null

step "test doubles image"
# Built on the host for the Docker daemon's architecture, then copied into the pinned
# distroless image (fast/Dockerfile): no Go image to pull.
case "$(docker info --format '{{.Architecture}}')" in
  aarch64 | arm64) goarch=arm64 ;;
  x86_64 | amd64) goarch=amd64 ;;
  *) die "unsupported Docker architecture" ;;
esac
BUILD=$(mktemp -d)
for double in fakeslack stubagent sink; do
  GOOS=linux GOARCH=${goarch} CGO_ENABLED=0 go build -C "${ROOT}" -o "${BUILD}/${double}" "./test/${double}/cmd/${double}"
done
docker build -q -f "${FAST}/Dockerfile" --build-arg "RUNTIME_IMAGE=${RUNTIME_IMAGE}" -t "${DOUBLES_IMAGE}" "${BUILD}" >/dev/null
kind load docker-image "${DOUBLES_IMAGE}" --name "${CLUSTER}"

step "fake Slack, stub agent, sink, Alertmanager"
"${K[@]}" apply -f "${FAST}/doubles.yaml" >/dev/null
# Restart so a rebuilt image (same tag) is picked up.
"${K[@]}" -n e2e rollout restart deploy/fakeslack deploy/stubagent deploy/sink >/dev/null
"${K[@]}" -n e2e rollout status deploy/fakeslack deploy/stubagent deploy/sink --timeout=180s
"${K[@]}" apply -f "${FAST}/alertmanager.yaml" >/dev/null
"${K[@]}" -n monitoring wait --for=condition=Available alertmanager/courier --timeout=180s

step "courier image ${COURIER_IMAGE}"
if docker image inspect "${COURIER_IMAGE}" >/dev/null 2>&1; then
  kind load docker-image "${COURIER_IMAGE}" --name "${CLUSTER}"
else
  echo "skipping: no Docker image ${COURIER_IMAGE}. Courier is not a broker until WP-35, which adds its Dockerfile; build the image and re-run to load it."
fi

step "verify"
"${FAST}/verify.sh"
echo "fast tier up in ${SECONDS}s (context ${CTX})"

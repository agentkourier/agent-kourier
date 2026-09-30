# shellcheck shell=bash
# Shared by up-full.sh, up-fast.sh, fast/verify.sh and down.sh (sourced, not executed).

# The kind node image and the base of the images the tiers build (mockllm; the fast tier's test
# doubles, through a build argument). Used by the scripts that source this file.
# shellcheck disable=SC2034
NODE_IMAGE=kindest/node:v1.35.0@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661
RUNTIME_IMAGE=gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
# One full tier per Docker host: the cluster name is fixed, so two checkouts share it. The fast
# tier is a separate cluster; use_tier fast points CLUSTER and OWNER_LABEL at it.
CLUSTER=courier-full
# Set on the kind node by full/kind-config.yaml (fast/kind-config.yaml for the fast tier);
# proves this tooling created the cluster.
OWNER_LABEL=courier.agent-courier.dev/owner=up-full
# Set by up-full.sh after creation; records which checkout owns the cluster.
CHECKOUT_KEY=courier.agent-courier.dev/checkout

die() {
  echo "error: $*" >&2
  exit 1
}

# use_tier full|fast: make the functions below act on that tier's cluster.
use_tier() {
  case "$1" in
    full | fast)
      CLUSTER=courier-$1
      OWNER_LABEL=courier.agent-courier.dev/owner=up-$1
      ;;
    *) die "unknown tier '$1' (want full or fast)" ;;
  esac
}

# node_kubectl ARGS...: kubectl against the CLUSTER cluster using kind's own kubeconfig
# for it, never the host's current context.
node_kubectl() {
  # kind's stderr is dropped: callers report failure themselves, and its message must not
  # read as advice (the ERR trap in up-full.sh prints the recovery step).
  kubectl --kubeconfig <(kind get kubeconfig --name "${CLUSTER}" 2>/dev/null) --request-timeout=20s "$@"
}

cluster_exists() {
  kind get clusters | grep -qx "${CLUSTER}"
}

# assert_context: die unless the kubeconfig context in CTX, which the fast tier's scripts pass to
# every kubectl and helm call, is the local courier-fast kind cluster. The name is checked
# literally and the API server must be on loopback, which no remote cluster is.
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

# assert_owned: die unless the existing courier-full cluster was created by this tooling.
# "Cannot ask" (API server down) is reported differently from "not ours".
assert_owned() {
  node_kubectl get nodes -o name >/dev/null 2>&1 ||
    die "${CLUSTER} exists but its API server is unreachable; if it is yours, run \`kind delete cluster --name ${CLUSTER}\`"
  [ -n "$(node_kubectl get nodes -l "${OWNER_LABEL}" -o name 2>/dev/null)" ] ||
    die "kind cluster ${CLUSTER} exists but was not created by this script; refusing to touch it"
}

# owner_checkout: the checkout path recorded on the node, or "unknown".
owner_checkout() {
  local path
  # jsonpath wants the dots in the annotation key escaped.
  path=$(node_kubectl get nodes -l "${OWNER_LABEL}" \
    -o "jsonpath={.items[0].metadata.annotations.${CHECKOUT_KEY//./\\.}}" 2>/dev/null) || path=
  echo "${path:-unknown}"
}

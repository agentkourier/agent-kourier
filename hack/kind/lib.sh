# shellcheck shell=bash
# Shared by up-full.sh and down.sh (sourced, not executed).

# One full tier per Docker host: the cluster name is fixed, so two checkouts share it.
CLUSTER=courier-full
# Set on the kind node by full/kind-config.yaml; proves this tooling created the cluster.
OWNER_LABEL=courier.agent-courier.dev/owner=up-full
# Set by up-full.sh after creation; records which checkout owns the cluster.
CHECKOUT_KEY=courier.agent-courier.dev/checkout

die() {
  echo "error: $*" >&2
  exit 1
}

# node_kubectl ARGS...: kubectl against the courier-full cluster using kind's own kubeconfig
# for it, never the host's current context.
node_kubectl() {
  # kind's stderr is dropped: callers report failure themselves, and its message must not
  # read as advice (the ERR trap in up-full.sh prints the recovery step).
  kubectl --kubeconfig <(kind get kubeconfig --name "${CLUSTER}" 2>/dev/null) --request-timeout=20s "$@"
}

cluster_exists() {
  kind get clusters | grep -qx "${CLUSTER}"
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

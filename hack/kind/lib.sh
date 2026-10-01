# shellcheck shell=bash
# Shared by up-full.sh, up-fast.sh, e2e-run.sh, fast/verify.sh and down.sh (sourced, not executed).

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

# assert_context TIER: die unless the kubeconfig context in CTX, which the scripts pass to every
# kubectl and helm call, is the local kind cluster of that tier: kind-courier-fast or
# kind-courier-full, checked literally against the kubeconfig. Its API server must be on loopback
# and must be the one kind itself reports for that cluster (kind get kubeconfig), so a loopback
# tunnel to another cluster under the same context name is refused too.
assert_context() {
  local tier=$1 name server kind_server
  case "${tier}" in
    fast | full) ;;
    *) die "assert_context: unknown tier '${tier}' (want fast or full)" ;;
  esac
  name=$(kubectl config view --minify --context "${CTX}" -o 'jsonpath={.contexts[0].name}' 2>/dev/null) || name=
  server=$(kubectl config view --minify --context "${CTX}" -o 'jsonpath={.clusters[0].cluster.server}' 2>/dev/null) || server=
  [ "${CTX}" = "kind-courier-${tier}" ] && [ "${name}" = "kind-courier-${tier}" ] ||
    die "context '${name:-?}' is not kind-courier-${tier}; refusing to run kubectl or helm"
  case "${server}" in
    https://127.0.0.1:* | https://localhost:*) ;;
    *) die "context ${CTX} points at a non-local API server; refusing to run kubectl or helm" ;;
  esac
  kind_server=$(kubectl config view --kubeconfig <(kind get kubeconfig --name "courier-${tier}" 2>/dev/null) \
    -o 'jsonpath={.clusters[0].cluster.server}' 2>/dev/null) || kind_server=
  [ -n "${kind_server}" ] && [ "${server}" = "${kind_server}" ] ||
    die "context ${CTX} points at ${server}, not at kind's courier-${tier} (${kind_server:-no such cluster}); refusing to run kubectl or helm"
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

# The test doubles (spec 12.1) are installed the same way in both tiers by up-fast.sh and up-full.sh,
# from one set of manifests in fast/. These functions expect the sourcing script to have set HERE
# (this directory), ROOT, CLUSTER, and K and H (kubectl and helm, each naming the tier's context).
OPERATOR_OCI=oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack
OPERATOR_CHART_VERSION=91.8.2
DOUBLES_IMAGE=courier-e2e/fast-doubles:dev

# install_operator: prometheus-operator, and nothing else of kube-prometheus-stack, in monitoring.
install_operator() {
  "${H[@]}" upgrade --install prometheus-operator "${OPERATOR_OCI}" --version "${OPERATOR_CHART_VERSION}" \
    --namespace monitoring --create-namespace -f "${HERE}/fast/operator-values.yaml" --wait --timeout 4m >/dev/null
}

# build_doubles_image: cross-compile the doubles on the host for the Docker daemon's architecture,
# copy them into the pinned distroless image (fast/Dockerfile; no Go image to pull) and load it into
# the tier's cluster. BUILD is the scratch directory; the caller's EXIT trap removes it.
build_doubles_image() {
  local goarch double
  case "$(docker info --format '{{.Architecture}}')" in
    aarch64 | arm64) goarch=arm64 ;;
    x86_64 | amd64) goarch=amd64 ;;
    *) die "unsupported Docker architecture" ;;
  esac
  BUILD=$(mktemp -d)
  for double in fakeslack stubagent sink; do
    GOOS=linux GOARCH=${goarch} CGO_ENABLED=0 go build -C "${ROOT}" -o "${BUILD}/${double}" "./test/${double}/cmd/${double}"
  done
  docker build -q -f "${HERE}/fast/Dockerfile" --build-arg "RUNTIME_IMAGE=${RUNTIME_IMAGE}" -t "${DOUBLES_IMAGE}" "${BUILD}" >/dev/null
  kind load docker-image "${DOUBLES_IMAGE}" --name "${CLUSTER}"
}

# apply_doubles: the fake Slack, stub agent and sink in namespace e2e, and Alertmanager (with its
# route to the sink) in monitoring; needs install_operator first. None is published: the fake
# Slack's /_control/ has no authentication.
apply_doubles() {
  "${K[@]}" apply -f "${HERE}/fast/doubles.yaml" >/dev/null
  # Restart so a rebuilt image (same tag) is picked up.
  "${K[@]}" -n e2e rollout restart deploy/fakeslack deploy/stubagent deploy/sink >/dev/null
  "${K[@]}" -n e2e rollout status deploy/fakeslack deploy/stubagent deploy/sink --timeout=180s
  "${K[@]}" apply -f "${HERE}/fast/alertmanager.yaml" >/dev/null
  "${K[@]}" -n monitoring wait --for=condition=Available alertmanager/courier --timeout=180s
}

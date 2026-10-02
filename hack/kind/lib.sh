# shellcheck shell=bash
# Shared by up-full.sh, up-fast.sh, e2e-run.sh, fast/verify.sh and down.sh (sourced, not executed).

# The kind node image and the base of the images the tiers build (mockllm; the fast tier's test
# doubles, through a build argument). Used by the scripts that source this file.
# shellcheck disable=SC2034
NODE_IMAGE=kindest/node:v1.35.0@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661
RUNTIME_IMAGE=gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
# The Go image courier is built in (the Dockerfile's builder stage); go.mod's go version is 1.27.1.
GO_IMAGE=golang:1.27.1@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190
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

# Host-side assertions, for the verify scripts: "ok: ..." or "FAIL: ..." and exit 1. (The probe pods
# have their own, in the scripts they run.)
assert() { d=$1; shift; if "$@"; then echo "ok: $d"; else echo "FAIL: $d" >&2; exit 1; fi; } # DESCRIPTION COMMAND...
is_empty() { [ -z "$1" ]; }
not_empty() { [ -n "$1" ]; }

# assert_never_published: prove the test doubles' unauthenticated control surfaces, the fake Slack's
# /_control/ and the stub agent's /_control/release/, are published nowhere (spec 12.1), in whichever
# tier holds them. Needs K (kubectl on the tier's context, as an array), NODE (the kind node's
# container name), jq and docker. For each of the two apps: every Service that selects its pods is
# ClusterIP with no nodePort, external IP or load-balancer ingress, and none of its pods has a
# hostPort or hostNetwork. Once: no Ingress sits in namespace e2e (an Ingress can only name Services
# in its own namespace, so that is every Ingress that could reach the doubles; the full tier's
# charts may have their own elsewhere), and the kind node publishes only the API server, on loopback.
assert_never_published() {
  local app pods published ingresses exposed ports hostports apiserver='^6443/tcp -> 127\.0\.0\.1:[0-9]+$'
  echo "== never published: the fake Slack's /_control/ and the stub agent's /_control/release/ have no authentication"
  for app in fakeslack stubagent; do
    pods=$("${K[@]}" -n e2e get pods -l "app=${app}" -o json)
    assert "a ${app} pod exists (the checks below are not vacuous)" not_empty "$(jq -r '.items[].metadata.name' <<<"${pods}")"
    # A Service selects the pods when every selector pair is one of a pod's labels. A Service with
    # no selector and hand-written EndpointSlices is out of scope: nothing here makes one.
    published=$("${K[@]}" -n e2e get services -o json | jq -r --argjson pods "$(jq '[.items[].metadata.labels]' <<<"${pods}")" '
      .items[]
      | (.spec.selector // {}) as $sel
      | select(($sel | length) > 0 and any($pods[]; . as $l | all(($sel | to_entries[]); $l[.key] == .value)))
      | select(.spec.type != "ClusterIP" or any(.spec.ports[]?; .nodePort != null)
               or ((.spec.externalIPs // []) | length > 0) or ((.status.loadBalancer.ingress // []) | length > 0))
      | .metadata.name')
    assert "no Service for ${app} is NodePort, LoadBalancer or has external IPs: [${published}]" is_empty "${published}"
    assert "${app} has a ClusterIP Service" not_empty "$("${K[@]}" -n e2e get service "${app}" -o jsonpath='{.spec.clusterIP}')"
    exposed=$(jq -r '.items[] | select(.spec.hostNetwork == true or any(.spec.containers[].ports[]?; .hostPort != null)) | .metadata.name' <<<"${pods}")
    assert "no ${app} pod uses hostPort or hostNetwork: [${exposed}]" is_empty "${exposed}"
  done
  ingresses=$("${K[@]}" -n e2e get ingress -o name)
  assert "no Ingress in namespace e2e: [${ingresses}]" is_empty "${ingresses}"
  ports=$(docker port "${NODE}" 2>/dev/null) || ports=
  assert "the kind node publishes the API server, 6443/tcp on 127.0.0.1: [${ports}]" grep -q -E "${apiserver}" <<<"${ports}"
  hostports=$(grep -v -E "${apiserver}" <<<"${ports}" || true) # grep exits 1 when nothing else is published
  assert "the kind node publishes nothing but the API server: [${hostports}]" is_empty "${hostports}"
}

# The courier images, built from the repo's Dockerfile. COURIER_IMAGE is the production build, which no
# kind tier runs. TESTHOOKS_IMAGE is built with the testhooks tag (the crash hook and the Slack API URL
# override of internal/testhooks): the tiers run it, because only it can be pointed at the fake Slack
# and made to crash. It is never pushed, it lives under the local courier-e2e/ prefix, and it carries
# TESTHOOKS_LABEL, which the load into a tier checks.
COURIER_IMAGE=courier:dev
TESTHOOKS_IMAGE=courier-e2e/courier-testhooks:dev
TESTHOOKS_LABEL=dev.agent-courier.testhooks

# build_courier_image [testhooks]: build COURIER_IMAGE, or with "testhooks" TESTHOOKS_IMAGE, for the
# Docker daemon's own architecture. Needs ROOT, the repository root.
build_courier_image() {
  local image=${COURIER_IMAGE} tags=() label=()
  if [ "${1:-}" = testhooks ]; then
    image=${TESTHOOKS_IMAGE} tags=(--build-arg TAGS=testhooks) label=(--label "${TESTHOOKS_LABEL}=true")
  fi
  docker build -q -f "${ROOT}/Dockerfile" --build-arg "GO_IMAGE=${GO_IMAGE}" --build-arg "RUNTIME_IMAGE=${RUNTIME_IMAGE}" \
    ${tags[@]+"${tags[@]}"} ${label[@]+"${label[@]}"} -t "${image}" "${ROOT}" >/dev/null # (an empty array is unset to bash 3.2)
}

# assert_testhooks_image IMAGE: die unless IMAGE carries TESTHOOKS_LABEL, so a production image is never
# loaded into a tier by mistake. Run it before every kind load of a courier image.
assert_testhooks_image() {
  [ "$(docker image inspect --format "{{index .Config.Labels \"${TESTHOOKS_LABEL}\"}}" "$1" 2>/dev/null)" = true ] ||
    die "$1 is not a testhooks image (no ${TESTHOOKS_LABEL}=true label); refusing to load it into a tier"
}

# load_courier_image: build the testhooks image, check its label, and load it into the tier's cluster. The tiers
# run the testhooks build, which only can be pointed at the fake Slack. Nothing here pushes either image.
load_courier_image() {
  build_courier_image testhooks
  assert_testhooks_image "${TESTHOOKS_IMAGE}"
  kind load docker-image "${TESTHOOKS_IMAGE}" --name "${CLUSTER}"
}

# deploy_courier AGENTBACKEND: courier's namespace, fake Slack Secret, config and manifests, which both tiers
# share (fast/courier), and a rollout restart so a rebuilt image or a changed ConfigMap is picked up. The config
# is the fast tier's Bindings and ChatConnection, with AGENTBACKEND, a file, in place of the fast tier's
# AgentBackend: the Bindings name it `agents` in both tiers. The Secrets are fake constants made here and never
# committed, and the agent token (Secret courier-agent-token) is the caller's to make: it differs by tier.
# Needs HERE, K.
deploy_courier() {
  local config
  config=$(mktemp -d)
  trap 'rm -rf "${config}"; trap - RETURN' RETURN # the scratch directory goes however this ends
  cp "${HERE}"/fast/courier/config/*.yaml "${config}"
  cp "$1" "${config}/agentbackend.yaml"
  "${K[@]}" create namespace courier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n courier create secret generic slack-fast --from-literal=botToken=xoxb-fake-test-token \
    --from-literal=appToken=xapp-fake-test-token --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n courier create configmap courier-config --from-file="${config}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" apply -f "${HERE}/fast/courier" >/dev/null
  "${K[@]}" -n courier rollout restart deploy/courier >/dev/null
}

# The curl image the scripts' probe pods run, pinned; the same as the verify scripts'.
CURL_IMAGE=curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69

# delete_courier_pod NAME: delete a throwaway pod of namespace courier, if there is one. Needs K.
delete_courier_pod() { "${K[@]}" -n courier delete pod "$1" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }

# dex_token_from: the token in what refresh_dex_token's pod printed, on stdin. The pod prints it as one line,
# TOKEN:<jwt>:END, and the token is the first line that is exactly that. kubectl, when it cannot attach to the pod
# under load, falls back to streaming the pod's logs, and can print them twice or cut a copy short and glue it to
# the next: a line that is not wholly good is no token, and nothing is printed for it, so the caller tries again and
# never stores a mangled one.
dex_token_from() {
  sed -n 's/^TOKEN:\([A-Za-z0-9_-]\{1,\}\.[A-Za-z0-9_-]\{1,\}\.[A-Za-z0-9_-]\{1,\}\):END$/\1/p' | head -n 1
}

# refresh_dex_token: mint the Binding identity's ID token from Dex and store it in the Secret
# courier-agent-token, key token, in namespace courier. Dex has no client_credentials grant (H-02), so the
# identity is a static user that signs in with the password grant, and its token lives 24 hours: run this before
# each use of the full tier whose last refresh is not recent (up-full.sh and e2e-run.sh do). The token is made
# inside the cluster by a throwaway pod, where Dex is, and goes from there to the Secret through pipes: it is
# never printed, put on a command line or written to a file. The pod is deleted when this returns, whatever
# happened, since its logs hold the token. Courier reads the Secret on every request, so a refreshed token is sent
# from the next call. The client and the user are the fake ones of full/dex.yaml. Needs K.
refresh_dex_token() {
  local pod tok attempt
  "${K[@]}" create namespace courier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  for attempt in 1 2 3; do
    pod=dex-token-${RANDOM}
    trap 'delete_courier_pod "${pod}"; trap - RETURN' RETURN # a return from here, an error included, still deletes it
    tok=$("${K[@]}" -n courier run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" --command -- sh -c '
      curl -sf -m 20 -d grant_type=password -d client_id=courier-binding -d client_secret=fake-courier-binding-secret \
        -d username=binding-test@example.test -d password=password -d "scope=openid email" \
        http://dex.dex.svc.cluster.local:5556/dex/token | sed -n "s/.*\"id_token\":\"\([^\"]*\)\".*/TOKEN:\1:END/p"' 2>/dev/null | dex_token_from) || tok=
    delete_courier_pod "${pod}"
    [ -z "${tok}" ] || break
  done
  [ -n "${tok}" ] || die "Dex gave no ID token for the Binding identity after three tries (is Dex up? did the pod run?)"
  printf %s "${tok}" | "${K[@]}" -n courier create secret generic courier-agent-token --from-file=token=/dev/stdin \
    --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
}

# deploy_scenario_agents: the full tier's Phase 1 scenario agents (full/scenarios): the scenario model and the MCP
# tool server, whose code is kept in ConfigMaps made from the files there, and the kagent Agents that use them.
# Restarts the two servers so changed code is picked up, and waits for them and for the agents. Needs HERE, K.
deploy_scenario_agents() {
  local dir=${HERE}/full/scenarios
  "${K[@]}" -n kagent create configmap scenariollm --from-file=scenariollm.py="${dir}/scenariollm.py" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n kagent create configmap scenariomcp --from-file=scenariomcp.py="${dir}/scenariomcp.py" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" apply -f "${dir}/scenarios.yaml" >/dev/null
  "${K[@]}" -n kagent rollout restart deploy/scenariollm deploy/scenariomcp >/dev/null
  "${K[@]}" -n kagent rollout status deploy/scenariollm deploy/scenariomcp --timeout=180s
  "${K[@]}" -n kagent wait --for=condition=Ready agent/scenario-gated agent/scenario-memory agent/scenario-ask agent/spike-approve --timeout=300s
}

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
    full | fast | ax)
      CLUSTER=courier-$1
      OWNER_LABEL=courier.agent-courier.dev/owner=up-$1
      ;;
    *) die "unknown tier '$1' (want full, fast or ax)" ;;
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
    fast | full | ax) ;;
    *) die "assert_context: unknown tier '${tier}' (want fast, full or ax)" ;;
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

# refuse_while_sandbox_up: die while the sandbox tier's courier (hack/kind/sandbox) is in the cluster. Its pod carries the label
# app.kubernetes.io/name=courier that the front door's NetworkPolicy trusts (so it must), and the tests find courier's pods by it, so a run of
# the E2E tests would drive the sandbox's courier and a bring-up would start the tier's beside it. An API error is not a refusal: only a
# Deployment that is there is. Needs K.
refuse_while_sandbox_up() {
  local found
  found=$("${K[@]}" -n courier get deploy courier-sandbox --ignore-not-found -o name 2>/dev/null) || found=
  [ -z "${found}" ] || die "the sandbox tier is up (${found}); its courier carries the tier's label. Run: make sandbox-down first"
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
# assert_app_unpublished NAMESPACE APP: the pods labelled app=APP in NAMESPACE are there (so the checks are not
# vacuous), and every Service that selects them is ClusterIP with no nodePort, external IP or load-balancer ingress,
# and none of them has a hostPort or hostNetwork. A Service selects the pods when every selector pair is one of a
# pod's labels. A Service with no selector and hand-written EndpointSlices is out of scope: nothing here makes one.
# Needs K, jq.
assert_app_unpublished() {
  local ns=$1 app=$2 pods published exposed
  pods=$("${K[@]}" -n "${ns}" get pods -l "app=${app}" -o json)
  assert "a ${app} pod exists (the checks below are not vacuous)" not_empty "$(jq -r '.items[].metadata.name' <<<"${pods}")"
  # A Service selects the pods when every selector pair is one of a pod's labels. A Service with
  # no selector and hand-written EndpointSlices is out of scope: nothing here makes one.
  published=$("${K[@]}" -n "${ns}" get services -o json | jq -r --argjson pods "$(jq '[.items[].metadata.labels]' <<<"${pods}")" '
    .items[]
    | (.spec.selector // {}) as $sel
    | select(($sel | length) > 0 and any($pods[]; . as $l | all(($sel | to_entries[]); $l[.key] == .value)))
    | select(.spec.type != "ClusterIP" or any(.spec.ports[]?; .nodePort != null)
             or ((.spec.externalIPs // []) | length > 0) or ((.status.loadBalancer.ingress // []) | length > 0))
    | .metadata.name')
  assert "no Service for ${app} is NodePort, LoadBalancer or has external IPs: [${published}]" is_empty "${published}"
  assert "${app} has a ClusterIP Service" not_empty "$("${K[@]}" -n "${ns}" get service "${app}" -o jsonpath='{.spec.clusterIP}')"
  exposed=$(jq -r '.items[] | select(.spec.hostNetwork == true or any(.spec.containers[].ports[]?; .hostPort != null)) | .metadata.name' <<<"${pods}")
  assert "no ${app} pod uses hostPort or hostNetwork: [${exposed}]" is_empty "${exposed}"
}

assert_never_published() {
  local app ingresses ports hostports apiserver='^6443/tcp -> 127\.0\.0\.1:[0-9]+$'
  echo "== never published: the fake Slack's /_control/ and the stub agent's /_control/release/ have no authentication"
  for app in fakeslack stubagent; do
    assert_app_unpublished e2e "${app}"
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

# Courier is installed in every tier by `helm upgrade --install` of charts/courier, with the values file of the tier (charts/courier/ci:
# fast-values.yaml for the fast and full tiers, ax-values.yaml for the AX tier, sandbox-values.yaml for the sandbox). What each tier makes
# itself, and the chart does not, is the config (a ConfigMap built from the tier's files, named by config.existingConfigMap) and the
# Secrets (fake constants made here, or the operator's in the sandbox); the values hold neither. The functions below expect the sourcing
# script to have set ROOT, K and H (kubectl and helm, each naming the tier's context).

# adopt_into_release RELEASE KIND/NAME...: let Helm take over the hand-made objects of namespace courier that the chart now makes (the
# annotations and label Helm checks before it will manage an object it did not create). Objects that are not there, or that some release
# already owns, are left alone: Helm refuses the second, saying which.
adopt_into_release() {
  local release=$1 obj owner
  shift
  for obj in "$@"; do
    [ -n "$("${K[@]}" -n courier get "${obj}" --ignore-not-found -o name)" ] || continue
    owner=$("${K[@]}" -n courier get "${obj}" -o 'jsonpath={.metadata.annotations.meta\.helm\.sh/release-name}')
    [ -z "${owner}" ] || continue
    "${K[@]}" -n courier annotate --overwrite "${obj}" "meta.helm.sh/release-name=${release}" meta.helm.sh/release-namespace=courier >/dev/null
    "${K[@]}" -n courier label --overwrite "${obj}" app.kubernetes.io/managed-by=Helm >/dev/null
  done
}

# replace_unowned_deployment NAME: delete the Deployment NAME of namespace courier when no Helm release owns it, which is how a tier brought
# up before the chart has it (hand-made manifests). The chart's Deployment selects pods by app.kubernetes.io/instance as well as by name, and
# a Deployment's selector cannot change, so it cannot be upgraded in place: it is deleted, and its pods with it (foreground, so that the old
# pod has released the SQLite volume before the new one starts), and the volume stays. One a release owns, or none, is left as it is.
replace_unowned_deployment() {
  local name=$1 owner
  [ -n "$("${K[@]}" -n courier get "deploy/${name}" --ignore-not-found -o name)" ] || return 0
  owner=$("${K[@]}" -n courier get "deploy/${name}" -o 'jsonpath={.metadata.annotations.meta\.helm\.sh/release-name}')
  [ -z "${owner}" ] || return 0
  echo "replacing the hand-made Deployment ${name} with the chart's (a Deployment's selector cannot change); its volume stays"
  "${K[@]}" -n courier delete "deploy/${name}" --cascade=foreground --wait --timeout=120s >/dev/null
}

# require_helm4: die unless helm is Helm 4. helm_install_courier needs --force-conflicts, which Helm 3 lacks, and it would be found out only
# after replace_unowned_deployment had deleted a Deployment. Call it in each script's tool preflight, before anything is changed.
require_helm4() {
  local version
  version=$(helm version --template '{{.Version}}' 2>/dev/null) || version=
  case "${version}" in
    v4.*) ;;
    *) die "Helm 4 is required (the chart is installed with --force-conflicts); found '${version:-no helm}'" ;;
  esac
}

# helm_install_courier RELEASE VALUES [HELM ARG...]: install or upgrade the chart as RELEASE in namespace courier with the values file VALUES
# (and any more arguments, such as --set replicaCount=0). It does not wait: callers wait for the rollout, and show courier's log when it
# does not finish. Output is dropped; an error is not. Helm applies server-side, which refuses to change a field that another manager set
# (an adopted Service's selector, kubectl scale's replicas): the release is the owner of these objects, so it wins.
helm_install_courier() {
  local release=$1 values=$2
  shift 2
  "${H[@]}" upgrade --install "${release}" "${ROOT}/charts/courier" --namespace courier -f "${values}" --force-conflicts "$@" >/dev/null
}

# install_courier VALUES: the tier's courier, release "courier": a Deployment, ServiceAccount, Role, PersistentVolumeClaim and Service
# from the chart (the config, which the caller has made as the ConfigMap courier-config, is not the chart's). On a cluster brought up before
# the chart, its hand-made Deployment is replaced and its other objects adopted, so that re-running a bring-up is how a tier moves over, with
# its volume. Then a rollout restart, so that a rebuilt image (same tag) or a changed ConfigMap is picked up: Helm rolls the pod for neither,
# since the values did not change.
install_courier() {
  replace_unowned_deployment courier
  adopt_into_release courier serviceaccount/courier role/courier-secrets rolebinding/courier-secrets service/courier pvc/courier-data
  helm_install_courier courier "$1"
  "${K[@]}" -n courier rollout restart deploy/courier >/dev/null
}

# deploy_courier AGENTBACKEND: courier's namespace, fake Slack Secret, config and chart, which the fast and full tiers share. The config
# is the fast tier's Bindings and ChatConnection (fast/courier/config), with AGENTBACKEND, a file, in place of the fast tier's AgentBackend:
# the Bindings name it `agents` in both tiers. The Secrets are fake constants made here and never committed, and the agent token (Secret
# courier-agent-token) is the caller's to make: it differs by tier. Needs HERE, ROOT, K, H.
deploy_courier() {
  local config
  refuse_while_sandbox_up
  config=$(mktemp -d)
  trap 'rm -rf "${config}"; trap - RETURN' RETURN # the scratch directory goes however this ends
  cp "${HERE}"/fast/courier/config/*.yaml "${config}"
  cp "$1" "${config}/agentbackend.yaml"
  "${K[@]}" create namespace courier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n courier create secret generic slack-fast --from-literal=botToken=xoxb-fake-test-token \
    --from-literal=appToken=xapp-fake-test-token --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n courier create configmap courier-config --from-file="${config}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  install_courier "${ROOT}/charts/courier/ci/fast-values.yaml"
}

# The curl image the scripts' probe pods run, pinned; the same as the verify scripts'.
CURL_IMAGE=curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69

# delete_courier_pod NAME: delete a throwaway pod of namespace courier, if there is one. Needs K.
# An empty NAME is nothing to delete.
delete_courier_pod() { [ -z "$1" ] || "${K[@]}" -n courier delete pod "$1" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }

# DEX_TOKEN_POD is the name of the pod refresh_dex_token is running, for a caller's EXIT trap: a Ctrl-C during its
# `kubectl run` skips the function's own RETURN trap, and the pod's logs hold the token. Empty when there is none.
DEX_TOKEN_POD=

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
    DEX_TOKEN_POD=${pod}
    trap 'delete_courier_pod "${pod}"; trap - RETURN' RETURN # a return from here, an error included, still deletes it
    tok=$("${K[@]}" -n courier run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" --command -- sh -c '
      curl -sf -m 20 -d grant_type=password -d client_id=courier-binding -d client_secret=fake-courier-binding-secret \
        -d username=binding-test@example.test -d password=password -d "scope=openid email" \
        http://dex.dex.svc.cluster.local:5556/dex/token | sed -n "s/.*\"id_token\":\"\([^\"]*\)\".*/TOKEN:\1:END/p"' 2>/dev/null | dex_token_from) || tok=
    delete_courier_pod "${pod}"
    DEX_TOKEN_POD=
    [ -z "${tok}" ] || break
  done
  [ -n "${tok}" ] || die "Dex gave no ID token for the Binding identity after three tries (is Dex up? did the pod run?)"
  printf %s "${tok}" | "${K[@]}" -n courier create secret generic courier-agent-token --from-file=token=/dev/stdin \
    --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
}

# deploy_scenario_agents: the full tier's Phase 1 scenario agents (full/scenarios): the scenario model and the MCP
# tool server, whose code is kept in ConfigMaps made from the files there (servers.yaml), and the kagent Agents that
# use them (scenarios.yaml), made after the servers answer. Restarts the two servers so changed code is picked up, and
# waits for them, for kagent to have accepted the tool server, and for the agents. Needs HERE, K.
deploy_scenario_agents() {
  local dir=${HERE}/full/scenarios
  "${K[@]}" -n kagent create configmap scenariollm --from-file=scenariollm.py="${dir}/scenariollm.py" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n kagent create configmap scenariomcp --from-file=scenariomcp.py="${dir}/scenariomcp.py" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  # The servers first, and the agents only once the tool server answers and kagent has discovered its tool (the wait is
  # for the RemoteMCPServer's Accepted=True with reason DiscoverySucceeded, which kagent sets only after it has
  # listed the server's tools, so a server that is up but not yet answering does not pass it): an agent
  # made beside a server that is still starting, or about to be restarted, can have its golden actor (the snapshot of
  # its template) crash on a tool server that is not there (GoldenActorCrashed), and then it never becomes Ready.
  "${K[@]}" apply -f "${dir}/servers.yaml" >/dev/null
  "${K[@]}" -n kagent rollout restart deploy/scenariollm deploy/scenariomcp >/dev/null
  "${K[@]}" -n kagent rollout status deploy/scenariollm deploy/scenariomcp --timeout=180s
  "${K[@]}" -n kagent wait --for=condition=Accepted remotemcpserver/scenariomcp --timeout=180s
  "${K[@]}" apply -f "${dir}/scenarios.yaml" >/dev/null
  "${K[@]}" -n kagent wait --for=condition=Ready agent/scenario-gated agent/scenario-memory agent/scenario-ask agent/spike-approve agent/scenario-tools --timeout=300s
}

# probe_frontdoor: whether a pod labelled as courier reaches kagent's front door by its Service name, which is the
# one path the full tier exists to show. Five tries a few seconds apart, from a throwaway pod that is deleted after.
# Needs K.
probe_frontdoor() {
  local pod=probe-door-${RANDOM} rc=0
  # shellcheck disable=SC2016 # the script runs in the pod, where its variables are
  "${K[@]}" -n courier run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" \
    --labels=app.kubernetes.io/name=courier --command -- sh -c '
    for _ in 1 2 3 4 5; do
      [ "$(curl -s -m 6 -o /dev/null -w "%{http_code}" http://kagent-frontdoor.kagent.svc.cluster.local:4180/ping)" = 200 ] && exit 0
      sleep 2
    done
    exit 1' >/dev/null 2>&1 || rc=$?
  delete_courier_pod "${pod}"
  return "${rc}"
}

# settle_full_tier: before the full tier is verified, make sure the cluster is in a state its checks can mean
# something in. On a loaded host the kagent controller can crash-loop on a DNS lookup that was cancelled, and
# kindnet's network policy engine can time connections out (it logs "netlink receive: no such file or directory"
# in bursts); a restart of either clears it. So: the controller is Ready (its pod is deleted, up to twice, if it is
# not), then a courier-labelled pod reaches the front door by its Service name (kindnet is restarted, up to twice, if
# it does not). Every wait is bounded, and it dies after the last. Needs K.
settle_full_tier() {
  local attempt
  for attempt in 1 2 3; do
    if "${K[@]}" -n kagent rollout status deploy/kagent-controller --timeout=120s >/dev/null 2>&1; then
      break
    fi
    [ "${attempt}" -lt 3 ] || die "the kagent controller did not become ready"
    echo "the kagent controller is not ready (try ${attempt}); deleting its pod" >&2
    "${K[@]}" -n kagent delete pod -l app.kubernetes.io/component=controller,app.kubernetes.io/name=kagent --wait=false >/dev/null 2>&1 || true
  done
  for attempt in 1 2 3; do
    if probe_frontdoor; then
      echo "ok: the front door answers by its Service name"
      return 0
    fi
    [ "${attempt}" -lt 3 ] || break
    echo "the front door did not answer by its Service name (try ${attempt}); restarting kindnet" >&2
    restart_kindnet
  done
  die "the front door never answered by its Service name, even after restarting kindnet"
}

# The full tier's network policy is enforced by kindnet (the node image's CNI, which also runs kube-network-policies), and it can
# go stale without a sign: when it loses the API server (a host under load: `"Failed to watch" ... TLS handshake timeout`,
# `http2: client connection lost`), its pod cache freezes and NetworkPolicy stops applying to pods made after that. A courier-labelled
# pod then reaches the kagent controller on 8083, which controller-admit-frontdoor-only must refuse, and every test that leans on the
# policy is wrong with nothing saying so (seen 2026-10-03; found 36 hours later). The fast tier has no kagent and no NetworkPolicy,
# so none of this applies there.
KINDNET_LOG_SINCE=10m
NETPOL_PROBE_URL=http://kagent-controller.kagent.svc.cluster.local:8083
# The control of the probe: a path the policy lets a courier-labelled pod take (the front door's), asked first. A pod that cannot
# take it either has not been learned by kindnet yet (it logs "netlink receive: no such file or directory" in bursts under load, and a
# pod it does not know is dropped), and its timeout on the controller would show nothing.
NETPOL_CONTROL_URL=http://kagent-frontdoor.kagent.svc.cluster.local:4180/ping
# Seconds between the re-probes after a restart (a restarted kindnet takes a few seconds to learn the pods); tests set it to 0.
NETPOL_RETRY_SLEEP=5

# kindnet_log_recent: kindnet's own log for the last KINDNET_LOG_SINCE (what a restarted kindnet has not yet written is not here,
# so a restart clears it). Empty when it cannot be read. Needs K.
kindnet_log_recent() {
  "${K[@]}" -n kube-system logs -l app=kindnet --since="${KINDNET_LOG_SINCE}" --tail=2000 2>/dev/null || true
}

# kindnet_log_failing TEXT: whether a kindnet log shows it has lost the API server.
kindnet_log_failing() {
  grep -q -E 'Failed to watch|TLS handshake timeout|http2: client connection lost' <<<"$1"
}

# NETPOL_PROBE_LABEL marks the probe pods of policy_probe, so that delete_netpol_probes finds them. The probe runs inside command
# substitutions, which cannot hand a pod name back to the caller's trap, and the pod carries the courier label (a leftover one fails
# the next e2e-full, which wants exactly one such pod): so the traps delete by this label.
NETPOL_PROBE_LABEL=courier.agent-courier.dev/probe=netpol

# NETPOL_PROBE_SCRIPT: what the probe pod runs, with the control's URL as $1 and the controller's as $2. It prints one line,
# PROBE:<door>:<controller>:<connect>, where door and controller are curl's exit statuses (the door's is 0 once an attempt in up to
# five has been answered, else 1) and connect is the controller request's time_connect, 0.000000 when no connection was made.
# --connect-timeout is under -m so that a drop is seen as a connection that never started, not as a slow answer.
# shellcheck disable=SC2016 # it runs in the pod, where its variables are
NETPOL_PROBE_SCRIPT='
door=1
for _ in 1 2 3 4 5; do
  if curl -sf -m 5 -o /dev/null "$1"; then door=0; break; fi
  sleep 2
done
connect=$(curl -s --connect-timeout 6 -m 8 -o /dev/null -w "%{time_connect}" "$2")
echo "PROBE:${door}:$?:${connect}"'

# policy_probe_parse: from what the probe pod printed, on stdin, "DOOR CONTROLLER CONNECT" for the first line that is wholly a PROBE
# line, else nothing. kubectl, when it cannot attach under load, falls back to the pod's logs and can print them twice or cut a copy
# short and glue it to the next, as with the Dex token (dex_token_from): a line that is not wholly good is no verdict.
policy_probe_parse() {
  sed -n 's/^PROBE:\([0-9]\{1,\}\):\([0-9]\{1,\}\):\([0-9.]\{1,\}\)$/\1 \2 \3/p' | head -n 1
}

# NETPOL_PROBING is set by the callers of network_policy_verdict, in their own shell (not in its command substitution), so that
# delete_netpol_probes asks the cluster nothing when no probe could have run (a refusal before the check, say).
NETPOL_PROBING=

# delete_netpol_probes: delete the probe pods, for an EXIT trap (a Ctrl-C skips policy_probe's own delete). Needs K.
delete_netpol_probes() {
  [ -n "${NETPOL_PROBING}" ] || return 0
  "${K[@]}" -n courier delete pod -l "${NETPOL_PROBE_LABEL}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

# policy_probe: from a throwaway pod labelled as courier, in namespace courier, "DOOR CONTROLLER CONNECT" (NETPOL_PROBE_SCRIPT): the
# front door's /ping, the control; then the kagent controller on 8083 for anything: curl exit 28 with no connection made (a drop, the
# policy working), 0 (an answer of any kind: the policy is not applied), or another (no verdict). Empty if the pod gave none; it is
# tried three times for that. About ten seconds when the policy works. Callers' EXIT traps run delete_netpol_probes. Needs K.
policy_probe() {
  local pod out="" attempt
  "${K[@]}" get namespace courier >/dev/null 2>&1 || "${K[@]}" create namespace courier >/dev/null 2>&1 || true
  for attempt in 1 2 3; do
    pod=probe-netpol-$$-${RANDOM}
    out=$("${K[@]}" -n courier run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" \
      --labels="app.kubernetes.io/name=courier,${NETPOL_PROBE_LABEL}" --command -- \
      sh -c "${NETPOL_PROBE_SCRIPT}" sh "${NETPOL_CONTROL_URL}" "${NETPOL_PROBE_URL}" 2>/dev/null | policy_probe_parse) || out=
    delete_courier_pod "${pod}"
    [ -z "${out}" ] || break
  done
  printf %s "${out}"
}

# policy_verdict DOOR CONTROLLER CONNECT: what the probe's three numbers say: enforced (the controller was never connected to, and the
# front door answered, so the pod was known), stale (the controller answered, or accepted the connection and only was slow: a policy
# that works does not let it that far), or unknown.
policy_verdict() {
  case "$2" in
    28)
      if [[ $3 =~ ^0(\.0+)?$ ]]; then # no connection was made
        [ "$1" = 0 ] && echo enforced || echo unknown
      else
        echo stale
      fi
      ;;
    0) echo stale ;;
    *) echo unknown ;;
  esac
}

# restart_kindnet: restart ds/kindnet in kube-system and wait for it (120 s). Needs K.
restart_kindnet() {
  "${K[@]}" -n kube-system rollout restart ds/kindnet >/dev/null &&
    "${K[@]}" -n kube-system rollout status ds/kindnet --timeout=120s >/dev/null
}

# network_policy_verdict: whether the tier's network policy is live, as one line, "enforced: ...", "stale: ..." or "unknown: ...".
# The probe is the proof, and it always runs: the log is only a cheap sign, and not a sufficient one (the watches that failed in
# the incident logged about once in three minutes, so a quiet ten is no clean bill, and a frozen cache logs nothing at all).
# The probe costs about ten seconds (a drop is only ever a timeout). The log names the cause when there is one, tips a probe that
# gave no verdict (a DNS or connect error while kindnet is failing) to stale, and is only a note beside a policy that does drop.
# Needs K.
network_policy_verdict() {
  local sign="" door ctrl connect
  if kindnet_log_failing "$(kindnet_log_recent)"; then
    sign="kindnet's log shows failed watches or TLS handshake timeouts in the last ${KINDNET_LOG_SINCE}: it has lost the API server"
  fi
  read -r door ctrl connect <<<"$(policy_probe)" || true # no line: all stay empty
  case "$(policy_verdict "${door}" "${ctrl}" "${connect}")" in
    enforced)
      echo "enforced: a courier-labelled pod reached the front door but got no answer from the kagent controller on 8083, so NetworkPolicy applies${sign:+ (note: ${sign})}"
      return 0
      ;;
    stale)
      echo "stale: a courier-labelled pod's connection to the kagent controller on 8083 was accepted (curl exit ${ctrl}), which controller-admit-frontdoor-only must refuse: kindnet is not applying NetworkPolicy${sign:+; ${sign}}"
      return 0
      ;;
  esac
  if [ -n "${sign}" ]; then
    echo "stale: the probe of the kagent controller on 8083 gave no verdict (curl exit ${ctrl:-none}, front door ${door:-none}), and ${sign}"
  else
    echo "unknown: the probe of the kagent controller on 8083 gave no verdict (curl exit ${ctrl:-none}; the front door, its control, gave ${door:-none}); is kagent up, and is kindnet learning new pods?"
  fi
}

# require_network_policy: die unless the full tier's network policy is live, saying why and what to run. Needs K.
require_network_policy() {
  local line
  NETPOL_PROBING=1
  line=$(network_policy_verdict)
  case "${line%%:*}" in
    enforced) echo "ok: ${line#*: }" ;;
    stale) die "the network policy is not live in ${CLUSTER}: ${line#*: }
  Run: make sandbox-heal (it restarts kindnet), or: ${K[*]} -n kube-system rollout restart ds/kindnet" ;;
    *) die "cannot tell whether the network policy is live in ${CLUSTER}: ${line#*: }
  Look at: ${K[*]} -n kagent get pods; then run make sandbox-heal again" ;;
  esac
}

# probe_ax_agents: verify authenticated cards through Courier's actual cluster network path.
# The token is read from a Secret volume and passed to curl's stdin config, never its argv or logs.
probe_ax_agents() {
  local pod=probe-ax-${RANDOM} phase deadline=$((SECONDS + 300))
  sed "s/@NAME@/${pod}/g" "${HERE}/ax/probe.yaml" | "${K[@]}" apply -f - >/dev/null
  while :; do
    phase=$("${K[@]}" -n courier get pod "${pod}" -o jsonpath='{.status.phase}') || phase=Failed
    case "${phase}" in
      Succeeded) delete_courier_pod "${pod}"; return 0 ;;
      Failed) break ;;
    esac
    [ "${SECONDS}" -lt "${deadline}" ] || break
    sleep 2
  done
  "${K[@]}" -n courier logs "${pod}" --tail=5 >&2 || true
  delete_courier_pod "${pod}"
  die "AX agents did not answer authenticated card requests through the router"
}

# settle_ax_tier: use the full tier's bounded kind-network recovery when ingress is not reachable.
settle_ax_tier() {
  local attempt
  for attempt in 1 2 3; do
    if (probe_ax_agents); then return 0; fi
    [ "${attempt}" -lt 3 ] || break
    echo "AX ingress did not settle (try ${attempt}); restarting kindnet" >&2
    "${K[@]}" -n kube-system rollout restart ds/kindnet >/dev/null
    "${K[@]}" -n kube-system rollout status ds/kindnet --timeout=120s >/dev/null
  done
  die "AX ingress never settled after kind network recovery"
}

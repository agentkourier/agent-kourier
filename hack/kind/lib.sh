# shellcheck shell=bash
# Shared by up-full.sh, up-fast.sh, e2e-run.sh, fast/verify.sh and down.sh (sourced, not executed).

# The kind node image and the base of the images the tiers build (mockllm; the fast tier's test
# doubles, through a build argument). Used by the scripts that source this file.
# shellcheck disable=SC2034
NODE_IMAGE=kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
# The oldest Kubernetes the chart admits (crMode's check in charts/agent-kourier/templates/_helpers.tpl), which only
# the CRD corpus runs on (CRD_CORPUS_NODE=min, hack/kind/crd-corpus.sh). The digest is from the notes of kind v0.32.0,
# the newest release that publishes a 1.33 node image; renovate.json holds it on 1.33. Keep its minor equal to that
# check's, which test/kindmanifests guards.
# shellcheck disable=SC2034
NODE_IMAGE_MIN=kindest/node:v1.33.12@sha256:3f5c8443c620245e4d355cfe09e96a91ead32ceaa569d3f1ca9edf0cb2fe2ff4
RUNTIME_IMAGE=gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
# The Go image Agent Kourier is built in (the Dockerfile's builder stage); go.mod's go version is 1.27.4.
GO_IMAGE=golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666
# One full tier per Docker host: the cluster name is fixed, so two checkouts share it. The fast
# tier is a separate cluster; use_tier fast points CLUSTER and OWNER_LABEL at it.
CLUSTER=agent-kourier-full
# Set on the kind node by full/kind-config.yaml (fast/kind-config.yaml for the fast tier);
# proves this tooling created the cluster.
OWNER_LABEL=agentkourier.agent-kourier.dev/owner=up-full
# Set by up-full.sh after creation; records which checkout owns the cluster.
CHECKOUT_KEY=agentkourier.agent-kourier.dev/checkout
# The pods of a tier's own Agent Kourier, release "agent-kourier": the two labels its Deployment selects them by. The name label alone will not
# do: every release of the chart in the namespace carries it (the sandbox's and any other), since the front door's NetworkPolicy trusts it.
# test/e2e's agentKourierPods is the same selector.
TIER_AGENTKOURIER_PODS=app.kubernetes.io/name=agent-kourier,app.kubernetes.io/instance=agent-kourier
# E2E_SLACK_MODE=http (the fast and full tiers): the fake Slack sends its default app's events and presses as signed requests to this URL, the
# chat Service of the tier's release (hack/kind/http-values.yaml), which selects the leader, at the route of the tier's ChatConnection
# agent-kourier/slack-fast; and signs them with this secret, which Agent Kourier reads from the Secret slack-fast. A fake constant.
FAKE_SLACK_REQUEST_URL=http://agent-kourier-chat.agent-kourier.svc.cluster.local:8081/chat/slack/agent-kourier/slack-fast
FAKE_SLACK_SIGNING_SECRET=fake-slack-signing-secret

die() {
  echo "error: $*" >&2
  exit 1
}

# use_tier full|fast: make the functions below act on that tier's cluster.
use_tier() {
  case "$1" in
    full | fast | ax)
      CLUSTER=agent-kourier-$1
      [ "$1" != ax ] || CLUSTER=agent-kourier-ax-launcher
      OWNER_LABEL=agentkourier.agent-kourier.dev/owner=up-$1
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
# kubectl and helm call, is the local kind cluster of that tier: kind-agent-kourier-fast or
# kind-agent-kourier-full, checked literally against the kubeconfig. Its API server must be on loopback
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
  [ "${CTX}" = "kind-${CLUSTER}" ] && [ "${name}" = "kind-${CLUSTER}" ] ||
    die "context '${name:-?}' is not kind-${CLUSTER}; refusing to run kubectl or helm"
  case "${server}" in
    https://127.0.0.1:* | https://localhost:*) ;;
    *) die "context ${CTX} points at a non-local API server; refusing to run kubectl or helm" ;;
  esac
  kind_server=$(kubectl config view --kubeconfig <(kind get kubeconfig --name "${CLUSTER}" 2>/dev/null) \
    -o 'jsonpath={.clusters[0].cluster.server}' 2>/dev/null) || kind_server=
  [ -n "${kind_server}" ] && [ "${server}" = "${kind_server}" ] ||
    die "context ${CTX} points at ${server}, not at kind's ${CLUSTER} (${kind_server:-no such cluster}); refusing to run kubectl or helm"
}

# assert_owned: die unless the existing agent-kourier-full cluster was created by this tooling.
# "Cannot ask" (API server down) is reported differently from "not ours".
assert_owned() {
  node_kubectl get nodes -o name >/dev/null 2>&1 ||
    die "${CLUSTER} exists but its API server is unreachable; if it is yours, run \`kind delete cluster --name ${CLUSTER}\`"
  [ -n "$(node_kubectl get nodes -l "${OWNER_LABEL}" -o name 2>/dev/null)" ] ||
    die "kind cluster ${CLUSTER} exists but was not created by this script; refusing to touch it"
}

# refuse_while_sandbox_up: die while the sandbox tier's Agent Kourier (hack/kind/sandbox) is in the cluster. It takes the place of the tier's, which
# sandbox/up.sh has scaled to zero, so a run of the E2E tests would find no Agent Kourier and a bring-up would start the tier's beside it. It looks for the
# sandbox's Deployment by name, not for a label: another release of the chart in the namespace is no reason to refuse. An API error is not a
# refusal: only a Deployment that is there is. Needs K.
refuse_while_sandbox_up() {
  local found
  found=$("${K[@]}" -n agent-kourier get deploy agent-kourier-sandbox --ignore-not-found -o name 2>/dev/null) || found=
  [ -z "${found}" ] || die "the sandbox tier is up (${found}), with the tier's Agent Kourier at zero replicas. Run: make sandbox-down first"
}

# wait_tier_agentkourier_gone: wait for the tier's own Agent Kourier, which the caller has scaled to zero, to have no pod left: the chart's, and the
# hand-made one of a tier brought up before the chart, which has no instance label at all. A selector has no "or", so each is a wait of its
# own, and a wait that matches no pod returns at once. Another release of the chart in the namespace is not waited for: it does not exit. Needs K.
wait_tier_agentkourier_gone() {
  local selector
  for selector in "${TIER_AGENTKOURIER_PODS}" 'app.kubernetes.io/name=agent-kourier,!app.kubernetes.io/instance'; do
    "${K[@]}" -n agent-kourier wait --for=delete pod -l "${selector}" --timeout=90s >/dev/null
  done
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
DOUBLES_IMAGE=agent-kourier-e2e/fast-doubles:dev

# install_operator: prometheus-operator, and nothing else of kube-prometheus-stack, in monitoring.
install_operator() {
  "${H[@]}" upgrade --install prometheus-operator "${OPERATOR_OCI}" --version "${OPERATOR_CHART_VERSION}" \
    --namespace monitoring --create-namespace -f "${HERE}/fast/operator-values.yaml" --wait --timeout 4m >/dev/null
}

# build_doubles_image: cross-compile the doubles (the signaler too, which only the fast tier on two replicas runs) on the host for the Docker daemon's architecture,
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
  for double in fakeslack faketeams stubagent sink signaler hitlagent fakevendors; do
    GOOS=linux GOARCH=${goarch} CGO_ENABLED=0 go build -C "${ROOT}" -o "${BUILD}/${double}" "./test/${double}/cmd/${double}"
  done
  docker build -q -f "${HERE}/fast/Dockerfile" --build-arg "RUNTIME_IMAGE=${RUNTIME_IMAGE}" -t "${DOUBLES_IMAGE}" "${BUILD}" >/dev/null
  kind load docker-image "${DOUBLES_IMAGE}" --name "${CLUSTER}"
}

# apply_doubles: the fake Slack, stub agent, sink and fake vendors in namespace e2e, and Alertmanager (with its
# route to the sink) in monitoring; needs install_operator first. None is published: the fake
# Slack's /_control/ has no authentication, nor have the fake vendors'. E2E_SLACK_MODE says how the fake Slack sends its default app's events and presses (fake_slack_mode_patch).
apply_doubles() {
  require_slack_mode
  "${K[@]}" apply -f "${HERE}/fast/doubles.yaml" >/dev/null
  "${K[@]}" -n e2e patch deploy/fakeslack --type=strategic -p "$(fake_slack_mode_patch)" >/dev/null
  # Restart so a rebuilt image (same tag) is picked up.
  "${K[@]}" -n e2e rollout restart deploy/fakeslack deploy/stubagent deploy/hitlagent deploy/sink deploy/fakevendors >/dev/null
  "${K[@]}" -n e2e rollout status deploy/fakeslack deploy/stubagent deploy/hitlagent deploy/sink deploy/fakevendors --timeout=180s
  "${K[@]}" apply -f "${HERE}/fast/alertmanager.yaml" >/dev/null
  "${K[@]}" -n monitoring wait --for=condition=Available alertmanager/agent-kourier --timeout=180s
}

# The fast tier's prompt agent (examples/recipes/prompt-agent) and its doubles, which fast/prompt-agent/prompt-agent.yaml runs (test/kindmanifests
# pins that it runs these): the recipe's own image, from its Dockerfile, and the fake model and the test MCP server, from fast/prompt-agent/Dockerfile,
# on PROMPT_PYTHON_IMAGE, the python stage of the recipe's Dockerfile, which is built here and not loaded into the cluster. Its name is under
# localhost/, so a builder that cannot see the local image store fails to pull it rather than fetching a namesake from Docker Hub.
PROMPT_AGENT_IMAGE=agent-kourier-e2e/prompt-agent:dev
PROMPT_PYTHON_IMAGE=localhost/agent-kourier-e2e/prompt-python:dev
PROMPT_FAKE_MODEL_IMAGE=agent-kourier-e2e/prompt-fake-model:dev
PROMPT_MCP_IMAGE=agent-kourier-e2e/prompt-mcp:dev
# The token Agent Kourier sends the prompt agent and the agent checks. A fake constant, made into the Secret of each side here and never committed.
PROMPT_AGENT_BEARER=fake-prompt-agent-bearer

# deploy_prompt_agent: build the prompt agent's three images on the host, load them into the tier's cluster, make its Secrets (the bearer in e2e, which
# the agent mounts, and in agent-kourier, which the Binding of channel CPROMPT reads; the MCP server's token in e2e) and apply the agent, the fake model
# and the MCP server in namespace e2e; needs apply_doubles first. Restarted, so that a rebuilt image (same tag) or a changed ConfigMap is picked up.
# An agent that does not become ready, such as one that refuses its agent.yaml at start, is shown with its log.
deploy_prompt_agent() {
  local recipe=${ROOT}/examples/recipes/prompt-agent/agent
  docker build -q -t "${PROMPT_AGENT_IMAGE}" "${recipe}" >/dev/null
  docker build -q --target python -t "${PROMPT_PYTHON_IMAGE}" "${recipe}" >/dev/null
  docker build -q --target fake-model -f "${HERE}/fast/prompt-agent/Dockerfile" --build-arg "PYTHON_IMAGE=${PROMPT_PYTHON_IMAGE}" -t "${PROMPT_FAKE_MODEL_IMAGE}" "${recipe}" >/dev/null
  docker build -q --target mcp -f "${HERE}/fast/prompt-agent/Dockerfile" --build-arg "PYTHON_IMAGE=${PROMPT_PYTHON_IMAGE}" -t "${PROMPT_MCP_IMAGE}" "${recipe}" >/dev/null
  kind load docker-image "${PROMPT_AGENT_IMAGE}" "${PROMPT_FAKE_MODEL_IMAGE}" "${PROMPT_MCP_IMAGE}" --name "${CLUSTER}"
  "${K[@]}" create namespace agent-kourier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n e2e create secret generic prompt-agent-bearer --from-literal=token="${PROMPT_AGENT_BEARER}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n agent-kourier create secret generic prompt-agent-bearer --from-literal=token="${PROMPT_AGENT_BEARER}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n e2e create secret generic prompt-agent-mcp --from-literal=token=fake-prompt-mcp-token --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" apply -f "${HERE}/fast/prompt-agent/prompt-agent.yaml" >/dev/null
  "${K[@]}" -n e2e rollout restart deploy/prompt-agent deploy/prompt-fake-model deploy/prompt-mcp >/dev/null
  if ! "${K[@]}" -n e2e rollout status deploy/prompt-fake-model deploy/prompt-mcp deploy/prompt-agent --timeout=180s; then
    "${K[@]}" -n e2e logs deploy/prompt-agent --tail=40 >&2 || true
    die "the prompt agent, its fake model or its MCP server did not become ready"
  fi
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
  echo "== never published: the fake Slack's /_control/, the stub agent's /_control/release/ and the fake vendors' /_control/ have no authentication, nor has hitlagent"
  for app in fakeslack stubagent hitlagent fakevendors; do
    assert_app_unpublished e2e "${app}"
  done
  ingresses=$("${K[@]}" -n e2e get ingress -o name)
  assert "no Ingress in namespace e2e: [${ingresses}]" is_empty "${ingresses}"
  ports=$(docker port "${NODE}" 2>/dev/null) || ports=
  assert "the kind node publishes the API server, 6443/tcp on 127.0.0.1: [${ports}]" grep -q -E "${apiserver}" <<<"${ports}"
  hostports=$(grep -v -E "${apiserver}" <<<"${ports}" || true) # grep exits 1 when nothing else is published
  assert "the kind node publishes nothing but the API server: [${hostports}]" is_empty "${hostports}"
}

# The Agent Kourier images, built from the repo's Dockerfile. AGENTKOURIER_IMAGE is the production build, which no
# kind tier runs. TESTHOOKS_IMAGE is built with the testhooks tag (the crash hook and the Slack API URL
# override of internal/testhooks): the tiers run it, because only it can be pointed at the fake Slack
# and made to crash. It is never pushed, it lives under the local agent-kourier-e2e/ prefix, and it carries
# TESTHOOKS_LABEL, which the load into a tier checks.
AGENTKOURIER_IMAGE=agent-kourier:dev
TESTHOOKS_IMAGE=agent-kourier-e2e/agent-kourier-testhooks:dev
TESTHOOKS_LABEL=dev.agent-kourier.testhooks

# build_agentkourier_image [testhooks]: build AGENTKOURIER_IMAGE, or with "testhooks" TESTHOOKS_IMAGE, for the
# Docker daemon's own architecture. Needs ROOT, the repository root.
build_agentkourier_image() {
  local image=${AGENTKOURIER_IMAGE} tags=() label=()
  if [ "${1:-}" = testhooks ]; then
    image=${TESTHOOKS_IMAGE} tags=(--build-arg TAGS=testhooks) label=(--label "${TESTHOOKS_LABEL}=true")
  fi
  docker build -q -f "${ROOT}/Dockerfile" --build-arg "GO_IMAGE=${GO_IMAGE}" --build-arg "RUNTIME_IMAGE=${RUNTIME_IMAGE}" \
    ${tags[@]+"${tags[@]}"} ${label[@]+"${label[@]}"} -t "${image}" "${ROOT}" >/dev/null # (an empty array is unset to bash 3.2)
}

# assert_testhooks_image IMAGE: die unless IMAGE carries TESTHOOKS_LABEL, so a production image is never
# loaded into a tier by mistake. Run it before every kind load of an Agent Kourier image.
assert_testhooks_image() {
  [ "$(docker image inspect --format "{{index .Config.Labels \"${TESTHOOKS_LABEL}\"}}" "$1" 2>/dev/null)" = true ] ||
    die "$1 is not a testhooks image (no ${TESTHOOKS_LABEL}=true label); refusing to load it into a tier"
}

# load_agentkourier_image: build the testhooks image, check its label, and load it into the tier's cluster. The tiers
# run the testhooks build, which only can be pointed at the fake Slack. Nothing here pushes either image.
load_agentkourier_image() {
  build_agentkourier_image testhooks
  assert_testhooks_image "${TESTHOOKS_IMAGE}"
  kind load docker-image "${TESTHOOKS_IMAGE}" --name "${CLUSTER}"
}

# Agent Kourier is installed in every tier by `helm upgrade --install` of charts/agent-kourier, with the values file of the tier (charts/agent-kourier/ci:
# fast-values.yaml for the fast and full tiers, ax-values.yaml for the AX tier, sandbox-values.yaml for the sandbox). What each tier makes
# itself, and the chart does not, is the config (a ConfigMap built from the tier's files, named by config.existingConfigMap) and the
# Secrets (fake constants made here, or the operator's in the sandbox); the values hold neither. The functions below expect the sourcing
# script to have set ROOT, K and H (kubectl and helm, each naming the tier's context).

# adopt_into_release RELEASE KIND/NAME...: let Helm take over the hand-made objects of namespace agent-kourier that the chart now makes (the
# annotations and label Helm checks before it will manage an object it did not create). Objects that are not there, or that some release
# already owns, are left alone: Helm refuses the second, saying which.
adopt_into_release() {
  local release=$1 obj owner
  shift
  for obj in "$@"; do
    [ -n "$("${K[@]}" -n agent-kourier get "${obj}" --ignore-not-found -o name)" ] || continue
    owner=$("${K[@]}" -n agent-kourier get "${obj}" -o 'jsonpath={.metadata.annotations.meta\.helm\.sh/release-name}')
    [ -z "${owner}" ] || continue
    "${K[@]}" -n agent-kourier annotate --overwrite "${obj}" "meta.helm.sh/release-name=${release}" meta.helm.sh/release-namespace=agent-kourier >/dev/null
    "${K[@]}" -n agent-kourier label --overwrite "${obj}" app.kubernetes.io/managed-by=Helm >/dev/null
  done
}

# replace_unowned_deployment NAME: delete the Deployment NAME of namespace agent-kourier when no Helm release owns it, which is how a tier brought
# up before the chart has it (hand-made manifests). The chart's Deployment selects pods by app.kubernetes.io/instance as well as by name, and
# a Deployment's selector cannot change, so it cannot be upgraded in place: it is deleted, and its pods with it (foreground, so that the old
# pod has released the SQLite volume before the new one starts), and the volume stays. One a release owns, or none, is left as it is.
replace_unowned_deployment() {
  local name=$1 owner
  [ -n "$("${K[@]}" -n agent-kourier get "deploy/${name}" --ignore-not-found -o name)" ] || return 0
  owner=$("${K[@]}" -n agent-kourier get "deploy/${name}" -o 'jsonpath={.metadata.annotations.meta\.helm\.sh/release-name}')
  [ -z "${owner}" ] || return 0
  echo "replacing the hand-made Deployment ${name} with the chart's (a Deployment's selector cannot change); its volume stays"
  "${K[@]}" -n agent-kourier delete "deploy/${name}" --cascade=foreground --wait --timeout=120s >/dev/null
}

# require_helm4: die unless helm is Helm 4. helm_install_agentkourier needs --force-conflicts, which Helm 3 lacks, and it would be found out only
# after replace_unowned_deployment had deleted a Deployment. Call it in each script's tool preflight, before anything is changed.
require_helm4() {
  local version
  version=$(helm version --template '{{.Version}}' 2>/dev/null) || version=
  case "${version}" in
    v4.*) ;;
    *) die "Helm 4 is required (the chart is installed with --force-conflicts); found '${version:-no helm}'" ;;
  esac
}

# helm_install_agentkourier RELEASE VALUES [HELM ARG...]: install or upgrade the chart as RELEASE in namespace agent-kourier with the values file VALUES
# (and any more arguments, such as --set replicaCount=0). It does not wait: callers wait for the rollout, and show Agent Kourier's log when it
# does not finish. Output is dropped; an error is not. Helm applies server-side, which refuses to change a field that another manager set
# (an adopted Service's selector, kubectl scale's replicas): the release is the owner of these objects, so it wins.
helm_install_agentkourier() {
  local release=$1 values=$2
  shift 2
  "${H[@]}" upgrade --install "${release}" "${ROOT}/charts/agent-kourier" --namespace agent-kourier -f "${values}" --force-conflicts "$@" >/dev/null
}

# install_agentkourier VALUES [HELM ARG...]: the tier's Agent Kourier, release "agent-kourier": a Deployment, ServiceAccount, Role, PersistentVolumeClaim and
# Service from the chart (the config, which the caller has made as the ConfigMap agent-kourier-config, is not the chart's). Further arguments go to
# helm, such as -f OVERLAY. On a cluster brought up before the chart, its hand-made Deployment is replaced and its other objects adopted, so
# that re-running a bring-up is how a tier moves over, with its volume. Then a rollout restart, so that a rebuilt image (same tag) or a changed
# ConfigMap is picked up: Helm rolls the pod for neither, since the values did not change.
install_agentkourier() {
  local values=$1
  shift
  replace_unowned_deployment agent-kourier
  adopt_into_release agent-kourier serviceaccount/agent-kourier role/agent-kourier-secrets rolebinding/agent-kourier-secrets service/agent-kourier pvc/agent-kourier-data
  helm_install_agentkourier agent-kourier "${values}" "$@"
  "${K[@]}" -n agent-kourier rollout restart deploy/agent-kourier >/dev/null
}

# require_config_source: die unless E2E_CONFIG_SOURCE is files (the tiers' default, the ConfigMap route: their values name it, since the chart's own default is crds) or crds (custom resources). Call it in a script's
# preflight, before anything is changed.
require_config_source() {
  case "${E2E_CONFIG_SOURCE:-files}" in
    files | crds) ;;
    *) die "E2E_CONFIG_SOURCE must be files or crds" ;;
  esac
}

# require_slack_mode: die unless E2E_SLACK_MODE is socket (the default: the fake Slack and the tier's ChatConnection over Socket Mode) or http (signed
# requests to the broker's chat listener). Call it in a script's preflight, before anything is changed.
require_slack_mode() {
  case "${E2E_SLACK_MODE:-socket}" in
    socket | http) ;;
    *) die "E2E_SLACK_MODE must be socket or http" ;;
  esac
}

# fake_slack_mode_patch: the strategic merge patch of the Deployment fakeslack for E2E_SLACK_MODE. In http mode the container's args (which follow
# its command, in fast/doubles.yaml) give the default app FAKE_SLACK_REQUEST_URL and FAKE_SLACK_SIGNING_SECRET, and the fake retries on its default
# schedule; in socket mode the patch takes the args away. doubles.yaml sets no args, so kubectl apply never touches them and the patch alone decides
# them, in either direction. The team apps (-app) stay on Socket Mode: the MC_ tests, which use them, run in socket mode only.
fake_slack_mode_patch() {
  local args=null
  if [ "${E2E_SLACK_MODE:-socket}" = http ]; then
    args="[\"-request-url\",\"${FAKE_SLACK_REQUEST_URL}\",\"-signing-secret\",\"${FAKE_SLACK_SIGNING_SECRET}\"]"
  fi
  printf '{"spec":{"template":{"spec":{"containers":[{"name":"fakeslack","args":%s}]}}}}' "${args}"
}

# slack_mode_config DIR: in http mode, put the tier's ChatConnection (DIR/chatconnection.yaml, a copy of fast/agent-kourier/config's) in HTTP mode;
# in socket mode leave it as it is, which is Socket Mode. The same file is the ConfigMap's and the custom resource's, so both routes read the mode.
slack_mode_config() {
  [ "${E2E_SLACK_MODE:-socket}" = http ] || return 0
  # The line goes at the end of the file, so the last top-level key must be spec.
  awk '/^[^ #]/ { last = $0 } END { exit last != "spec:" }' "$1/chatconnection.yaml" ||
    die "the ChatConnection's last top-level key is not spec:, so slack.mode cannot be appended to it"
  printf '  slack: {mode: http}\n' >>"$1/chatconnection.yaml"
}

# crds_held_by_release: succeed when the CRDs in the cluster were installed by the release agent-kourier (crds.install), so that a tier that goes back to
# the ConfigMap route keeps crds.install on, as the chart README says to: a release without it no longer holds them. Needs K.
crds_held_by_release() {
  [ "$("${K[@]}" get crd bindings.agentkourier.dev -o 'jsonpath={.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || true)" = agent-kourier ]
}

# deploy_agentkourier AGENTBACKEND [EXTRA_CONFIG_DIR...]: Agent Kourier's namespace, fake Slack Secret, config and chart, which the fast and full tiers share.
# AGENTKOURIER_VALUES is the values file (the fast tier's by default) and the array AGENTKOURIER_HELM_ARGS, if the caller set it, holds more helm
# arguments. The config is the fast tier's Bindings and ChatConnection (fast/agent-kourier/config), with AGENTBACKEND, a file, in place of the fast
# tier's AgentBackend: the Bindings name it `agents` in both tiers. Each EXTRA_CONFIG_DIR adds the YAML files of a directory: the fast tier
# passes the Bindings that reach the stub agent through the a2a dialect and those that verification checks against the stub, which the full
# tier, with no stub, must not load.
# E2E_CONFIG_SOURCE says where Agent Kourier reads that config from. files (the default of the tiers, whatever the chart's default is: the tiers' values set config.source files) puts it in the ConfigMap agent-kourier-config, which the chart
# mounts. crds follows the move a user makes, in two upgrades: the CRDs first (the release as it was, with crds.install on), then the same files applied
# as custom resources and the release upgraded with crds-values.yaml, so that the pod reads them and the ConfigMap is not mounted. The ConfigMap is still
# made (a ConfigMap of a user's own is not touched by the move either), so that the tier can go back: running this again with files upgrades the release
# to the ConfigMap route, and the custom resources stay in the cluster, unread, as the chart README says. Either way the config is the same files.
# The array AGENTKOURIER_CRDS_ARGS, if the caller set it, holds more helm arguments for the route upgrade of crds only (the platform's admission list of
# the tier with two Slack apps: a value the ConfigMap route refuses). The Secrets are fake constants made here and never committed, and the agent
# token (Secret agent-kourier-agent-token) is the caller's to make: it differs by tier.
# E2E_SLACK_MODE=http puts the ChatConnection in HTTP mode (slack_mode_config, on either route) and adds hack/kind/http-values.yaml, the chat listener
# and its Service, to every helm call; socket, the default, leaves both out, so that running this again in socket mode moves the tier back. The Secret
# slack-fast holds the keys of both modes, and the broker reads those of its mode. Needs HERE, ROOT, K, H.
deploy_agentkourier() {
  local config dir source=${E2E_CONFIG_SOURCE:-files} values=${AGENTKOURIER_VALUES:-${ROOT}/charts/agent-kourier/ci/fast-values.yaml}
  local mode_args=()
  require_config_source
  require_slack_mode
  [ "${E2E_SLACK_MODE:-socket}" = socket ] || mode_args=(-f "${HERE}/http-values.yaml")
  refuse_while_sandbox_up
  config=$(mktemp -d)
  trap 'rm -rf "${config}"; trap - RETURN' RETURN # the scratch directory goes however this ends
  cp "${HERE}"/fast/agent-kourier/config/*.yaml "${config}"
  cp "$1" "${config}/agentbackend.yaml"
  shift
  for dir in "$@"; do
    cp "${dir}"/*.yaml "${config}"
  done
  slack_mode_config "${config}"
  "${K[@]}" create namespace agent-kourier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n agent-kourier create secret generic slack-fast --from-literal=botToken=xoxb-fake-test-token \
    --from-literal=appToken=xapp-fake-test-token --from-literal=signingSecret="${FAKE_SLACK_SIGNING_SECRET}" \
    --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n agent-kourier create configmap agent-kourier-config --from-file="${config}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  case "${source}" in
    files)
      # A tier that was on crds holds the CRDs under the release: keep them there. (An empty array is unset to bash 3.2.)
      if crds_held_by_release; then
        install_agentkourier "${values}" ${AGENTKOURIER_HELM_ARGS[@]+"${AGENTKOURIER_HELM_ARGS[@]}"} ${mode_args[@]+"${mode_args[@]}"} --set crds.install=true
      else
        install_agentkourier "${values}" ${AGENTKOURIER_HELM_ARGS[@]+"${AGENTKOURIER_HELM_ARGS[@]}"} ${mode_args[@]+"${mode_args[@]}"}
      fi
      ;;
    crds)
      # 1. The CRDs, the route still files: the pod's template does not move, so there is no restart. Skipped when the release already holds them,
      # which is a tier that is already on crds being brought up again: it would put the pod on the ConfigMap route for the length of the step.
      if ! crds_held_by_release; then
        replace_unowned_deployment agent-kourier
        adopt_into_release agent-kourier serviceaccount/agent-kourier role/agent-kourier-secrets rolebinding/agent-kourier-secrets service/agent-kourier pvc/agent-kourier-data
        helm_install_agentkourier agent-kourier "${values}" ${AGENTKOURIER_HELM_ARGS[@]+"${AGENTKOURIER_HELM_ARGS[@]}"} ${mode_args[@]+"${mode_args[@]}"} --set crds.install=true
      fi
      "${K[@]}" wait --for=condition=Established --timeout=60s crd/bindings.agentkourier.dev crd/agentbackends.agentkourier.dev crd/chatconnections.agentkourier.dev >/dev/null
      # 2. The config as custom resources (the files are manifests of the three kinds already), then the route. A server-side dry run first, so that a
      # file the CRDs refuse stops here with the apiserver's words.
      "${K[@]}" apply --dry-run=server -f "${config}" >/dev/null
      "${K[@]}" apply -f "${config}" >/dev/null
      install_agentkourier "${values}" ${AGENTKOURIER_HELM_ARGS[@]+"${AGENTKOURIER_HELM_ARGS[@]}"} ${mode_args[@]+"${mode_args[@]}"} -f "${HERE}/crds-values.yaml" ${AGENTKOURIER_CRDS_ARGS[@]+"${AGENTKOURIER_CRDS_ARGS[@]}"}
      ;;
  esac
}

# unsigned_jwt EMAIL: an unsigned JWT with an email claim, which is all the stub agent (-require-auth) asks of a bearer. A fake constant.
unsigned_jwt() {
  local header claims
  header=$(printf %s '{"alg":"none"}' | base64 | tr -d '=\n' | tr '/+' '_-')
  claims=$(printf %s "{\"email\":\"$1\"}" | base64 | tr -d '=\n' | tr '/+' '_-')
  printf '%s.%s.' "${header}" "${claims}"
}

# fake_slack_app_connections APP_ID: how many Socket Mode clients of that app of the fake Slack are connected (/_control/apps), or nothing when it
# could not be read. Asked from a throwaway pod in e2e, since the fake's control surface is published nowhere. Needs K, jq.
fake_slack_app_connections() {
  local pod=probe-apps-${RANDOM} out
  out=$("${K[@]}" -n e2e run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" --command -- \
    curl -sf -m 10 http://fakeslack.e2e.svc.cluster.local:8080/_control/apps 2>/dev/null) || out=
  jq -r --arg app "$1" '.[] | select(.app_id == $app) | .connections' <<<"${out}" 2>/dev/null || true
}

# seat_two_apps_bots: put the bots in the channels of the Bindings of hack/kind/fast/two-apps in the fake Slack, before the Bindings are made: the bot of the
# default app (UFAKEBOT) in the platform's, team-a's (ATEAMA) in its own, and both in CMCLOOP. A Binding's first probe then finds its channel joined; one probed
# before the bot is in it is probed again only at the next pass (every five minutes), which a case that waits for Ready would wait for. The fake keeps this in
# memory, so a restarted fake has to be told again, which the MC_ tests do for their channels. test/e2e's twoapps.go names the channels. Needs K.
seat_two_apps_bots() {
  local pod=seat-bots-${RANDOM} fs=http://fakeslack.e2e.svc.cluster.local:8080/_control
  # shellcheck disable=SC2016 # the script runs in the pod
  "${K[@]}" -n e2e run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" --command -- sh -c '
    set -e
    fs=$1
    post() { curl -sf -m 10 -X POST -H "Content-Type: application/json" -d "$2" "$fs/$1" >/dev/null; }
    post members "{\"channel\":\"CMCPLATFORM\",\"users\":[\"UFAKEBOT\"]}"
    post members "{\"channel\":\"CMCPLATFINAL\",\"users\":[\"UFAKEBOT\"]}"
    post members "{\"channel\":\"CMCLOOP\",\"users\":[\"UFAKEBOT\",\"UTEAMA\"]}"
    post join "{\"channel\":\"CMCTEAMA\",\"app\":\"ATEAMA\"}"
    post join "{\"channel\":\"CMCTEAMAFINAL\",\"app\":\"ATEAMA\"}"' sh "${fs}" >/dev/null || die "could not put the bots of the two apps in their channels in the fake Slack"
}

# deploy_two_apps: the tier's second Slack app (E2E_TWO_APPS=1; custom resources only): the namespaces team-a, team-b and team-c, team-a's Secrets (the
# Slack tokens of the app ATEAMA that the fake Slack hosts, and an agent token), and hack/kind/fast/two-apps/two-apps.yaml: team-a's Role,
# ChatConnection, AgentBackend and Binding, and the platform's Binding mc-platform. It returns once the fake Slack has a Socket Mode client of team-a's app,
# which is the broker serving the connection, and dies if none comes in three minutes (each look is a pod, so the bound is the clock's). The Secrets are made with apply from a pipe, as deploy_agentkourier
# makes the tier's. Needs HERE, K, jq, and the CRDs served (deploy_agentkourier with crds).
deploy_two_apps() {
  local ns deadline connected=
  for ns in team-a team-b team-c; do
    "${K[@]}" create namespace "${ns}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  done
  "${K[@]}" -n team-a create secret generic slack-team-a --from-literal=botToken=xoxb-fake-team-a-token \
    --from-literal=appToken=xapp-fake-team-a-token --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n team-a create secret generic team-a-agent-token --from-literal=token="$(unsigned_jwt team-a@example.test)" \
    --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  seat_two_apps_bots
  "${K[@]}" apply -f "${HERE}/fast/two-apps/two-apps.yaml" >/dev/null
  deadline=$((SECONDS + 180))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    connected=$(fake_slack_app_connections ATEAMA)
    [ "${connected:-0}" -ge 1 ] 2>/dev/null && break
    sleep 2
  done
  [ "${connected:-0}" -ge 1 ] 2>/dev/null || {
    "${K[@]}" -n agent-kourier logs deploy/agent-kourier --tail=40 >&2 || true
    die "the broker never connected team-a's Slack app to the fake Slack; is team-a in config.connectionNamespaces, and does team-a's Role name its Secret?"
  }
  echo "ok: team-a's Slack app is connected to the fake Slack"
}

# The fast tier's webhook connections (hack/kind/fast/webhook, custom resources only): FAKE_WEBHOOK_SECRETS is the fake webhook secret of each
# recipe's connection, vendor=secret, which deploy_webhook puts in the Secret <vendor>-webhook that the recipe's ChatConnection names and the fake
# vendors (fast/doubles.yaml, the Deployment fakevendors) sign with. WEBHOOK_AGENT_TOKEN is the Secret of the agent token each recipe's Binding is
# given here, an unsigned JWT of WEBHOOK_AGENT_EMAIL: the tier's stub agent asks for a bearer, as kagent in OIDC mode does. Fake constants, made into
# Secrets here and never committed; test/kindmanifests pins that they agree with the fake's command and the recipes.
FAKE_WEBHOOK_SECRETS=(github=fake-webhook-github-secret bitbucket=fake-webhook-bitbucket-secret jira=fake-webhook-jira-secret linear=fake-webhook-linear-secret)
WEBHOOK_NAMESPACE=acme-webhooks
WEBHOOK_AGENT_TOKEN=webhook-agent-token
WEBHOOK_AGENT_EMAIL=acme-webhooks@example.test

# webhook_binding: a recipe's Binding, as JSON on stdin, on stdout with WEBHOOK_AGENT_TOKEN as its identity's tokenSecretRef, and nothing else
# changed: the tier's stub agent asks for a token of its own, in place of none (most recipes) or the token of the agent a recipe ships (the
# GitHub recipe's github-review-agent-bearer, which the tier does not make). Needs jq.
webhook_binding() {
  jq -c --arg secret "${WEBHOOK_AGENT_TOKEN}" '.spec.identity.tokenSecretRef = {name: $secret}'
}

# deploy_webhook: the tier's webhook connections on custom resources: the namespace WEBHOOK_NAMESPACE, which hack/kind/webhook-values.yaml admits,
# the Secret of each connection (FAKE_WEBHOOK_SECRETS) and of the agent token, hack/kind/fast/webhook/webhook.yaml (the Role that grants them to the
# broker, and the AgentBackends of the stub agent's webhook agents), and each recipe of examples/recipes/webhook-<vendor>: its ChatConnection as it
# is, and its Binding through webhook_binding. It returns once the broker says the four connections are up (agentkourier_chat_connected, read from
# its metrics Service by a probe pod), and dies if they are not in three minutes. The Secrets are made with apply from a pipe, as
# deploy_agentkourier makes the tier's. Needs HERE, ROOT, K, CURL_IMAGE, jq, and the CRDs served (deploy_agentkourier with crds).
deploy_webhook() {
  local pod=probe-webhook-${RANDOM} metrics=http://agent-kourier.agent-kourier.svc.cluster.local:8080/metrics pair vendor recipe conns=()
  "${K[@]}" create namespace "${WEBHOOK_NAMESPACE}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  for pair in "${FAKE_WEBHOOK_SECRETS[@]}"; do
    vendor=${pair%%=*}
    "${K[@]}" -n "${WEBHOOK_NAMESPACE}" create secret generic "${vendor}-webhook" --from-literal=webhookSecret="${pair#*=}" \
      --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  done
  "${K[@]}" -n "${WEBHOOK_NAMESPACE}" create secret generic "${WEBHOOK_AGENT_TOKEN}" --from-literal=token="$(unsigned_jwt "${WEBHOOK_AGENT_EMAIL}")" \
    --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" apply --dry-run=server -f "${HERE}/fast/webhook/webhook.yaml" >/dev/null
  "${K[@]}" apply -f "${HERE}/fast/webhook/webhook.yaml" >/dev/null
  for pair in "${FAKE_WEBHOOK_SECRETS[@]}"; do
    vendor=${pair%%=*}
    recipe=${ROOT}/examples/recipes/webhook-${vendor}/kourier
    "${K[@]}" apply -f "${recipe}/chatconnection.yaml" >/dev/null
    "${K[@]}" create --dry-run=client -o json -f "${recipe}/binding.yaml" | webhook_binding | "${K[@]}" apply -f - >/dev/null
    conns+=("${WEBHOOK_NAMESPACE}/${vendor}")
  done
  # shellcheck disable=SC2016 # the script runs in the pod
  "${K[@]}" -n e2e run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" --command -- sh -c '
    metrics=$1
    shift
    end=$(( $(date +%s) + 180 ))
    while [ "$(date +%s)" -lt "${end}" ]; do
      m=$(curl -s -m 5 "${metrics}")
      up=1
      for c in "$@"; do
        printf %s "$m" | grep -q "^agentkourier_chat_connected{connection=\"$c\"} 1$" || up=
      done
      [ -z "${up}" ] || exit 0
      sleep 2
    done
    exit 1' sh "${metrics}" "${conns[@]}" >/dev/null || {
    "${K[@]}" -n agent-kourier logs deploy/agent-kourier --tail=40 >&2 || true
    die "the broker never said its webhook connections in ${WEBHOOK_NAMESPACE} are up; is ${WEBHOOK_NAMESPACE} in config.connectionNamespaces, with the chat listener on (hack/kind/webhook-values.yaml)?"
  }
  echo "ok: the webhook connections ${conns[*]} are up"
}

# deploy_fake_teams: the fake Microsoft Teams service (fast/teams/faketeams.yaml, in namespace e2e; E2E_TEAMS=1), before deploy_agentkourier, whose pod reads
# the fake's certificate from the ConfigMap faketeams-ca in agent-kourier (hack/kind/teams-values.yaml, AGENTKOURIER_TEAMS_CA). The certificate and its key are
# made here for the fake's Service name (faketeams cert), on each bring-up, in a scratch directory that goes with the function: the key goes to the cluster
# as the Secret faketeams-tls in e2e and nowhere else, and only the certificate to agent-kourier. A test key, for a fake that is published nowhere.
# Restarted, so that a rebuilt image (same tag) or a new certificate is picked up. Needs ROOT, HERE, K and the doubles image.
deploy_fake_teams() {
  local dir
  dir=$(mktemp -d)
  trap 'rm -rf "${dir}"; trap - RETURN' RETURN
  go run -C "${ROOT}" ./test/faketeams/cmd/faketeams cert -host faketeams.e2e.svc.cluster.local -dir "${dir}"
  "${K[@]}" -n e2e create secret tls faketeams-tls --cert="${dir}/tls.crt" --key="${dir}/tls.key" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" create namespace agent-kourier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n agent-kourier create configmap faketeams-ca --from-file=ca.crt="${dir}/tls.crt" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" apply -f "${HERE}/fast/teams/faketeams.yaml" >/dev/null
  "${K[@]}" -n e2e rollout restart deploy/faketeams >/dev/null
  "${K[@]}" -n e2e rollout status deploy/faketeams --timeout=180s
}

# deploy_teams_connections: the tier's Microsoft Teams connections (E2E_TEAMS=1; custom resources only): the namespace teams-e2e, its Secrets (the client
# secret that the fake Teams service takes, and an agent token), and fast/teams/teams.yaml: the Role that grants the broker those Secrets, the ChatConnections
# teams-push and teams-poll, the AgentBackend and the Bindings. Then the setup a team makes with a real bot: the bot's app added to the team (the fake's
# install, a conversationUpdate on the team's conversation) and a person's mention of the bot in each bound channel, which is what gives the broker the
# record of that channel (its team, Microsoft Entra group and service URL) that a subscription and a poll need. The approval channel's mention starts a turn
# that waits on its card. It returns once the broker answers each of those activities with 200, which is the broker serving both connections on its chat
# listener, and dies if it does not in three minutes. The TEAMS_ tests make the setup again once a run, since the fake keeps nothing over a restart.
# Needs HERE, K, jq, and the CRDs served (deploy_agentkourier with crds).
deploy_teams_connections() {
  local pod deadline ok="" fake=http://faketeams.e2e.svc.cluster.local:8080/_control
  "${K[@]}" create namespace teams-e2e --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n teams-e2e create secret generic teams-bot --from-literal=clientSecret=fake-teams-client-secret \
    --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" -n teams-e2e create secret generic teams-e2e-agent-token --from-literal=token="$(unsigned_jwt teams-e2e@example.test)" \
    --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  "${K[@]}" apply --dry-run=server -f "${HERE}/fast/teams/teams.yaml" >/dev/null
  "${K[@]}" apply -f "${HERE}/fast/teams/teams.yaml" >/dev/null
  deadline=$((SECONDS + 180))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    pod=install-teams-${RANDOM}
    # shellcheck disable=SC2016 # the script runs in the pod
    if "${K[@]}" -n e2e run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" --command -- sh -c '
      set -e
      fake=$1
      ok() { grep -q "\"status\":200"; }
      setup() {
        curl -sf -m 40 -X POST -H "Content-Type: application/json" -d "{\"app\":\"$1\",\"channel\":\"$2\"}" "$fake/install" | ok
        curl -sf -m 40 -X POST -H "Content-Type: application/json" \
          -d "{\"app\":\"$1\",\"channel\":\"$2\",\"user\":\"7e2e7ea5-0000-4000-8000-0000000000d0\",\"name\":\"Setup\",\"text\":\"setup\"}" "$fake/mention" | ok
      }
      push=7e2e7ea5-0000-4000-8000-0000000000a1 poll=7e2e7ea5-0000-4000-8000-0000000000a2
      for c in teams-mention teams-approval teams-alert-push teams-failover; do setup "$push" "19:$c@thread.tacv2"; done
      setup "$poll" "19:teams-alert-poll@thread.tacv2"' sh "${fake}" >/dev/null 2>&1; then
      ok=1
      break
    fi
    sleep 5
  done
  [ -n "${ok}" ] || {
    "${K[@]}" -n agent-kourier logs deploy/agent-kourier --tail=40 >&2 || true
    die "the broker never answered the fake Teams service's install and setup mentions with 200; is teams-e2e in config.connectionNamespaces, does its Role name its Secrets, and is the chat listener on?"
  }
  echo "ok: the broker serves the Teams connections teams-e2e/teams-push and teams-e2e/teams-poll"
}

# runner_admission DIR: the broker's admission (spec D54) for the E2E runner, as the Deployment agent-kourier has it: its AGENTKOURIER_CHAT_CONNECTION,
# AGENTKOURIER_CONNECTION_NAMESPACES and AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR (a LabelSelector as JSON), and the namespaces the MC_ tests ask about
# (agent-kourier, team-a, team-b and team-c, which must exist) with their labels, as kubectl get namespace -o json prints them. Each is written to a
# file of DIR and goes from there, as it is, into a key of the ConfigMap e2e-admission in namespace e2e, which the Job's variables read
# (hack/kind/e2e/runner.yaml): never through sed or the Job's YAML, since a selector holds quotes, braces and slashes. test/e2e's Env decodes the
# selector strictly, as the broker does, and fails a scenario on one it cannot read. A Deployment that is not there reads as nothing set. Needs K.
runner_admission() {
  local dir=$1 pair key var list
  mkdir -p "${dir}"
  for pair in chat-connection=AGENTKOURIER_CHAT_CONNECTION connection-namespaces=AGENTKOURIER_CONNECTION_NAMESPACES \
    connection-namespace-selector=AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR; do
    key=${pair%%=*} var=${pair#*=}
    "${K[@]}" -n agent-kourier get deploy/agent-kourier -o "jsonpath={.spec.template.spec.containers[0].env[?(@.name==\"${var}\")].value}" \
      --ignore-not-found >"${dir}/${key}" || die "could not read the Deployment agent-kourier to see its ${var}"
  done
  list=$(<"${dir}/connection-namespaces")
  [[ "${list}" =~ ^[a-z0-9,-]*$ ]] || die "the Deployment's AGENTKOURIER_CONNECTION_NAMESPACES is '${list}'; want namespace names separated by commas"
  "${K[@]}" get namespace agent-kourier team-a team-b team-c -o json >"${dir}/namespaces" || die "could not read the labels of the namespaces agent-kourier, team-a, team-b and team-c"
  "${K[@]}" -n e2e delete configmap e2e-admission --ignore-not-found >/dev/null
  "${K[@]}" -n e2e create configmap e2e-admission --from-file=chat-connection="${dir}/chat-connection" \
    --from-file=connection-namespaces="${dir}/connection-namespaces" --from-file=connection-namespace-selector="${dir}/connection-namespace-selector" \
    --from-file=namespaces="${dir}/namespaces" >/dev/null || die "could not make the ConfigMap e2e-admission that hands the admission to the runner"
  echo "own connection: $(<"${dir}/chat-connection")"
  echo "admitted namespaces: ${list:-none}"
  echo "namespace selector: $(<"${dir}/connection-namespace-selector")"
}

# HOST_PAUSE_SECONDS: the longest gap between two of wait_job's polls while the host keeps running it. A poll is a 2 second sleep and a
# kubectl call with a 10 second request timeout, so a gap past this means the host stopped running the wait: it slept (a laptop whose lid
# closed on battery) or froze, and the kind node's VM most likely stopped with it. The tests make no progress through such a pause, but the
# wait's clock, the host's wall clock, keeps going. In run 8 of a release round on 2026-11-27, when the Mac's lid closed for 571 seconds, the
# test binary's -test.timeout, which counts the node's own clock, did not fire either, and the run timed out with no stack.
HOST_PAUSE_SECONDS=30

# wait_job JOB SECONDS: poll the Job JOB in namespace e2e until it has succeeded or failed, or SECONDS of the host's clock have passed.
# Sets JOB_RESULT (0 succeeded, 1 failed, 124 timed out) and HOST_PAUSED, the seconds of gaps between polls that were longer than
# HOST_PAUSE_SECONDS (0 when there were none), and writes a line to stderr when such a gap ends. Each gap is one poll and its sleep,
# so a pause in the poll that ends the wait counts too. A kubectl that fails prints nothing, which reads as a Job still running. Needs K.
wait_job() {
  local job=$1 wait=$2 deadline last
  JOB_RESULT='' HOST_PAUSED=0
  deadline=$((SECONDS + wait)) last=${SECONDS}
  while [ -z "${JOB_RESULT}" ]; do
    # "<succeeded>/<failed>", each empty until it is 1.
    case "$("${K[@]}" -n e2e get job "${job}" --request-timeout=10s -o 'jsonpath={.status.succeeded}/{.status.failed}')" in
      1/*) JOB_RESULT=0 ;;
      */1) JOB_RESULT=1 ;;
      *) [ "${SECONDS}" -lt "${deadline}" ] || JOB_RESULT=124 ;;
    esac
    [ -n "${JOB_RESULT}" ] || sleep 2
    note_host_pause "$((SECONDS - last))"
    last=${SECONDS}
  done
}

# note_host_pause GAP: wait_job's count of a gap between two polls that is longer than HOST_PAUSE_SECONDS.
note_host_pause() {
  [ "$1" -gt "${HOST_PAUSE_SECONDS}" ] || return 0
  HOST_PAUSED=$((HOST_PAUSED + $1))
  echo "e2e: the wait did not run for $1s, to $(date '+%H:%M:%S'): the host slept or froze, and the kind node and the tests most likely stopped with it" >&2
}

# apply_signaler: the container signaler (fast/signaler.yaml, in namespace e2e), which S16 uses to freeze or kill an Agent Kourier replica; the fast
# tier on two replicas only. It shares the node's PID namespace and is published nowhere. Restarted so that a rebuilt image (same tag) is
# picked up. Needs K, HERE.
apply_signaler() {
  "${K[@]}" apply -f "${HERE}/fast/signaler.yaml" >/dev/null
  "${K[@]}" -n e2e rollout restart deploy/signaler >/dev/null
  "${K[@]}" -n e2e rollout status deploy/signaler --timeout=180s
}

# remove_signaler: take the signaler away, which a tier that runs one replica has no use for. Nothing if it is not there. Needs K, HERE.
remove_signaler() {
  "${K[@]}" delete -f "${HERE}/fast/signaler.yaml" --ignore-not-found >/dev/null
}

# deploy_postgres: the fast tier's Postgres (fast/postgres.yaml, in namespace e2e) and the two Secrets that carry its password: postgres-auth
# in e2e, which the server initialises with, and agent-kourier-pg in Agent Kourier, key dsn, which Agent Kourier reads (charts/agent-kourier/ci/fast-postgres-values.yaml
# names it). The password is generated here, for this cluster, and goes from the shell's memory to the API server through a pipe: it is
# never on an argument list (kubectl create secret --from-literal would put it there), in a file or in the output. The Secrets are made with
# `create`, not `apply`, so that no last-applied-configuration annotation holds a second copy. A re-run keeps them while both exist, since
# the running server was initialised with that password; when either is missing both are made again and the server is replaced, which starts
# it empty (its data is an emptyDir). Needs K, HERE.
deploy_postgres() {
  local password
  "${K[@]}" create namespace agent-kourier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  if [ -z "$("${K[@]}" -n e2e get secret postgres-auth --ignore-not-found -o name)" ] || [ -z "$("${K[@]}" -n agent-kourier get secret agent-kourier-pg --ignore-not-found -o name)" ]; then
    if command -v openssl >/dev/null 2>&1; then
      password=$(openssl rand -hex 24)
    else
      password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
    fi
    [ "${#password}" -eq 48 ] || die "could not generate a password for the fast tier's Postgres"
    "${K[@]}" -n e2e delete secret postgres-auth --ignore-not-found >/dev/null
    "${K[@]}" -n agent-kourier delete secret agent-kourier-pg --ignore-not-found >/dev/null
    # printf is a builtin: what it prints is not an argument of any process. The password is hex, so the URL needs no escaping.
    printf 'apiVersion: v1\nkind: Secret\nmetadata: {name: postgres-auth, namespace: e2e}\nstringData: {password: "%s"}\n' "${password}" |
      "${K[@]}" create -f - >/dev/null
    printf 'apiVersion: v1\nkind: Secret\nmetadata: {name: agent-kourier-pg, namespace: agent-kourier}\nstringData: {dsn: "postgres://agentkourier:%s@postgres.e2e.svc.cluster.local:5432/agentkourier?sslmode=disable"}\n' "${password}" |
      "${K[@]}" create -f - >/dev/null
    password=
    # A server that was initialised with another password is replaced: its data goes with it.
    "${K[@]}" -n e2e delete pod -l app=postgres --ignore-not-found --wait=true >/dev/null
  fi
  "${K[@]}" apply -f "${HERE}/fast/postgres.yaml" >/dev/null
  "${K[@]}" -n e2e rollout status deploy/postgres --timeout=240s
}

# The curl image the scripts' probe pods run, pinned; the same as the verify scripts'.
CURL_IMAGE=curlimages/curl:8.23.0@sha256:3fb7e13e5d179a845c97d78204f3f1064d1907037f26a3ebf91710571151fd93

# delete_agentkourier_pod NAME: delete a throwaway pod of namespace agent-kourier, if there is one. Needs K.
# An empty NAME is nothing to delete.
delete_agentkourier_pod() { [ -z "$1" ] || "${K[@]}" -n agent-kourier delete pod "$1" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }

# DEX_TOKEN_POD is the name of the pod refresh_dex_token is running, for a caller's EXIT trap: a Ctrl-C during one of its
# kubectl calls skips the function's own RETURN trap, and the pod's status holds the token. Empty when there is none.
DEX_TOKEN_POD=

# DEX_TOKEN_TRIES, DEX_TOKEN_POLLS and the sleeps between them are what refresh_dex_token waits for: up to DEX_TOKEN_TRIES pods, each given
# DEX_TOKEN_POLLS looks, DEX_TOKEN_POLL_SLEEP seconds apart (two minutes: the first pod pulls the curl image), with DEX_TOKEN_RETRY_SLEEP
# seconds between pods. The tests of test/kindmanifests set the sleeps to 0.
: "${DEX_TOKEN_TRIES:=3}" "${DEX_TOKEN_POLLS:=120}" "${DEX_TOKEN_POLL_SLEEP:=1}" "${DEX_TOKEN_RETRY_SLEEP:=3}"

# DEX_TOKEN_SCRIPT is what the pod of refresh_dex_token runs: it asks Dex for the Binding identity's ID token with the password grant (the client
# and the user are the fake ones of full/dex.yaml) and writes it, as the one line TOKEN:<jwt>:END, to the pod's termination message, the file the
# kubelet reads into the pod's status when the container ends. It exits 1 when Dex gave no token. Nothing is written to its output.
# shellcheck disable=SC2016 # it runs in the pod, where its variables are
DEX_TOKEN_SCRIPT='
tok=$(curl -sf -m 20 -d grant_type=password -d client_id=agent-kourier-binding -d client_secret=fake-agent-kourier-binding-secret \
  -d username=binding-test@example.test -d password=password -d "scope=openid email" \
  http://dex.dex.svc.cluster.local:5556/dex/token | sed -n "s/.*\"id_token\":\"\([^\"]*\)\".*/\1/p")
[ -n "${tok}" ] || exit 1
printf "TOKEN:%s:END\n" "${tok}" >/dev/termination-log'

# dex_token_from: the token in the termination message of refresh_dex_token's pod, on stdin. The pod writes it as one line,
# TOKEN:<jwt>:END, and the token is the first line that is exactly that. A message that is not wholly good (the kubelet keeps 4096 bytes of
# one, and cuts the rest) is no token, and nothing is printed for it, so the caller tries again and never stores a mangled one.
dex_token_from() {
  sed -n 's/^TOKEN:\([A-Za-z0-9_-]\{1,\}\.[A-Za-z0-9_-]\{1,\}\.[A-Za-z0-9_-]\{1,\}\):END$/\1/p' | head -n 1
}

# refresh_dex_token: mint the Binding identity's ID token from Dex and store it in the Secret
# agent-kourier-agent-token, key token, in namespace agent-kourier. Dex has no client_credentials grant (H-02), so the
# identity is a static user that signs in with the password grant, and its token lives 24 hours: run this before
# each use of the full tier whose last refresh is not recent (up-full.sh and e2e-run.sh do). The token is made
# inside the cluster by a throwaway pod, where Dex is, and goes from there to the Secret through pipes: it is
# never printed, put on a command line or written to a file. The pod runs to completion and the token is read from its status
# (DEX_TOKEN_SCRIPT writes it to the termination message), not streamed: `kubectl run --rm -i` attaches to the pod and, on a loaded machine,
# returned nothing, or a mangled copy of the logs, in about one try in ten. A try whose pod could not be made, did not finish or left no token
# is tried again with a new pod, up to DEX_TOKEN_TRIES; the failure names what each try saw. The pod is deleted when
# its try ends, whatever happened, since its status holds the token. The tracing of `set -x` is off inside the function, which
# holds the token in a variable. Agent Kourier reads the Secret on every request, so a refreshed token is sent from the next call.
# Needs K.
refresh_dex_token() {
  local pod tok attempt phase polls out exit_code msg why tried= xtrace=
  case $- in *x*) xtrace=1 ;; esac
  set +x
  "${K[@]}" create namespace agent-kourier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  for ((attempt = 1; attempt <= DEX_TOKEN_TRIES; attempt++)); do
    [ "${attempt}" -eq 1 ] || sleep "${DEX_TOKEN_RETRY_SLEEP}"
    pod=dex-token-${RANDOM}
    DEX_TOKEN_POD=${pod}
    tok=
    why=
    if ! "${K[@]}" -n agent-kourier run "${pod}" --restart=Never --image="${CURL_IMAGE}" --command -- sh -c "${DEX_TOKEN_SCRIPT}" >/dev/null 2>&1; then
      why="kubectl run could not make the pod"
    else
      phase=
      for ((polls = 0; polls < DEX_TOKEN_POLLS; polls++)); do
        phase=$("${K[@]}" -n agent-kourier get pod "${pod}" -o 'jsonpath={.status.phase}' 2>/dev/null) || phase=
        case "${phase}" in Succeeded | Failed) break ;; esac
        sleep "${DEX_TOKEN_POLL_SLEEP}"
      done
      case "${phase}" in
        Succeeded | Failed)
          # "<exit code> <termination message>": the space is the template's, so a pod with no container status leaves both empty.
          out=$("${K[@]}" -n agent-kourier get pod "${pod}" -o 'jsonpath={.status.containerStatuses[0].state.terminated.exitCode} {.status.containerStatuses[0].state.terminated.message}' 2>/dev/null) || out=" "
          exit_code=${out%% *}
          msg=${out#* }
          tok=$(dex_token_from <<<"${msg}")
          why="the pod ended ${phase} with exit code ${exit_code:-unknown} and ${#msg} bytes of termination message, none of them a token"
          ;;
        *) why="the pod was ${phase:-not found} after ${DEX_TOKEN_POLLS} looks" ;;
      esac
    fi
    delete_agentkourier_pod "${pod}"
    DEX_TOKEN_POD=
    [ -z "${tok}" ] || break
    tried+="; try ${attempt}, pod ${pod}: ${why}"
  done
  [ -n "${tok}" ] || die "Dex gave no ID token for the Binding identity after ${DEX_TOKEN_TRIES} tries${tried} (is Dex up in namespace dex? each try runs a pod that asks it for the token and reads the pod's termination message)"
  printf %s "${tok}" | "${K[@]}" -n agent-kourier create secret generic agent-kourier-agent-token --from-file=token=/dev/stdin \
    --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
  tok=
  [ -z "${xtrace}" ] || set -x
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
  "${K[@]}" -n kagent wait --for=condition=Accepted remotemcpserver/scenariomcp remotemcpserver/scenariomcp-approval remotemcpserver/scenariomcp-approval-multi --timeout=180s
  "${K[@]}" apply -f "${dir}/scenarios.yaml" >/dev/null
  "${K[@]}" -n kagent wait --for=condition=Ready agent/scenario-gated agent/scenario-memory agent/scenario-ask agent/spike-approve agent/scenario-tools agent/scenario-approval agent/scenario-approval-multi --timeout=300s
}

# probe_frontdoor: whether a pod labelled as agent-kourier reaches kagent's front door by its Service name, which is the
# one path the full tier exists to show. Five tries a few seconds apart, from a throwaway pod that is deleted after.
# Needs K.
probe_frontdoor() {
  local pod=probe-door-${RANDOM} rc=0
  # shellcheck disable=SC2016 # the script runs in the pod, where its variables are
  "${K[@]}" -n agent-kourier run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" \
    --labels=app.kubernetes.io/name=agent-kourier --command -- sh -c '
    for _ in 1 2 3 4 5; do
      [ "$(curl -s -m 6 -o /dev/null -w "%{http_code}" http://kagent-frontdoor.kagent.svc.cluster.local:4180/ping)" = 200 ] && exit 0
      sleep 2
    done
    exit 1' >/dev/null 2>&1 || rc=$?
  delete_agentkourier_pod "${pod}"
  return "${rc}"
}

# settle_full_tier: before the full tier is verified, make sure the cluster is in a state its checks can mean
# something in. On a loaded host the kagent controller can crash-loop on a DNS lookup that was cancelled, and
# kindnet's network policy engine can time connections out (it logs "netlink receive: no such file or directory"
# in bursts); a restart of either clears it. So: the controller is Ready (its pod is deleted, up to twice, if it is
# not), then an agent-kourier-labelled pod reaches the front door by its Service name (kindnet is restarted, up to twice, if
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
# `http2: client connection lost`), its pod cache freezes and NetworkPolicy stops applying to pods made after that. An agent-kourier-labelled
# pod then reaches the kagent controller on 8083, which controller-admit-frontdoor-only must refuse, and every test that leans on the
# policy is wrong with nothing saying so (seen 2026-10-03; found 36 hours later). The fast tier has no kagent and no NetworkPolicy,
# so none of this applies there.
KINDNET_LOG_SINCE=10m
NETPOL_PROBE_URL=http://kagent-controller.kagent.svc.cluster.local:8083
# The control of the probe: a path the policy lets an agent-kourier-labelled pod take (the front door's), asked first. A pod that cannot
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
# substitutions, which cannot hand a pod name back to the caller's trap, and the pod carries the Agent Kourier label and no instance label (a leftover
# one is trusted by the front door, and holds up wait_tier_agentkourier_gone in the next make sandbox-up): so the traps delete by this label.
NETPOL_PROBE_LABEL=agentkourier.agent-kourier.dev/probe=netpol

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
  "${K[@]}" -n agent-kourier delete pod -l "${NETPOL_PROBE_LABEL}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

# policy_probe: from a throwaway pod labelled as agent-kourier, in namespace agent-kourier, "DOOR CONTROLLER CONNECT" (NETPOL_PROBE_SCRIPT): the
# front door's /ping, the control; then the kagent controller on 8083 for anything: curl exit 28 with no connection made (a drop, the
# policy working), 0 (an answer of any kind: the policy is not applied), or another (no verdict). Empty if the pod gave none; it is
# tried three times for that. About ten seconds when the policy works. Callers' EXIT traps run delete_netpol_probes. Needs K.
policy_probe() {
  local pod out="" attempt
  "${K[@]}" get namespace agent-kourier >/dev/null 2>&1 || "${K[@]}" create namespace agent-kourier >/dev/null 2>&1 || true
  for attempt in 1 2 3; do
    pod=probe-netpol-$$-${RANDOM}
    out=$("${K[@]}" -n agent-kourier run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" \
      --labels="app.kubernetes.io/name=agent-kourier,${NETPOL_PROBE_LABEL}" --command -- \
      sh -c "${NETPOL_PROBE_SCRIPT}" sh "${NETPOL_CONTROL_URL}" "${NETPOL_PROBE_URL}" 2>/dev/null | policy_probe_parse) || out=
    delete_agentkourier_pod "${pod}"
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
      echo "enforced: an agent-kourier-labelled pod reached the front door but got no answer from the kagent controller on 8083, so NetworkPolicy applies${sign:+ (note: ${sign})}"
      return 0
      ;;
    stale)
      echo "stale: an agent-kourier-labelled pod's connection to the kagent controller on 8083 was accepted (curl exit ${ctrl}), which controller-admit-frontdoor-only must refuse: kindnet is not applying NetworkPolicy${sign:+; ${sign}}"
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

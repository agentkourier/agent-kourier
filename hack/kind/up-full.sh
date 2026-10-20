#!/usr/bin/env bash
# Bring up the full E2E tier from nothing: kind, Agent Substrate (gVisor), kagent 1.0 alpha in
# OIDC mode behind Dex, mockllm as the model, two test agents, Agent Kourier's verifying front door,
# the NetworkPolicies that make that door the only way in, the fast tier's test doubles (fake
# Slack, sink, and Alertmanager under prometheus-operator; spec 12.1) so `make e2e-full` can run
# the same E2E tests, and Agent Kourier itself, which reaches kagent through that door. Their manifests and install steps are shared with up-fast.sh through lib.sh.
# Safe to re-run. Findings and workarounds: docs/spikes/wp01-kind-full.md.
# One full tier per Docker host: the cluster name is fixed, so parallel worktrees share it.
set -eEuo pipefail

# Pinned versions. Substrate must be the version kagent pins (its go.mod replace directive).
# The kind node image and the runtime image are pinned in lib.sh.
# Dex, the front door, and the probe image are pinned in full/dex.yaml, full/frontdoor.yaml, full/verify.sh.
KAGENT_VERSION=1.0.0-alpha5
MOCKLLM_REF=v0.1.0 # tag name, for humans; the commit below is what is built
MOCKLLM_COMMIT=35792a004f9387562a8760b2a513be8edeea6cf7
GO_IMAGE=golang:1.27.3@sha256:957861185dbf29d93668ace41e011d091c6e779953d47f0316ef2cc58e28fe2b
MOCKLLM_IMAGE=agent-kourier-e2e/mockllm:${MOCKLLM_REF}

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
FULL=${HERE}/full
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")
KAGENT_OCI=oci://ghcr.io/kagent-dev/kagent/helm

STEP=preflight
step() {
  STEP=$*
  echo "==> $*"
}
on_error() {
  echo "error: step '${STEP}' failed (line $1). Recover with: hack/kind/down.sh full && make e2e-full-up" >&2
}
trap 'on_error $LINENO' ERR
BUILD=
trap '[ -z "${BUILD}" ] || rm -rf "${BUILD}"; delete_agentkourier_pod "${DEX_TOKEN_POD:-}"' EXIT

for tool in docker kind kubectl helm curl openssl jq go; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"
require_helm4

# shellcheck source-path=SCRIPTDIR source=substrate.sh
. "${HERE}/substrate.sh"

step "kind cluster ${CLUSTER}"
if cluster_exists; then
  assert_owned
  owner=$(owner_checkout)
  if [ "${owner}" = "${ROOT}" ]; then
    echo "reusing the cluster this checkout created earlier"
  else
    echo "warning: reusing ${CLUSTER}, which was created from checkout ${owner}; only one full tier fits a Docker host" >&2
  fi
else
  kind create cluster --name "${CLUSTER}" --config "${FULL}/kind-config.yaml" --image "${NODE_IMAGE}"
  assert_context full
  "${K[@]}" annotate nodes -l "${OWNER_LABEL}" "${CHECKOUT_KEY}=${ROOT}" >/dev/null
fi
assert_context full
refuse_while_sandbox_up
# gVisor's loopback pod-to-pod path needs proxy ARP on the node (Substrate's create-kind-cluster.sh).
for node in $(kind get nodes --name "${CLUSTER}"); do
  docker exec "${node}" sysctl -w net.ipv4.conf.all.proxy_arp=1 >/dev/null
done

step "Agent Substrate ${SUBSTRATE_VERSION}"
install_substrate

step "namespaces, mockllm ${MOCKLLM_REF}, Dex"
for ns in kagent agent-kourier; do
  "${K[@]}" create namespace "${ns}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
done
# kagent-dev/mockllm publishes no image, so build the pinned commit and side-load it. The
# Dockerfile carries every pin, so a bump changes the build cache key and always rebuilds.
docker build -q -t "${MOCKLLM_IMAGE}" - <<EOF >/dev/null
FROM ${GO_IMAGE} AS build
RUN git init -q /src && cd /src \
 && git fetch -q --depth 1 https://github.com/kagent-dev/mockllm ${MOCKLLM_COMMIT} \
 && git checkout -q FETCH_HEAD \
 && test "\$(git rev-parse HEAD)" = ${MOCKLLM_COMMIT} \
 && CGO_ENABLED=0 go build -o /mockllm ./cmd/mockllm
FROM ${RUNTIME_IMAGE}
COPY --from=build /mockllm /mockllm
ENTRYPOINT ["/mockllm"]
EOF
kind load docker-image "${MOCKLLM_IMAGE}" --name "${CLUSTER}"
"${K[@]}" apply -f "${FULL}/mockllm.yaml" -f "${FULL}/dex.yaml" >/dev/null
# Restart so a changed ConfigMap or a rebuilt image (same tag) is picked up.
"${K[@]}" -n kagent rollout restart deploy/mockllm >/dev/null
"${K[@]}" -n kagent rollout status deploy/mockllm --timeout=180s
"${K[@]}" -n dex rollout status deploy/dex --timeout=180s
# oauth2-proxy credentials: fake client values matching dex.yaml, a fresh random cookie key.
"${K[@]}" -n kagent get secret kagent-oidc >/dev/null 2>&1 ||
  "${K[@]}" -n kagent create secret generic kagent-oidc --from-literal=client-id=kagent-ui \
    --from-literal=client-secret=fake-oauth2-proxy-secret --from-literal=cookie-secret="$(openssl rand -hex 16)"
# The front door's credentials: the agent-kourier-binding client from dex.yaml, its own cookie key.
"${K[@]}" -n kagent get secret kagent-frontdoor >/dev/null 2>&1 ||
  "${K[@]}" -n kagent create secret generic kagent-frontdoor --from-literal=client-secret=fake-agent-kourier-binding-secret \
    --from-literal=cookie-secret="$(openssl rand -hex 16)"

step "kagent ${KAGENT_VERSION}"
"${H[@]}" upgrade --install kagent-crds "${KAGENT_OCI}/kagent-crds" --namespace kagent --version "${KAGENT_VERSION}" \
  --set kmcp.enabled=false --wait --timeout 5m >/dev/null
"${H[@]}" upgrade --install kagent "${KAGENT_OCI}/kagent" --namespace kagent --version "${KAGENT_VERSION}" \
  -f "${FULL}/kagent-values.yaml" \
  --set-string "substrateWorkerPool.workerImage=ghcr.io/kagent-dev/substrate/ateom-gvisor:v${SUBSTRATE_VERSION}" \
  --wait --timeout 8m >/dev/null

step "test agents, front door, NetworkPolicy"
"${K[@]}" apply -f "${FULL}/agent.yaml" -f "${FULL}/slow-agent.yaml" -f "${FULL}/frontdoor.yaml" -f "${FULL}/networkpolicy.yaml" >/dev/null
# WP-01's policy had another name and apply does not prune; left in place it would still admit Agent Kourier.
"${K[@]}" -n kagent delete networkpolicy controller-admit-agent-kourier-only --ignore-not-found >/dev/null
# Restart, as for mockllm, so a changed slowllm ConfigMap or front-door alpha config is picked up
# (a mounted file is read at start).
"${K[@]}" -n kagent rollout restart deploy/slowllm deploy/kagent-frontdoor >/dev/null
"${K[@]}" -n kagent rollout status deploy/slowllm deploy/kagent-frontdoor --timeout=180s
"${K[@]}" -n kagent wait --for=condition=Ready agent/test-agent agent/slow-agent --timeout=300s

step "test doubles: prometheus-operator ${OPERATOR_CHART_VERSION}, fake Slack, sink, Alertmanager"
# In namespaces e2e and monitoring, outside every policy above, and never labelled as agent-kourier.
install_operator
build_doubles_image
apply_doubles

step "scenario agents: the scenario model, its tool server, one kagent agent per scenario"
deploy_scenario_agents

step "agent-kourier: image, the Binding's Dex token, config, chart"
# Agent Kourier is the chart (charts/agent-kourier, ci/fast-values.yaml, as the fast tier has it), running the testhooks image against the fake
# Slack in e2e, with the fast tier's Bindings and an AgentBackend for kagent's front door. Its pod carries the
# label that frontdoor-admit-agent-kourier-only trusts, which no other pod has. The Binding's token is an ID token from
# Dex, which expires after 24 hours (refresh_dex_token; e2e-run.sh refreshes it before each run). A tier brought up before the
# chart has a hand-made Deployment, which deploy_agentkourier replaces, keeping the volume.
load_agentkourier_image
refresh_dex_token
deploy_agentkourier "${FULL}/agent-kourier/agentbackend.yaml" "${FULL}/agent-kourier/config"

step "settle: the kagent controller is ready and the front door answers by its Service name"
settle_full_tier

step "verify"
"${FULL}/verify.sh"
docker stats --no-stream --format 'node cpu={{.CPUPerc}} mem={{.MemUsage}}' "$(kind get nodes --name "${CLUSTER}")"
echo "full tier up in ${SECONDS}s (context ${CTX})"

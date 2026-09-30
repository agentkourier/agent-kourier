#!/usr/bin/env bash
# Bring up the full E2E tier from nothing: kind, Agent Substrate (gVisor), kagent 1.0 alpha in
# OIDC mode behind Dex, mockllm as the model, two test agents, courier's verifying front door,
# and the NetworkPolicies that make that door the only way in.
# Safe to re-run. Findings and workarounds: docs/spikes/wp01-kind-full.md.
# One full tier per Docker host: the cluster name is fixed, so parallel worktrees share it.
set -eEuo pipefail

# Pinned versions. Substrate must be the version kagent pins (its go.mod replace directive).
# Dex, the front door, and the probe image are pinned in full/dex.yaml, full/frontdoor.yaml, full/verify.sh.
NODE_IMAGE=kindest/node:v1.35.0@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661
SUBSTRATE_VERSION=0.3.0-alpha1
KAGENT_VERSION=1.0.0-alpha5
MOCKLLM_REF=v0.1.0 # tag name, for humans; the commit below is what is built
MOCKLLM_COMMIT=35792a004f9387562a8760b2a513be8edeea6cf7
GO_IMAGE=golang:1.25.14@sha256:699337d620559a59b4a2bb298ad59611e535d2ee755a34cf2d2a98f37578dc80
RUNTIME_IMAGE=gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
MOCKLLM_IMAGE=courier-e2e/mockllm:${MOCKLLM_REF}

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
FULL=${HERE}/full
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")
SUBSTRATE_OCI=oci://ghcr.io/kagent-dev/substrate/helm
KAGENT_OCI=oci://ghcr.io/kagent-dev/kagent/helm

STEP=preflight
step() {
  STEP=$*
  echo "==> $*"
}
on_error() {
  echo "error: step '${STEP}' failed (line $1). Recover with: make e2e-down && make e2e-full-up" >&2
}
trap 'on_error $LINENO' ERR

for tool in docker kind kubectl helm curl openssl jq; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"

# kubectl-ate builds the Substrate CA and JWT pools. Fetched once, checksum-verified.
fetch_kubectl_ate() {
  local os arch sha bin
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)
  case "${arch}" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
  case "${os}-${arch}" in
    darwin-arm64) sha=398ee3cc884f278dbdd6bc8429c3249069ac71991083ea93c69c284a471bca79 ;;
    darwin-amd64) sha=1c99949ae2865ff65486a9475c99c95afc9ddadeb59e02950010fbf963cb8dbe ;;
    linux-arm64) sha=938d6804329439a4300c038bceea6f5e1142ce585ca3de96797dd7395ef4ef12 ;;
    linux-amd64) sha=2c074a8b63222fbd613c614bc75cf17798cdac818c2fba1aff9df32a500b3fbc ;;
    *) die "no kubectl-ate build for ${os}-${arch}" ;;
  esac
  bin=${ROOT}/bin/kubectl-ate-${SUBSTRATE_VERSION}-${os}-${arch}
  if [ ! -x "${bin}" ]; then
    mkdir -p "${ROOT}/bin"
    curl -fsSL -o "${bin}" "https://github.com/kagent-dev/substrate/releases/download/v${SUBSTRATE_VERSION}/kubectl-ate-${os}-${arch}"
    [ "$(openssl dgst -sha256 -r "${bin}" | cut -d' ' -f1)" = "${sha}" ] || { rm -f "${bin}"; die "kubectl-ate checksum mismatch"; }
    chmod +x "${bin}"
  fi
  echo "${bin}"
}

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
  "${K[@]}" annotate nodes -l "${OWNER_LABEL}" "${CHECKOUT_KEY}=${ROOT}" >/dev/null
fi
# gVisor's loopback pod-to-pod path needs proxy ARP on the node (Substrate's create-kind-cluster.sh).
for node in $(kind get nodes --name "${CLUSTER}"); do
  docker exec "${node}" sysctl -w net.ipv4.conf.all.proxy_arp=1 >/dev/null
done

step "Agent Substrate ${SUBSTRATE_VERSION}"
ATE=$(fetch_kubectl_ate)
SUBSTRATE_ARGS=(--namespace ate-system --version "${SUBSTRATE_VERSION}"
  --set-string 'ateApi.extraArgs[0]=--template-resync-interval=250ms'
  --set 'credentialProvider.namespacePolicies[0].atespace=kagent'
  --set 'credentialProvider.namespacePolicies[0].allowedNamespaces[0]=kagent')
"${H[@]}" upgrade --install substrate-crds "${SUBSTRATE_OCI}/substrate-crds" --namespace ate-system --create-namespace --version "${SUBSTRATE_VERSION}" >/dev/null
# Installed twice on purpose, as in kagent's CI. Substrate pods mount the pool Secrets below,
# so they sit in ContainerCreating until those exist and the first call cannot wait. The
# second call re-applies the same values and waits.
"${H[@]}" upgrade --install substrate "${SUBSTRATE_OCI}/substrate" "${SUBSTRATE_ARGS[@]}" >/dev/null
# Generated material is created only when absent, so a re-run never rotates keys.
ensure_pool() { # kind namespace name extra-args...
  "${K[@]}" -n "$2" get secret "$3" >/dev/null 2>&1 ||
    "${ATE}" --context "${CTX}" admin "make-$1-pool" --name="$3" --secret-namespace="$2" "${@:4}"
}
ensure_pool ca podcertificate-controller-system service-dns-ca-pool --ca-id=1
ensure_pool ca podcertificate-controller-system pod-identity-ca-pool --ca-id=1
ensure_pool jwt ate-system actor-id-jwt-pool --key-id=1
ensure_pool ca ate-system actor-id-ca-pool --ca-id=1
ensure_pool ca ate-system egress-mitm-ca-pool --ca-id=1 --key-type=ECDSAP256
if ! "${K[@]}" -n ate-system get secret actor-id-ca-certs >/dev/null 2>&1; then
  root=$("${K[@]}" -n ate-system get secret actor-id-ca-pool -o jsonpath='{.data.pool}' | base64 --decode |
    jq -r '.CAs[0].RootCertificateDER' | base64 --decode | openssl x509 -inform der -outform pem)
  "${K[@]}" -n ate-system create secret generic actor-id-ca-certs --from-literal=ca.crt="${root}"
fi
"${K[@]}" apply -f "${FULL}/ate-api-authentication.yaml" >/dev/null
"${H[@]}" upgrade --install substrate "${SUBSTRATE_OCI}/substrate" "${SUBSTRATE_ARGS[@]}" --wait --timeout 8m >/dev/null

step "namespaces, mockllm ${MOCKLLM_REF}, Dex"
for ns in kagent courier; do
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
# The front door's credentials: the courier-binding client from dex.yaml, its own cookie key.
"${K[@]}" -n kagent get secret kagent-frontdoor >/dev/null 2>&1 ||
  "${K[@]}" -n kagent create secret generic kagent-frontdoor --from-literal=client-secret=fake-courier-binding-secret \
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
# WP-01's policy had another name and apply does not prune; left in place it would still admit courier.
"${K[@]}" -n kagent delete networkpolicy controller-admit-courier-only --ignore-not-found >/dev/null
"${K[@]}" -n kagent rollout status deploy/slowllm deploy/kagent-frontdoor --timeout=180s
"${K[@]}" -n kagent wait --for=condition=Ready agent/test-agent agent/slow-agent --timeout=300s

step "verify"
"${FULL}/verify.sh"
docker stats --no-stream --format 'node cpu={{.CPUPerc}} mem={{.MemUsage}}' "$(kind get nodes --name "${CLUSTER}")"
echo "full tier up in ${SECONDS}s (context ${CTX})"

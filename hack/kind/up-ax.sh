#!/usr/bin/env bash
# Isolated AX/Substrate tier. Never changes agent-kourier-full or its live Slack sandbox.
set -eEuo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
# shellcheck source-path=SCRIPTDIR source=substrate.sh
. "${HERE}/substrate.sh"
use_tier ax
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")
AX_IMAGE=agent-kourier-e2e/ax:dev
REGISTRY=agent-kourier-ax-registry
REGISTRY_PORT=5002
BUILD=
trap '[ -z "${BUILD}" ] || rm -rf "${BUILD}"; delete_agentkourier_pod "${DEX_TOKEN_POD:-}"' EXIT
for tool in docker kind kubectl helm curl openssl jq go; do command -v "${tool}" >/dev/null || die "${tool} is required"; done
docker info >/dev/null 2>&1 || die "Docker is not reachable"
require_helm4
if cluster_exists; then
  assert_owned
else
  kind create cluster --name "${CLUSTER}" --config "${HERE}/ax/kind-config.yaml" --image "${NODE_IMAGE}"
  assert_context ax
  "${K[@]}" annotate nodes -l "${OWNER_LABEL}" "${CHECKOUT_KEY}=${ROOT}" >/dev/null
fi
assert_context ax
refuse_while_sandbox_up
for node in $(kind get nodes --name "${CLUSTER}"); do docker exec "${node}" sysctl -w net.ipv4.conf.all.proxy_arp=1 >/dev/null; done
# Substrate pulls actors through its own image cache, not containerd's kind image store.
# A dedicated, loopback-only registry lets it pull the locally built fixture image.
if docker inspect "${REGISTRY}" >/dev/null 2>&1; then
  [ "$(docker inspect -f '{{index .Config.Labels "dev.agent-kourier.ax-registry"}}' "${REGISTRY}")" = true ] || die "${REGISTRY} is not owned by this tooling"
  docker start "${REGISTRY}" >/dev/null
else
  docker run -d --restart=unless-stopped --name "${REGISTRY}" --label dev.agent-kourier.ax-registry=true \
    -p "127.0.0.1:${REGISTRY_PORT}:5000" registry:3@sha256:d4032d4cc323c1731c707931ecada07a53076eac38e3b61ae57533f170f2c6c3 >/dev/null
fi
if [ "$(docker inspect -f '{{if index .NetworkSettings.Networks "kind"}}yes{{end}}' "${REGISTRY}")" != yes ]; then docker network connect kind "${REGISTRY}"; fi
# The image cache resolves the registry through CoreDNS's forwarding to Docker DNS.
registry_ip=$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "${REGISTRY}")
SUBSTRATE_ATESPACE=agent-kourier-ax
SUBSTRATE_AUTH_CONFIG=${HERE}/ax/ate-api-authentication.yaml
SUBSTRATE_REGISTRY=${registry_ip}:5000
install_substrate
for ns in agent-kourier e2e agent-kourier-ax; do "${K[@]}" create namespace "${ns}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null; done
"${K[@]}" apply -f "${HERE}/ax/workerpool.yaml" >/dev/null
"${K[@]}" -n agent-kourier-ax wait --for=jsonpath='{.status.readyReplicas}'=1 workerpool/agent-kourier-ax --timeout=180s
"${K[@]}" apply -f "${HERE}/full/dex.yaml" >/dev/null
"${K[@]}" -n dex rollout status deploy/dex --timeout=180s
refresh_dex_token
# Decode only the public subject claim; never print or persist the token itself.
subject=$("${K[@]}" -n agent-kourier get secret agent-kourier-agent-token -o json | jq -r '.data.token|@base64d|split(".")[1]|@base64d|fromjson|.sub')

echo '==> AX image: pinned, unmodified control plane and ADK scenario agent'
docker build -f "${ROOT}/examples/ax/Dockerfile.e2e" -t "${AX_IMAGE}" "${ROOT}/examples/ax"
kind load docker-image "${AX_IMAGE}" --name "${CLUSTER}"
docker tag "${AX_IMAGE}" "localhost:${REGISTRY_PORT}/agent-kourier-ax:dev"
docker push "localhost:${REGISTRY_PORT}/agent-kourier-ax:dev" >/dev/null
fixture_digest=$(docker image inspect "localhost:${REGISTRY_PORT}/agent-kourier-ax:dev" --format '{{range .RepoDigests}}{{println .}}{{end}}' | sed -n "s|^localhost:${REGISTRY_PORT}/agent-kourier-ax@||p" | head -n 1)
case "${fixture_digest}" in sha256:*) ;; *) die "registry push returned no immutable image digest" ;; esac
"${K[@]}" -n e2e create configmap ax-fixtures --from-literal="subject=${subject}" \
  --from-literal="image=localhost:${REGISTRY_PORT}/agent-kourier-ax@${fixture_digest}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
"${K[@]}" apply -f "${HERE}/ax/controlplane.yaml" >/dev/null
"${K[@]}" -n ax-system rollout restart deploy/ax-server >/dev/null
"${K[@]}" -n ax-system rollout status deploy/ax-redis deploy/ax-server --timeout=180s

echo '==> test doubles and AX fixtures'
install_operator
build_doubles_image
apply_doubles
"${K[@]}" apply -f "${HERE}/ax/controls.yaml" >/dev/null
"${K[@]}" -n e2e rollout restart deploy/ax-gates >/dev/null
"${K[@]}" -n e2e rollout status deploy/ax-gates --timeout=540s
settle_ax_tier

echo '==> Agent Kourier bindings'
load_agentkourier_image
"${K[@]}" -n agent-kourier create secret generic slack-fast --from-literal=botToken=xoxb-fake-test-token \
  --from-literal=appToken=xapp-fake-test-token --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
"${K[@]}" -n agent-kourier create configmap agent-kourier-config --from-file="${HERE}/ax/agent-kourier/config" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
# The chart (ci/ax-values.yaml) gives this tier a database file of its own, agent-kourier-ax.db, so that a development cluster's older
# fixture database, whose experimental migration 12 predates main's trace migration, stays intact in the volume. A cluster
# brought up before the chart has a hand-made Deployment, which install_agentkourier replaces (its volume stays).
install_agentkourier "${ROOT}/charts/agent-kourier/ci/ax-values.yaml"
"${K[@]}" -n agent-kourier rollout status deploy/agent-kourier --timeout=180s
NODE=$(kind get nodes --name "${CLUSTER}")
assert_never_published
assert_app_unpublished e2e ax-gates
echo "AX tier up in ${SECONDS}s (context ${CTX}); run make e2e-ax"

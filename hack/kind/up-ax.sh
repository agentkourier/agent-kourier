#!/usr/bin/env bash
# Isolated AX/Substrate tier with a deterministic command and no model access.
set -eEuo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
use_tier ax
[ "${E2E_SLACK_MODE:-socket}" = socket ] || die "the AX tier runs Socket Mode"
AX_VERSION=${AX_E2E_VERSION:-v0.4.0-rc.2}
AX_RUNNER_VOLUME=${AX_E2E_RUNNER_VOLUME:-}
if [ -z "${AX_RUNNER_VOLUME}" ]; then
  [ "${AX_VERSION}" = v0.4.0-rc.2 ] || die "AX_E2E_RUNNER_VOLUME is required with a different AX release"
  AX_RUNNER_VOLUME=ghcr.io/google/ax/ax-runner-volume@sha256:a85dc4ac3e4b060d9157e3f80c01463544e79a0e92106bacad1e4f3f3cf5e4d5
fi
REGISTRY=kind-registry # Substrate's pinned local profile uses this name inside kind.
FIXTURE=localhost:5003/ax-launcher-fixture:local
LAUNCHER=agent-kourier-e2e/ax-launcher:local
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")
for tool in docker kind kubectl helm curl openssl jq go shasum; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
docker info >/dev/null 2>&1 || die "Docker is not reachable"
require_helm4
BUILD=
scratch=$(mktemp -d)
port_forward=
trap 'if [ -n "$port_forward" ]; then kill "$port_forward" 2>/dev/null || true; fi; rm -rf "$scratch"; if [ -n "$BUILD" ]; then rm -rf "$BUILD"; fi' EXIT

if cluster_exists; then
  assert_owned
else
  KUBECONFIG="${scratch}/setup" kind create cluster --name "${CLUSTER}" \
    --config "${HERE}/ax/kind-config.yaml" --image "${NODE_IMAGE}"
  previous=$(kubectl config current-context 2>/dev/null || true)
  kind export kubeconfig --name "${CLUSTER}" >/dev/null
  [ -z "$previous" ] || kubectl config use-context "$previous" >/dev/null
fi
assert_context ax
"${K[@]}" annotate nodes -l "${OWNER_LABEL}" "${CHECKOUT_KEY}=${ROOT}" --overwrite >/dev/null

if docker inspect "${REGISTRY}" >/dev/null 2>&1; then
  [ "$(docker inspect -f '{{index .Config.Labels "dev.agent-kourier.ax-launcher-proof"}}' "${REGISTRY}")" = true ] ||
    die "${REGISTRY} exists and is not owned by the AX launcher tier"
  docker start "${REGISTRY}" >/dev/null
else
  docker run -d --name "${REGISTRY}" --network kind --label dev.agent-kourier.ax-launcher-proof=true \
    -p 127.0.0.1:5003:5000 registry:3@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c >/dev/null
fi
if [ "$(docker inspect -f '{{if index .NetworkSettings.Networks "kind"}}yes{{end}}' "${REGISTRY}")" != yes ]; then
  docker network connect kind "${REGISTRY}"
fi

case "$(uname -m)" in
  aarch64 | arm64) host_arch=arm64 ;;
  x86_64 | amd64) host_arch=amd64 ;;
  *) die "unsupported host architecture" ;;
esac
case "$(docker info --format '{{.Architecture}}')" in
  aarch64 | arm64) docker_arch=arm64 ;;
  x86_64 | amd64) docker_arch=amd64 ;;
  *) die "unsupported Docker architecture" ;;
esac
asset=ax_$(uname -s | tr '[:upper:]' '[:lower:]')_${host_arch}.tar.gz
base="https://github.com/google/ax/releases/download/${AX_VERSION}"
curl -fsSL "${base}/${asset}" -o "${scratch}/${asset}"
curl -fsSL "${base}/checksums.txt" -o "${scratch}/checksums.txt"
(cd "${scratch}" && awk -v asset="${asset}" '$2 == asset { print }' checksums.txt | shasum -a 256 -c && tar xzf "${asset}")
kind export kubeconfig --name "${CLUSTER}" --kubeconfig "${scratch}/bootstrap" >/dev/null
KUBECONFIG="${scratch}/bootstrap" "${scratch}/ax" local start --name "${CLUSTER}" --runner-volume-image "${AX_RUNNER_VOLUME}"

for ns in e2e agent-kourier; do
  "${K[@]}" create namespace "${ns}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
done
"${K[@]}" -n agent-kourier create secret generic slack-main --from-literal=botToken=xoxb-fake-test-token \
  --from-literal=appToken=xapp-fake-test-token --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
"${K[@]}" -n agent-kourier create secret generic slack-second --from-literal=botToken=xoxb-fake-team-b-token \
  --from-literal=appToken=xapp-fake-team-b-token --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
# This A2A token is an intentionally public test fixture. The result token is random.
"${K[@]}" -n agent-kourier create secret generic ax-a2a-auth --from-literal=token=ax-e2e-a2a-fake \
  --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
if [ -z "$("${K[@]}" -n agent-kourier get secret ax-result-auth --ignore-not-found -o name)" ]; then
  (umask 077 && printf '%s' "$(openssl rand -hex 32)" > "${scratch}/token")
  "${K[@]}" -n agent-kourier create secret generic ax-result-auth --from-file="token=${scratch}/token" >/dev/null
  "${K[@]}" -n agent-kourier label secret ax-result-auth ax.io/credential=true >/dev/null
fi

"${HERE}/ax/tls.sh" "${CTX}" "${scratch}"
# Extend the credential provider's default atespace grant, gated by the same label.
policy="${scratch}/credential-policy.yaml"
cat > "${policy}" <<'YAML'
policies:
  - atespace: default
    allowedNamespaces: [ax-system, default, agent-kourier]
    secretSelector:
      matchLabels:
        ax.io/credential: "true"
YAML
jq -n --rawfile policy "${policy}" '{"data":{"namespace-policy.yaml":$policy}}' > "${scratch}/credential-policy.json"
"${K[@]}" -n ate-system patch cm k8s-credential-provider-namespace-policy --type=merge --patch-file="${scratch}/credential-policy.json" >/dev/null
"${K[@]}" -n ate-system rollout restart deploy/k8s-credential-provider >/dev/null
"${K[@]}" -n ate-system rollout status deploy/k8s-credential-provider --timeout=120s

cat > "${scratch}/.dockerignore" <<'DOCKER'
*
!ax-fixture
!agent-kourier-ax
!agent-kourier-ax-result
DOCKER
for program in ax-fixture agent-kourier-ax-result; do
  GOOS=linux GOARCH=${docker_arch} CGO_ENABLED=0 go build -C "${ROOT}/addons/ax" -trimpath -o "${scratch}/${program}" "./cmd/${program}"
done
docker build -q -f "${HERE}/ax/Dockerfile" -t "${FIXTURE}" "${scratch}" >/dev/null
docker push "${FIXTURE}" >/dev/null
fixture_digest=$(docker image inspect "${FIXTURE}" --format '{{index .RepoDigests 0}}')
[[ "${fixture_digest}" == *@sha256:* ]] || die "the registry did not report a fixture digest"
kind load docker-image "${FIXTURE}" --name "${CLUSTER}"

echo '==> Build the AX add-on'
GOOS=linux GOARCH=${docker_arch} CGO_ENABLED=0 go build -C "${ROOT}/addons/ax" -tags testhooks -trimpath -o "${scratch}/agent-kourier-ax" ./cmd/agent
cat <<'DOCKER' | docker build -q -f - -t "${LAUNCHER}" "${scratch}" >/dev/null
FROM scratch
COPY agent-kourier-ax /agent-kourier-ax
USER 65532:65532
ENTRYPOINT ["/agent-kourier-ax"]
DOCKER
kind load docker-image "${LAUNCHER}" --name "${CLUSTER}"
cat > "${scratch}/task.yaml" <<YAML
apiVersion: ax.io/v1alpha1
kind: Task
metadata: {name: echo-template, atespace: default}
spec:
  profile:
    image: "${fixture_digest}"
  commands: ["/usr/local/bin/agent-kourier-ax-result", "--", "/usr/local/bin/ax-fixture", "echo"]
  gateway: {name: kourier-results}
  debug: false
YAML
"${K[@]}" -n agent-kourier create configmap ax-echo-template --from-file="${scratch}/task.yaml" --dry-run=client -o yaml |
  "${K[@]}" apply -f - >/dev/null
"${K[@]}" -n ax-system port-forward svc/ax-server :8080 >"${scratch}/ax-forward" 2>&1 &
port_forward=$!
ax_port=
for _ in {1..50}; do
  ax_port=$(sed -nE 's/^Forwarding from 127\.0\.0\.1:([0-9]+) .*/\1/p' "${scratch}/ax-forward")
  [ -z "$ax_port" ] || break
  sleep 0.2
done
[ -n "$ax_port" ] || die "AX port-forward did not start"
"${scratch}/ax" --server "127.0.0.1:${ax_port}" apply -f "${HERE}/ax/gateway.yaml"

deploy_postgres
if [ "$("${K[@]}" -n e2e exec deploy/postgres -- psql -U agentkourier -Atqc "SELECT 1 FROM pg_database WHERE datname='ax_kourier'")" != 1 ]; then
  "${K[@]}" -n e2e exec deploy/postgres -- psql -U agentkourier -c 'CREATE DATABASE ax_kourier' >/dev/null
fi
if [ -z "$("${K[@]}" -n agent-kourier get secret ax-kourier-pg --ignore-not-found -o name)" ]; then
  "${K[@]}" -n agent-kourier get secret agent-kourier-pg -o json |
    jq '{apiVersion:"v1",kind:"Secret",metadata:{name:"ax-kourier-pg",namespace:"agent-kourier"},
      data:{dsn:(.data.dsn|@base64d|sub("/agentkourier\\?";"/ax_kourier?")|@base64)}}' |
    "${K[@]}" create -f - >/dev/null
fi

install_operator
build_doubles_image
apply_doubles
load_agentkourier_image
install_agentkourier "${ROOT}/charts/agent-kourier/ci/ax-values.yaml"
"${K[@]}" apply -f "${HERE}/ax/resources.yaml" -f "${HERE}/ax/fixture.yaml" >/dev/null
"${K[@]}" -n agent-kourier rollout restart deploy/agent-kourier-ax deploy/ax-results >/dev/null
"${K[@]}" -n agent-kourier rollout status deploy/agent-kourier deploy/agent-kourier-ax deploy/ax-results --timeout=180s
echo "AX launcher tier up in ${SECONDS}s (context ${CTX}); run make e2e-ax"

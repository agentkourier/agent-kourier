#!/usr/bin/env bash
# Run the CRD corpus test (make crd-corpus): a kind cluster with only the three CRDs, no images and no Agent Kourier,
# against which test/crdcorpus sends every entry of its corpus as a server-side dry-run create and holds the schema to
# the invariant of spec D50 (2): every object the schema refuses, the broker refuses too, and every valid object passes
# both. Nothing is stored but the Namespaces the entries need, so the cluster is thrown away after.
#
# Without arguments the script makes a cluster named agent-kourier-crds (CRD_CORPUS_CLUSTER overrides it) from lib.sh's
# pinned node image, applies charts/agent-kourier/files/crds to it, runs the test, and deletes the cluster. It is
# not one of the E2E tiers, so `make e2e-down` does not touch it, and it keeps its kubeconfig in a temporary file, so the
# host kubeconfig and its current context are never read or changed. A cluster of that name that this script did not
# create (it lacks the owner label) is refused, not reused or deleted. CRD_CORPUS_KEEP=1 leaves the cluster up for a
# second run, which reuses it.
#
# CRD_CORPUS_KUBECONFIG=<file> runs against the cluster that kubeconfig's current context names instead: it is not
# created and not deleted, and the script and the test both refuse an API server that is not on this machine. The CRDs are applied to it.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"

CRDS=${ROOT}/charts/agent-kourier/files/crds
CORPUS_CLUSTER=${CRD_CORPUS_CLUSTER:-agent-kourier-crds}
CORPUS_OWNER=agentkourier.agent-kourier.dev/owner=crd-corpus

for tool in kubectl go; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
[ -d "${CRDS}" ] || die "${CRDS} does not exist: run make crds"

TMP=$(mktemp -d)
CREATED=
cleanup() {
  if [ -n "${CREATED}" ] && [ "${CRD_CORPUS_KEEP:-}" != 1 ]; then
    kind delete cluster --name "${CORPUS_CLUSTER}" >/dev/null 2>&1 || echo "warning: could not delete kind cluster ${CORPUS_CLUSTER}" >&2
  fi
  rm -rf "${TMP}"
}
trap cleanup EXIT

if [ -n "${CRD_CORPUS_KUBECONFIG:-}" ]; then
  KUBECONFIG_FILE=${CRD_CORPUS_KUBECONFIG}
  [ -r "${KUBECONFIG_FILE}" ] || die "CRD_CORPUS_KUBECONFIG ${KUBECONFIG_FILE} is not readable"
else
  command -v kind >/dev/null || die "kind is required (or set CRD_CORPUS_KUBECONFIG to a local cluster's kubeconfig)"
  docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"
  KUBECONFIG_FILE=${TMP}/kubeconfig
  if kind get clusters | grep -qx "${CORPUS_CLUSTER}"; then
    kind get kubeconfig --name "${CORPUS_CLUSTER}" >"${KUBECONFIG_FILE}"
    [ -n "$(kubectl --kubeconfig "${KUBECONFIG_FILE}" --request-timeout=20s get nodes -l "${CORPUS_OWNER}" -o name)" ] ||
      die "kind cluster ${CORPUS_CLUSTER} exists but was not created by this script; refusing to touch it (CRD_CORPUS_CLUSTER names another)"
    echo "==> reusing kind cluster ${CORPUS_CLUSTER}"
  else
    echo "==> kind cluster ${CORPUS_CLUSTER}"
    kind create cluster --name "${CORPUS_CLUSTER}" --image "${NODE_IMAGE}" --kubeconfig "${KUBECONFIG_FILE}" --wait 120s --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${CORPUS_CLUSTER}
networking:
  apiServerAddress: "127.0.0.1"
nodes:
  - role: control-plane
    labels:
      ${CORPUS_OWNER%%=*}: ${CORPUS_OWNER#*=}
EOF
    CREATED=1
  fi
fi

# The kubeconfig's current context is the cluster; the host's kubeconfig is not consulted.
K=(kubectl --kubeconfig "${KUBECONFIG_FILE}" --request-timeout=60s)
server=$("${K[@]}" config view --minify -o 'jsonpath={.clusters[0].cluster.server}')
case "${server}" in
  https://127.0.0.1:* | https://localhost:* | https://\[::1\]:*) ;;
  *) die "API server ${server} is not on this machine; refusing to apply CRDs to it" ;;
esac

echo "==> CRDs"
"${K[@]}" apply --server-side -f "${CRDS}"
"${K[@]}" wait --for=condition=Established --timeout=60s -f "${CRDS}"

echo "==> corpus test"
cd "${ROOT}"
# -v is for the test's own summary (what the schema refuses, and what only the broker does); the line of each of the
# few hundred entries that passed is dropped. A failure still prints its lines, and pipefail keeps go test's status.
CRD_CORPUS_KUBECONFIG=${KUBECONFIG_FILE} go test -tags crdcorpus -count=1 -timeout 10m -v ./test/crdcorpus/ \
  -run TestTheSchemaNeverRefusesWhatTheBrokerAccepts | awk '!/^ *--- PASS|^=== (RUN|PAUSE|CONT)/'

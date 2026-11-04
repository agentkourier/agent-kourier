#!/usr/bin/env bash
# Move the fast tier's Agent Kourier from the ConfigMap route to custom resources and back with Helm, as the chart README says to, while a session is
# in flight, and say what a user would have seen (make e2e-crs-migration). The tier must be up on Postgres with two replicas on the ConfigMap route:
#   E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up
# so that each move is a failover, with the old leader still running while Helm changes what it reads, and the new one recovering the session.
#
# The release's config is the chart's own (hack/kind/crs-migration-values.yaml), since that is the case where the move is "the same config.* values",
# rendered as a ConfigMap on one route and as custom resources on the other, and where Helm deletes the custom resources on the way back. It replaces
# the tier's config for the length of the run; the script puts it back at the end (CRS_MIGRATION_KEEP=1 leaves the release as it is, for a look).
#
# The steps, each an upgrade of release agent-kourier with the tier's values and the file above:
#   1. files route: a session (a task held on a gate, a reply queued behind it) is opened.                    (E2E_PHASE open-1)
#   2. crds.install=true: the CRDs, which are inert, and no pod rolls.
#   3. config.source=crds: the same values render custom resources, and the ConfigMap goes.
#   4. the session is found again, in whatever is running now, released, and must end with both answers once. (finish-1)
#   5. a second session is opened on the custom resources route.                                             (open-2)
#   6. config.source=files: the ConfigMap returns, and Helm deletes the custom resources it rendered, while the old pods, which read them,
#      may still be up.
#   7. the second session is found again and released.                                                       (finish-2)
# Each phase is the E2E test TestCR_MigrationKeepsASessionAlive (hack/kind/e2e-run.sh with E2E_PHASE). A phase that fails does not stop the run: what
# each saw is printed, with the log lines of the old pods that carry the words binding, gone, removed or served, and a timeline of the pods and of
# how many Bindings the cluster held, every two seconds, from the upgrade of steps 3 and 6. Everything goes to CRS_MIGRATION_OUT (a new temporary
# directory by default; no token or Secret value is in it, as Agent Kourier logs none).
#
# Every kubectl and helm call names --context kind-agent-kourier-fast, as the tier's scripts do; assert_context aborts unless that is the local kind cluster.
set -eEuo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
use_tier fast
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")
CHART=${ROOT}/charts/agent-kourier
OUT=${CRS_MIGRATION_OUT:-$(mktemp -d)}
mkdir -p "${OUT}"

for tool in docker kind kubectl helm; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
require_helm4
cluster_exists || die "kind cluster ${CLUSTER} does not exist; bring it up with: E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up"
assert_owned
assert_context fast

dep() { "${K[@]}" -n agent-kourier get deploy/agent-kourier -o "jsonpath=$1"; }
[ "$(dep '{.spec.replicas}')" = 2 ] || die "the tier must run two Agent Kourier replicas on Postgres: E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up"
[ "$(dep '{.spec.template.spec.containers[0].env[?(@.name=="AGENTKOURIER_STORE")].value}')" = postgres ] || die "the tier must run on Postgres: E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up"
[ -z "$(dep '{.spec.template.spec.containers[0].env[?(@.name=="AGENTKOURIER_CONFIG_SOURCE")].value}')" ] ||
  die "the tier must start on the ConfigMap route: E2E_CONFIG_SOURCE=files make e2e-fast-up"
"${K[@]}" get crd bindings.agentkourier.dev >/dev/null 2>&1 &&
  die "the cluster holds the CRDs already; the scenario installs them as step 2 of the move. Bring the tier up fresh: hack/kind/down.sh fast, then E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up"

# upgrade [HELM ARG...]: release agent-kourier, with the tier's values on Postgres and two replicas and the scenario's config. Does not wait.
upgrade() {
  helm_install_agentkourier agent-kourier "${CHART}/ci/fast-postgres-values.yaml" -f "${CHART}/ci/fast-postgres-ha-values.yaml" -f "${HERE}/crs-migration-values.yaml" "$@"
}
rolled_out() { "${K[@]}" -n agent-kourier rollout status deploy/agent-kourier --timeout=300s; }

RESULTS=()
# phase NAME: the E2E test of that phase in the tier's runner. A failure is noted and the run goes on.
phase() {
  local name=$1
  echo "==> phase ${name}"
  if E2E_PHASE=${name} E2E_RUN='TestCR_MigrationKeepsASessionAlive' "${HERE}/e2e-run.sh" fast >"${OUT}/phase-${name}.log" 2>&1; then
    RESULTS+=("PASS ${name}")
  else
    RESULTS+=("FAIL ${name}")
  fi
  grep -E '^[[:space:]]+(cr_migration_test|scenario_test|wait).go|^--- |session [0-9] ' "${OUT}/phase-${name}.log" | cut -c1-600 || true
}

# observe TAG: until stop_watching, a timeline (the pods of the release, and how many Bindings the cluster holds) every two seconds, and the log of each
# Agent Kourier pod that is there now, to the end of the pod. The pods of the release are named by the labels the tier uses.
WATCHERS=()
observe() {
  local tag=$1 pod
  (
    while :; do
      printf '%s pods:' "$(date -u +%T)"
      "${K[@]}" -n agent-kourier get pods -l "${TIER_AGENTKOURIER_PODS}" -o 'jsonpath={range .items[*]}{" "}{.metadata.name}{"="}{.status.phase}{if .metadata.deletionTimestamp}{"/terminating"}{end}{end}' 2>/dev/null || true
      printf ' bindings=%s\n' "$("${K[@]}" get akbinding -A --no-headers 2>/dev/null | wc -l | tr -d ' ')"
      sleep 2
    done
  ) >"${OUT}/timeline-${tag}.txt" &
  WATCHERS+=($!)
  for pod in $("${K[@]}" -n agent-kourier get pods -l "${TIER_AGENTKOURIER_PODS}" -o 'jsonpath={.items[*].metadata.name}'); do
    "${K[@]}" -n agent-kourier logs -f --timestamps "${pod}" >"${OUT}/log-${tag}-${pod}.txt" 2>&1 &
    WATCHERS+=($!)
  done
}
stop_watching() {
  local pid
  for pid in ${WATCHERS[@]+"${WATCHERS[@]}"}; do kill "${pid}" 2>/dev/null || true; done
  WATCHERS=()
}
trap 'stop_watching' EXIT

echo "outputs in ${OUT}"
echo "==> the release on the ConfigMap route, with the scenario's config (the chart makes the ConfigMap; the tier's own goes)"
"${K[@]}" -n agent-kourier delete configmap agent-kourier-config --ignore-not-found >/dev/null
upgrade
rolled_out
"${K[@]}" -n agent-kourier get configmap agent-kourier-config >/dev/null

phase open-1

echo "==> step 1 of the move: the CRDs, the route still files"
upgrade --set crds.install=true
"${K[@]}" wait --for=condition=Established --timeout=60s crd/bindings.agentkourier.dev crd/agentbackends.agentkourier.dev crd/chatconnections.agentkourier.dev >/dev/null
rolled_out

echo "==> step 2 of the move: config.source=crds"
observe to-crds
date -u +'%T upgrading' >>"${OUT}/timeline-to-crds.txt"
upgrade -f "${HERE}/crds-values.yaml"
date -u +'%T helm returned' >>"${OUT}/timeline-to-crds.txt"
rolled_out
sleep 20
stop_watching
echo "Bindings now: $("${K[@]}" get akbinding -A --no-headers | wc -l | tr -d ' ') (kubectl get akbinding -A)"
"${K[@]}" get akbinding,akbackend,akconnection -A

phase finish-1
phase open-2

echo "==> the way back: config.source=files, the same values (crds.install stays on)"
observe to-files
date -u +'%T upgrading' >>"${OUT}/timeline-to-files.txt"
upgrade --set crds.install=true
date -u +'%T helm returned' >>"${OUT}/timeline-to-files.txt"
echo "Bindings right after the upgrade returned: $("${K[@]}" get akbinding -A --no-headers 2>/dev/null | wc -l | tr -d ' ')"
rolled_out
sleep 20
stop_watching

phase finish-2

echo "==> what the old pods said (lines about bindings, gone, removed or served), by pod"
for f in "${OUT}"/log-to-*.txt; do
  echo "--- $(basename "${f}")"
  grep -iE 'binding|gone|removed|served|notice' "${f}" | cut -c1-300 | head -40 || true
done
echo "==> timelines: ${OUT}/timeline-to-crds.txt and ${OUT}/timeline-to-files.txt"

if [ "${CRS_MIGRATION_KEEP:-}" != 1 ]; then
  echo "==> the tier back as it was: Helm deletes the ConfigMap it made, since the tier's values use its own; bring-up makes that one"
  helm_install_agentkourier agent-kourier "${CHART}/ci/fast-postgres-values.yaml" -f "${CHART}/ci/fast-postgres-ha-values.yaml" --set crds.install=true >/dev/null 2>&1 || true
  E2E_STORE=postgres E2E_REPLICAS=2 "${HERE}/up-fast.sh" >"${OUT}/restore.log" 2>&1 || die "putting the tier back failed; see ${OUT}/restore.log (hack/kind/down.sh fast and a bring-up is the way out)"
fi

echo "==> results"
printf '%s\n' "${RESULTS[@]}"
case " ${RESULTS[*]} " in
  *" FAIL "*) exit 1 ;;
esac

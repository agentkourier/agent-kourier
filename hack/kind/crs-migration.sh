#!/usr/bin/env bash
# Move the fast tier's Agent Kourier from the ConfigMap route to custom resources and back with Helm, as the chart README says to, while a session is
# in flight, and say what a user would have seen (make e2e-crs-migration). The tier must be up on Postgres with two replicas on the ConfigMap route:
#   E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up
# so that each move is a failover, with the old leader still running while Helm changes what it reads, and the new one recovering the session.
#
# The release's config is the chart's own (hack/kind/crs-migration-values.yaml), since that is the case where the move is "the same config.* values",
# rendered as a ConfigMap on one route and as custom resources on the other, and where Helm deletes the custom resources on the way back. It replaces
# the tier's config for the length of the run; on any exit the script puts the tier back on the ConfigMap route (CRS_MIGRATION_KEEP=1 leaves the release as it is, for a look). The CRDs stay under the release, so a second run needs a fresh tier (hack/kind/down.sh fast, then a bring-up).
#
# The steps, each an upgrade of release agent-kourier with the tier's values and the file above:
#   0. a plain rollout on the ConfigMap route, which is what any restart costs and the baseline for the rest.
#   1. files route: a session (a task held on a gate, a reply queued behind it) is opened.                    (E2E_PHASE open-1)
#   2. crds.install=true: the CRDs, which are inert, and no pod rolls.
#   3. config.source=crds: the same values render custom resources, and the ConfigMap goes.
#   4. the session is found again, in whatever is running now, released, and must end with both answers once. (finish-1)
#   5. a second session is opened on the custom resources route.                                             (open-2)
#   6. config.source=files: the ConfigMap returns, and Helm deletes the custom resources it rendered, while the old pods, which read them,
#      may still be up.
#   7. the second session is found again and released.                                                       (finish-2)
#   8. the way back once more, from the custom resources, with the rollout held for 45 seconds after the upgrade, so that the old pods run that long without the
#      custom resources (a third session, open-3 and finish-3, and a probe).
#   9. the way forward once more, from the ConfigMap, held the same way, so that the old pods run that long with their mounted ConfigMap deleted (open-4, finish-4).
#  10. the destructive order: the way back held, the first task of a session ended and a new reply written while the old pods run without the custom resources
#      (open-5, release-5, reply-5, settle-5). Before the old pods hold their Bindings the thread ends with the notice that the agent is no longer set up.
# Each phase is the E2E test TestCR_MigrationKeepsASessionAlive (hack/kind/e2e-run.sh with E2E_PHASE). A phase that fails does not stop the run. Steps
# 0, 3, 6, 8 and 9 are watched: a person's mention of the Binding's channel every two seconds for 100 seconds from before the step (phase probe-100), which
# says how many were answered, and how many were delivered and never answered, which is a message that a broker without the Binding ignored without
# a word; a timeline of the pods and of how many Bindings the cluster holds, every two seconds; and the log of each pod that was there when the
# step began. Everything goes to CRS_MIGRATION_OUT (a new temporary directory by default; no token or Secret value is in it, as Agent Kourier logs none).
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
  grep -E '^[[:space:]]+(cr_migration_test|scenario_test|wait).go|^--- |^[[:space:]]+(UMIG|bot)' "${OUT}/phase-${name}.log" | cut -c1-600 || true
}

# observe TAG: until stop_watching, a timeline (the pods of the release, and how many Bindings the cluster holds) every two seconds, and the log of each
# Agent Kourier pod that is there now, to the end of the pod. The pods of the release are named by the labels the tier uses.
WATCHERS=()
observe() {
  local tag=$1 pod
  (
    while :; do
      printf '%s pods:' "$(date -u +%T)"
      "${K[@]}" -n agent-kourier get pods -l "${TIER_AGENTKOURIER_PODS}" --no-headers -o 'custom-columns=N:.metadata.name,P:.status.phase,D:.metadata.deletionTimestamp' 2>/dev/null |
        awk '{n=$1; sub(/^agent-kourier-/, "", n); printf " %s=%s%s", n, $2, ($3 != "<none>" ? "/terminating" : "")}' || true
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

# restore_tier: Helm deletes the ConfigMap it made, since the tier's values use a ConfigMap of their own: bring-up makes that one. The CRDs stay under the
# release (crds.install stays on), so the tier is back on the ConfigMap route with them in the cluster, and a second run of this script needs a fresh tier.
restore_tier() {
  helm_install_agentkourier agent-kourier "${CHART}/ci/fast-postgres-values.yaml" -f "${CHART}/ci/fast-postgres-ha-values.yaml" --set crds.install=true >/dev/null 2>&1 || true
  E2E_STORE=postgres E2E_REPLICAS=2 "${HERE}/up-fast.sh" >"${OUT}/restore.log" 2>&1
}
# On any exit, a failed one included: stop the watchers, let a held rollout go, and put the tier back unless CRS_MIGRATION_KEEP=1. RESTORE is set by the first change.
RESTORE=
cleanup() {
  local rc=$?
  stop_watching
  if [ -n "${RESTORE}" ]; then
    "${K[@]}" -n agent-kourier rollout resume deploy/agent-kourier >/dev/null 2>&1 || true
    if [ "${CRS_MIGRATION_KEEP:-}" != 1 ]; then
      echo "==> the tier back on the ConfigMap route (the CRDs stay under the release)"
      restore_tier || {
        echo "error: putting the tier back failed; see ${OUT}/restore.log (hack/kind/down.sh fast and a bring-up is the way out)" >&2
        rc=1
      }
    fi
  fi
  exit "${rc}"
}
trap cleanup EXIT

# move TAG COMMAND...: the move itself, watched: a person's mention of the Binding's channel every two seconds from before the command to after the
# rollout (the E2E test, in the phase probe-SECONDS), a timeline and the logs of the pods that are there when it starts. The command is run when the
# probe says it has started; the probe's own summary is printed at the end. A move that fails the script is a failed move.
move() {
  local tag=$1 probe_pid waited=0
  shift
  observe "${tag}"
  E2E_PHASE=probe-100 E2E_RUN='TestCR_MigrationKeepsASessionAlive' "${HERE}/e2e-run.sh" fast >"${OUT}/probe-${tag}.log" 2>&1 &
  probe_pid=$!
  # The Job's log is read while it runs: go test -v prints a line of t.Logf as it is made.
  until "${K[@]}" -n e2e logs job/e2e-runner 2>/dev/null | grep -q 'probing started'; do
    kill -0 "${probe_pid}" 2>/dev/null || die "the probe ended before it started; see ${OUT}/probe-${tag}.log"
    sleep 2
    waited=$((waited + 2))
    [ "${waited}" -lt 240 ] || die "the probe did not start within four minutes"
  done
  date -u +'%T the command starts' >>"${OUT}/timeline-${tag}.txt"
  "$@"
  date -u +'%T the command returned' >>"${OUT}/timeline-${tag}.txt"
  rolled_out
  date -u +'%T rolled out' >>"${OUT}/timeline-${tag}.txt"
  # A probe that did not end well is a failure of the run, not a missing line: its log is what says why.
  if wait "${probe_pid}"; then
    RESULTS+=("PASS probe-${tag}")
  else
    RESULTS+=("FAIL probe-${tag}")
  fi
  stop_watching
  echo "--- the probe during ${tag}"
  { grep -E 'probe summary|probe [0-9]+ at' "${OUT}/probe-${tag}.log" | sed -e 's/^[[:space:]]*cr_migration_test.go:[0-9]*: //' | cut -c1-400; } || echo "(the probe log has no summary: ${OUT}/probe-${tag}.log)"
  echo "--- timeline of ${tag} (${OUT}/timeline-${tag}.txt)"
  cat "${OUT}/timeline-${tag}.txt"
}

echo "outputs in ${OUT}"
RESTORE=1
echo "==> the release on the ConfigMap route, with the scenario's config (the chart makes the ConfigMap; the tier's own goes)"
"${K[@]}" -n agent-kourier delete configmap agent-kourier-config --ignore-not-found >/dev/null
upgrade
rolled_out
"${K[@]}" -n agent-kourier get configmap agent-kourier-config >/dev/null

echo "==> a plain rollout on the ConfigMap route, for what any restart costs (the baseline)"
move files-restart "${K[@]}" -n agent-kourier rollout restart deploy/agent-kourier

phase open-1

echo "==> step 1 of the move: the CRDs, the route still files"
upgrade --set crds.install=true
"${K[@]}" wait --for=condition=Established --timeout=60s crd/bindings.agentkourier.dev crd/agentbackends.agentkourier.dev crd/chatconnections.agentkourier.dev >/dev/null
rolled_out

echo "==> step 2 of the move: config.source=crds"
move to-crds upgrade -f "${HERE}/crds-values.yaml"
echo "Bindings now: $("${K[@]}" get akbinding -A --no-headers | wc -l | tr -d ' ') (kubectl get akbinding -A)"
"${K[@]}" get akbinding,akbackend,akconnection -A

phase finish-1
phase open-2

echo "==> the way back: config.source=files, the same values (crds.install stays on)"
move to-files upgrade --set crds.install=true

phase finish-2

# The way back again, with a rollout that is slow to start: on kind the new pod is ready in about three seconds, so the old pods, which read the custom
# resources Helm has just deleted, are gone before the config is read again (the Reloader looks every ten seconds). A real cluster pulls an image or
# waits for a probe. The Deployment is paused for 45 seconds after the upgrade, so that the old pods run for that long with the custom resources gone,
# as they would behind a new pod that takes that long.
slow_way_back() {
  "${K[@]}" -n agent-kourier rollout pause deploy/agent-kourier >/dev/null
  upgrade --set crds.install=true
  date -u +'%T the Deployment is paused for 45s, with the custom resources deleted' >>"${OUT}/timeline-to-files-slow.txt"
  sleep 45
  "${K[@]}" -n agent-kourier rollout resume deploy/agent-kourier >/dev/null
}
echo "==> onto the custom resources again, for a slow way back"
upgrade -f "${HERE}/crds-values.yaml"
rolled_out
phase open-3
echo "==> the way back, with a rollout that waits 45 seconds"
move to-files-slow slow_way_back
phase finish-3

# And the way forward, held the same way: the chart deletes the ConfigMap that the old pods have mounted, and the pods go on running behind a slow new one.
slow_way_forward() {
  "${K[@]}" -n agent-kourier rollout pause deploy/agent-kourier >/dev/null
  upgrade -f "${HERE}/crds-values.yaml"
  date -u +'%T the Deployment is paused for 45s, with the ConfigMap deleted' >>"${OUT}/timeline-to-crds-slow.txt"
  sleep 45
  "${K[@]}" -n agent-kourier rollout resume deploy/agent-kourier >/dev/null
}
phase open-4
echo "==> the way forward, with a rollout that waits 45 seconds"
move to-crds-slow slow_way_forward
phase finish-4

# The destructive order, which no hold above runs: the first task ends and a new reply is written while the old pods run without the custom resources.
# The old pods have seen the Bindings go (a line of the log says so, within the reload's ten seconds) when the first gate is released: the turn ends in a
# pod that has lost its Binding, and the reply queued behind it is next. Then a new reply is written in the thread. After the rollout, settle-5 wants all
# three answered once. If the old pods hold what the custom resources had (the broker's own connection is held, and its Bindings with it), the line is not
# in their log and the phases pass; if they do not, the thread says the agent is no longer set up, and settle-5 fails with it.
hold_way_back() {
  local seen=no _
  "${K[@]}" -n agent-kourier rollout pause deploy/agent-kourier >/dev/null
  upgrade --set crds.install=true
  for _ in $(seq 20); do
    if grep -qs 'stopped verifying a Binding' "${OUT}"/log-to-files-hold-*.txt; then
      seen=yes
      break
    fi
    sleep 2
  done
  echo "the old pods said they stopped verifying a Binding, within 40s of the upgrade: ${seen}"
  phase release-5
  phase reply-5
  "${K[@]}" -n agent-kourier rollout resume deploy/agent-kourier >/dev/null
}
echo "==> onto the custom resources again, for the destructive order"
upgrade -f "${HERE}/crds-values.yaml"
rolled_out
phase open-5
echo "==> the way back, held while the first task ends and a new reply is written"
observe to-files-hold
hold_way_back
rolled_out
sleep 15 # the new leader has taken over and read its config
stop_watching
phase settle-5

echo "==> what the pods that were there before each move said (lines about gone, removed, notices, a reload, a refusal or the lease), by pod"
for f in "${OUT}"/log-*.txt; do
  echo "--- $(basename "${f}")"
  grep -iE 'gone|removed|notice|shutting|signal|another replica|reloaded|refused|abandon|dropp' "${f}" | grep -v 'condition is false' | cut -c1-300 | head -15 || true
done

echo "==> results"
printf '%s\n' "${RESULTS[@]}"
case " ${RESULTS[*]} " in
  *" FAIL "*) exit 1 ;;
esac

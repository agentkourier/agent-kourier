#!/usr/bin/env bash
# The scenarios of a broker that serves two Slack apps (make e2e-two-apps): every MC_ test in a Job of its own, and then the one that needs Helm, which the E2E
# Job (a distroless image with no tools) does not have: a team taken off config.connectionNamespaces, and put back. The MC_ tests are not in the Job of make
# e2e-fast: with the failover cases they would bring its run close to the Job's time limit. The broker reads the admission list at start, so each change of it
# is a Helm upgrade and a rollout. The fast tier must be up with two Slack apps on custom resources:
#   E2E_TWO_APPS=1 E2E_CONFIG_SOURCE=crds E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up
# (one replica on SQLite runs it too: the record of what the broker served is in its store either way).
#
# The steps:
#   0. every test whose name starts with TestMC_, as one Job (E2E_RUN=^TestMC_), on the tier as it was brought up. The admission test skips in it, with no phase.
# The next two are each an upgrade of release agent-kourier with the tier's values and the overlays of the tier's bring-up:
#   1. config.connectionNamespaces without team-a. team-a's ChatConnection is still in the cluster and its Binding must say it is not served, while the
#      platform's connection goes on.                                                                                   (E2E_PHASE removed)
#   2. config.connectionNamespaces as the tier has it (team-a, team-b, team-c): team-a is served again.                    (restored)
# Each phase is the E2E test TestMC_ATeamTakenOffTheAdmissionListIsNotServed (hack/kind/e2e-run.sh with E2E_PHASE). A step that fails does not stop the run.
# On any exit the tier is put back on the full list (the list is the tier's own, hack/kind/two-apps-values.yaml); TWO_APPS_KEEP=1 leaves the release as it is, for
# a look. Everything the phases print goes to TWO_APPS_OUT (a new temporary directory by default); no token or Secret value is in it.
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
OUT=${TWO_APPS_OUT:-$(mktemp -d)}
mkdir -p "${OUT}"

for tool in kind kubectl helm; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
require_helm4
cluster_exists || die "kind cluster ${CLUSTER} does not exist; bring it up with: E2E_TWO_APPS=1 E2E_CONFIG_SOURCE=crds E2E_STORE=postgres E2E_REPLICAS=2 make e2e-fast-up"
assert_owned
assert_context fast

env_of() { "${K[@]}" -n agent-kourier get deploy/agent-kourier -o "jsonpath={.spec.template.spec.containers[0].env[?(@.name==\"$1\")].value}"; }
case ",$(env_of AGENTKOURIER_CONNECTION_NAMESPACES)," in
  *,team-a,*) ;;
  *) die "the tier does not admit team-a's namespace: bring it up with E2E_TWO_APPS=1 E2E_CONFIG_SOURCE=crds make e2e-fast-up (this run takes team-a off the list, and a tier that is already without it shows nothing)" ;;
esac
[ -n "$(env_of AGENTKOURIER_CONFIG_SOURCE)" ] || die "the tier is on the ConfigMap route: bring it up on custom resources with E2E_TWO_APPS=1 E2E_CONFIG_SOURCE=crds make e2e-fast-up"
replicas=$("${K[@]}" -n agent-kourier get deploy/agent-kourier -o 'jsonpath={.spec.replicas}')
store=$(env_of AGENTKOURIER_STORE)

# upgrade [HELM ARG...]: release agent-kourier, with the values the tier was brought up with (its store and replicas, the custom resources route and the
# two apps' admission list) and the further arguments. Does not wait.
upgrade() {
  local base=${CHART}/ci/fast-values.yaml more=()
  if [ "${store}" = postgres ]; then
    base=${CHART}/ci/fast-postgres-values.yaml
    [ "${replicas}" -lt 2 ] || more=(-f "${CHART}/ci/fast-postgres-ha-values.yaml")
  fi
  helm_install_agentkourier agent-kourier "${base}" ${more[@]+"${more[@]}"} -f "${HERE}/crds-values.yaml" -f "${HERE}/two-apps-values.yaml" "$@"
}
rolled_out() { "${K[@]}" -n agent-kourier rollout status deploy/agent-kourier --timeout=300s; }

RESULTS=()
# run NAME RUN PHASE WANT: the E2E tests that RUN selects, in the tier's runner, with the phase PHASE (empty for none). A failure is noted and the run goes on. An
# E2E_RUN that matches no test (a renamed one) passes with no test run, so a run counts only when its log shows the test WANT passed.
run() {
  local name=$1 select=$2 phase=$3 want=$4
  echo "==> ${name}"
  if E2E_PHASE=${phase} E2E_RUN=${select} "${HERE}/e2e-run.sh" fast >"${OUT}/${name}.log" 2>&1 &&
    grep -q -- "^--- PASS: ${want}" "${OUT}/${name}.log"; then
    RESULTS+=("PASS ${name}")
  else
    RESULTS+=("FAIL ${name}")
  fi
  grep -E '^[[:space:]]+(mc[a-z_0-9]*|wait|threads)\.go|^--- |^(ok|FAIL|PASS)|^e2e fast' "${OUT}/${name}.log" | cut -c1-600 || true
}
# phase NAME: the admission test in that phase.
phase() { run "phase-$1" '^TestMC_ATeamTakenOffTheAdmissionListIsNotServed$' "$1" TestMC_ATeamTakenOffTheAdmissionListIsNotServed; }

RESTORE=
cleanup() {
  local rc=$?
  if [ -n "${RESTORE}" ] && [ "${TWO_APPS_KEEP:-}" != 1 ]; then
    echo "==> the tier back on the full admission list"
    { upgrade && rolled_out; } >"${OUT}/restore.log" 2>&1 || {
      echo "error: putting the admission list back failed; see ${OUT}/restore.log (hack/kind/down.sh fast and a bring-up is the way out)" >&2
      [ "${rc}" -ne 0 ] || rc=1
    }
  fi
  exit "${rc}"
}
trap cleanup EXIT

echo "outputs in ${OUT}"
RESTORE=1
run all-mc '^TestMC_' '' TestMC_BothBindingsAnswerThroughTheirOwnApp
echo "==> step 1: team-a taken off config.connectionNamespaces"
upgrade --set 'config.connectionNamespaces={team-b,team-c}'
rolled_out
[ "$(env_of AGENTKOURIER_CONNECTION_NAMESPACES)" = team-b,team-c ] || die "the Deployment's admission list is '$(env_of AGENTKOURIER_CONNECTION_NAMESPACES)' after the upgrade; want team-b,team-c"
phase removed

echo "==> step 2: team-a put back"
upgrade
rolled_out
case ",$(env_of AGENTKOURIER_CONNECTION_NAMESPACES)," in *,team-a,*) ;; *) die "the Deployment's admission list is '$(env_of AGENTKOURIER_CONNECTION_NAMESPACES)' after the upgrade; want team-a on it" ;; esac
phase restored

echo "==> results"
printf '%s\n' "${RESULTS[@]}"
case " ${RESULTS[*]} " in
  *" FAIL "*) exit 1 ;;
esac

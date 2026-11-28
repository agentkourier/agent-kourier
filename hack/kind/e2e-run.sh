#!/usr/bin/env bash
# Run the E2E tests in a tier's cluster: cross-build the test binary (go test -c -tags e2e), put it
# in the pinned distroless image, load that into the kind cluster, and run it as a Job in namespace
# e2e (hack/kind/e2e/runner.yaml). The tests then reach the test doubles by their ClusterIP Service
# names, and nothing is published to the host: the fake Slack's /_control/ has no authentication.
# The Job's logs are printed, the Job is deleted, and the exit status is the test result.
# Usage: e2e-run.sh fast|full|ax. E2E_RUN=REGEXP runs only the tests whose names match it (go test -run; the Job's own pattern, the
# whole suite, when unset), to run one scenario again. E2E_PHASE=NAME is the part of a scenario that a script runs between steps of its own
# (hack/kind/crs-migration.sh: a Helm upgrade in the middle of a session, and hack/kind/two-apps.sh: a change of the admission list); the scenarios that take one skip when it is empty. The tier must be up (make e2e-fast-up or e2e-full-up); both tiers
# have the same doubles in namespaces e2e and monitoring, so the same tests run in both.
#
# Every kubectl call names --context kind-agent-kourier-<tier> and every kind call names
# --name agent-kourier-<tier>; assert_context aborts unless the context is that local kind cluster.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
tier=${1:-}
use_tier "${tier}"
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
IMAGE=agent-kourier-e2e/runner:dev
JOB=e2e-runner
# How long to wait for the Job, by the host's clock (wait_job, lib.sh). The test binary's own timeout, 20 minutes for the scenarios, which
# roll Agent Kourier out and wait on it, is shorter, so that a test that hangs fails with a stack before this wait ends; the Job's own
# activeDeadlineSeconds (1500) is the backstop. hack/kind/wait_test.sh holds the three in that order. A host that sleeps most likely
# stops the node, and the binary's clock with it, but not this wait: wait_job reports such a pause.
WAIT_SECONDS=${E2E_WAIT_SECONDS:-1320}
[[ "${WAIT_SECONDS}" =~ ^[0-9]+$ ]] || die "E2E_WAIT_SECONDS must be a whole number of seconds"
# E2E_RUN goes into the Job's YAML as a string, and into a sed command: letters, digits and what a Go regexp of test names needs, and no
# quote, backslash, space or other character that either would read.
[[ -z "${E2E_RUN:-}" || "${E2E_RUN}" =~ ^[A-Za-z0-9_.*+?^$|()/-]+$ ]] || die "E2E_RUN must be a Go regexp of letters, digits and _.*+?^\$|()/- only (a / separates the pattern of a subtest)"

# E2E_PHASE goes into the Job's YAML and a sed command too: lower-case letters, digits and hyphens only.
[[ "${E2E_PHASE:-}" =~ ^[a-z0-9-]*$ ]] || die "E2E_PHASE must be lower-case letters, digits and hyphens only"

for tool in docker kind kubectl go; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"
cluster_exists || die "kind cluster ${CLUSTER} does not exist; bring it up with: make e2e-${tier}-up"
assert_owned
assert_context "${tier}"
refuse_while_sandbox_up
# On any exit: the scratch build directory (made below), the Job and its ConfigMap e2e-admission, and any probe or token pod (the cluster itself is never torn down
# here). Set before the network policy check, whose probe pod a Ctrl-C would otherwise leave behind.
BUILD=
trap '[ -z "${BUILD}" ] || rm -rf "${BUILD}"; "${K[@]}" -n e2e delete job "${JOB}" --ignore-not-found --wait=false >/dev/null 2>&1 || true; "${K[@]}" -n e2e delete configmap e2e-admission --ignore-not-found --wait=false >/dev/null 2>&1 || true; delete_agentkourier_pod "${DEX_TOKEN_POD:-}"; delete_netpol_probes' EXIT
trap 'exit 130' INT TERM
# The full tier's tests lean on its NetworkPolicy, which kindnet stops applying when it loses the API server (lib.sh): refuse
# a run on a tier where it is not live. The fast tier has no kagent and no NetworkPolicy, and the AX tier has no kagent
# controller to fence (its only NetworkPolicy is Substrate's own, which no test relies on), so there is nothing to check.
case "${tier}" in
  full) require_network_policy ;;
  fast) echo "skip: the fast tier has no kagent and no NetworkPolicy; no network policy to check" ;;
  *) echo "skip: the ${tier} tier has no kagent controller to fence; no network policy to check" ;;
esac
# The full tier's agents are reached with the Binding's Dex ID token, which lasts 24 hours: make a fresh one for
# the run, so that a tier that has been up a day does not fail on a token that expired.
if [ "${tier}" = full ]; then
  refresh_dex_token
fi
if [ "${tier}" = ax ]; then
  E2E_RUN=${E2E_RUN:-^TestAX_}
fi

BUILD=$(mktemp -d)

echo "==> build the test binary"
case "$(docker info --format '{{.Architecture}}')" in
  aarch64 | arm64) goarch=arm64 ;;
  x86_64 | amd64) goarch=amd64 ;;
  *) die "unsupported Docker architecture" ;;
esac
GOOS=linux GOARCH=${goarch} CGO_ENABLED=0 go test -C "${ROOT}" -c -tags e2e -o "${BUILD}/e2e.test" ./test/e2e
docker build -q -f "${HERE}/e2e/Dockerfile" --build-arg "RUNTIME_IMAGE=${RUNTIME_IMAGE}" -t "${IMAGE}" "${BUILD}" >/dev/null
kind load docker-image "${IMAGE}" --name "${CLUSTER}"

echo "==> run the Job in ${CTX}"
# A Job's pod template cannot change, so a leftover Job from an interrupted run goes first.
"${K[@]}" -n e2e delete job "${JOB}" --ignore-not-found >/dev/null
# The runner's Role in Agent Kourier's namespace needs the namespace to exist.
"${K[@]}" get namespace agent-kourier >/dev/null 2>&1 || "${K[@]}" create namespace agent-kourier >/dev/null
# So does its Role in e2e-team, the empty namespace where the custom-resource scenarios play a team (hack/kind/e2e/runner.yaml).
"${K[@]}" get namespace e2e-team >/dev/null 2>&1 || "${K[@]}" create namespace e2e-team >/dev/null
# So do its Roles in team-a, team-b and team-c, the namespaces of the scenarios that bring a Slack app of their own (the MC_ tests, E2E_TWO_APPS=1 in up-fast.sh).
for ns in team-a team-b team-c; do
  "${K[@]}" get namespace "${ns}" >/dev/null 2>&1 || "${K[@]}" create namespace "${ns}" >/dev/null
done
# Where Agent Kourier reads its config from now: the Deployment says (the chart sets AGENTKOURIER_CONFIG_SOURCE only for crds), not the tier's bring-up,
# so a tier moved between the two in place is run as it is. The CR_ and WH_ tests skip unless it is crds.
config_source=$("${K[@]}" -n agent-kourier get deploy/agent-kourier -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="AGENTKOURIER_CONFIG_SOURCE")].value}' --ignore-not-found) || die "could not read the Deployment agent-kourier to see where it reads its config from; a run that guessed files would skip every CR_ test and pass"
config_source=${config_source:-files}
case "${config_source}" in
  files | crds) ;;
  *) die "the Deployment's AGENTKOURIER_CONFIG_SOURCE is '${config_source}'; want files or crds" ;;
esac
echo "config source: ${config_source}"
# How the fake Slack sends the broker its events and presses now: its Deployment says (the args that lib.sh's fake_slack_mode_patch sets in http
# mode), not the tier's bring-up, as for the config source. A fake Slack in http mode needs the broker's chat listener: a tier that is half moved
# is refused rather than run to a timeout. A tier with no fake Slack reads as socket.
fake_slack_args=$("${K[@]}" -n e2e get deploy/fakeslack -o 'jsonpath={.spec.template.spec.containers[0].args}' --ignore-not-found) || die "could not read the Deployment fakeslack to see how it sends the broker its events"
case "${fake_slack_args}" in
  *-request-url*) slack_mode=http ;;
  *) slack_mode=socket ;;
esac
if [ "${slack_mode}" = http ]; then
  chat_listen=$("${K[@]}" -n agent-kourier get deploy/agent-kourier -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="AGENTKOURIER_CHAT_LISTEN")].value}' --ignore-not-found) || die "could not read the Deployment agent-kourier"
  [ -n "${chat_listen}" ] || die "the fake Slack is in http mode but Agent Kourier has no chat listener; bring the tier up again with one E2E_SLACK_MODE"
fi
echo "slack mode: ${slack_mode}"
# The broker's admission, as its Deployment says (the chart sets AGENTKOURIER_CHAT_CONNECTION, AGENTKOURIER_CONNECTION_NAMESPACES and
# AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR only when each is set), and the labels of the namespaces the MC_ tests ask about: the ConfigMap e2e-admission,
# which the Job's variables read (lib.sh's runner_admission). The MC_ tests skip unless it admits team-a. A Deployment that cannot be read was read above.
runner_admission "${BUILD}/admission"
# The scenarios release their gates (Stub.Release) where the tier holds them: the stub agent in the fast tier, and
# the scenario model in the full one, which has the stub's release path.
case "${tier}" in
  fast) gate_url=http://stubagent.e2e.svc.cluster.local:8080 ;;
  full) gate_url=http://scenariollm.kagent.svc.cluster.local:8080 ;;
  ax) gate_url=http://stubagent.e2e.svc.cluster.local:8080 ;;
esac
sed -e "s/@TIER@/${tier}/" -e "s|@STUB_URL@|${gate_url}|" -e "s/@CONFIG_SOURCE@/${config_source}/" -e "s/@PHASE@/${E2E_PHASE:-}/" -e "s/@SLACK_MODE@/${slack_mode}/" "${HERE}/e2e/runner.yaml" |
  { if [ -n "${E2E_RUN:-}" ]; then sed -e "s#-test.run=[^\"]*#-test.run=${E2E_RUN}#"; else cat; fi; } |
  "${K[@]}" apply -f - >/dev/null # the tier is fast or full: use_tier checked it
wait_job "${JOB}" "${WAIT_SECONDS}"
result=${JOB_RESULT}

echo "==> Job logs"
"${K[@]}" -n e2e logs "job/${JOB}" ||
  "${K[@]}" -n e2e describe pods -l "job-name=${JOB}" >&2 || true
case "${result}" in
  0) echo "e2e ${tier}: passed in ${SECONDS}s" ;;
  124) echo "e2e ${tier}: timed out after ${WAIT_SECONDS}s" >&2 ;;
  *) echo "e2e ${tier}: FAILED" >&2 ;;
esac
[ "${HOST_PAUSED}" -eq 0 ] || echo "e2e ${tier}: the wait did not run for ${HOST_PAUSED}s in all (the host slept or froze), and most likely the tests with it: a timeout, or a failure of the test that was running, may be the pause's and not the tests'; run again with the host awake (on power, lid open)" >&2
exit "${result}"

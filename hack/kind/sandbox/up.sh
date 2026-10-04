#!/usr/bin/env bash
# Bring up the sandbox tier on the full tier's cluster (kind-courier-full; never a new cluster): courier on the
# production image against a real Slack workspace (Socket Mode), answered by sandbox-sre, a read-only SRE agent on a real
# model that investigates faults seeded in namespace shop. hack/kind/sandbox/README.md is the guide.
#
# The secrets come from the file $SANDBOX_ENV (default ~/.config/agent-courier/sandbox.env), which is checked first, before
# anything else is touched, and parsed, never sourced (env.sh). Their values go into Secret manifests that are piped to
# `kubectl apply -f -`: on no command line, in no file, in no output.
# Brings the full tier up first if it is not (up-full.sh), replaces its courier with the sandbox's (down.sh puts it back),
# and is safe to re-run. It also restarts the full tier's egress gateway if its certificate is stale (heal.sh; `make sandbox-heal`).
# Exit 0: up, and courier is connected to Slack. 1: a refused secrets file, or something failed. 3: deployed and verified, but
# Slack rejected the tokens (courier exited 1, fatal auth), so courier is not connected.
set -eEuo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd) # hack/kind, which lib.sh's functions expect
ROOT=$(cd "${HERE}/../.." && pwd) # build_courier_image (lib.sh) builds from it
SBX=${HERE}/sandbox
# shellcheck source-path=SCRIPTDIR source=../lib.sh
. "${HERE}/lib.sh"
# shellcheck source-path=SCRIPTDIR source=env.sh
. "${SBX}/env.sh"
# shellcheck source-path=SCRIPTDIR source=heal.sh
. "${SBX}/heal.sh"
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")
TOOLS_OCI=oci://ghcr.io/kagent-dev/tools/helm/kagent-tools
TOOLS_VERSION=0.3.0 # the version kagent 1.0.0-alpha5 depends on; the image is pinned in tools-values.yaml
TOOLS_SA=system:serviceaccount:kagent:sandbox-tools
SANDBOX_ENV=${SANDBOX_ENV:-${HOME:?HOME is not set}/.config/agent-courier/sandbox.env}

STEP=preflight
step() {
  STEP=$*
  echo "==> $*"
}
on_error() {
  echo "error: step '${STEP}' failed (line $1). Safe to re-run: make sandbox-up; to take the sandbox away: make sandbox-down" >&2
}
trap 'on_error $LINENO' ERR
SCRATCH=
trap '[ -z "${SCRATCH}" ] || rm -rf "${SCRATCH}"' EXIT

# The secrets file comes first: a wrong one is refused before the cluster, Docker or any tool is asked anything.
step "secrets file ${SANDBOX_ENV}"
sandbox_env_load "${SANDBOX_ENV}"
echo "ok: ${SANDBOX_ENV_FOUND// /, } are there and well-formed (values are never printed)"

for tool in docker kind kubectl helm jq; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"

# ---------------------------------------------------------------------------------------------------------------------
step "the full tier (kind cluster ${CLUSTER})"
if cluster_exists; then
  assert_owned
  echo "the full tier's cluster exists; reusing it"
else
  "${HERE}/up-full.sh"
fi
assert_context full
"${K[@]}" -n kagent get deploy kagent-controller >/dev/null 2>&1 ||
  die "the full tier is not complete (no kagent controller in ${CTX}); bring it up with: make e2e-full-up"
"${K[@]}" -n kagent rollout status deploy/kagent-controller --timeout=120s >/dev/null

# The egress gateway reads its 24-hour certificate once, so a tier that has been up a day turns every actor away (heal.sh).
step "the egress gateway's certificate"
heal_egress_gateway

step "the production courier image"
build_courier_image
# The tiers run the testhooks image; this tier runs the production one, which must not be that.
[ "$(docker image inspect --format "{{index .Config.Labels \"${TESTHOOKS_LABEL}\"}}" "${COURIER_IMAGE}")" != true ] ||
  die "${COURIER_IMAGE} carries the ${TESTHOOKS_LABEL} label: it is not the production image"
kind load docker-image "${COURIER_IMAGE}" --name "${CLUSTER}"

step "seeded faults in namespace shop"
"${K[@]}" apply -f "${SBX}/faults.yaml" >/dev/null

step "read-only tool server (kagent-tools ${TOOLS_VERSION}, release sandbox-tools): the view ClusterRole, --read-only, the k8s tools"
"${K[@]}" apply -f "${SBX}/tools-rbac.yaml" >/dev/null
"${H[@]}" upgrade --install sandbox-tools "${TOOLS_OCI}" --version "${TOOLS_VERSION}" --namespace kagent \
  -f "${SBX}/tools-values.yaml" --wait --timeout 4m >/dev/null

step "sandbox-sre: the model's API key, ModelConfig (${MODEL_PROVIDER}), tool server, agent"
if [ "${MODEL_PROVIDER}" = bedrock ]; then
  # kagent's Bedrock provider reads a bearer token from the Secret key of this fixed name, and its egress gateway sends it as
  # Authorization: Bearer; the key is not named in the ModelConfig.
  apply_secret kagent sandbox-model AWS_BEARER_TOKEN_BEDROCK=MODEL_API_KEY
else
  apply_secret kagent sandbox-model apiKey=MODEL_API_KEY
fi
render "${SBX}/modelconfig-${MODEL_PROVIDER}.yaml" | "${K[@]}" apply -f - >/dev/null
"${K[@]}" apply -f "${SBX}/agent.yaml" >/dev/null
# The agent only after kagent has discovered the tool server's tools (the full tier's scenario agents wait the same way).
"${K[@]}" -n kagent wait --for=condition=Accepted remotemcpserver/sandbox-tools modelconfig/sandbox-model --timeout=180s
"${K[@]}" -n kagent wait --for=condition=Ready agent/sandbox-sre --timeout=300s

step "courier: Slack Secret, the Binding's Dex token and its refresh, config, Deployment"
"${K[@]}" create namespace courier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
apply_secret courier sandbox-slack botToken=SLACK_BOT_TOKEN appToken=SLACK_APP_TOKEN
"${K[@]}" apply -f "${SBX}/token-refresh.yaml" >/dev/null
# The first token is made by the CronJob's own script, as a Job from it, so that the one path that keeps the token fresh is
# the one that made it and is proved now, not hours into a session; and no throwaway pod is left with the token in its log.
"${K[@]}" -n courier delete job courier-token-refresh-bringup --ignore-not-found >/dev/null
"${K[@]}" -n courier create job courier-token-refresh-bringup --from=cronjob/courier-token-refresh >/dev/null
if ! "${K[@]}" -n courier wait --for=condition=Complete job/courier-token-refresh-bringup --timeout=120s >/dev/null; then
  echo "error: the token refresh Job did not complete (is Dex up? does the Secret courier-agent-token exist?). Its log:" >&2
  "${K[@]}" -n courier logs job/courier-token-refresh-bringup --tail=10 >&2 || true
  "${K[@]}" -n courier delete job courier-token-refresh-bringup --ignore-not-found --wait=false >/dev/null || true
  die "no Binding token"
fi
"${K[@]}" -n courier delete job courier-token-refresh-bringup >/dev/null
SCRATCH=$(mktemp -d)
for f in "${SBX}"/courier/config/*.yaml; do render "${f}" >"${SCRATCH}/$(basename "${f}")"; done
cp "${HERE}/full/courier/agentbackend.yaml" "${SCRATCH}/agentbackend.yaml" # the verifying front door
"${K[@]}" -n courier create configmap courier-sandbox-config --from-file="${SCRATCH}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
# One courier at a time: the full tier's own goes to zero (down.sh puts it back), and the sandbox's starts.
"${K[@]}" -n courier scale deploy/courier --replicas=0 >/dev/null
"${K[@]}" -n courier wait --for=delete pod -l 'app.kubernetes.io/name=courier,!app.kubernetes.io/instance' --timeout=90s >/dev/null
"${K[@]}" apply -f "${SBX}/courier/deployment.yaml" >/dev/null
"${K[@]}" -n courier rollout restart deploy/courier-sandbox >/dev/null

# ---------------------------------------------------------------------------------------------------------------------
step "verify"
failed=0
check() { # DESCRIPTION COMMAND...: "ok: ..." or "FAIL: ..." (and the exit status is non-zero at the end)
  local d=$1
  shift
  if "$@" >/dev/null 2>&1; then echo "ok: ${d}"; else echo "FAIL: ${d}" >&2; failed=1; fi
}
cannot() { [ "$("${K[@]}" auth can-i "$1" "$2" -n "$3" --as="${TOOLS_SA}" 2>/dev/null || true)" = no ]; } # VERB RESOURCE NAMESPACE
can() { [ "$("${K[@]}" auth can-i "$1" "$2" -n "$3" --as="${TOOLS_SA}" 2>/dev/null || true)" = yes ]; }

# The tools' ServiceAccount reads and cannot write, and cannot read a Secret anywhere.
check "the tool server can read pods in shop (so the refusals below mean something)" can get pods shop
check "the tool server can read pod logs in shop" can get pods/log shop
check "the tool server cannot create pods" cannot create pods shop
check "the tool server cannot delete pods" cannot delete pods shop
check "the tool server cannot delete deployments" cannot delete deployments shop
check "the tool server cannot patch deployments" cannot patch deployments shop
check "the tool server cannot exec into a pod" cannot create pods/exec shop
check "the tool server cannot get Secrets in shop" cannot get secrets shop
check "the tool server cannot get Secrets in courier" cannot get secrets courier
check "the tool server cannot list Secrets in kagent" cannot list secrets kagent

# shop_state: one line per app of namespace shop, from its pod, and inventory's endpoint count, for the check below.
shop_state() {
  local pods eps
  pods=$("${K[@]}" -n shop get pods -o json)
  eps=$("${K[@]}" -n shop get endpointslices -l kubernetes.io/service-name=inventory -o json)
  jq -r --argjson eps "${eps}" '
    .items[] | .metadata.labels.app as $app | .status.containerStatuses[0] as $c
    | "\($app) ready=\($c.ready) restarts=\($c.restartCount) waiting=\($c.state.waiting.reason // "-") last=\($c.lastState.terminated.reason // "-") now=\($c.state.terminated.reason // "-") running=\($c.state.running != null)",
      (if $app == "inventory" then "inventory-endpoints=\([$eps.items[].endpoints[]?] | length)" else empty end)' <<<"${pods}"
}
faults_in_place() { # every fault is in its state, and the healthy app is fine
  local s
  s=$(shop_state)
  grep -q '^checkout .*restarts=[1-9]' <<<"${s}" &&
    grep -Eq '^catalog .*waiting=(ErrImagePull|ImagePullBackOff)' <<<"${s}" &&
    grep -Eq '^recommender .*(last|now)=OOMKilled' <<<"${s}" &&
    grep -q '^inventory ready=true' <<<"${s}" && grep -q '^inventory-endpoints=0' <<<"${s}" &&
    grep -q '^search ready=false restarts=0 .*running=true' <<<"${s}" &&
    grep -q '^frontend ready=true' <<<"${s}"
}
deadline=$((SECONDS + 240))
until faults_in_place; do
  [ "${SECONDS}" -lt "${deadline}" ] || break
  sleep 5
done
if faults_in_place; then
  echo "ok: the seeded faults are in place: checkout crash loops, catalog cannot pull its image, recommender is OOM killed, inventory's Service has no endpoints, search is not ready, frontend is healthy"
else
  echo "FAIL: the seeded faults are not all in their states:" >&2
  shop_state >&2
  failed=1
fi

check "the agent sandbox-sre is Accepted" "${K[@]}" -n kagent wait --for=condition=Accepted agent/sandbox-sre --timeout=5s
check "the agent sandbox-sre is Ready" "${K[@]}" -n kagent wait --for=condition=Ready agent/sandbox-sre --timeout=5s

# Courier: the pod is Ready as soon as /readyz holds, which is before the Slack connection is tried and says nothing of it (Slack is
# not a readiness check on purpose; internal/broker/health.go). So Ready is not "connected": courier says that itself, with the
# INFO line the Slack adapter logs when Slack says hello over Socket Mode (slack.ConnectedMessage, which a test keeps this to), and
# as courier_chat_connected{...} 1 on /metrics. If Slack rejected the tokens it exits 1 (fatal auth: a restart would be rejected
# again, so the pod crash-loops and says so); any other exit is a failure of the tier.
SLACK_CONNECTED='slack: socket mode connected'
courier_state() { # ready | fatal-auth | exit <code> | starting
  "${K[@]}" -n courier get pods -l app.kubernetes.io/instance=sandbox -o json | jq -r '
    [.items[] | select(.metadata.deletionTimestamp == null)] | sort_by(.metadata.creationTimestamp) | last // empty
    | .status.containerStatuses[0] // empty
    | if .ready then "ready"
      else ((.state.terminated // .lastState.terminated // null) as $t
            | if $t == null then "starting" elif $t.exitCode == 1 then "fatal-auth" else "exit \($t.exitCode)" end) end'
}
slack_connected() { # whether the current container has logged that Socket Mode connected
  local log
  log=$("${K[@]}" -n courier logs deploy/courier-sandbox --tail=1000 2>/dev/null || true)
  [[ ${log} == *"${SLACK_CONNECTED}"* ]]
}
deadline=$((SECONDS + 150))
state=starting
while [ "${SECONDS}" -lt "${deadline}" ]; do
  state=$(courier_state || echo starting)
  case "${state}" in
    ready) if slack_connected; then state=connected; break; fi ;;
    fatal-auth | exit*) break ;;
  esac
  sleep 3
done
case "${state}" in
  connected) echo "ok: courier is connected to Slack (it logged \"${SLACK_CONNECTED}\": auth.test passed and Socket Mode said hello)" ;;
  fatal-auth)
    echo "error: courier exited 1, which is fatal auth: Slack rejected the tokens, and a restart would be rejected again." >&2
    echo "         Fix SLACK_BOT_TOKEN and SLACK_APP_TOKEN in ${SANDBOX_ENV} and re-run make sandbox-up. Its log:" >&2
    "${K[@]}" -n courier logs deploy/courier-sandbox --tail=6 2>&1 | sed 's/^/         /' >&2 || true
    ;;
  ready)
    echo "FAIL: courier is Ready but never connected to Slack (no \"${SLACK_CONNECTED}\" line in 150 s). Its Slack lines:" >&2
    "${K[@]}" -n courier logs deploy/courier-sandbox --tail=1000 2>&1 | grep 'slack' | tail -10 | sed 's/^/         /' >&2 || true
    echo "         'connection attempt failed' lines mean courier cannot reach Slack (egress, DNS or a proxy); see 'Where to look' in hack/kind/sandbox/README.md." >&2
    failed=1
    ;;
  *)
    echo "FAIL: courier is not up (${state}). Its log:" >&2
    "${K[@]}" -n courier logs deploy/courier-sandbox --tail=10 2>&1 | sed 's/^/         /' >&2 || true
    failed=1
    ;;
esac

docker stats --no-stream --format 'node cpu={{.CPUPerc}} mem={{.MemUsage}}' "$(kind get nodes --name "${CLUSTER}")"
[ "${failed}" -eq 0 ] || die "the sandbox is not as it should be (see FAIL above)"
if [ "${state}" = connected ]; then
  echo "sandbox up in ${SECONDS}s (context ${CTX}): @-mention the bot in the channel; the dogfood checklist is hack/kind/sandbox/README.md"
else
  # Exit 3 is "deployed, but Slack rejected the tokens": all else is up and verified, and courier crash-loops until the file is fixed.
  echo "sandbox deployed in ${SECONDS}s, but Slack rejected the tokens, so courier is NOT connected (exit 3; see the error above)" >&2
  exit 3
fi

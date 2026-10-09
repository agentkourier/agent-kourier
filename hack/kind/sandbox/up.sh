#!/usr/bin/env bash
# Bring up the sandbox tier on the full tier's cluster (kind-agent-kourier-full; never a new cluster): Agent Kourier on the
# production image against a real Slack workspace (Socket Mode), answered by sandbox-sre, a read-only SRE agent on a real
# model that investigates faults seeded in namespace shop. hack/kind/sandbox/README.md is the guide.
#
# The secrets come from the file $SANDBOX_ENV (default ~/.config/agent-kourier/sandbox.env), which is checked first, before
# anything else is touched, and parsed, never sourced (env.sh). Their values go into Secret manifests that are piped to
# `kubectl apply -f -`: on no command line, in no file, in no output.
# Optionally, it also makes the full tier's Alertmanager the sandbox's alert source: posting, through a copy of the owner's receiver, to
# the owner's real alert channel, tagged [kind] (alertmanager/, alert.sh). Its webhook URL comes from the file
# $ALERTMANAGER_WEBHOOK_FILE (default ~/.config/agent-kourier/alertmanager-webhook-url). With no such file the alert source is skipped, and
# the run says so; a file that is there is checked second, right after the secrets file (webhook.sh), refused as strictly, and handled as
# the secrets are: parsed, never printed, and only ever piped to `kubectl apply -f -`. Nothing posts until `make sandbox-alert` fires an alert.
# With the alert source, Agent Kourier's config also gets the alert trial's Binding (agent-kourier/alert-trial): it watches that real channel for the [kind]
# alerts, and only them (README, Run the alert trial). Without the webhook file the config is the dogfood Binding's alone, as it was.
# Brings the full tier up first if it is not (up-full.sh), replaces its Agent Kourier with the sandbox's (down.sh puts it back),
# and is safe to re-run. It also restarts the full tier's egress gateway if its certificate is stale, and refuses a tier whose network plugin has stopped applying NetworkPolicy (heal.sh; `make sandbox-heal` heals both).
# Exit 0: up, and Agent Kourier is connected to Slack. 1: a refused secrets file, or something failed. 3: deployed and verified, but
# Slack rejected the tokens (Agent Kourier exited 1, fatal auth), so Agent Kourier is not connected.
set -eEuo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd) # hack/kind, which lib.sh's functions expect
ROOT=$(cd "${HERE}/../.." && pwd) # build_agentkourier_image (lib.sh) builds from it
SBX=${HERE}/sandbox
# shellcheck source-path=SCRIPTDIR source=../lib.sh
. "${HERE}/lib.sh"
# shellcheck source-path=SCRIPTDIR source=env.sh
. "${SBX}/env.sh"
# shellcheck source-path=SCRIPTDIR source=webhook.sh
. "${SBX}/webhook.sh"
# shellcheck source-path=SCRIPTDIR source=heal.sh
. "${SBX}/heal.sh"
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")
TOOLS_OCI=oci://ghcr.io/kagent-dev/tools/helm/kagent-tools
TOOLS_VERSION=0.3.0 # the version kagent 1.0.0-alpha5 depends on; the image is pinned in tools-values.yaml
TOOLS_SA=system:serviceaccount:kagent:sandbox-tools
# The sandbox's Agent Kourier is the chart (charts/agent-kourier, ci/sandbox-values.yaml) as this Helm release, in namespace agent-kourier: the release name
# makes the Deployment, the volume and the CronJob's names, and the pod label app.kubernetes.io/instance, which tells it from the tier's Agent Kourier.
SANDBOX_RELEASE=agent-kourier-sandbox
SANDBOX_ENV=${SANDBOX_ENV:-${HOME:?HOME is not set}/.config/agent-kourier/sandbox.env}
ALERTMANAGER_WEBHOOK_FILE=${ALERTMANAGER_WEBHOOK_FILE:-${HOME}/.config/agent-kourier/alertmanager-webhook-url}

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
trap '[ -z "${SCRATCH}" ] || rm -rf "${SCRATCH}"; delete_netpol_probes' EXIT # the probe of the network policy check leaves no pod
trap 'exit 130' INT TERM

# The secrets file comes first: a wrong one is refused before the cluster, Docker or any tool is asked anything.
step "secrets file ${SANDBOX_ENV}"
sandbox_env_load "${SANDBOX_ENV}"
echo "ok: ${SANDBOX_ENV_FOUND// /, } are there and well-formed (values are never printed)"
# The alert source is optional: without the webhook file it is skipped. A file that is there, a dangling link included, is held to every
# check, and one that fails stops the run here, as the secrets file does.
step "alert webhook file ${ALERTMANAGER_WEBHOOK_FILE}"
if [ -e "${ALERTMANAGER_WEBHOOK_FILE}" ] || [ -L "${ALERTMANAGER_WEBHOOK_FILE}" ]; then
  webhook_url_load "${ALERTMANAGER_WEBHOOK_FILE}"
  alert_channel_check # the trial Binding's channel is a channel ID and not the dogfood channel; checked before anything is touched
  ALERT_SOURCE=1
  echo "ok: ${ALERTMANAGER_WEBHOOK_FILE} holds an incoming-webhook URL (never printed)"
else
  ALERT_SOURCE=0
  echo "alert source: not configured; see README Alert trial"
fi

for tool in docker kind kubectl helm jq; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"
require_helm4

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

# kindnet stops applying NetworkPolicy when it loses the API server, which the verify below would only notice late and obscurely; refuse
# here, saying why (lib.sh). `make sandbox-heal` heals it.
step "the network policy"
require_network_policy

# The egress gateway reads its 24-hour certificate once, so a tier that has been up a day turns every actor away (heal.sh).
step "the egress gateway's certificate"
heal_egress_gateway

step "the production Agent Kourier image"
build_agentkourier_image
# The tiers run the testhooks image; this tier runs the production one, which must not be that.
[ "$(docker image inspect --format "{{index .Config.Labels \"${TESTHOOKS_LABEL}\"}}" "${AGENTKOURIER_IMAGE}")" != true ] ||
  die "${AGENTKOURIER_IMAGE} carries the ${TESTHOOKS_LABEL} label: it is not the production image"
kind load docker-image "${AGENTKOURIER_IMAGE}" --name "${CLUSTER}"

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

# The alert source: Alertmanager's receiver slack-critical-kind, a copy of the owner's slack-critical with a [kind] title prefix, reading
# the webhook URL by api_url_file from a Secret that the Alertmanager mounts. The Secrets first, so that the operator never rolls the
# Alertmanager onto a Secret that is not there; then the three fields of the Alertmanager (overlay.yaml), which the operator rolls out.
if [ "${ALERT_SOURCE}" = 1 ]; then
  step "alert source: Alertmanager's receiver slack-critical-kind (posts to the real alert channel as [kind] once an alert is fired)"
  # shellcheck disable=SC2034 # read by apply_secret (env.sh), by name
  ALERTMANAGER_CONFIG=$(<"${SBX}/alertmanager/config.yaml")
  apply_secret monitoring alertmanager-slack-secret webhook-url=ALERTMANAGER_WEBHOOK_URL
  apply_secret monitoring alertmanager-kind-config alertmanager.yaml=ALERTMANAGER_CONFIG
  "${K[@]}" apply --server-side --force-conflicts --field-manager=agent-kourier-sandbox -f "${SBX}/alertmanager/overlay.yaml" >/dev/null
fi

step "agent-kourier: Slack Secret, the Dex client, config, chart, the Binding's Dex token"
"${K[@]}" create namespace agent-kourier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
apply_secret agent-kourier sandbox-slack botToken=SLACK_BOT_TOKEN appToken=SLACK_APP_TOKEN
# The token refresh's credentials for Dex: the fake client and user of full/dex.yaml (which are public, and the same ones the full tier's
# token exchange and the CronJob always used), in a Secret that the chart's CronJob reads into its environment.
"${K[@]}" apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Secret
metadata: {name: agent-kourier-dex-client, namespace: agent-kourier}
stringData: {clientSecret: fake-agent-kourier-binding-secret, username: "binding-test@example.test", password: password}
EOF
SCRATCH=$(mktemp -d)
render_agentkourier_config "${SCRATCH}" # the Binding, the connection and the verifying front door; with the alert source, the alert trial's Binding too
"${K[@]}" -n agent-kourier create configmap agent-kourier-sandbox-config --from-file="${SCRATCH}" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
# A sandbox brought up before the chart has a hand-made Deployment (its pod carries instance=sandbox), its own token refresh (which the chart's
# replaces, under another name) and the volume, which the chart's release takes over. The Deployment goes first: nothing below waits for a pod
# that it would otherwise leave running.
replace_unowned_deployment "${SANDBOX_RELEASE}"
adopt_into_release "${SANDBOX_RELEASE}" "pvc/${SANDBOX_RELEASE}-data"
"${K[@]}" -n agent-kourier delete cronjob/agent-kourier-token-refresh serviceaccount/agent-kourier-token-refresh role/agent-kourier-token-refresh rolebinding/agent-kourier-token-refresh --ignore-not-found >/dev/null
# The release goes in with its replicas at zero (the chart's kill switch): the CronJob is in place, and nothing is started on the real Slack
# or taken from the full tier's Agent Kourier until the Binding's token is proved to be made.
helm_install_agentkourier "${SANDBOX_RELEASE}" "${ROOT}/charts/agent-kourier/ci/sandbox-values.yaml" --set replicaCount=0
# The first token is made by the CronJob's own script, as a Job from it, so that the one path that keeps the token fresh is
# the one that made it and is proved now, not hours into a session; and no throwaway pod is left with the token in its log.
# (The Secret agent-kourier-agent-token is the full tier's, with a token that may be stale; Agent Kourier reads it on every request, so the Job's
# is what it sends from the next one.)
"${K[@]}" -n agent-kourier delete job agent-kourier-token-refresh-bringup --ignore-not-found >/dev/null
"${K[@]}" -n agent-kourier create job agent-kourier-token-refresh-bringup --from="cronjob/${SANDBOX_RELEASE}-token-refresh" >/dev/null
if ! "${K[@]}" -n agent-kourier wait --for=condition=Complete job/agent-kourier-token-refresh-bringup --timeout=120s >/dev/null; then
  echo "error: the token refresh Job did not complete (is Dex up? do the Secrets agent-kourier-agent-token and agent-kourier-dex-client exist?). Its log:" >&2
  "${K[@]}" -n agent-kourier logs job/agent-kourier-token-refresh-bringup --tail=10 >&2 || true
  "${K[@]}" -n agent-kourier delete job agent-kourier-token-refresh-bringup --ignore-not-found --wait=false >/dev/null || true
  die "no Binding token"
fi
"${K[@]}" -n agent-kourier delete job agent-kourier-token-refresh-bringup >/dev/null
# One Agent Kourier at a time: the full tier's own goes to zero (down.sh puts it back), and the sandbox's starts. The tier's pod is the one that does not
# carry this release's instance label: the hand-made one of a tier brought up before the chart has none at all.
"${K[@]}" -n agent-kourier scale deploy/agent-kourier --replicas=0 >/dev/null
"${K[@]}" -n agent-kourier wait --for=delete pod -l "app.kubernetes.io/name=agent-kourier,app.kubernetes.io/instance!=${SANDBOX_RELEASE}" --timeout=90s >/dev/null
helm_install_agentkourier "${SANDBOX_RELEASE}" "${ROOT}/charts/agent-kourier/ci/sandbox-values.yaml"
# The restart picks up a rebuilt image (same tag) or a changed ConfigMap, for which Helm rolls nothing.
"${K[@]}" -n agent-kourier rollout restart "deploy/${SANDBOX_RELEASE}" >/dev/null

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
check "the tool server cannot get Secrets in agent-kourier" cannot get secrets agent-kourier
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

# The alert source: the Alertmanager that runs is the one the overlay made (it has the externalUrl and the receiver, and the mounted
# webhook file is not empty; the file's content is not read here or anywhere). The operator rolls its pod, so this is waited for.
alert_source_in_place() {
  "${K[@]}" -n monitoring get pod alertmanager-agent-kourier-0 -o json |
    jq -e '.status.containerStatuses | all(.ready)' >/dev/null &&
    "${K[@]}" -n monitoring get pod alertmanager-agent-kourier-0 -o json |
    jq -e '[.spec.containers[] | select(.name == "alertmanager") | .args[]] | index("--web.external-url=http://alertmanager.kind.local") != null' >/dev/null &&
    "${K[@]}" -n monitoring exec alertmanager-agent-kourier-0 -c alertmanager -- wget -q -O - http://localhost:9093/api/v2/status |
    jq -e '.config.original | contains("receiver: slack-critical-kind")' >/dev/null &&
    "${K[@]}" -n monitoring exec alertmanager-agent-kourier-0 -c alertmanager -- test -s /etc/alertmanager/secrets/alertmanager-slack-secret/webhook-url
}
if [ "${ALERT_SOURCE}" = 1 ]; then
  deadline=$((SECONDS + 240))
  until alert_source_in_place >/dev/null 2>&1; do
    [ "${SECONDS}" -lt "${deadline}" ] || break
    sleep 5
  done
  check "Alertmanager is the alert source: externalUrl http://alertmanager.kind.local, receiver slack-critical-kind, the webhook file mounted" alert_source_in_place
fi

check "the agent sandbox-sre is Accepted" "${K[@]}" -n kagent wait --for=condition=Accepted agent/sandbox-sre --timeout=5s
check "the agent sandbox-sre is Ready" "${K[@]}" -n kagent wait --for=condition=Ready agent/sandbox-sre --timeout=5s

# Agent Kourier: the pod is Ready as soon as /readyz holds, which is before the Slack connection is tried and says nothing of it (Slack is
# not a readiness check on purpose; internal/broker/health.go). So Ready is not "connected": Agent Kourier says that itself, with the
# INFO line the Slack adapter logs when Slack says hello over Socket Mode (slack.ConnectedMessage, which a test keeps this to), and
# as agentkourier_chat_connected{...} 1 on /metrics. If Slack rejected the tokens it exits 1 (fatal auth: a restart would be rejected
# again, so the pod crash-loops and says so); any other exit is a failure of the tier.
SLACK_CONNECTED='slack: socket mode connected'
agentkourier_state() { # ready | fatal-auth | exit <code> | starting
  "${K[@]}" -n agent-kourier get pods -l "app.kubernetes.io/instance=${SANDBOX_RELEASE}" -o json | jq -r '
    [.items[] | select(.metadata.deletionTimestamp == null)] | sort_by(.metadata.creationTimestamp) | last // empty
    | .status.containerStatuses[0] // empty
    | if .ready then "ready"
      else ((.state.terminated // .lastState.terminated // null) as $t
            | if $t == null then "starting" elif $t.exitCode == 1 then "fatal-auth" else "exit \($t.exitCode)" end) end'
}
slack_connected() { # whether the current container has logged that Socket Mode connected
  local log
  log=$("${K[@]}" -n agent-kourier logs deploy/"${SANDBOX_RELEASE}" --tail=1000 2>/dev/null || true)
  [[ ${log} == *"${SLACK_CONNECTED}"* ]]
}
deadline=$((SECONDS + 150))
state=starting
while [ "${SECONDS}" -lt "${deadline}" ]; do
  state=$(agentkourier_state || echo starting)
  case "${state}" in
    ready) if slack_connected; then state=connected; break; fi ;;
    fatal-auth | exit*) break ;;
  esac
  sleep 3
done
case "${state}" in
  connected) echo "ok: Agent Kourier is connected to Slack (it logged \"${SLACK_CONNECTED}\": auth.test passed and Socket Mode said hello)" ;;
  fatal-auth)
    echo "error: Agent Kourier exited 1, which is fatal auth: Slack rejected the tokens, and a restart would be rejected again." >&2
    echo "         Fix SLACK_BOT_TOKEN and SLACK_APP_TOKEN in ${SANDBOX_ENV} and re-run make sandbox-up. Its log:" >&2
    "${K[@]}" -n agent-kourier logs deploy/"${SANDBOX_RELEASE}" --tail=6 2>&1 | sed 's/^/         /' >&2 || true
    ;;
  ready)
    echo "FAIL: Agent Kourier is Ready but never connected to Slack (no \"${SLACK_CONNECTED}\" line in 150 s). Its Slack lines:" >&2
    "${K[@]}" -n agent-kourier logs deploy/"${SANDBOX_RELEASE}" --tail=1000 2>&1 | grep 'slack' | tail -10 | sed 's/^/         /' >&2 || true
    echo "         'connection attempt failed' lines mean Agent Kourier cannot reach Slack (egress, DNS or a proxy); see 'Where to look' in hack/kind/sandbox/README.md." >&2
    failed=1
    ;;
  *)
    echo "FAIL: Agent Kourier is not up (${state}). Its log:" >&2
    "${K[@]}" -n agent-kourier logs deploy/"${SANDBOX_RELEASE}" --tail=10 2>&1 | sed 's/^/         /' >&2 || true
    failed=1
    ;;
esac

docker stats --no-stream --format 'node cpu={{.CPUPerc}} mem={{.MemUsage}}' "$(kind get nodes --name "${CLUSTER}")"
[ "${failed}" -eq 0 ] || die "the sandbox is not as it should be (see FAIL above)"
if [ "${state}" = connected ]; then
  echo "sandbox up in ${SECONDS}s (context ${CTX}): @-mention the bot in the channel; the dogfood checklist is hack/kind/sandbox/README.md"
  [ "${ALERT_SOURCE}" != 1 ] || echo "alert source up, and Agent Kourier watches ${ALERT_CHANNEL_ID} for [kind] alerts: fire one with make sandbox-alert (README, Run the alert trial)"
else
  # Exit 3 is "deployed, but Slack rejected the tokens": all else is up and verified, and Agent Kourier crash-loops until the file is fixed.
  echo "sandbox deployed in ${SECONDS}s, but Slack rejected the tokens, so Agent Kourier is NOT connected (exit 3; see the error above)" >&2
  exit 3
fi

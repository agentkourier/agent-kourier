#!/usr/bin/env bash
# Fire or resolve one synthetic alert in the full tier's Alertmanager: alert.sh ALERT [firing|resolved], or
# `make sandbox-alert ALERT=<name> [STATE=firing|resolved]`.
#
# There is no Prometheus in the tier, so the alert is posted straight to Alertmanager's v2 API, from a probe pod in the cluster,
# with the labels and annotations a kube-prometheus rule would give it for one of the faults seeded in namespace shop
# (alertmanager/alerts.json, the sample alerts). With the sandbox up, Alertmanager sends it to your REAL alert channel,
# tagged [kind], through the receiver slack-critical-kind (alertmanager/config.yaml); hack/kind/sandbox/README.md, Alert trial.
# ALERT must be a sample alert: nothing else is ever posted. Every alert it posts carries the label agentkourier_trial="true", which the receiver's
# route requires (alertmanager/config.yaml), so nothing else in the cluster can reach the production channel through it.
#
# A firing alert carries an end an hour away, so that one that is forgotten resolves itself; resolved posts the same alert,
# with the same labels, with an end in the past. Pod names are read from the live pod, so a pod that was replaced between a
# fire and its resolve makes the resolve a different alert: resolve before the pod is replaced, or let the hour pass.
# Prints "sent: <alertname> <state>" and nothing else of the alert. Every kubectl call names --context kind-agent-kourier-full.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd) # hack/kind, which lib.sh's functions expect
SBX=${HERE}/sandbox
# shellcheck source-path=SCRIPTDIR source=../lib.sh
. "${HERE}/lib.sh"
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
SAMPLES=${SBX}/alertmanager/alerts.json
CONFIG_SECRET=alertmanager-kind-config # the Alertmanager's configSecret when sandbox-up has made it the alert source
ALERTMANAGER_API=http://alertmanager.monitoring.svc.cluster.local:9093/api/v2/alerts
FIRING_FOR=3600 # seconds a fired alert stays firing without a resolve

command -v jq >/dev/null || die "jq is required"
alert=${1:-}
state=${2:-firing}
[ -n "${alert}" ] || die "usage: make sandbox-alert ALERT=<name> [STATE=firing|resolved]; ALERT is one of: $(jq -r 'keys | join(" ")' "${SAMPLES}")"
jq -e --arg a "${alert}" 'has($a)' "${SAMPLES}" >/dev/null || die "'${alert}' is not a sample alert; ALERT is one of: $(jq -r 'keys | join(" ")' "${SAMPLES}")"
case "${state}" in firing | resolved) ;; *) die "STATE must be firing or resolved" ;; esac

for tool in kubectl kind; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
assert_context full
[ "$("${K[@]}" -n monitoring get alertmanager agent-kourier -o 'jsonpath={.spec.configSecret}' 2>/dev/null || true)" = "${CONFIG_SECRET}" ] ||
  die "the alert source is not configured: the full tier's Alertmanager is not the sandbox's (its configSecret is not ${CONFIG_SECRET}). Put the webhook URL in ~/.config/agent-kourier/alertmanager-webhook-url and run: make sandbox-up (README, Alert trial)"

# The pod the alert is about, for the sample alerts that name one: the live pod of the fault's app, which the agent can then look at.
pod=
app=$(jq -r --arg a "${alert}" '.[$a].app // empty' "${SAMPLES}")
if [ -n "${app}" ]; then
  # The live pod: not one that is terminating. (kubectl's JSONPath has no null to filter on, so jq does it.)
  pods=$("${K[@]}" -n shop get pods -l "app=${app}" -o json) || die "could not list the ${app} pods in namespace shop"
  pod=$(jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name][0] // empty' <<<"${pods}") ||
    die "could not read the ${app} pods in namespace shop"
  [ -n "${pod}" ] || die "there is no ${app} pod in namespace shop; bring the sandbox up first: make sandbox-up"
fi

body=$(jq -c --arg a "${alert}" --arg pod "${pod}" --arg state "${state}" --argjson ttl "${FIRING_FOR}" '
  .[$a] | walk(if type == "string" then gsub("__POD__"; $pod) else . end) as $p
  | (now | floor) as $now
  | [{labels: ($p.labels + {agentkourier_trial: "true"}), annotations: $p.annotations,
      startsAt: (if $state == "resolved" then $now - 120 else $now end | todate),
      endsAt: (if $state == "resolved" then $now - 1 else $now + $ttl end | todate)}]' "${SAMPLES}")

# From a pod in the cluster, where Alertmanager is. The body goes on the pod's stdin; the answer is the HTTP status. The post is
# idempotent (the same labels are the same alert), so a try that came to nothing is made again.
probe=
trap '[ -z "${probe}" ] || "${K[@]}" -n monitoring delete pod "${probe}" --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT
code=
for _ in 1 2 3; do
  probe=sandbox-alert-${RANDOM}
  code=$("${K[@]}" -n monitoring run "${probe}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" --command -- sh -c '
    curl -s -m 20 -o /dev/null -w "%{http_code}" -X POST -H "Content-Type: application/json" --data-binary @- '"${ALERTMANAGER_API}" \
    <<<"${body}" 2>/dev/null) || code=
  code=${code: -3}
  "${K[@]}" -n monitoring delete pod "${probe}" --ignore-not-found --wait=false >/dev/null 2>&1 || true # --rm has; a try that failed may not have
  probe= # none is in flight now: the trap has nothing to do
  [ -z "${code}" ] || break # an answer, even a refusal, is final; no answer is tried again
done
[ "${code}" = 200 ] || die "Alertmanager did not accept the alert (HTTP ${code:-none}); is it running? kubectl --context ${CTX} -n monitoring get pods"
echo "sent: ${alert} ${state}"

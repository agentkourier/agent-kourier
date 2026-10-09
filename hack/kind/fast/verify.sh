#!/usr/bin/env bash
# Prove, against a running agent-kourier-fast cluster, that each component answers from inside it and
# that the unauthenticated /_control/ surfaces, the fake Slack's and the stub agent's release
# endpoint, are published nowhere (spec 12.1).
# Every check prints "ok: ..." or "FAIL: ..." (exit 1). Two groups:
#  1. host side, lib.sh's assert_never_published (full/verify.sh runs it too): every Service that
#     selects the fake Slack's or the stub agent's pods is ClusterIP with no nodePort, external IP
#     or load-balancer ingress; no Ingress in e2e; none of those pods has a hostPort or
#     hostNetwork; the kind node publishes only the API server, on loopback;
#  2. Agent Kourier: its pods (one, or two with E2E_REPLICAS=2), ready (the Deployment's rollout has finished and the API server can reach
#     /healthz and /readyz on each, through its proxy), exactly one of them the leader (agentkourier_leader, which a lone replica says too), and,
#     on the Postgres store (E2E_STORE=postgres), a Postgres that is published nowhere, and with two replicas a container signaler that
#     is published nowhere either;
#  3. one probe pod in the cluster: fake Slack auth.test and /_control/connections, which shows Agent Kourier
#     connected over Socket Mode (at least one connection, waited for); the stub
#     agent's card with an unsigned JWT (200) and without one (401) and its release endpoint
#     (204, with no token, as the fake Slack's); Alertmanager's API; the
#     sink taking a POST and reading it back; and an alert posted to Alertmanager reaching the sink.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=../lib.sh
. "${HERE}/../lib.sh"
use_tier fast
CTX=kind-${CLUSTER}
NODE=${CLUSTER}-control-plane
K=(kubectl --context "${CTX}")
# The same curl image as full/verify.sh.
CURL_IMAGE=curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69

for tool in kubectl docker jq; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
# Safe to run on its own: nothing below runs against any other context.
assert_context fast
assert_never_published

echo "== agent-kourier: ready"
# The testhooks image starts, and reaches ready. An Agent Kourier that does not is shown with its own log, which
# holds no token or Secret value, and its pod's events.
agentkourier_fail() {
  echo "FAIL: $1" >&2
  "${K[@]}" -n agent-kourier get pods -l app.kubernetes.io/name=agent-kourier -o wide >&2 || true
  "${K[@]}" -n agent-kourier logs deploy/agent-kourier --tail=60 >&2 || true
  "${K[@]}" -n agent-kourier describe pods -l app.kubernetes.io/name=agent-kourier 2>&1 | sed -n '/^Events:/,$p' >&2 || true
  exit 1
}
"${K[@]}" -n agent-kourier rollout status deploy/agent-kourier --timeout=180s || agentkourier_fail "agent-kourier's rollout did not finish"
# A pod that is shutting down does not count: on Postgres the Deployment rolls with a surge, and the old pod, which hands its Lease over as it
# stops, may still be listed (Terminating) when the rollout has finished.
want=$("${K[@]}" -n agent-kourier get deploy/agent-kourier -o 'jsonpath={.spec.replicas}')
pods=$("${K[@]}" -n agent-kourier get pods -l app.kubernetes.io/name=agent-kourier -o json | jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name] | join(" ")')
[ "$(wc -w <<<"${pods}")" -eq "${want}" ] || agentkourier_fail "want ${want} Agent Kourier pod(s), have: ${pods:-none}"
echo "ok: ${want} Agent Kourier pod(s), ${pods}, rolled out"
for pod in ${pods}; do
  for path in healthz readyz; do
    "${K[@]}" get --raw "/api/v1/namespaces/agent-kourier/pods/${pod}:8080/proxy/${path}" >/dev/null || agentkourier_fail "${pod}'s /${path} does not answer 200"
    echo "ok: ${pod}'s /${path} answers 200"
  done
done
# Exactly one pod does Agent Kourier's work: the one that holds the Lease (a lone replica leads at once). Waited for, since a standby that has
# just taken over a Lease from a pod that was replaced is a few seconds behind the rollout.
leaders=
for _ in $(seq 60); do
  leaders=
  for pod in ${pods}; do
    metrics=$("${K[@]}" get --raw "/api/v1/namespaces/agent-kourier/pods/${pod}:8080/proxy/metrics" 2>/dev/null || true) # not grep -q on the pipe: pipefail would see its SIGPIPE
    ! grep -q '^agentkourier_leader 1$' <<<"${metrics}" || leaders="${leaders} ${pod}"
  done
  [ "$(wc -w <<<"${leaders}")" -eq 1 ] && break
  sleep 1
done
[ "$(wc -w <<<"${leaders}")" -eq 1 ] || agentkourier_fail "want exactly one Agent Kourier leader (agentkourier_leader 1), have:${leaders:- none}"
echo "ok: exactly one leader,${leaders}"

# The store Agent Kourier runs on (E2E_STORE of up-fast.sh), as the chart set it. Postgres is a credentialed database that nothing outside the
# cluster may reach: like the doubles' control surfaces, it is published nowhere.
store=$("${K[@]}" -n agent-kourier get deploy/agent-kourier -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="AGENTKOURIER_STORE")].value}')
echo "ok: Agent Kourier runs on the ${store:-unknown} store"
if [ "${store}" = postgres ]; then
  assert_app_unpublished e2e postgres
fi
# The signaler can stop or kill any process of the node: with two replicas it is there, and it is published nowhere.
if [ "${want}" -gt 1 ]; then
  assert_app_unpublished e2e signaler
fi

echo "== in the cluster: each component answers"
# --rm removes the probe when kubectl attaches and finishes; the trap covers a kubectl that fails
# before that, or an interrupt.
probe=probe-${RANDOM}
trap '"${K[@]}" -n e2e delete pod "${probe}" --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT
trap 'exit 130' INT TERM
"${K[@]}" -n e2e run "${probe}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" --command -- sh -s <<'EOF'
FS=http://fakeslack.e2e.svc.cluster.local:8080
STUB=http://stubagent.e2e.svc.cluster.local:8080
AGENT=$STUB/agents/kagent/test-agent
AM=http://alertmanager.monitoring.svc.cluster.local:9093
SINK=http://sink.e2e.svc.cluster.local:8080
has() { printf %s "$1" | grep -q -- "$2"; } # TEXT PATTERN
eq() { [ "$1" = "$2" ]; }
assert() { d=$1; shift; if "$@"; then echo "ok: $d"; else echo "FAIL: $d" >&2; exit 1; fi; } # DESCRIPTION COMMAND...
b64() { printf %s "$1" | base64 | tr -d '=\n' | tr '/+' '_-'; }
code() { curl -s -m 10 -o /dev/null -w '%{http_code}' "$@"; } # CURL ARG...

assert "fake Slack auth.test" has "$(curl -s -m 10 -X POST -H 'Authorization: Bearer xoxb-fake-test-token' "$FS/auth.test")" '"ok":true'
conns=0
for _ in $(seq 30); do
  conns=$(curl -s -m 10 "$FS/_control/connections" | sed -n 's/.*"connections":\([0-9]*\).*/\1/p')
  [ "${conns:-0}" -ge 1 ] && break
  sleep 1
done
assert "Agent Kourier is connected to the fake Slack over Socket Mode (connections: ${conns:-none})" test "${conns:-0}" -ge 1

jwt="$(b64 '{"alg":"none"}').$(b64 '{"email":"verify@example.test"}')."
card=$(curl -s -m 10 -H "Authorization: Bearer $jwt" "$AGENT/.well-known/agent-card.json")
assert "stub agent card with an unsigned JWT" has "$card" '"name":"test-agent"'
assert "stub agent card advertises streaming" has "$card" '"streaming":true'
assert "stub agent card without a token is refused" eq "$(code "$AGENT/.well-known/agent-card.json")" 401
assert "stub agent release endpoint answers without a token" eq "$(code -X POST "$STUB/_control/release/verify-probe")" 204

assert "Alertmanager API answers, with the sink route loaded" has "$(curl -s -m 10 "$AM/api/v2/status")" 'monitoring/sink/sink'

assert "sink log can be cleared" eq "$(code -X DELETE "$SINK/requests")" 204
assert "sink accepts a POST" eq "$(code -X POST -d '{"probe":"direct"}' "$SINK/direct")" 200
assert "sink reads the POST back" has "$(curl -s -m 10 "$SINK/requests")" '"path":"/direct"'

# Through real routing: Alertmanager groups the alert (1s), then posts it to the sink. The name
# is new each run: a firing alert already notified is not sent again, so a repeat would never arrive.
alert=VerifyFastTier$(date +%s)
assert "Alertmanager accepts an alert" eq "$(code -X POST -H 'Content-Type: application/json' -d '[{"labels":{"alertname":"'"$alert"'"}}]' "$AM/api/v2/alerts")" 200
for _ in $(seq 30); do
  has "$(curl -s -m 10 "$SINK/requests")" "$alert" && break
  sleep 1
done
assert "the alert reached the sink through Alertmanager" has "$(curl -s -m 10 "$SINK/requests")" "$alert"
code -X DELETE "$SINK/requests" >/dev/null
EOF
echo "fast tier verified"

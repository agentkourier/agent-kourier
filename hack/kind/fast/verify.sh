#!/usr/bin/env bash
# Prove, against a running courier-fast cluster, that each component answers from inside it and
# that the unauthenticated /_control/ surfaces, the fake Slack's and the stub agent's release
# endpoint, are published nowhere (spec 12.1).
# Every check prints "ok: ..." or "FAIL: ..." (exit 1). Two groups:
#  1. host side: every Service that selects the fake Slack's or the stub agent's pods is ClusterIP
#     with no nodePort, external IP or load-balancer ingress; no Ingress exists; none of those
#     pods has a hostPort or hostNetwork; the kind node publishes only the API server, on loopback;
#  2. one probe pod in the cluster: fake Slack auth.test and /_control/connections; the stub
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
assert() { d=$1; shift; if "$@"; then echo "ok: $d"; else echo "FAIL: $d" >&2; exit 1; fi; } # DESCRIPTION COMMAND...
is_empty() { [ -z "$1" ]; }
not_empty() { [ -n "$1" ]; }

echo "== never published: the fake Slack's /_control/ and the stub agent's /_control/release/ have no authentication"
for app in fakeslack stubagent; do
  pods=$("${K[@]}" -n e2e get pods -l "app=${app}" -o json)
  assert "a ${app} pod exists (the checks below are not vacuous)" not_empty "$(jq -r '.items[].metadata.name' <<<"${pods}")"
  # A Service selects the pods when every selector pair is one of a pod's labels. A Service with
  # no selector and hand-written EndpointSlices is out of scope: nothing here makes one.
  published=$("${K[@]}" -n e2e get services -o json | jq -r --argjson pods "$(jq '[.items[].metadata.labels]' <<<"${pods}")" '
    .items[]
    | (.spec.selector // {}) as $sel
    | select(($sel | length) > 0 and any($pods[]; . as $l | all(($sel | to_entries[]); $l[.key] == .value)))
    | select(.spec.type != "ClusterIP" or any(.spec.ports[]?; .nodePort != null)
             or ((.spec.externalIPs // []) | length > 0) or ((.status.loadBalancer.ingress // []) | length > 0))
    | .metadata.name')
  assert "no Service for ${app} is NodePort, LoadBalancer or has external IPs: [${published}]" is_empty "${published}"
  assert "${app} has a ClusterIP Service" not_empty "$("${K[@]}" -n e2e get service "${app}" -o jsonpath='{.spec.clusterIP}')"
  exposed=$(jq -r '.items[] | select(.spec.hostNetwork == true or any(.spec.containers[].ports[]?; .hostPort != null)) | .metadata.name' <<<"${pods}")
  assert "no ${app} pod uses hostPort or hostNetwork: [${exposed}]" is_empty "${exposed}"
done
ingresses=$("${K[@]}" get ingress -A -o name)
assert "no Ingress in the cluster: [${ingresses}]" is_empty "${ingresses}"
apiserver='^6443/tcp -> 127\.0\.0\.1:[0-9]+$'
ports=$(docker port "${NODE}" 2>/dev/null) || ports=
assert "the kind node publishes the API server, 6443/tcp on 127.0.0.1: [${ports}]" grep -q -E "${apiserver}" <<<"${ports}"
hostports=$(grep -v -E "${apiserver}" <<<"${ports}" || true) # grep exits 1 when nothing else is published
assert "the kind node publishes nothing but the API server: [${hostports}]" is_empty "${hostports}"

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
assert "fake Slack /_control/connections" has "$(curl -s -m 10 "$FS/_control/connections")" '"connections":0'

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

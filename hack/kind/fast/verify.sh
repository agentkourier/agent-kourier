#!/usr/bin/env bash
# Prove, against a running courier-fast cluster, that each component answers from inside it and
# that the fake Slack's unauthenticated /_control/ surface is published nowhere (spec 12.1).
# Every check prints "ok: ..." or "FAIL: ..." (exit 1). Two groups:
#  1. host side: every Service that selects the fake Slack's pods is ClusterIP with no nodePort,
#     external IP or load-balancer ingress; no Ingress exists; no fake Slack pod has a hostPort
#     or hostNetwork; the kind node publishes only the API server, on loopback;
#  2. one probe pod in the cluster: fake Slack auth.test and /_control/connections; the stub
#     agent's card with an unsigned JWT (200) and without one (401); Alertmanager's API; the
#     sink taking a POST and reading it back; and an alert posted to Alertmanager reaching the sink.
set -euo pipefail

CTX=kind-courier-fast
NODE=courier-fast-control-plane
K=(kubectl --context "${CTX}")
# The same curl image as full/verify.sh.
CURL_IMAGE=curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69

for tool in kubectl docker jq; do
  command -v "${tool}" >/dev/null || { echo "error: ${tool} is required" >&2; exit 1; }
done
assert() { d=$1; shift; if "$@"; then echo "ok: $d"; else echo "FAIL: $d" >&2; exit 1; fi; } # DESCRIPTION COMMAND...
is_empty() { [ -z "$1" ]; }
not_empty() { [ -n "$1" ]; }

echo "== never published: the fake Slack's /_control/ has no authentication"
pods=$("${K[@]}" -n e2e get pods -l app=fakeslack -o json)
assert "a fake Slack pod exists (the checks below are not vacuous)" not_empty "$(jq -r '.items[].metadata.name' <<<"${pods}")"
# A Service selects the fake Slack's pods when every selector pair is one of a pod's labels.
published=$("${K[@]}" -n e2e get services -o json | jq -r --argjson pods "$(jq '[.items[].metadata.labels]' <<<"${pods}")" '
  .items[]
  | (.spec.selector // {}) as $sel
  | select(($sel | length) > 0 and any($pods[]; . as $l | all(($sel | to_entries[]); $l[.key] == .value)))
  | select(.spec.type != "ClusterIP" or any(.spec.ports[]?; .nodePort != null)
           or ((.spec.externalIPs // []) | length > 0) or ((.status.loadBalancer.ingress // []) | length > 0))
  | .metadata.name')
assert "no Service for the fake Slack is NodePort, LoadBalancer or has external IPs: [${published}]" is_empty "${published}"
assert "the fake Slack has a ClusterIP Service" not_empty "$("${K[@]}" -n e2e get service fakeslack -o jsonpath='{.spec.clusterIP}')"
assert "no Ingress in the cluster" is_empty "$("${K[@]}" get ingress -A -o name)"
exposed=$(jq -r '.items[] | select(.spec.hostNetwork == true or any(.spec.containers[].ports[]?; .hostPort != null)) | .metadata.name' <<<"${pods}")
assert "no fake Slack pod uses hostPort or hostNetwork: [${exposed}]" is_empty "${exposed}"
hostports=$(docker port "${NODE}" | grep -v -E '^6443/tcp -> 127\.0\.0\.1:[0-9]+$' || true)
assert "the kind node publishes only the API server, on loopback: [${hostports}]" is_empty "${hostports}"

echo "== in the cluster: each component answers"
"${K[@]}" -n e2e run "probe-${RANDOM}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" --command -- sh -s <<'EOF'
FS=http://fakeslack.e2e.svc.cluster.local:8080
AGENT=http://stubagent.e2e.svc.cluster.local:8080/agents/kagent/test-agent
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

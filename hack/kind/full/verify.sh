#!/usr/bin/env bash
# Prove the WP-01 acceptance criteria against a running courier-full cluster:
#  1. a pod labelled app.kubernetes.io/name=courier reads the agent card and gets the mock
#     model's answer to an A2A SendMessage that carries a Dex-issued token;
#  2. an unlabelled pod cannot reach the controller's A2A port;
#  3. oauth2-proxy rejects an unsigned token while the controller (a canary) still accepts it.
set -euo pipefail

CTX=kind-courier-full
NS=courier
CURL_IMAGE=curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69
K="kubectl --context ${CTX}"

# Shell prelude for the probe pods. The Binding identity signs in with Dex's password grant
# (Dex has no client_credentials); its ID token is the bearer credential and is never printed.
PRELUDE=$(
  cat <<'EOF'
DEX=http://dex.dex.svc.cluster.local:5556/dex
A2A=http://kagent-controller.kagent.svc.cluster.local:8083/agents/kagent/test-agent
PROXY=http://kagent-oauth2-proxy.kagent.svc.cluster.local:4180/agents/kagent/test-agent
token() {
  curl -sf -d grant_type=password -d client_id=courier-binding -d client_secret=fake-courier-binding-secret \
    -d username=binding-test@example.test -d password=password -d 'scope=openid email' "$DEX/token" |
    sed -n 's/.*"id_token":"\([^"]*\)".*/\1/p'
}
EOF
)

# probe LABELS: run the script on stdin in a throwaway curl pod that carries LABELS.
probe() {
  { printf '%s\n' "${PRELUDE}"; cat; } |
    ${K} -n "${NS}" run "probe-${RANDOM}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" \
      --labels="$1" --command -- sh -s
}

${K} get namespace "${NS}" >/dev/null 2>&1 || ${K} create namespace "${NS}" >/dev/null

echo "== labelled pod: agent card and SendMessage through the controller"
# The messageId is unique per run: kagent replays the stored task for a repeated one.
probe app.kubernetes.io/name=courier <<'EOF'
set -e
tok=$(token)
[ -n "$tok" ]
curl -sf -m 20 -H "Authorization: Bearer $tok" "$A2A/.well-known/agent-card.json" | grep -q '"name":"test_agent"'
curl -sf -m 120 -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' "$A2A" \
  -d '{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"m-'"$(date +%s)-$$"'","role":"ROLE_USER","parts":[{"text":"What is 2+2?"}]}}}' |
  grep -q 'The answer is 4.'
EOF
echo "ok: card served and mock answer returned"

echo "== unlabelled pod: connection to port 8083 must time out"
probe app=intruder <<'EOF'
set -e
curl -sf -m 8 -o /dev/null "$DEX/.well-known/openid-configuration" # control: the pod itself has network
rc=0
curl -s -m 8 -o /dev/null "$A2A/.well-known/agent-card.json" || rc=$?
echo "curl exit $rc"
[ "$rc" = 28 ] # only a timeout counts: a policy drop must not be confused with another failure
EOF
echo "ok: unlabelled pod cannot reach the controller"

echo "== oauth2-proxy rejects an unsigned token; the controller does not"
probe app.kubernetes.io/name=courier <<'EOF'
set -e
b64() { printf %s "$1" | base64 | tr -d '=\n' | tr '/+' '_-'; }
forged="$(b64 '{"alg":"none"}').$(b64 '{"email":"mallory@evil.test"}')."
code() { curl -s -o /dev/null -m 15 -w '%{http_code}' -H "Authorization: Bearer $1" "$2/.well-known/agent-card.json"; }
real=$(token)
proxy_real=$(code "$real" "$PROXY")
proxy_forged=$(code "$forged" "$PROXY")
ctrl_forged=$(code "$forged" "$A2A")
echo "proxy: real=$proxy_real forged=$proxy_forged; controller: forged=$ctrl_forged"
# Canary: at alpha5 the controller accepts any well-formed JWT on 8083. If this fails, kagent
# changed; revisit D32/R4 and the WP-01b front-door design.
[ "$ctrl_forged" = 200 ] || { echo "kagent now verifies JWTs on 8083; revisit D32/R4" >&2; exit 1; }
[ "$proxy_real" = 200 ] && [ "$proxy_forged" = 403 ]
EOF
echo "ok: proxy rejects the unsigned token; controller still trusts it (expected at alpha5)"

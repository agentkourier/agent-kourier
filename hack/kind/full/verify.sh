#!/usr/bin/env bash
# Prove, against a running courier-full cluster, that courier reaches kagent only through the
# verifying front door (spec D32, R4). Every check asserts a response body, a status code, or a
# curl exit code, and prints "ok: ..." or "FAIL: ..." (exit 1). Groups, one probe pod each:
#  1. a courier pod gets the card, answers, HITL payloads, and an incremental SSE stream through
#     the front door, and has no route to the controller (8083) or the kagent UI (8080);
#     and path-traversal probes through the door never reach a controller route;
#  2. the front door refuses forged (well-formed), foreign-audience, and missing tokens, and so
#     does the UI door;
#  3. canary: the controller itself still accepts a forged token on /mcp (alpha5 does not verify);
#  4. a courier-labelled pod in another namespace, an unlabelled pod, and pods in that namespace
#     carrying the labels the 8083 and 8080 policies trust, reach nothing.
set -euo pipefail

CTX=kind-courier-full
CURL_IMAGE=curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69
K="kubectl --context ${CTX}"

# Shell prelude for the probe pods. The Binding identity signs in with Dex's password grant
# (Dex has no client_credentials); its ID token is the bearer credential and is never printed.
PRELUDE=$(
  cat <<'EOF'
DEX=http://dex.dex.svc.cluster.local:5556/dex
CTRL=http://kagent-controller.kagent.svc.cluster.local:8083
FD=http://kagent-frontdoor.kagent.svc.cluster.local:4180
UIDOOR=http://kagent-oauth2-proxy.kagent.svc.cluster.local:4180
UI=http://kagent-ui.kagent.svc.cluster.local:8080
CARD=/agents/kagent/test-agent/.well-known/agent-card.json
HITL=https://kagent.dev/extensions/hitl/v1
token() { # CLIENT SECRET: an ID token for the test identity from that Dex client
  curl -sf -d grant_type=password -d "client_id=$1" -d "client_secret=$2" -d username=binding-test@example.test \
    -d password=password -d 'scope=openid email' "$DEX/token" | sed -n 's/.*"id_token":"\([^"]*\)".*/\1/p'
}
binding_token() { token courier-binding fake-courier-binding-secret; }
b64() { printf %s "$1" | base64 | tr -d '=\n' | tr '/+' '_-'; }
# forge TOKEN sets two forgeries of a real token: A keeps the real header and signature but
# changes the payload's email; B is {"alg":"none"} with the real payload and no signature. It also
# sets decoded and altered (the payload before and after the change) so callers can check A is
# well-formed. The payload is padded to a multiple of 4 for base64 -d; its stderr is kept.
forge() {
  payload=$(printf %s "$1" | cut -d. -f2)
  padded=$(printf %s "$payload" | tr '_-' '/+')
  while [ $((${#padded} % 4)) -ne 0 ]; do padded="$padded="; done
  decoded=$(printf %s "$padded" | base64 -d)
  altered=$(printf %s "$decoded" | sed 's/binding-test@example.test/mallory@evil.test/')
  forgedA="$(printf %s "$1" | cut -d. -f1).$(b64 "$altered").$(printf %s "$1" | cut -d. -f3)"
  forgedB="$(b64 '{"alg":"none"}').${payload}."
}
json_object() { [ "${1#\{}" != "$1" ] && [ "${1%\}}" != "$1" ]; } # TEXT starts with { and ends with } (no jq in the probe image)
INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}'
# mcp URL [TOKEN]: status of an MCP initialize POST, with a bearer token if given.
mcp() {
  curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' ${2:+-H "Authorization: Bearer $2"} -d "$INIT" "$1/mcp" || true
}
# trav PATH [CURL ARG...]: send PATH untouched (--path-as-is) through the front door with the real
# token in $tok. Sets tst (status) and tbody. Redirects are followed only if -L is passed.
trav() {
  p=$1
  shift
  tst=$(curl -s --path-as-is -m 15 -o /tmp/tbody -w '%{http_code}' -H "Authorization: Bearer $tok" "$@" "$FD$p")
  tbody=$(cat /tmp/tbody)
}
code() { curl -s -o /dev/null -m 15 -w '%{http_code}' -H "Authorization: Bearer $1" "$2"; } # TOKEN URL
# rpc TOKEN AGENT METHOD TEXT [HEADER]: one A2A call through the front door. The messageId is
# unique per call: kagent replays the stored task for a repeated one.
rpc() {
  curl -sN -m 120 -H "Authorization: Bearer $1" -H 'Content-Type: application/json' ${5:+-H "$5"} "$FD/agents/kagent/$2" \
    -d '{"jsonrpc":"2.0","id":"1","method":"'"$3"'","params":{"message":{"messageId":"m-'"$(date +%s)$RANDOM"'","role":"ROLE_USER","parts":[{"text":"'"$4"'"}]}}}'
}
# Only a timeout counts as refused: a policy drop must not be confused with another failure.
refused() { rc=0; curl -s -m 8 -o /dev/null "$1" || rc=$?; [ "$rc" = 28 ]; }
has() { printf %s "$1" | grep -q -- "$2"; } # TEXT PATTERN
lacks() { ! has "$@"; }
eq() { [ "$1" = "$2" ]; }
assert() { d=$1; shift; if "$@"; then echo "ok: $d"; else echo "FAIL: $d" >&2; exit 1; fi; } # DESCRIPTION COMMAND...
EOF
)

# probe NAMESPACE LABELS: run the script on stdin in a throwaway curl pod with those labels.
probe() {
  { printf '%s\n' "${PRELUDE}"; cat; } |
    ${K} -n "$1" run "probe-${RANDOM}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" \
      --labels="$2" --command -- sh -s
}

${K} get namespace courier >/dev/null 2>&1 || ${K} create namespace courier >/dev/null

echo "== courier pod: A2A through the front door, nothing else reachable"
probe courier app.kubernetes.io/name=courier <<'EOF'
tok=$(binding_token)
assert "card served through the front door" has "$(curl -s -m 20 -H "Authorization: Bearer $tok" "$FD$CARD")" '"name":"test_agent"'
assert "SendMessage answered" has "$(rpc "$tok" test-agent SendMessage 'What is 2+2?')" 'The answer is 4.'
ask='Which database should we use?'
assert "A2A-Extensions reaches the controller: HITL payload present" has "$(rpc "$tok" test-agent SendMessage "$ask" "A2A-Extensions: $HITL")" '"type":"ask_user_request"'
assert "without the header the payload is absent" lacks "$(rpc "$tok" test-agent SendMessage "$ask")" '"type":"ask_user_request"'
assert "front door forwards only /agents/" eq "$(code "$tok" "$FD/api/agents")" 404
# Path traversal from /agents/ toward the controller's /api and /mcp. The controller answers every
# unknown path 200 with an empty body, so any 200 is a pass-through and the status is the real
# assertion; the body checks catch a future real route. A 301 is the door's own path clean-up, and
# -L follows it to the door's 404.
for p in /agents/../api/agents /agents/..%2Fapi%2Fagents /agents/kagent%2F..%2F..%2Fapi/agents; do
  for flag in "" -L; do
    trav "$p" $flag
    assert "traversal $p ${flag:-no redirect}: status $tst is not 200" [ "$tst" != 200 ]
    assert "traversal $p ${flag:-no redirect}: no controller /api body" lacks "$tbody" 'test-agent'
  done
done
trav /agents/../mcp -X POST -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -d "$INIT"
assert "traversal /agents/../mcp: status $tst is not 200" [ "$tst" != 200 ]
assert "traversal /agents/../mcp: no MCP answer" lacks "$tbody" protocolVersion
# The slow agent's model emits a chunk a second for five seconds. Buffered, every event would
# reach this loop in one burst; streamed, the first and last arrive seconds apart.
stream=$(rpc "$tok" slow-agent SendStreamingMessage go | while IFS= read -r line; do case "$line" in data:*) echo "$(date +%s) $line" ;; esac; done)
first=$(printf '%s\n' "$stream" | head -n 1 | cut -d' ' -f1)
last=$(printf '%s\n' "$stream" | tail -n 1 | cut -d' ' -f1)
assert "SSE stream complete" has "$stream" 'tick 5'
assert "SSE events arrive incrementally ($((last - first)) s apart)" [ $((last - first)) -ge 3 ]
assert "no route to the controller on 8083" refused "$CTRL$CARD"
assert "no route to the kagent UI relay on 8080" refused "$UI/mcp"
EOF

echo "== courier pod: forged, foreign-audience, and missing tokens"
probe courier app.kubernetes.io/name=courier <<'EOF'
tok=$(binding_token)
forge "$tok"
assert "forgery A has a different payload" has "$altered" mallory@evil.test
assert "forgery A: real payload decodes to a JSON object" json_object "$decoded"
assert "forgery A: altered payload is a JSON object" json_object "$altered"
assert "forgery A: only the email changed" eq "$(printf %s "$altered" | sed 's/mallory@evil.test/binding-test@example.test/')" "$decoded"
assert "real token accepted at the front door" eq "$(code "$tok" "$FD$CARD")" 200
assert "A: real signature, altered email, refused" eq "$(code "$forgedA" "$FD$CARD")" 401
assert "B: alg none, real payload, refused" eq "$(code "$forgedB" "$FD$CARD")" 401
assert "token minted for another client (audience) refused" eq "$(code "$(token kagent-ui fake-oauth2-proxy-secret)" "$FD$CARD")" 401
assert "no token refused" eq "$(curl -s -o /dev/null -w '%{http_code}' "$FD$CARD")" 401
# The UI door verifies too, but for the UI's own audience: the Binding's token is not welcome.
assert "UI door refuses the Binding's token" eq "$(code "$tok" "$UIDOOR/api/agents")" 403
# The 200 is kagent's catch-all route (empty body), not an API answer: the token passed the door
# and the fence, and the request reached the controller, nothing more.
assert "UI door passes a UI-audience token through the fence (200 is the controller's catch-all)" eq "$(code "$(token kagent-ui fake-oauth2-proxy-secret)" "$UIDOOR/api/agents")" 200
EOF

echo "== canary: the controller does not verify JWTs (kagent 1.0.0-alpha5)"
# A pod labelled like the UI nginx is admitted to 8083 by design; this is what the front door is for.
# /mcp is the controller's one route that answers with a body, so it shows a token being accepted.
probe kagent app.kubernetes.io/component=ui <<'EOF'
forge "$(binding_token)"
assert "controller /mcp initialize: forged token accepted" eq "$(mcp "$CTRL" "$forgedB")" 200
assert "controller /mcp initialize: no token refused" eq "$(mcp "$CTRL")" 401
if [ "$(code "$forgedA" "$CTRL$CARD")" = 200 ] && [ "$(code "$forgedB" "$CTRL$CARD")" = 200 ]; then
  echo "ok: controller accepted both forgeries (expected at alpha5)"
else
  echo "FAIL: kagent now verifies JWTs on 8083; revisit D32/R4" >&2
  exit 1
fi
EOF

# fenced NAMESPACE LABELS DESCRIPTION: a pod that is not courier reaches none of the three.
fenced() {
  echo "== $3: no route to the controller, the front door, or the UI"
  probe "$1" "$2" <<'EOF'
assert "control: the pod itself has network (Dex answers)" curl -sf -m 8 -o /dev/null "$DEX/.well-known/openid-configuration"
for url in "$CTRL$CARD" "$FD$CARD" "$UI/mcp"; do assert "no route to $url" refused "$url"; done
EOF
}
fenced default app.kubernetes.io/name=courier "courier-labelled pod in another namespace"
fenced courier app=intruder "unlabelled pod"
# The labels the policies trust, on pods outside the namespace those policies select.
fenced default app.kubernetes.io/component=ui,app.kubernetes.io/name=kagent-frontdoor "pod in default labelled as the UI and the front door (8083 trust)"
fenced default app.kubernetes.io/name=oauth2-proxy "pod in default labelled as the UI's oauth2-proxy (8080 trust)"

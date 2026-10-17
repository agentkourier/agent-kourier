#!/usr/bin/env bash
# Prove, against a running agent-kourier-full cluster, that Agent Kourier reaches kagent only through the
# verifying front door (spec D32, R4). Every check asserts a response body, a status code, or a
# curl exit code, and prints "ok: ..." or "FAIL: ..." (exit 1). Groups, one probe pod each:
#  1. an Agent Kourier pod gets the card, answers, HITL payloads, and an incremental SSE stream through
#     the front door, and has no route to the controller (8083) or the kagent UI (8080);
#     and path-traversal probes through the door never reach a controller route;
#  2. the front door refuses forged (well-formed), foreign-audience, and missing tokens, and so
#     does the UI door;
#  3. B's valid token cannot act as A through the door by adding X-Agent-Name plus X-User-Id or
#     ?user_id=, in canonical and lower case (-31403 on send and GetTask), and X-Share-Token is not
#     forwarded, while A's own send and GetTask still work (spec D32, R4; WP-01d);
#  4. canary: the controller itself still accepts a forged token on /mcp, lets the same spoof
#     through, and reads X-Share-Token (alpha5 does not verify); group 3 means something only
#     because of this;
#  5. an agent-kourier-labelled pod in another namespace, an unlabelled pod, and pods in that namespace
#     carrying the labels the 8083 and 8080 policies trust, reach nothing.
set -euo pipefail

CTX=kind-agent-kourier-full
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=../lib.sh
. "${HERE}/../lib.sh"
CURL_IMAGE=curlimages/curl:8.23.0@sha256:a134550be58ab783810e613ae96fd253c38f7bdd16d9bf9be350700c7a29ac06
K=(kubectl --context "${CTX}")
NODE=agent-kourier-full-control-plane
for tool in kubectl docker jq; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
# Safe to run on its own: nothing below runs against any other context.
assert_context full

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
token() { # CLIENT SECRET [USER]: an ID token for that Dex user (default: the Binding identity A) from that client
  curl -sf -d grant_type=password -d "client_id=$1" -d "client_secret=$2" -d "username=${3:-$A_EMAIL}" \
    -d password=password -d 'scope=openid email' "$DEX/token" | sed -n 's/.*"id_token":"\([^"]*\)".*/\1/p'
}
A_EMAIL=binding-test@example.test
B_EMAIL=identity-b@example.test # a second person: a different owner of kagent sessions
binding_token() { token agent-kourier-binding fake-agent-kourier-binding-secret; }
b_token() { token agent-kourier-binding fake-agent-kourier-binding-secret "$B_EMAIL"; }
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
# a2a BASE TOKEN QUERY BODY [CURL ARG...]: one JSON-RPC POST to test-agent at BASE ($FD or $CTRL).
# QUERY is "" or "?k=v"; the extra curl arguments carry the spoof headers.
a2a() {
  base=$1 bearer=$2 query=$3 body=$4
  shift 4
  curl -s -m 60 -H "Authorization: Bearer $bearer" -H 'Content-Type: application/json' "$@" "$base/agents/kagent/test-agent$query" -d "$body"
}
send_body() { # TEXT [CONTEXT_ID]: a SendMessage body with a unique messageId
  printf '{"jsonrpc":"2.0","id":"1","method":"SendMessage","params":{"message":{"messageId":"m-%s%s",%s"role":"ROLE_USER","parts":[{"text":"%s"}]}}}' \
    "$(date +%s)" "$RANDOM" "${2:+\"contextId\":\"$2\",}" "$1"
}
get_body() { printf '{"jsonrpc":"2.0","id":"1","method":"GetTask","params":{"id":"%s"}}' "$1"; } # TASK_ID
task_id() { printf %s "$1" | sed -n 's/.*"task":{"id":"\([^"]*\)".*/\1/p'; }
context_id() { printf %s "$1" | sed -n 's/.*"contextId":"\([^"]*\)".*/\1/p'; }
# spoof BASE WANT LABEL QUERY [CURL ARG...]: B ($tokB) continues A's session ($cid) and reads A's
# task ($tid) with the extra request data; both answers must contain WANT.
spoof() {
  sbase=$1 want=$2 label=$3 squery=$4
  shift 4
  [ "$sbase" = "$FD" ] && where=door || where=controller
  assert "$where, $label: B continues A's session: $want" has "$(a2a "$sbase" "$tokB" "$squery" "$(send_body 'What is 2+2?' "$cid")" "$@")" "$want"
  assert "$where, $label: B reads A's task: $want" has "$(a2a "$sbase" "$tokB" "$squery" "$(get_body "$tid")" "$@")" "$want"
}
# spoof_all BASE WANT: the four ways kagent lets a caller pick the acting user (WP-03), B as caller.
spoof_all() {
  spoof "$1" "$2" 'X-Agent-Name + X-User-Id' '' -H 'X-Agent-Name: test-agent' -H "X-User-Id: $A_EMAIL"
  spoof "$1" "$2" 'x-agent-name + x-user-id (lower case)' '' -H 'x-agent-name: test-agent' -H "x-user-id: $A_EMAIL"
  spoof "$1" "$2" 'X-Agent-Name + ?user_id' "?user_id=$A_EMAIL" -H 'X-Agent-Name: test-agent'
  spoof "$1" "$2" 'x-agent-name + ?user_id (lower case)' "?user_id=$A_EMAIL" -H 'x-agent-name: test-agent'
}
# Only a timeout counts as refused: a policy drop must not be confused with another failure.
refused() { rc=0; curl -s -m 8 -o /dev/null "$1" || rc=$?; [ "$rc" = 28 ]; }
has() { printf %s "$1" | grep -q -- "$2"; } # TEXT PATTERN
# A negative check must not pass because its command failed: lacks needs a non-empty TEXT, and an
# empty answer (curl failed) is a failure. absent allows an empty TEXT, so use it only for a body
# whose request a status check, just before it, has shown was answered. not200 wants a real status
# other than 200: curl's 000 (no answer) is neither.
lacks() { [ -n "$1" ] && ! has "$@"; }
absent() { ! has "$@"; }
not200() { [ -n "$1" ] && [ "$1" != 000 ] && [ "$1" != 200 ]; } # STATUS
eq() { [ "$1" = "$2" ]; }
assert() { d=$1; shift; if "$@"; then echo "ok: $d"; else echo "FAIL: $d" >&2; exit 1; fi; } # DESCRIPTION COMMAND...
EOF
)

# --rm removes a probe when kubectl attaches and finishes; this trap removes the one a kubectl that
# failed before that, or an interrupt, leaves behind.
PROBE_NS=''
PROBE_POD=''
trap '[ -z "${PROBE_POD}" ] || "${K[@]}" -n "${PROBE_NS}" delete pod "${PROBE_POD}" --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT
trap 'exit 130' INT TERM

# probe NAMESPACE LABELS: run the script on stdin in a throwaway curl pod with those labels.
probe() {
  PROBE_NS=$1 PROBE_POD=probe-${RANDOM}
  { printf '%s\n' "${PRELUDE}"; cat; } |
    "${K[@]}" -n "$1" run "${PROBE_POD}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" \
      --labels="$2" --command -- sh -s
}

"${K[@]}" get namespace agent-kourier >/dev/null 2>&1 || "${K[@]}" create namespace agent-kourier >/dev/null

assert_never_published
# The full tier's own unauthenticated test surfaces: the scenario model's /_control/release/ and the MCP tool server.
echo "== never published: the scenario model's /_control/release/ and the tool server have no authentication"
for app in scenariollm scenariomcp; do
  assert_app_unpublished kagent "${app}"
done

echo "== Agent Kourier pod: A2A through the front door, nothing else reachable"
probe agent-kourier app.kubernetes.io/name=agent-kourier <<'EOF'
tok=$(binding_token)
card=$(curl -s -m 20 -H "Authorization: Bearer $tok" "$FD$CARD")
assert "card served through the front door" has "$card" '"name":"test_agent"'
assert "card advertises streaming (WP-27e refuses agents that do not)" has "$card" '"streaming":true'
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
    assert "traversal $p ${flag:-no redirect}: status $tst is not 200" not200 "$tst"
    assert "traversal $p ${flag:-no redirect}: no controller /api body" absent "$tbody" 'test-agent'
  done
done
trav /agents/../mcp -X POST -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -d "$INIT"
assert "traversal /agents/../mcp: status $tst is not 200" not200 "$tst"
assert "traversal /agents/../mcp: no MCP answer" absent "$tbody" protocolVersion
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

echo "== Agent Kourier pod: forged, foreign-audience, and missing tokens"
probe agent-kourier app.kubernetes.io/name=agent-kourier <<'EOF'
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

echo "== Agent Kourier pod: the spoof is refused at the door; X-Share-Token is dropped"
probe agent-kourier app.kubernetes.io/name=agent-kourier <<'EOF'
tokA=$(binding_token)
tokB=$(b_token)
r=$(a2a "$FD" "$tokA" '' "$(send_body 'What is 2+2?')")
tid=$(task_id "$r")
cid=$(context_id "$r")
assert "A's own send answered" has "$r" 'The answer is 4.'
assert "A's session has a task id" [ -n "$tid" ]
assert "A's session has a context id" [ -n "$cid" ]
assert "A continues its own session" has "$(a2a "$FD" "$tokA" '' "$(send_body 'What is 2+2?' "$cid")")" TASK_STATE_COMPLETED
assert "A reads its own task" has "$(a2a "$FD" "$tokA" '' "$(get_body "$tid")")" TASK_STATE_COMPLETED
# Control: without the spoof B is already refused, so the rows below differ from it only by the
# headers. B has a valid token and its own sessions; only A's are out of reach.
spoof "$FD" -31403 'no extra headers' ''
spoof_all "$FD" -31403
assert "X-Share-Token is not forwarded: a bogus one is ignored" eq "$(curl -s -o /dev/null -m 15 -w '%{http_code}' -H "Authorization: Bearer $tokA" -H 'X-Share-Token: bogus' "$FD$CARD")" 200
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
# The same spoof the front door refuses works here, so group 3's -31403 is the door's doing. A
# session of A's is made directly; B, without headers, is refused (control), and with them acts as A.
tokA=$(binding_token)
tokB=$(b_token)
r=$(a2a "$CTRL" "$tokA" '' "$(send_body 'What is 2+2?')")
tid=$(task_id "$r")
cid=$(context_id "$r")
assert "controller: A's session has a task id" [ -n "$tid" ]
assert "controller: A's session has a context id" [ -n "$cid" ]
spoof "$CTRL" -31403 'no extra headers' ''
spoof_all "$CTRL" TASK_STATE_COMPLETED
assert "controller: a bogus X-Share-Token is read (403); the door drops it" eq "$(curl -s -o /dev/null -m 15 -w '%{http_code}' -H "Authorization: Bearer $tokA" -H 'X-Share-Token: bogus' "$CTRL$CARD")" 403
EOF

# fenced NAMESPACE LABELS DESCRIPTION: a pod that is not Agent Kourier reaches none of the three.
fenced() {
  echo "== $3: no route to the controller, the front door, or the UI"
  probe "$1" "$2" <<'EOF'
assert "control: the pod itself has network (Dex answers)" curl -sf -m 8 -o /dev/null "$DEX/.well-known/openid-configuration"
for url in "$CTRL$CARD" "$FD$CARD" "$UI/mcp"; do assert "no route to $url" refused "$url"; done
EOF
}
fenced default app.kubernetes.io/name=agent-kourier "agent-kourier-labelled pod in another namespace"
fenced agent-kourier app=intruder "unlabelled pod"
fenced e2e app=e2e-runner "pod in e2e, where the test doubles and the runner run"
# The labels the policies trust, on pods outside the namespace those policies select.
fenced default app.kubernetes.io/component=ui,app.kubernetes.io/name=kagent-frontdoor "pod in default labelled as the UI and the front door (8083 trust)"
fenced default app.kubernetes.io/name=oauth2-proxy "pod in default labelled as the UI's oauth2-proxy (8080 trust)"

# Agent Kourier itself, last: the checks above show the door and the fences, and this shows Agent Kourier going through
# them. It is the fast tier's Deployment, ready and connected to the fake Slack, and a mention of it in C1 comes
# back answered by kagent: Agent Kourier read the Binding's Dex token from its Secret, the door verified it, kagent ran
# the turn on mockllm, and the reply streamed into the thread.
echo "== agent-kourier: ready, connected to the fake Slack, and a turn through the front door"
agentkourier_fail() {
  echo "FAIL: $1" >&2
  "${K[@]}" -n agent-kourier get pods -l app.kubernetes.io/name=agent-kourier -o wide >&2 || true
  "${K[@]}" -n agent-kourier logs deploy/agent-kourier --tail=60 >&2 || true
  "${K[@]}" -n agent-kourier describe pods -l app.kubernetes.io/name=agent-kourier 2>&1 | sed -n '/^Events:/,$p' >&2 || true
  exit 1
}
"${K[@]}" -n agent-kourier rollout status deploy/agent-kourier --timeout=180s || agentkourier_fail "agent-kourier's rollout did not finish"
agentkourier_pod=$("${K[@]}" -n agent-kourier get pods -l app.kubernetes.io/name=agent-kourier -o jsonpath='{.items[*].metadata.name}')
[[ -n "${agentkourier_pod}" && "${agentkourier_pod}" != *" "* ]] || agentkourier_fail "want exactly one Agent Kourier pod, have: ${agentkourier_pod:-none}"
echo "ok: one Agent Kourier pod, ${agentkourier_pod}, rolled out"
for path in healthz readyz; do
  "${K[@]}" get --raw "/api/v1/namespaces/agent-kourier/pods/${agentkourier_pod}:8080/proxy/${path}" >/dev/null || agentkourier_fail "agent-kourier's /${path} does not answer 200"
  echo "ok: Agent Kourier's /${path} answers 200"
done
probe e2e app=verify-agent-kourier <<'EOS'
FS=http://fakeslack.e2e.svc.cluster.local:8080
conns=0
for _ in $(seq 60); do
  conns=$(curl -s -m 10 "$FS/_control/connections" | sed -n 's/.*"connections":\([0-9]*\).*/\1/p')
  [ "${conns:-0}" -ge 1 ] && break
  sleep 1
done
assert "Agent Kourier is connected to the fake Slack over Socket Mode (connections: ${conns:-none})" test "${conns:-0}" -ge 1
mark=verify-$(date +%s)
sent=$(curl -s -m 20 -X POST -H 'Content-Type: application/json' -d '{"channel":"C1","user":"UVERIFY","text":"<@UFAKEBOT> What is 2+2? '"$mark"'"}' "$FS/_control/mention")
ts=$(printf %s "$sent" | sed -n 's/.*"ts":"\([^"]*\)".*/\1/p')
assert "a mention of Agent Kourier in C1 was delivered (ts ${ts:-none})" test -n "$ts"
thread=
for _ in $(seq 90); do
  thread=$(curl -s -m 10 "$FS/_control/thread?channel=C1&ts=$ts")
  has "$thread" 'The answer is 4.' && break
  sleep 2
done
assert "agent-kourier's turn went through the front door to kagent and back: the thread has the answer" has "$thread" 'The answer is 4.'
EOS

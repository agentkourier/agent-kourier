#!/usr/bin/env bash
# Tests for http-probe.sh. Offline: a fake curl, first on PATH, answers the probe's requests from canned values and logs
# its arguments, so no case reaches the network. Run as `hack/slack/test.sh`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=${HERE}/http-probe.sh

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# The fake tokens. They are not shaped like Slack's, so the secret scan has nothing to match.
PROBE_TOKEN=fake-probe-token
POSTER_TOKEN=fake-poster-token
MENTION_TOKEN=fake-mention-token
URL=https://chat.example.test/chat/slack/agent-kourier/probe
METRICS=http://127.0.0.1:9090/metrics
CHANNEL=C0PROBE

# The fake curl. It reads the options the probe passes (-o, -w, -H, --data-binary, --data-urlencode, -G) and the URL,
# appends its arguments to ${STUB_DIR}/calls, and answers:
#   - a POST to the connection's URL: 401 (STUB_SIGNED, STUB_UNSIGNED or STUB_STALE override it by which headers came);
#   - a POST to any other /chat/slack/ URL: 503 with the chat listener's text (STUB_UNKNOWN overrides the code);
#   - auth.test: the identity of the token in the -H @file, by the table below;
#   - chat.postMessage: ok with a new ts, and the text is appended to ${STUB_DIR}/posts;
#   - conversations.replies: the parent and, unless STUB_NO_REPLY is set, a reply from the bot STUB_REPLY_BOT (B0PROBE);
#   - the metrics URL: ${STUB_DIR}/metrics.<n>, n counting the reads from 1.
mkdir -p "${TMP}/bin"
cat >"${TMP}/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${STUB_DIR}/calls"
out=- write= url= data= token= ts_header= sig_header=
query=()
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift ;;
    -w) write=$2; shift ;;
    -H)
      case "$2" in
        @*)
          printf 'header-file %s\n' "${2#@}" >>"${STUB_DIR}/calls"
          token=$(sed -n 's/^Authorization: Bearer //p' "${2#@}")
          ;;
        X-Slack-Request-Timestamp:*) ts_header=${2#*: } ;;
        X-Slack-Signature:*) sig_header=${2#*: } ;;
      esac
      shift
      ;;
    --data-binary) data=${2#@}; shift ;;
    --data-urlencode) query+=("$2"); shift ;;
    -m | -X) shift ;;
    http://* | https://*) url=$1 ;;
  esac
  shift
done
code=200 body=
case "${url}" in
  https://chat.example.test/chat/slack/agent-kourier/probe)
    if [ -z "${ts_header}" ]; then
      code=${STUB_UNSIGNED:-401}
    elif [ $(($(date +%s) - ts_header)) -gt 1800 ]; then
      code=${STUB_STALE:-401}
    else
      [ "${sig_header}" = "v0=$(printf '0%.0s' $(seq 64))" ] || { echo "fake curl: unexpected signature" >&2; exit 1; }
      code=${STUB_SIGNED:-401}
    fi
    body=Unauthorized
    ;;
  https://chat.example.test/chat/slack/*)
    code=${STUB_UNKNOWN:-503}
    body='the connection does not run on this replica'
    ;;
  https://slack.com/api/auth.test)
    case "${token}" in
      fake-probe-token) body='{"ok":true,"user_id":"U0PROBE","bot_id":"B0PROBE"}' ;;
      fake-poster-token) body='{"ok":true,"user_id":"U0POSTER","bot_id":"B0POSTER"}' ;;
      fake-mention-token) body='{"ok":true,"user_id":"U0MENTION","bot_id":"B0MENTION"}' ;;
      *) body='{"ok":false,"error":"invalid_auth"}' ;;
    esac
    ;;
  https://slack.com/api/chat.postMessage)
    jq -r '.text' "${data}" >>"${STUB_DIR}/posts"
    n=$(wc -l <"${STUB_DIR}/posts" | tr -d ' ')
    body=$(printf '{"ok":true,"channel":"%s","ts":"1700000000.00000%s"}' "$(jq -r .channel "${data}")" "${n}")
    ;;
  https://slack.com/api/conversations.replies)
    parent=
    for q in "${query[@]}"; do case "${q}" in ts=*) parent=${q#ts=} ;; esac; done
    if [ -n "${STUB_NO_REPLY:-}" ]; then
      body=$(printf '{"ok":true,"messages":[{"ts":"%s","bot_id":"B0POSTER"}]}' "${parent}")
    else
      body=$(printf '{"ok":true,"messages":[{"ts":"%s","bot_id":"B0POSTER"},{"ts":"1700000001.000001","bot_id":"%s","text":"a reply"}]}' \
        "${parent}" "${STUB_REPLY_BOT:-B0PROBE}")
    fi
    ;;
  http://127.0.0.1:9090/metrics)
    n=$(($(cat "${STUB_DIR}/metrics-reads" 2>/dev/null || echo 0) + 1))
    echo "${n}" >"${STUB_DIR}/metrics-reads"
    body=$(cat "${STUB_DIR}/metrics.${n}")
    ;;
  *) echo "fake curl: unexpected URL ${url}" >&2; exit 1 ;;
esac
if [ "${out}" = - ]; then printf '%s\n' "${body}"; else printf '%s\n' "${body}" >"${out}"; fi
[ -z "${write}" ] || printf '%s' "${write//'%{http_code}'/${code}}"
FAKE
chmod 0755 "${TMP}/bin/curl"

# cred FILE KEY VALUE [MODE]: a credentials file holding KEY=VALUE (and a signing secret the probe must not need).
cred() {
  printf 'signingSecret=not-a-secret\n%s=%s\n' "$2" "$3" >"$1"
  chmod "${4:-600}" "$1"
}
cred "${TMP}/probe.env" botToken "${PROBE_TOKEN}"
cred "${TMP}/poster.env" SLACK_BOT_TOKEN "${POSTER_TOKEN}"
cred "${TMP}/mention.env" SLACK_BOT_TOKEN "${MENTION_TOKEN}"

# metrics N BAD STALE ACK: the series of read N, for the connection under test and, unchanged between reads, another.
metrics() {
  cat >"${CASE}/metrics.$1" <<EOF
# HELP agentkourier_slack_http_requests_total Requests Slack sent to a connection in HTTP mode.
# TYPE agentkourier_slack_http_requests_total counter
agentkourier_slack_http_requests_total{connection="agent-kourier/probe",outcome="acknowledged"} $4
agentkourier_slack_http_requests_total{connection="agent-kourier/probe",outcome="bad_signature"} $2
agentkourier_slack_http_requests_total{connection="agent-kourier/probe",outcome="stale"} $3
agentkourier_slack_http_requests_total{connection="agent-kourier/other",outcome="bad_signature"} 9
EOF
}

# new_case NAME: a fresh case directory, CASE, for the fake curl's log and fixtures.
new_case() {
  CASE=${TMP}/$1
  rm -rf "${CASE}" && mkdir -p "${CASE}"
  touch "${CASE}/calls" "${CASE}/posts"
}

# REQUIRED is every required flag, with the files above.
REQUIRED=(--url "${URL}" --channel "${CHANNEL}" --probe-env "${TMP}/probe.env" --poster-env "${TMP}/poster.env"
  --mention-env "${TMP}/mention.env")

# probe [ENV=VALUE...] -- ARG...: run the probe in CASE with the fake curl first on PATH and the ENVs set for it, its
# output in ${CASE}/out and its exit status in STATUS.
probe() {
  local vars=()
  while [ "$1" != -- ]; do
    vars+=("$1")
    shift
  done
  shift
  STATUS=0
  env ${vars[@]+"${vars[@]}"} PATH="${TMP}/bin:${PATH}" STUB_DIR="${CASE}" "${SCRIPT}" "$@" >"${CASE}/out" 2>&1 || STATUS=$?
}

# expect STATUS PATTERN DESCRIPTION: the last run exited STATUS and its output has a line matching PATTERN (grep -E).
expect() {
  [ "${STATUS}" = "$1" ] || { cat "${CASE}/out" >&2; fail "$3: exit ${STATUS}, want $1"; }
  grep -Eq -- "$2" "${CASE}/out" || { cat "${CASE}/out" >&2; fail "$3: no line matches /$2/"; }
  echo "ok: $3 (exit ${STATUS})"
}

# no_secret DESCRIPTION: neither the output nor any argument of any curl call holds a token or the signing secret, and
# the header files are gone.
no_secret() {
  local token file
  for token in "${PROBE_TOKEN}" "${POSTER_TOKEN}" "${MENTION_TOKEN}" not-a-secret; do
    ! grep -q -- "${token}" "${CASE}/out" "${CASE}/calls" || fail "$1: ${token} reached the output or a curl argument"
  done
  grep -q '^header-file ' "${CASE}/calls" || fail "$1: no header went to curl by file"
  while read -r file; do
    [ ! -e "${file}" ] || fail "$1: the header file ${file} was not removed"
  done < <(sed -n 's/^header-file //p' "${CASE}/calls")
  echo "ok: $1: no token in the output or curl's arguments, and the header files are removed"
}

# nth N: the Nth text the probe posted.
nth() { sed -n "$1p" "${CASE}/posts"; }

echo "== every check passes"
new_case pass
probe -- "${REQUIRED[@]}"
expect 0 '^PASS signatures: a wrong v0= signature with a fresh timestamp: HTTP 401$' "a wrong signature is 401"
expect 0 '^PASS signatures: no signature headers: HTTP 401$' "no signature headers is 401"
expect 0 '^PASS signatures: a timestamp 3600 s old: HTTP 401$' "an old timestamp is 401"
expect 0 '^PASS unknown connection: .*/chat/slack/agent-kourier/kourier-probe-[0-9a-f]+: HTTP 503, the connection does not run on this replica$' \
  "an unknown connection is 503"
expect 0 '^PASS bot identity: probe U0PROBE/B0PROBE, poster B0POSTER, mention poster B0MENTION$' "three apps"
expect 0 '^PASS alert trigger: B0PROBE replied at 1700000001.000001 in thread 1700000000.000001$' "the alert is answered"
expect 0 '^PASS bot mention: B0PROBE replied at 1700000001.000001 in thread 1700000000.000002$' "the mention is answered"
expect 0 '^SKIP metrics: no --metrics-url$' "no metrics check without --metrics-url"
expect 0 '^all 7 checks passed$' "the summary"
[[ $(nth 1) =~ ^FIRING:\ KourierProbe[0-9a-f]+$ ]] || fail "the alert text is $(nth 1)"
[[ $(nth 2) =~ ^\<@U0PROBE\>\ live\ probe\ [0-9a-f]+:\ reply\ with\ one\ short\ sentence\ and\ call\ no\ tools\.$ ]] ||
  fail "the mention text is $(nth 2)"
echo "ok: the alert and the mention say what the checks post"
no_secret "pass"

echo "== another alert prefix"
new_case prefix
probe -- "${REQUIRED[@]}" --alert-prefix '[FIRING:1] '
expect 0 '^all 7 checks passed$' "the run passes"
[[ $(nth 1) =~ ^\[FIRING:1\]\ KourierProbe[0-9a-f]+$ ]] || fail "the alert text is $(nth 1)"
echo "ok: the alert starts with the prefix"

echo "== metrics"
new_case metrics-pass
metrics 1 3 0 10
metrics 2 5 1 14
probe -- "${REQUIRED[@]}" --metrics-url "${METRICS}"
expect 0 '^PASS metrics: agent-kourier/probe: bad_signature \+2, stale \+1, acknowledged \+4$' "the increments pass"
expect 0 '^all 8 checks passed$' "the summary counts the metrics check"
[ "$(cat "${CASE}/metrics-reads")" = 2 ] || fail "the metrics were read $(cat "${CASE}/metrics-reads") times, want 2"
new_case metrics-first-read
metrics 1 0 0 0
grep -v 'agent-kourier/probe' "${CASE}/metrics.1" >"${CASE}/metrics.0" && mv "${CASE}/metrics.0" "${CASE}/metrics.1"
metrics 2 2 1 2
probe -- "${REQUIRED[@]}" --metrics-url "${METRICS}"
expect 0 '^PASS metrics: agent-kourier/probe: bad_signature \+2, stale \+1, acknowledged \+2$' "a missing series reads as 0"
new_case metrics-fail
metrics 1 3 0 10
metrics 2 4 1 11
probe -- "${REQUIRED[@]}" --metrics-url "${METRICS}"
expect 1 '^FAIL metrics: agent-kourier/probe: bad_signature \+1 \(want \+2\), stale \+1, acknowledged \+1 \(want at least \+2\)$' \
  "wrong increments fail"
expect 1 '^1 of 8 checks failed$' "the summary counts the failure"
new_case metrics-unreadable
probe -- "${REQUIRED[@]}" --metrics-url "${METRICS}"
expect 1 '^FAIL metrics: could not read' "metrics that cannot be read fail"

echo "== failing checks"
new_case signed-200
probe STUB_SIGNED=200 -- "${REQUIRED[@]}"
expect 1 '^FAIL signatures: a wrong v0= signature with a fresh timestamp: HTTP 200, want 401$' "a 200 where 401 is expected fails"
expect 1 '^1 of 7 checks failed$' "the other checks still run"
new_case unknown-404
probe STUB_UNKNOWN=404 -- "${REQUIRED[@]}"
expect 1 '^FAIL unknown connection: .*: HTTP 404, want 503 and "the connection does not run on this replica"$' \
  "a 404 for an unknown connection fails"
new_case same-app
cred "${CASE}/same.env" SLACK_BOT_TOKEN "${PROBE_TOKEN}"
probe -- --url "${URL}" --channel "${CHANNEL}" --probe-env "${TMP}/probe.env" --poster-env "${CASE}/same.env" \
  --mention-env "${TMP}/mention.env"
expect 1 '^FAIL bot identity: the probe and the poster are the same app \(B0PROBE\)$' "the same app twice fails"
expect 1 '^FAIL alert trigger: skipped: no three distinct bot identities$' "the Slack checks do not run"
[ ! -s "${CASE}/posts" ] || fail "the probe posted with no distinct identities"
new_case same-posters
cred "${CASE}/same.env" SLACK_BOT_TOKEN "${POSTER_TOKEN}"
probe -- --url "${URL}" --channel "${CHANNEL}" --probe-env "${TMP}/probe.env" --poster-env "${TMP}/poster.env" \
  --mention-env "${CASE}/same.env"
expect 1 '^FAIL bot identity: the poster and the mention poster are the same app \(B0POSTER\)' "one poster for both fails"
new_case bad-token
cred "${CASE}/bad.env" SLACK_BOT_TOKEN not-a-token
probe -- --url "${URL}" --channel "${CHANNEL}" --probe-env "${TMP}/probe.env" --poster-env "${TMP}/poster.env" \
  --mention-env "${CASE}/bad.env"
expect 1 '^FAIL bot identity: auth.test of the mention poster: ok=false error=invalid_auth$' "a token Slack refuses fails"
new_case no-reply
probe STUB_NO_REPLY=1 -- "${REQUIRED[@]}" --timeout 1
expect 1 '^FAIL alert trigger: no reply from B0PROBE in thread 1700000000.000001 within 1 s$' "no reply fails at the timeout"
expect 1 '^FAIL bot mention: no reply from B0PROBE in thread 1700000000.000002 within 1 s$' "no mention reply fails at the timeout"
new_case other-bot
probe STUB_REPLY_BOT=B0MENTION -- "${REQUIRED[@]}" --timeout 1
expect 1 '^FAIL alert trigger: no reply from B0PROBE' "a reply from another bot does not count"
no_secret "failing run"

echo "== refusals"
# refuses DESCRIPTION PATTERN ARG...: the probe exits 2, says PATTERN, and calls curl not once.
refuses() {
  local description=$1 pattern=$2
  shift 2
  probe -- "$@"
  expect 2 "${pattern}" "${description}"
  [ ! -s "${CASE}/calls" ] || fail "${description}: curl was called"
}
# set_flag FLAG VALUE: ARGS is REQUIRED with FLAG set to VALUE, or without FLAG when VALUE is the word unset.
set_flag() {
  local i
  ARGS=()
  for ((i = 0; i < ${#REQUIRED[@]}; i += 2)); do
    if [ "${REQUIRED[i]}" != "$1" ]; then
      ARGS+=("${REQUIRED[i]}" "${REQUIRED[i + 1]}")
    elif [ "$2" != unset ]; then
      ARGS+=("$1" "$2")
    fi
  done
}
new_case refusals
cred "${CASE}/open.env" botToken "${PROBE_TOKEN}" 644
cred "${CASE}/group.env" botToken "${PROBE_TOKEN}" 640
cred "${CASE}/nokey.env" SLACK_BOT_TOKEN "${PROBE_TOKEN}"
cred "${CASE}/empty.env" botToken ''
ln -s "${TMP}/probe.env" "${CASE}/link.env"
for flag in --url --channel --probe-env --poster-env --mention-env; do
  set_flag "${flag}" unset
  refuses "no ${flag}" "^error: ${flag} is required" "${ARGS[@]}"
done
set_flag --probe-env "${CASE}/missing.env"
refuses "a missing file" 'missing.env: no such file' "${ARGS[@]}"
set_flag --probe-env "${CASE}/open.env"
refuses "a file of mode 644" 'open.env must be mode 600, not 644' "${ARGS[@]}"
set_flag --probe-env "${CASE}/group.env"
refuses "a file of mode 640" 'group.env must be mode 600, not 640' "${ARGS[@]}"
set_flag --probe-env "${CASE}/link.env"
refuses "a symbolic link" 'link.env is a symbolic link' "${ARGS[@]}"
set_flag --probe-env "${CASE}/nokey.env"
refuses "a probe file without botToken" 'nokey.env has no botToken= line with a value' "${ARGS[@]}"
set_flag --probe-env "${CASE}/empty.env"
refuses "an empty botToken" 'empty.env has no botToken= line with a value' "${ARGS[@]}"
set_flag --poster-env "${TMP}/probe.env"
refuses "a poster file without SLACK_BOT_TOKEN" 'probe.env has no SLACK_BOT_TOKEN= line with a value' "${ARGS[@]}"
set_flag --url https://chat.example.test/slack/agent-kourier/probe
refuses "a URL that is no connection's" '^error: --url must be http\(s\)://<host>/chat/slack/<namespace>/<name>' "${ARGS[@]}"
set_flag --url 'https://chat.example.test/chat/slack/agent-kourier/probe?x=1'
refuses "a URL with a query" '^error: --url must be' "${ARGS[@]}"
set_flag --channel general
refuses "a channel name" '^error: --channel must be a channel ID' "${ARGS[@]}"
refuses "a timeout that is not a number" '^error: --timeout must be' "${REQUIRED[@]}" --timeout soon
refuses "a timeout of 0" '^error: --timeout must be' "${REQUIRED[@]}" --timeout 0
refuses "an empty alert prefix" '^error: --alert-prefix must not be empty' "${REQUIRED[@]}" --alert-prefix ''
refuses "a metrics URL that is not http" '^error: --metrics-url must be' "${REQUIRED[@]}" --metrics-url localhost:9090
refuses "an unknown flag" '^error: unknown argument --verbose' "${REQUIRED[@]}" --verbose
refuses "a flag with no value" '^error: --channel needs a value' --channel

echo "== timeout forms"
new_case timeout-s
probe -- "${REQUIRED[@]}" --timeout 90s
expect 0 '^all 7 checks passed$' "a timeout in seconds with an s"

echo "== help"
new_case help
probe -- --help
expect 0 'botMentions: none' "the help names botMentions: none"
expect 0 'button press' "the help names the press, left manual"
expect 0 'leader' "the help names the killed leader, left manual"
[ ! -s "${CASE}/calls" ] || fail "--help called curl"

echo "== shellcheck"
if command -v shellcheck >/dev/null; then
  shellcheck -x "${SCRIPT}" "${HERE}/test.sh"
  echo "ok: shellcheck is clean"
else
  echo "skip: shellcheck is not installed"
fi

echo "all http-probe tests passed"


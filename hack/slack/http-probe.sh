#!/usr/bin/env bash
# Replays, against a live install, the checks that prove a ChatConnection in Slack's HTTP mode against Slack itself.
# Run `hack/slack/http-probe.sh --help` for its inputs, its checks and what it leaves to be done by hand. Needs bash,
# curl (7.55 or later, for -H @file) and jq.
set -euo pipefail

SLACK_API=https://slack.com/api
# What the chat listener answers, with 503, for a connection that does not run on the replica (internal/broker/chatlistener.go).
NOT_HERE='the connection does not run on this replica'
METRIC=agentkourier_slack_http_requests_total

usage() {
  cat <<'EOF'
Usage: hack/slack/http-probe.sh --url URL --channel ID --probe-env FILE --poster-env FILE --mention-env FILE
                                [--alert-prefix TEXT] [--timeout SECONDS] [--metrics-url URL]

Checks a ChatConnection in Slack's HTTP mode against Slack itself: what it answers to requests that Slack did not sign,
and that real Slack events reach it and get a reply. It posts two messages in the channel.

  --url URL            the connection's public request URL, https://<host>/chat/slack/<namespace>/<name>
  --channel ID         the channel ID (C... or G...) the connection's Binding answers in
  --probe-env FILE     the connection's own app: a mode-600 file with botToken= (a signingSecret= line is never read)
  --poster-env FILE    a second app, which posts the alert: a mode-600 file with SLACK_BOT_TOKEN=
  --mention-env FILE   a third app, which posts the mention: a mode-600 file with SLACK_BOT_TOKEN=
  --alert-prefix TEXT  what the Binding's botMessage trigger matches at the start of the alert (default "FIRING: ")
  --timeout SECONDS    how long to wait for each reply (default 120)
  --metrics-url URL    the leading replica's metrics endpoint, port-forwarded; adds the metrics check
  -h, --help           this help

The checks, each printed as PASS or FAIL with one line of evidence:
  signatures        POSTs an event_callback to --url with a fresh timestamp and a wrong v0= signature, with no
                    signature headers, and with a timestamp 3600 s old. Each must be answered 401.
  unknown connection
                    POSTs to --url with its last segment replaced by a random name: 503, "the connection does not
                    run on this replica".
  bot identity      auth.test of the three tokens: they must be three different apps.
  alert trigger     the poster posts "<prefix>KourierProbe<random>"; the probe's bot must reply in its thread.
  bot mention       the mention poster posts "<@probe> live probe <random>: ..."; the probe's bot must reply in its
                    thread.
  metrics           with --metrics-url, agentkourier_slack_http_requests_total for the connection, read before the
                    first check and after the last: bad_signature +2, stale +1, acknowledged at least +2.
It exits 0 when every check passes, 1 when one fails, and 2 when it refuses its inputs (before any request).

What the Binding of --channel must have:
  - a botMessage trigger whose from.id is the poster's bot ID and that matches the alert's text, with any extract
    finding its values there, and whose limits leave a run;
  - a mention trigger (or no triggers, which is the mention trigger), and chat.botMentions any, its default. A Binding
    with botMentions: none fails the bot mention check. A bot that a botMessage trigger names is never taken as
    a mention, so the mention poster must be an app that no botMessage trigger of the Binding names: that is why it
    is a third app. The bot mentions of one Binding start at most 3 turns in 30 minutes and 20 in a day, so a fourth
    run within 30 minutes fails that check.
The poster and the mention poster need chat:write and chat:write.public (or membership of the channel). The probe's
token reads the threads with conversations.replies (channels:history, or groups:history for a private channel).
With more than one replica, --metrics-url must reach the leader, which serves the connection.

Left to be done by hand:
  - a button press: Slack checks no Interactivity URL, so press a button on a reply (the feedback buttons) and see it
    taken (agentkourier_slack_http_requests_total acknowledged, and the vote recorded);
  - a killed leader: delete the leading pod while a message is posted, and see one reply in the thread and
    agentkourier_slack_http_retries_total count Slack's retry on the new leader.

No token is put on a command line, in the environment or in the output: each Authorization header goes to curl from
a file in a private temporary directory, which is removed on exit. Only fields such as ok, error, ts and bot_id of
Slack's answers are printed.
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  echo "run hack/slack/http-probe.sh --help for its inputs" >&2
  exit 2
}

url='' channel='' probe_env='' poster_env='' mention_env='' metrics_url=''
prefix='FIRING: '
timeout=120
while [ $# -gt 0 ]; do
  case "$1" in
    -h | --help)
      usage
      exit 0
      ;;
    --url | --channel | --probe-env | --poster-env | --mention-env | --alert-prefix | --timeout | --metrics-url)
      [ $# -ge 2 ] || die "$1 needs a value"
      case "$1" in
        --url) url=$2 ;;
        --channel) channel=$2 ;;
        --probe-env) probe_env=$2 ;;
        --poster-env) poster_env=$2 ;;
        --mention-env) mention_env=$2 ;;
        --alert-prefix) prefix=$2 ;;
        --timeout) timeout=$2 ;;
        --metrics-url) metrics_url=$2 ;;
      esac
      shift 2
      ;;
    *) die "unknown argument $1" ;;
  esac
done

[ -n "${url}" ] || die "--url is required"
[ -n "${channel}" ] || die "--channel is required"
[ -n "${probe_env}" ] || die "--probe-env is required"
[ -n "${poster_env}" ] || die "--poster-env is required"
[ -n "${mention_env}" ] || die "--mention-env is required"
segment='[^/?#[:space:]]+'
[[ ${url} =~ ^https?://${segment}(/${segment})*/chat/slack/${segment}/${segment}$ ]] ||
  die "--url must be http(s)://<host>/chat/slack/<namespace>/<name>, with no query or trailing slash"
[[ ${channel} =~ ^[CG][A-Z0-9]+$ ]] || die "--channel must be a channel ID (C... or G...), not a name"
[[ ${timeout} =~ ^[1-9][0-9]*s?$ ]] || die "--timeout must be a whole number of seconds above 0, such as 120 or 120s"
timeout=${timeout%s}
[ -n "${prefix}" ] || die "--alert-prefix must not be empty"
[ -z "${metrics_url}" ] || [[ ${metrics_url} =~ ^https?:// ]] || die "--metrics-url must be an http(s) URL"

# The connection's namespace/name, the value of the metric's connection label: the URL's last two segments.
name=${url##*/}
namespace=${url%/*}
namespace=${namespace##*/}
connection=${namespace}/${name}

umask 077
TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

# mode_of FILE: the permission bits in octal, with GNU stat or BSD stat (as hack/clip/record.sh reads them).
mode_of() {
  stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"
}

# load FILE KEY HEADER: check FILE and write the Authorization header of its KEY's value to HEADER, or refuse.
load() {
  local file=$1 key=$2 mode value
  [ ! -L "${file}" ] || die "${file} is a symbolic link; pass the file itself"
  [ -e "${file}" ] || die "${file}: no such file"
  [ -f "${file}" ] || die "${file} is not a regular file"
  mode=$(mode_of "${file}")
  [ "${mode}" = 600 ] || die "${file} must be mode 600, not ${mode}: it holds a token"
  value=$(sed -n "s/^${key}=//p" "${file}" | head -n 1)
  value=${value%$'\r'}
  case "${value}" in
    \"*\" | \'*\') value=${value:1:${#value}-2} ;;
  esac
  [ -n "${value}" ] || die "${file} has no ${key}= line with a value"
  printf 'Authorization: Bearer %s\n' "${value}" >"$3"
}
PROBE_AUTH=${TMP}/probe.auth
POSTER_AUTH=${TMP}/poster.auth
MENTION_AUTH=${TMP}/mention.auth
load "${probe_env}" botToken "${PROBE_AUTH}"
load "${poster_env}" SLACK_BOT_TOKEN "${POSTER_AUTH}"
load "${mention_env}" SLACK_BOT_TOKEN "${MENTION_AUTH}"

CHECKS=0 FAILED=0
pass() {
  CHECKS=$((CHECKS + 1))
  printf 'PASS %s\n' "$*"
}
fail() {
  CHECKS=$((CHECKS + 1))
  FAILED=$((FAILED + 1))
  printf 'FAIL %s\n' "$*"
}

# field FILE FILTER: jq's FILTER of the JSON in FILE, nothing when it is null or FILE is not JSON.
field() {
  jq -r "$2 | values" "$1" 2>/dev/null || true
}

# slack METHOD AUTH OUT [CURL ARG...]: call Slack's Web API METHOD with the header file AUTH, the answer in OUT. It fails
# when curl does.
slack() {
  local method=$1 auth=$2 out=$3
  shift 3
  curl -sS -m 20 -o "${out}" -H "@${auth}" "$@" "${SLACK_API}/${method}" 2>/dev/null
}

RAND=$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')

# read_metrics OUT: the metrics endpoint's answer in OUT; it fails when curl does.
read_metrics() {
  curl -fsS -m 20 -o "$1" "${metrics_url}" 2>/dev/null
}

# series FILE OUTCOME: the value of the metric for the connection and OUTCOME in FILE, 0 when it has no such series.
series() {
  awk -v metric="${METRIC}{" -v conn="connection=\"${connection}\"" -v outcome="outcome=\"$2\"" '
    index($0, metric) == 1 && index($0, conn) && index($0, outcome) { sub(/^[^}]*\} /, ""); sum += $1 }
    END { printf "%d\n", sum }' "$1"
}

metrics_before=
if [ -n "${metrics_url}" ]; then
  read_metrics "${TMP}/metrics.before" && metrics_before=${TMP}/metrics.before
fi

echo "== requests Slack did not sign, to ${url}"
printf '%s\n' '{"token":"kourier-probe","team_id":"T0","api_app_id":"A0","type":"event_callback","event_id":"Ev0KOURIERPROBE","event_time":0,"event":{"type":"message","text":"kourier probe"}}' \
  >"${TMP}/event.json"
# post_event URL [HEADER...]: POST the event to URL with HEADERs; prints the status (000 when curl got none), the
# answer's body in ${TMP}/answer.
post_event() {
  local target=$1 code headers=() h
  shift
  for h in "$@"; do headers+=(-H "${h}"); done
  code=$(curl -sS -m 20 -o "${TMP}/answer" -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    ${headers[@]+"${headers[@]}"} --data-binary "@${TMP}/event.json" "${target}" 2>/dev/null) || true
  printf '%s' "${code:-000}"
}
# want_401 WHAT [HEADER...]: the check that a request with HEADERs is answered 401.
want_401() {
  local what=$1 code
  shift
  code=$(post_event "${url}" "$@")
  if [ "${code}" = 401 ]; then
    pass "signatures: ${what}: HTTP 401"
  else
    fail "signatures: ${what}: HTTP ${code}, want 401"
  fi
}
now=$(date +%s)
zeros=$(printf '%064d' 0)
want_401 "a wrong v0= signature with a fresh timestamp" "X-Slack-Request-Timestamp: ${now}" "X-Slack-Signature: v0=${zeros}"
want_401 "no signature headers"
want_401 "a timestamp 3600 s old" "X-Slack-Request-Timestamp: $((now - 3600))" "X-Slack-Signature: v0=${zeros}"
unknown=${url%/*}/kourier-probe-${RAND}
code=$(post_event "${unknown}")
if [ "${code}" = 503 ] && grep -qF "${NOT_HERE}" "${TMP}/answer"; then
  pass "unknown connection: ${unknown}: HTTP 503, ${NOT_HERE}"
else
  fail "unknown connection: ${unknown}: HTTP ${code}, want 503 and \"${NOT_HERE}\""
fi

echo "== Slack"
# identify LABEL AUTH: prints "USER_ID BOT_ID" of the token in AUTH, or why it could not, and fails.
identify() {
  local out=${TMP}/auth-test.json ok bot
  slack auth.test "$2" "${out}" -X POST || {
    echo "auth.test of the $1: curl failed"
    return 1
  }
  ok=$(field "${out}" .ok)
  bot=$(field "${out}" .bot_id)
  if [ "${ok}" != true ]; then
    echo "auth.test of the $1: ok=${ok:-none} error=$(field "${out}" .error)"
    return 1
  fi
  [ -n "${bot}" ] || {
    echo "auth.test of the $1: no bot_id: not a bot token"
    return 1
  }
  printf '%s %s\n' "$(field "${out}" .user_id)" "${bot}"
}
identities() {
  local who
  who=$(identify probe "${PROBE_AUTH}") || {
    echo "${who}"
    return 1
  }
  read -r probe_user probe_bot <<<"${who}"
  who=$(identify poster "${POSTER_AUTH}") || {
    echo "${who}"
    return 1
  }
  read -r _ poster_bot <<<"${who}"
  who=$(identify "mention poster" "${MENTION_AUTH}") || {
    echo "${who}"
    return 1
  }
  read -r _ mention_bot <<<"${who}"
  if [ "${probe_bot}" = "${poster_bot}" ]; then
    echo "the probe and the poster are the same app (${probe_bot})"
  elif [ "${probe_bot}" = "${mention_bot}" ]; then
    echo "the probe and the mention poster are the same app (${probe_bot})"
  elif [ "${poster_bot}" = "${mention_bot}" ]; then
    echo "the poster and the mention poster are the same app (${poster_bot}): a bot that the alert's botMessage trigger names is never taken as a mention"
  else
    return 0
  fi
  return 1
}
probe_user='' probe_bot='' poster_bot='' mention_bot=''
identified=false
# identities runs in this shell, so that the IDs it reads stay set; its message goes through a file.
if identities >"${TMP}/identities"; then
  identified=true
  pass "bot identity: probe ${probe_user}/${probe_bot}, poster ${poster_bot}, mention poster ${mention_bot}"
else
  fail "bot identity: $(cat "${TMP}/identities")"
fi

# post AUTH TEXT: post TEXT in the channel and print its ts, or print why it could not and fail.
post() {
  local out=${TMP}/post.json body=${TMP}/post-body.json
  jq -n --arg channel "${channel}" --arg text "$2" '{channel: $channel, text: $text}' >"${body}"
  slack chat.postMessage "$1" "${out}" -H 'Content-Type: application/json; charset=utf-8' --data-binary "@${body}" || {
    echo "chat.postMessage: curl failed"
    return 1
  }
  if [ "$(field "${out}" .ok)" != true ]; then
    echo "chat.postMessage: ok=$(field "${out}" .ok) error=$(field "${out}" .error)"
    return 1
  fi
  field "${out}" .ts
}

# wait_reply TS: poll the thread of TS with the probe's token until a message of the probe's bot is in it, and print
# that message's ts; or print why not, and fail, once the timeout has passed or Slack refused the read.
wait_reply() {
  local ts=$1 out=${TMP}/replies.json deadline=$((SECONDS + timeout)) interval=3 reply err
  [ "${timeout}" -ge "${interval}" ] || interval=${timeout}
  while :; do
    if slack conversations.replies "${PROBE_AUTH}" "${out}" -G --data-urlencode "channel=${channel}" --data-urlencode "ts=${ts}"; then
      if [ "$(field "${out}" .ok)" = true ]; then
        reply=$(jq -r --arg bot "${probe_bot}" --arg parent "${ts}" \
          '[.messages[]? | select(.bot_id == $bot and .ts != $parent)][0].ts | values' "${out}" 2>/dev/null || true)
        if [ -n "${reply}" ]; then
          printf '%s\n' "${reply}"
          return 0
        fi
      else
        err=$(field "${out}" .error)
        if [ "${err}" != ratelimited ]; then
          echo "conversations.replies of thread ${ts}: ok=false error=${err}"
          return 1
        fi
      fi
    fi
    [ "${SECONDS}" -lt "${deadline}" ] || break
    sleep "${interval}"
  done
  echo "no reply from ${probe_bot} in thread ${ts} within ${timeout} s"
  return 1
}

# turn CHECK AUTH TEXT: the check that TEXT, posted with AUTH, gets a reply from the probe's bot in its thread.
turn() {
  local check=$1 ts reply
  if ! ${identified}; then
    fail "${check}: skipped: no three distinct bot identities"
    return 0
  fi
  if ! ts=$(post "$2" "$3"); then
    fail "${check}: ${ts}"
    return 0
  fi
  if reply=$(wait_reply "${ts}"); then
    pass "${check}: ${probe_bot} replied at ${reply} in thread ${ts}"
  else
    fail "${check}: ${reply}"
  fi
}
turn "alert trigger" "${POSTER_AUTH}" "${prefix}KourierProbe${RAND}"
turn "bot mention" "${MENTION_AUTH}" "<@${probe_user}> live probe ${RAND}: reply with one short sentence and call no tools."

if [ -n "${metrics_url}" ]; then
  echo "== metrics, ${METRIC}{connection=\"${connection}\"}"
  if [ -z "${metrics_before}" ]; then
    fail "metrics: could not read ${metrics_url} before the checks"
  elif ! read_metrics "${TMP}/metrics.after"; then
    fail "metrics: could not read ${metrics_url} after the checks"
  else
    ok=true evidence=
    # delta OUTCOME WANT EXACT: append OUTCOME's increment to the evidence; ok=false unless it is WANT (EXACT true) or at least WANT.
    delta() {
      local d=$(($(series "${TMP}/metrics.after" "$1") - $(series "${metrics_before}" "$1")))
      evidence+="${evidence:+, }$1 +${d}"
      if ${3} && [ "${d}" -ne "$2" ]; then
        evidence+=" (want +$2)"
        ok=false
      elif ! ${3} && [ "${d}" -lt "$2" ]; then
        evidence+=" (want at least +$2)"
        ok=false
      fi
    }
    delta bad_signature 2 true
    delta stale 1 true
    delta acknowledged 2 false
    if ${ok}; then pass "metrics: ${connection}: ${evidence}"; else fail "metrics: ${connection}: ${evidence}"; fi
  fi
else
  echo "SKIP metrics: no --metrics-url"
fi

if [ "${FAILED}" -eq 0 ]; then
  echo "all ${CHECKS} checks passed"
  exit 0
fi
echo "${FAILED} of ${CHECKS} checks failed"
exit 1

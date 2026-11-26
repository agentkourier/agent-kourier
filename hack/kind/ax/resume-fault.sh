#!/usr/bin/env bash
# Prove that an actor can run before AX delivers its task, then retry Resume.
# Run on the dedicated AX tier while its E2E Job is idle.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source-path=SCRIPTDIR source=../lib.sh
. "${HERE}/lib.sh"
use_tier ax
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
assert_context ax
assert_owned
job=$("${K[@]}" --request-timeout=15s -n e2e get job e2e-runner --ignore-not-found -o name)
if [ -n "${job}" ]; then
  die "wait for the AX E2E Job before injecting the router fault"
fi
AX_CLI=${AX_CLI:-ax}
command -v "${AX_CLI}" >/dev/null || die "set AX_CLI to the AX release CLI used by the tier"
"${K[@]}" -n ate-workers rollout status deploy/ax-workers --timeout=90s
router_replicas=$("${K[@]}" -n ate-system get deploy/atenet-router -o jsonpath='{.spec.replicas}')
[[ "${router_replicas}" =~ ^[1-9][0-9]*$ ]] || die "the AX router must be available before the test"
turn_timeout=$("${K[@]}" -n agent-kourier get deploy/agent-kourier-ax \
  -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="AX_TURN_TIMEOUT")].value}')
[ -n "${turn_timeout}" ] || die "the AX launcher must have a configured deadline"
watchdog=$("${K[@]}" -n agent-kourier get deploy/agent-kourier \
  -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="AGENTKOURIER_TEST_AGENT_STREAM_TIMEOUT")].value}')
[ -n "${watchdog}" ] || die "the AX tier must have a configured test watchdog"
umask 077
scratch=$(mktemp -d)
slack_forward=
ax_forward=
changed_timeout=false
changed_watchdog=false
changed_router=false

restore_router() {
  "${K[@]}" -n ate-system scale deploy/atenet-router --replicas="${router_replicas}" >/dev/null || return
  "${K[@]}" -n ate-system rollout status deploy/atenet-router --timeout=90s
}

restore() {
  local code=$?
  if "${changed_router}"; then restore_router || code=1; fi
  if "${changed_timeout}"; then
    if "${K[@]}" -n agent-kourier set env deploy/agent-kourier-ax "AX_TURN_TIMEOUT=${turn_timeout}" >/dev/null; then
      "${K[@]}" -n agent-kourier rollout status deploy/agent-kourier-ax --timeout=150s || code=1
    else
      code=1
    fi
  fi
  if "${changed_watchdog}"; then
    if "${K[@]}" -n agent-kourier set env deploy/agent-kourier "AGENTKOURIER_TEST_AGENT_STREAM_TIMEOUT=${watchdog}" >/dev/null; then
      "${K[@]}" -n agent-kourier rollout status deploy/agent-kourier --timeout=150s || code=1
    else
      code=1
    fi
  fi
  for pid in "${ax_forward}" "${slack_forward}"; do
    [ -z "${pid}" ] || kill "${pid}" 2>/dev/null || true
  done
  rm -rf "${scratch}"
  return "${code}"
}
trap restore EXIT
trap 'exit 130' INT TERM

forwarded_port() {
  local file=$1 port
  for _ in {1..75}; do
    port=$(sed -nE 's/^Forwarding from 127\.0\.0\.1:([0-9]+) .*/\1/p' "${file}")
    if [ -n "${port}" ]; then
      echo "${port}"
      return
    fi
    sleep 0.2
  done
  die "port-forward did not start"
}

"${K[@]}" -n agent-kourier set env deploy/agent-kourier-ax AX_TURN_TIMEOUT=90s >/dev/null
changed_timeout=true
"${K[@]}" -n agent-kourier rollout status deploy/agent-kourier-ax --timeout=150s
# Keep the stream open through the fault. A separate Job exercises reconnect.
"${K[@]}" -n agent-kourier set env deploy/agent-kourier AGENTKOURIER_TEST_AGENT_STREAM_TIMEOUT=120s >/dev/null
changed_watchdog=true
"${K[@]}" -n agent-kourier rollout status deploy/agent-kourier --timeout=150s
"${K[@]}" -n e2e port-forward svc/fakeslack :8080 >"${scratch}/slack.forward" 2>&1 &
slack_forward=$!
"${K[@]}" -n ax-system port-forward svc/ax-server :8080 >"${scratch}/ax.forward" 2>&1 &
ax_forward=$!
slack_port=$(forwarded_port "${scratch}/slack.forward")
ax_port=$(forwarded_port "${scratch}/ax.forward")

"${K[@]}" -n ate-system scale deploy/atenet-router --replicas=0 >/dev/null
changed_router=true
"${K[@]}" -n ate-system wait --for=delete pod -l app=atenet-router --timeout=90s
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
marker=AX_RESULT_OK-resume-fault-$(openssl rand -hex 8)
slack="http://127.0.0.1:${slack_port}/_control"
channel=CAXLAUNCH
curl -fsS --max-time 10 -H 'Content-Type: application/json' --data \
  '{"channel":"CAXLAUNCH","users":["UFAKEBOT","UAX"]}' "${slack}/members" >/dev/null
root_ts=$(jq -nc --arg channel "${channel}" --arg message "<@UFAKEBOT> ${marker}" \
  '{channel:$channel,user:"UAX",text:$message}' |
  curl -fsS --max-time 10 -H 'Content-Type: application/json' --data-binary @- "${slack}/mention" |
  jq -er .ts)

thread() {
  curl -fsSG --max-time 5 --data-urlencode "channel=${channel}" --data-urlencode "ts=${root_ts}" "${slack}/thread"
}

logs() {
  "${K[@]}" -n ax-system logs deploy/ax-server --since-time="${since}"
}
name=
running=false
failed=false
for _ in {1..55}; do
  logs >"${scratch}/ax.log"
  name=$(jq -r 'select(.msg == "resuming actor on Substrate worker")|.actor' "${scratch}/ax.log" | tail -n 1)
  if [ -n "${name}" ] && ! "${running}"; then
    if "${AX_CLI}" --server "127.0.0.1:${ax_port}" get task "${name}" >"${scratch}/task"; then
      if grep -Eq '^[[:space:]]+state: TASK_STATE_RUNNING$' "${scratch}/task"; then
        running=true
        echo 'The actor is running with the router unavailable.'
      fi
    fi
  fi
  if jq -e --arg name "${name}" '
    select(.task == $name and .msg == "resuming task on substrate failed" and
      ((.error // "") | contains("did not accept the task")))
  ' "${scratch}/ax.log" >/dev/null; then
    failed=true
    break
  fi
  sleep 0.5
done
if ! "${running}" || ! "${failed}"; then
  die "did not observe a running actor and an unsuccessful initial Resume"
fi
for notice in 'AX sandbox queued.' 'AX sandbox running.'; do
  visible=false
  for _ in {1..30}; do
    if thread | jq -e --arg notice "${notice}" '
      [ .[] | select(.bot_id and .streaming) | .chunks[]? | .. | strings |
        select(contains($notice)) ] | length == 1
    ' >/dev/null; then
      visible=true
      break
    fi
    sleep 0.2
  done
  "${visible}" || die "the live Slack thread must announce ${notice} once"
done
restore_router
changed_router=false
completed=false
for _ in {1..80}; do
  if thread | jq -e --arg marker "${marker}" '
    [ .[] | select(.bot_id) ] |
    length == 1 and any(.[]; (.streaming | not) and
      any((.text, .blocks[]?, .chunks[]?) | .. | strings; contains($marker)))
  ' >/dev/null; then
    completed=true
    break
  fi
  sleep 0.25
done
"${completed}" || die "the original Slack thread did not receive one final fixture response"
attempts=$(logs | jq -r --arg name "${name}" 'select(.actor==$name and .msg=="resuming actor on Substrate worker")|.actor' | wc -l)
[ "${attempts}" -ge 2 ] || die "the original AX Task did not record a Resume retry"
created=$(logs | jq -r --arg name "${name}" 'select(.name==$name and .msg=="task successfully suspended")|.name' | wc -l)
[ "${created}" -eq 1 ] || die "the retry did not reuse the original AX Task"
echo "The original AX Task completed after ${attempts// /} Resume attempts."

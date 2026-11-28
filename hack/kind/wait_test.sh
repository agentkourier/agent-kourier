#!/usr/bin/env bash
# Tests for e2e-run.sh's wait on the runner Job (lib.sh: wait_job, HOST_PAUSE_SECONDS) and for the order of the three limits on a run. They
# need no cluster: kubectl is a function that answers from a script of Job states, and sleep is a function that moves bash's SECONDS on
# instead of sleeping, so that a pause of the host takes no time.
# Run as `make test` does: hack/kind/wait_test.sh. Exit 0: all passed.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"

POLLS=$(mktemp)
CALLS=$(mktemp)
ERR=$(mktemp)
trap 'rm -f "${POLLS}" "${CALLS}" "${ERR}"' EXIT

fails=0
check() { # DESCRIPTION WANT GOT
  if [ "$2" = "$3" ]; then echo "ok: $1"; else echo "FAIL: $1: want [$2], got [$3]" >&2; fails=$((fails + 1)); fi
}
within() { # DESCRIPTION LOW HIGH GOT: SECONDS also counts the real time the test takes, so a measured gap may be a second longer
  if [ "$4" -ge "$2" ] && [ "$4" -le "$3" ]; then echo "ok: $1"; else echo "FAIL: $1: want $2 to $3, got [$4]" >&2; fails=$((fails + 1)); fi
}

# STATES: the Job's state at each poll ("<succeeded>/<failed>"), the last one repeated. kubectl runs in a command substitution, a subshell,
# so it counts its polls in a file.
STATES=()
fake_kubectl() {
  local n
  echo "$*" >>"${CALLS}"
  n=$(($(wc -l <"${POLLS}") + 0))
  echo poll >>"${POLLS}"
  [ "${n}" -lt "${#STATES[@]}" ] || n=$((${#STATES[@]} - 1))
  printf '%s' "${STATES[${n}]}"
}
K=(fake_kubectl --context kind-agent-kourier-fast)

# PAUSES: how far each call of sleep moves the clock, the last one repeated: 2 is a poll's own sleep, more is the host pausing in it.
PAUSES=(2)
SLEEPS=0
sleep() {
  local i=${SLEEPS}
  # A wait_job that ignores its deadline fails here instead of spinning for ever.
  [ "${SLEEPS}" -lt 10000 ] || { echo "FAIL: wait_job slept 10000 times: it never ended" >&2; exit 1; }
  [ "${i}" -lt "${#PAUSES[@]}" ] || i=$((${#PAUSES[@]} - 1))
  SECONDS=$((SECONDS + PAUSES[i]))
  SLEEPS=$((SLEEPS + 1))
}

run() { # WAIT: wait_job on a fresh script; its stderr goes to ERR
  : >"${POLLS}"
  : >"${CALLS}"
  SLEEPS=0
  SECONDS=0
  wait_job e2e-runner "$1" 2>"${ERR}"
}

echo "== wait_job: the Job's outcome"
STATES=(/ / 1/) PAUSES=(2)
run 60
check "a Job that succeeds on the third poll succeeded" 0 "${JOB_RESULT}"
check "it was polled three times" 3 "$(wc -l <"${POLLS}" | tr -d ' ')"
check "no pause" 0 "${HOST_PAUSED}"
check "and nothing on stderr" "" "$(cat "${ERR}")"
check "each poll has a request timeout, so a hung API server cannot pass for a pause" \
  "--context kind-agent-kourier-fast -n e2e get job e2e-runner --request-timeout=10s -o jsonpath={.status.succeeded}/{.status.failed}" \
  "$(head -1 "${CALLS}")"

STATES=(/ /1) PAUSES=(2)
run 60
check "a Job that fails failed" 1 "${JOB_RESULT}"

STATES=(/) PAUSES=(2)
run 20
check "a Job that never ends timed out" 124 "${JOB_RESULT}"
check "and the host did not pause" 0 "${HOST_PAUSED}"

echo "== wait_job: a pause of the host"
# Run 8 of 2026-11-27, cut down: the host sleeps for 571 seconds in the middle of the wait, and the Job is still running when the wait ends.
STATES=(/) PAUSES=(2 2 2 571 2)
run 600
check "a run the host slept through times out" 124 "${JOB_RESULT}"
within "the 571 seconds asleep are the pause" 571 572 "${HOST_PAUSED}"
case "$(cat "${ERR}")" in
  "e2e: the wait did not run for 57"[12]"s, to "*": the host slept or froze, and the kind node and the tests most likely stopped with it") got=said ;;
  *) got="$(cat "${ERR}")" ;;
esac
check "stderr says the host paused when the pause ends" said "${got}"
check "in one line" 1 "$(wc -l <"${ERR}" | tr -d ' ')"

STATES=(/) PAUSES=(2 40 2 100 2)
run 300
within "two pauses add up" 140 142 "${HOST_PAUSED}"
check "each has its line" 2 "$(wc -l <"${ERR}" | tr -d ' ')"

STATES=(/ / / 1/) PAUSES=(2 300 2)
run 1320
check "a Job that succeeds after a pause succeeded" 0 "${JOB_RESULT}"
within "and the pause is still reported" 300 301 "${HOST_PAUSED}"

# A gap a slow poll can make (its sleep and a kubectl call up to its 10 second request timeout) is not a pause, nor is one just under
# HOST_PAUSE_SECONDS (29 and not 30: SECONDS may add a second of real time).
STATES=(/ / 1/) PAUSES=(12)
run 60
check "polls slowed to 12 seconds are no pause" 0 "${HOST_PAUSED}"
STATES=(/ / 1/) PAUSES=(29)
run 120
check "polls slowed to 29 seconds are no pause" 0 "${HOST_PAUSED}"

echo "== note_host_pause: the threshold, exactly"
# wait_job counts each gap, the one that ends in its last poll included, through note_host_pause; given the gap, the edge is exact.
HOST_PAUSED=0
note_host_pause 30 2>"${ERR}"
check "a gap of HOST_PAUSE_SECONDS is no pause" 0 "${HOST_PAUSED}"
check "and says nothing" "" "$(cat "${ERR}")"
note_host_pause 31 2>"${ERR}"
check "a gap one second longer is" 31 "${HOST_PAUSED}"
check "and has its line" 1 "$(wc -l <"${ERR}" | tr -d ' ')"

echo "== the order of the limits on a run"
# The test binary's -test.timeout must end a hang first, so that it fails with a goroutine dump, before e2e-run.sh's wait gives up and before
# the Job's activeDeadlineSeconds kills the pod. The binary starts its clock after the pod has started and TestMain has cleared the sink
# (up to 30 seconds), so the wait gives it a minute of room at least.
binary=$(sed -nE 's/.*"-test\.timeout=([0-9]+)m".*/\1/p' "${HERE}/e2e/runner.yaml")
wait=$(sed -nE 's/^WAIT_SECONDS=\$\{E2E_WAIT_SECONDS:-([0-9]+)\}$/\1/p' "${HERE}/e2e-run.sh")
deadline=$(sed -nE 's/^  activeDeadlineSeconds: ([0-9]+)$/\1/p' "${HERE}/e2e/runner.yaml")
check "runner.yaml has one -test.timeout in minutes" 1 "$(printf '%s\n' "${binary}" | grep -c .)"
check "e2e-run.sh has one default wait" 1 "$(printf '%s\n' "${wait}" | grep -c .)"
check "runner.yaml has one activeDeadlineSeconds" 1 "$(printf '%s\n' "${deadline}" | grep -c .)"
binary=$((binary * 60))
[ $((binary + 60)) -le "${wait}" ] && got=yes || got=no
check "the binary's timeout (${binary}s) ends a minute or more before the wait (${wait}s)" yes "${got}"
[ "${wait}" -lt "${deadline}" ] && got=yes || got=no
check "the wait (${wait}s) ends before the Job's deadline (${deadline}s)" yes "${got}"

if [ "${fails}" -gt 0 ]; then
  echo "${fails} check(s) failed" >&2
  exit 1
fi
echo "all passed"

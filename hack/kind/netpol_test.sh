#!/usr/bin/env bash
# Tests for the full tier's network policy check (lib.sh: network_policy_verdict, require_network_policy) and its heal
# (sandbox/heal.sh: heal_network_policy). They need no cluster: the two functions that touch one, kindnet_log_recent and
# policy_probe, are replaced by canned answers, and kubectl is a function that records its arguments.
# Run as `make test` does: hack/kind/netpol_test.sh. Exit 0: all passed.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
# shellcheck source-path=SCRIPTDIR source=sandbox/heal.sh
. "${HERE}/sandbox/heal.sh"

NETPOL_RETRY_SLEEP=0
CALLS=$(mktemp)
SEQ=$(mktemp)
FAKEBIN=$(mktemp -d)
trap 'rm -rf "${CALLS}" "${SEQ}" "${SEQ}.bak" "${FAKEBIN}"' EXIT
fake_kubectl() { echo "$*" >>"${CALLS}"; }
K=(fake_kubectl --context kind-agent-kourier-full)

fails=0
check() { # DESCRIPTION WANT GOT
  if [ "$2" = "$3" ]; then echo "ok: $1"; else echo "FAIL: $1: want [$2], got [$3]" >&2; fails=$((fails + 1)); fi
}

# The log of the incident of 2026-10-03, as kindnet wrote it, and a healthy one.
STALE_LOG='E1003 03:59:41.101010       1 reflector.go:200] "Failed to watch" err="failed to list *v1.Pod: Get \"https://172.20.0.3:6443/api/v1/pods?limit=500\": net/http: TLS handshake timeout" logger="UnhandledError" type="*v1.Pod"
W1003 04:00:12.202020       1 reflector.go:561] pkg/mod/k8s.io/client-go/tools/cache/reflector.go:243: http2: client connection lost'
CLEAN_LOG='I1004 16:48:20.223768       1 main.go:297] Handling node with IPs: map[172.20.0.4:{}]
I1004 16:48:20.224811       1 main.go:301] handling current node'

echo "== kindnet_log_failing"
kindnet_log_failing "${STALE_LOG}" && got=failing || got=clean
check "the stale log of 2026-10-03 is failing" failing "${got}"
kindnet_log_failing 'W "Failed to watch" err="net/http: TLS handshake timeout"' && got=failing || got=clean
check "a failed watch alone is failing" failing "${got}"
kindnet_log_failing 'W1003 reflector.go: http2: client connection lost' && got=failing || got=clean
check "a lost http2 connection alone is failing" failing "${got}"
kindnet_log_failing "${CLEAN_LOG}" && got=failing || got=clean
check "a healthy log is clean" clean "${got}"
kindnet_log_failing "" && got=failing || got=clean
check "an empty log (kindnet just restarted, or unreadable) is clean" clean "${got}"

echo "== policy_verdict: the probe's three numbers"
check "28, no connection made, front door answering: a drop by policy" enforced "$(policy_verdict 0 28 0.000000)"
check "28, no connection, and the front door unreachable too: kindnet has not learned the pod, no verdict" unknown "$(policy_verdict 1 28 0.000000)"
check "28 but the connection was made (the controller was only slow): stale" stale "$(policy_verdict 0 28 0.004213)"
check "0, any answer (200 included): not enforced" stale "$(policy_verdict 0 0 0.003)"
check "0 with no answer from the front door: not enforced all the same" stale "$(policy_verdict 1 0 0.003)"
check "7, connection refused: no verdict" unknown "$(policy_verdict 0 7 0.000000)"
check "6, no DNS: no verdict" unknown "$(policy_verdict 0 6 0.000000)"
check "empty, the pod gave no status: no verdict" unknown "$(policy_verdict "" "" "")"

echo "== policy_probe_parse: what the probe pod printed"
check "a PROBE line" "0 28 0.000000" "$(printf 'PROBE:0:28:0.000000\n' | policy_probe_parse)"
check "a copy printed twice (kubectl's log fallback): the first" "0 28 0.000000" "$(printf 'PROBE:0:28:0.000000\nPROBE:0:28:0.000000\n' | policy_probe_parse)"
check "a copy cut short and glued to the next is no verdict" "" "$(printf 'PROBE:0:2PROBE:0:28:0.000000x\n' | policy_probe_parse)"
check "noise around the line" "1 0 0.0021" "$(printf 'If you do not see a command prompt, try pressing enter.\nPROBE:1:0:0.0021\npod deleted\n' | policy_probe_parse)"
check "no line" "" "$(printf 'error: timed out waiting for the condition\n' | policy_probe_parse)"

echo "== NETPOL_PROBE_SCRIPT, run under a fake curl and sleep"
cat >"${FAKEBIN}/sleep" <<'SH'
#!/bin/sh
exit 0
SH
# The fake curl records its arguments and answers by URL: the door's by FAKE_DOOR_RC, the controller's by FAKE_CTRL_RC and
# FAKE_CONNECT (what -w "%{time_connect}" prints).
cat >"${FAKEBIN}/curl" <<'SH'
#!/bin/sh
echo "$*" >>"${FAKE_CURL_LOG}"
for url; do :; done
case "${url}" in
  door://*) exit "${FAKE_DOOR_RC}" ;;
  *) printf %s "${FAKE_CONNECT}"; exit "${FAKE_CTRL_RC}" ;;
esac
SH
chmod +x "${FAKEBIN}/sleep" "${FAKEBIN}/curl"
# run_probe DOOR_RC CTRL_RC CONNECT: the pod script's output.
run_probe() {
  : >"${FAKEBIN}/curl.log"
  FAKE_CURL_LOG="${FAKEBIN}/curl.log" FAKE_DOOR_RC=$1 FAKE_CTRL_RC=$2 FAKE_CONNECT=$3 PATH="${FAKEBIN}:${PATH}" \
    sh -c "${NETPOL_PROBE_SCRIPT}" sh door://control ctrl://controller
}
check "healthy: the door answers, the controller is dropped" "PROBE:0:28:0.000000" "$(run_probe 0 28 0.000000)"
check "stale: the door answers, and so does the controller" "PROBE:0:0:0.002100" "$(run_probe 0 0 0.002100)"
check "the door never answers: the line says so (a probe with no door check cannot)" "PROBE:1:28:0.000000" "$(run_probe 22 28 0.000000)"
check "the door is asked five times before that" 5 "$(run_probe 22 28 0.000000 >/dev/null; grep -c '^-sf -m 5 -o /dev/null door://control$' "${FAKEBIN}/curl.log")"
check "the door is asked once when it answers" 1 "$(run_probe 0 28 0.000000 >/dev/null; grep -c 'door://control' "${FAKEBIN}/curl.log")"
run_probe 0 28 0.000000 >/dev/null
check "the controller is asked with a connect timeout under the total one, to print time_connect" \
  "-s --connect-timeout 6 -m 8 -o /dev/null -w %{time_connect} ctrl://controller" "$(grep 'ctrl://' "${FAKEBIN}/curl.log")"
# What the script's line becomes through the parse, end to end.
check "the script's line parses" "0 28 0.000000" "$(run_probe 0 28 0.000000 | policy_probe_parse)"

# verdict LOG PROBE: the line network_policy_verdict gives for that log and that policy_probe line ("DOOR CONTROLLER CONNECT").
STUB_LOG='' STUB_PROBE=''
kindnet_log_recent() { printf '%s' "${STUB_LOG}"; }
policy_probe() { printf '%s' "${STUB_PROBE}"; }
verdict() {
  STUB_LOG=$1 STUB_PROBE=$2
  network_policy_verdict
}

echo "== the probe pod and its cleanup"
: >"${CALLS}"
# The real policy_probe (lib.sh again, in a subshell: the section above replaced it) against the fake kubectl, which runs nothing and prints nothing.
# shellcheck source-path=SCRIPTDIR source=lib.sh
got=$(. "${HERE}/lib.sh" && policy_probe)
check "a probe pod that prints nothing is no verdict, after three tries" "3:" "$(grep -c ' run probe-netpol-' "${CALLS}"):${got}"
check "the probe pod carries the Agent Kourier label and the probe label" 3 "$(grep -c -e '--labels=app.kubernetes.io/name=agent-kourier,agentkourier.agent-kourier.dev/probe=netpol' "${CALLS}")"
: >"${CALLS}"
delete_netpol_probes
check "before any probe could have run, the cleanup asks the cluster nothing" "" "$(cat "${CALLS}")"
NETPOL_PROBING=1
delete_netpol_probes
check "the traps delete the probe pods by their label, in Agent Kourier only" \
  "--context kind-agent-kourier-full -n agent-kourier delete pod -l agentkourier.agent-kourier.dev/probe=netpol --ignore-not-found --wait=false" "$(cat "${CALLS}")"

echo "== network_policy_verdict"
line=$(verdict "${CLEAN_LOG}" "0 28 0.000000")
check "clean log, timeout: enforced" enforced "${line%%:*}"
line=$(verdict "${STALE_LOG}" "0 28 0.000000")
check "failed watches but the probe timed out: still enforced, with a note" enforced "${line%%:*}"
[[ ${line} == *"TLS handshake"* ]] && got=noted || got=silent
check "that note names the failed watches" noted "${got}"
line=$(verdict "${CLEAN_LOG}" "0 0 0.003")
check "clean log, the controller answered: stale" stale "${line%%:*}"
line=$(verdict "${STALE_LOG}" "0 0 0.003")
check "failed watches and the controller answered: stale" stale "${line%%:*}"
[[ ${line} == *"8083"* && ${line} == *"controller-admit-frontdoor-only"* && ${line} == *"TLS handshake"* ]] && got=cause || got=vague
check "the stale line names the probe, the policy and the log" cause "${got}"
line=$(verdict "${CLEAN_LOG}" "0 7 0.000000")
check "clean log, connection refused: unknown" unknown "${line%%:*}"
line=$(verdict "${STALE_LOG}" "0 7 0.000000")
check "failed watches and no usable probe: stale (the log corroborates)" stale "${line%%:*}"
line=$(verdict "${CLEAN_LOG}" "0 28 0.004")
check "clean log, controller timed out after connecting: stale" stale "${line%%:*}"
line=$(verdict "${CLEAN_LOG}" "")
check "clean log, no probe status: unknown" unknown "${line%%:*}"
line=$(verdict "${CLEAN_LOG}" "1 28 0.000000")
check "clean log, controller timed out but so did the front door: unknown, not enforced" unknown "${line%%:*}"

echo "== require_network_policy"
set +e
msg=$(network_policy_verdict() { echo "enforced: fine"; }; require_network_policy 2>&1)
rc=$?
set -e
check "enforced: passes" 0 "${rc}"
[[ ${msg} == "ok: fine" ]] && got=said-ok || got=${msg}
check "enforced: says ok" said-ok "${got}"
set +e
msg=$(network_policy_verdict() { echo "stale: a pod reached the controller"; }; require_network_policy 2>&1)
rc=$?
set -e
check "stale: refuses" 1 "${rc}"
[[ ${msg} == *"a pod reached the controller"* && ${msg} == *"make sandbox-heal"* && ${msg} == *"rollout restart ds/kindnet"* ]] && got=cause-and-remedy || got=${msg}
check "stale: names the cause and both remedies" cause-and-remedy "${got}"
set +e
msg=$(network_policy_verdict() { echo "unknown: no status"; }; require_network_policy 2>&1)
rc=$?
set -e
check "unknown: refuses" 1 "${rc}"
# The probe pod is cleaned up by the callers' traps only once NETPOL_PROBING is set, so the gate must set it before it probes.
probing=$(unset NETPOL_PROBING; network_policy_verdict() { echo "enforced: probing=${NETPOL_PROBING:-unset}"; }; require_network_policy 2>&1 || true)
check "require sets NETPOL_PROBING before it probes, so a Ctrl-C cleans up the probe pod" "ok: probing=1" "${probing}"

echo "== heal_network_policy"
# sequence VERDICT...: network_policy_verdict gives these lines in turn, then the last again.
sequence() {
  printf '%s\n' "$@" >"${SEQ}"
  network_policy_verdict() {
    local first
    first=$(head -n 1 "${SEQ}")
    [ "$(wc -l <"${SEQ}")" -le 1 ] || sed -i.bak 1d "${SEQ}"
    echo "${first}"
  }
}
: >"${CALLS}"
sequence "enforced: fine"
msg=$(heal_network_policy)
check "healthy: says ok" "ok: fine" "${msg}"
check "healthy: runs no kubectl" "" "$(cat "${CALLS}")"

: >"${CALLS}"
sequence "stale: cause" "enforced: fine"
heal_network_policy >/dev/null
check "stale: restarts kindnet in kube-system of the full tier, then waits for it" \
  "--context kind-agent-kourier-full -n kube-system rollout restart ds/kindnet
--context kind-agent-kourier-full -n kube-system rollout status ds/kindnet --timeout=120s" "$(cat "${CALLS}")"

: >"${CALLS}"
sequence "stale: cause" "stale: cause" "enforced: fine"
msg=$(heal_network_policy)
check "stale, then a probe that has not caught up, then enforced: ok after the re-probes" "ok: kindnet was restarted and the network policy applies again" "${msg##*$'\n'}"

: >"${CALLS}"
sequence "stale: cause"
set +e
(heal_network_policy >/dev/null 2>&1)
rc=$?
set -e
check "still stale after the restart: fails" 1 "${rc}"

: >"${CALLS}"
sequence "unknown: no status"
set +e
(heal_network_policy >/dev/null 2>&1)
rc=$?
set -e
check "unknown: fails, and restarts nothing" "1:" "${rc}:$(cat "${CALLS}")"

probing=$(unset NETPOL_PROBING; network_policy_verdict() { echo "enforced: probing=${NETPOL_PROBING:-unset}"; }; heal_network_policy 2>&1 || true)
check "heal sets NETPOL_PROBING before it probes, so a Ctrl-C cleans up the probe pod" "ok: probing=1" "${probing}"

[ "${fails}" = 0 ] || {
  echo "${fails} failed" >&2
  exit 1
}
echo "all passed"

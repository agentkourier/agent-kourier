#!/usr/bin/env bash
# Heal the full tier's egress gateway (deploy/atenet-egress in ate-system) when its client certificate is, or is about to be, expired.
# Every agent connection to kagent, to the tool server and to the model's host leaves through that gateway, and after about a day it
# rejects them all (`actor identity check denied ... received fatal alert: CertificateExpired`), which kagent answers Agent Kourier as
# "session not ready" (-32004) for good, with the pod still Running and at 0 restarts: the gateway reads its pod certificate
# once, when it starts, and does not reload the one the kubelet rotates in. A restart makes it read a new one.
# Run as `make sandbox-heal` (it checks the full tier's cluster, then heals; for a long session), or sourced: up.sh calls
# heal_egress_gateway before it verifies. Needs jq. Exit 0: the gateway is fine, or was restarted and is rolled out. 1: it
# could not tell, or the restart did not finish. Every call names --context kind-agent-kourier-full.
# Other ate-system pods carry the same 24-hour pod certificates but show no sign of the expiry (see the README); they are not
# restarted here.
# It also heals kindnet (ds/kindnet in kube-system), the tier's network plugin: when it has lost the API server, NetworkPolicy stops
# applying to new pods (lib.sh, "The full tier's network policy"). heal_network_policy restarts it if an agent-kourier-labelled pod reaches
# the kagent controller, which the policy must refuse, and then proves the policy again. up.sh does not heal this one: it refuses.

# EGRESS_MAX_AGE_SECONDS: how old the gateway's pod may be before it is restarted whatever its logs say. Its pod certificates
# live 24 hours (maxExpirationSeconds 86400 on its PodCertificateRequests) and are read once, at start, so the pod's age is the
# certificate's; the logs catch one that went early, and `make sandbox-heal` restarts a gateway that a long session has outlived.
# A pod younger than this has at least four hours left of its 24-hour certificate when a session starts.
EGRESS_MAX_AGE_SECONDS=$((20 * 3600))
# How many of the last lines of a log are read for CertificateExpired (the gateway logs a health check every second).
EGRESS_LOG_TAIL=5000

# heal_egress_gateway: restart deploy/atenet-egress, and wait for its rollout, if its pod is older than EGRESS_MAX_AGE_SECONDS, or
# its own log or the worker pool's (deploy/kagent-default in kagent, whose actors are the ones turned away) holds CertificateExpired.
# The logs are read from the pod's creation on: the worker pool's log outlives the restart, so a line from before it is no
# reason for another. A log that cannot be read counts as clean (the age is the backstop). Needs K, die (lib.sh) and jq.
heal_egress_gateway() {
  local podlist created age reason="" log hours
  podlist=$("${K[@]}" -n ate-system get pods -l app=atenet-egress -o json)
  # The newest pod that is not on its way out is the one that serves, or will.
  created=$(jq -r '[.items[] | select(.metadata.deletionTimestamp == null)] | sort_by(.metadata.creationTimestamp)
    | last // empty | .metadata.creationTimestamp' <<<"${podlist}")
  [ -n "${created}" ] || die "no atenet-egress pod in ate-system; is the full tier up? (make e2e-full-up)"
  age=$(jq -rn --arg t "${created}" 'now - ($t | fromdateiso8601) | floor')
  hours=$((age / 3600))

  if [ "${age}" -ge "${EGRESS_MAX_AGE_SECONDS}" ]; then
    reason="its pod is ${hours}h old (the limit is $((EGRESS_MAX_AGE_SECONDS / 3600))h; its certificate lives 24h and is never reloaded)"
  else
    log=$("${K[@]}" -n ate-system logs deploy/atenet-egress --all-containers --since-time="${created}" --tail="${EGRESS_LOG_TAIL}" 2>/dev/null || true)
    if [[ ${log} == *CertificateExpired* ]]; then
      reason="its own log has CertificateExpired"
    else
      log=$("${K[@]}" -n kagent logs deploy/kagent-default --all-containers --since-time="${created}" --tail="${EGRESS_LOG_TAIL}" 2>/dev/null || true)
      if [[ ${log} == *CertificateExpired* ]]; then
        reason="the worker pool's log (kagent-default) has CertificateExpired from its connections through the gateway"
      fi
    fi
  fi

  if [ -z "${reason}" ]; then
    echo "ok: the egress gateway's certificate is fine (its pod is ${hours}h old, and no CertificateExpired in the logs since it started)"
    return 0
  fi
  echo "the egress gateway needs a restart: ${reason}"
  "${K[@]}" -n ate-system rollout restart deploy/atenet-egress >/dev/null
  "${K[@]}" -n ate-system rollout status deploy/atenet-egress --timeout=180s >/dev/null ||
    die "the egress gateway's restart did not finish in 180 s; look at it: ${K[*]} -n ate-system get pods -l app=atenet-egress"
  echo "ok: the egress gateway was restarted and has a new certificate"
}

# heal_network_policy: restart kindnet in kube-system of the full tier, and wait for it, if its network policy is not live
# (network_policy_verdict in lib.sh says stale); then probe again, up to three times five seconds apart, since a kindnet that has
# only just started takes a moment to learn the pods, and die if it still is not. Nothing is restarted for a verdict of unknown:
# a restart would not help a controller that is down. Needs K, die and network_policy_verdict (lib.sh).
heal_network_policy() {
  local line attempt
  NETPOL_PROBING=1
  line=$(network_policy_verdict)
  case "${line%%:*}" in
    enforced)
      echo "ok: ${line#*: }"
      return 0
      ;;
    stale) echo "kindnet needs a restart: ${line#*: }" ;;
    *) die "cannot tell whether the network policy is live in ${CLUSTER}: ${line#*: }; nothing was restarted" ;;
  esac
  restart_kindnet ||
    die "kindnet's restart did not finish in 120 s; look at it: ${K[*]} -n kube-system get pods -l app=kindnet"
  for attempt in 1 2 3; do
    line=$(network_policy_verdict)
    if [ "${line%%:*}" = enforced ]; then
      echo "ok: kindnet was restarted and the network policy applies again"
      return 0
    fi
    [ "${attempt}" -eq 3 ] || sleep "${NETPOL_RETRY_SLEEP}"
  done
  die "kindnet was restarted but the network policy is still not live: ${line#*: }"
}

# Run, not sourced: what `make sandbox-heal` does.
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  set -eEuo pipefail
  HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd) # hack/kind, which lib.sh's functions expect
  # shellcheck source-path=SCRIPTDIR source=../lib.sh
  . "${HERE}/lib.sh"
  CTX=kind-${CLUSTER}
  K=(kubectl --context "${CTX}")
  for tool in kind kubectl jq; do
    command -v "${tool}" >/dev/null || die "${tool} is required"
  done
  trap delete_netpol_probes EXIT
  trap 'exit 130' INT TERM
  cluster_exists || die "kind cluster ${CLUSTER} does not exist; there is no egress gateway to heal (make e2e-full-up)"
  assert_owned
  assert_context full
  # The gateway first: a kindnet that cannot be healed dies here, and the gateway has been healed all the same.
  heal_egress_gateway
  heal_network_policy
fi

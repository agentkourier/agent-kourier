#!/usr/bin/env bash
# Take the sandbox tier away and give the full tier its own Agent Kourier back (the testhooks image on the fake Slack), so that
# `make e2e-full` works again: the sandbox Agent Kourier (its Helm release), its volume, config and Slack Secret; the token refresh; the agent, its
# prompt, model Secret and its tool server; the seeded faults (namespace shop). The full tier's cluster, and its Binding token
# Secret, stay, and so does the full tier's Alertmanager, which is taken back off the alert source (alertmanager/): its overlay and the
# two Secrets go. Needs no secrets file. Safe to re-run, and to run when the sandbox was never up.
# Every call names --context kind-agent-kourier-full.
set -eEuo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd) # hack/kind
ROOT=$(cd "${HERE}/../.." && pwd) # the repository root
AGENT=${ROOT}/examples/recipes/sre-triage/agent # the sre-triage recipe's agent: sandbox-sre, its tool server and the seeded faults
# shellcheck source-path=SCRIPTDIR source=../lib.sh
. "${HERE}/lib.sh"
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")

STEP=preflight
step() {
  STEP=$*
  echo "==> $*"
}
on_error() {
  echo "error: step '${STEP}' failed (line $1). Safe to re-run: make sandbox-down" >&2
}
trap 'on_error $LINENO' ERR

for tool in kind kubectl helm; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
cluster_exists || die "kind cluster ${CLUSTER} does not exist; there is no sandbox to take away"
assert_owned
assert_context full

step "the sandbox Agent Kourier, its volume, config and Slack Secret, and the token refresh"
# One Helm release: the Deployment, the volume (not kept: sandbox-values.yaml), the ServiceAccount and Role, and the token refresh CronJob.
if [ -n "$("${H[@]}" list --namespace agent-kourier --filter '^agent-kourier-sandbox$' --short)" ]; then
  "${H[@]}" uninstall agent-kourier-sandbox --namespace agent-kourier --wait >/dev/null
fi
# What a sandbox brought up before the chart made (its Deployment, token refresh and ServiceAccount's Role were hand-made), and what up.sh makes
# beside the release: its config, the Slack Secret and the Dex client. Each is deleted if it is there.
"${K[@]}" -n agent-kourier delete deploy/agent-kourier-sandbox --ignore-not-found --wait >/dev/null
"${K[@]}" -n agent-kourier delete pvc/agent-kourier-sandbox-data cm/agent-kourier-sandbox-config secret/sandbox-slack secret/agent-kourier-dex-client --ignore-not-found >/dev/null
"${K[@]}" -n agent-kourier delete cronjob/agent-kourier-token-refresh serviceaccount/agent-kourier-token-refresh role/agent-kourier-token-refresh rolebinding/agent-kourier-token-refresh --ignore-not-found >/dev/null
"${K[@]}" -n agent-kourier delete jobs -l batch.kubernetes.io/cronjob-name=agent-kourier-token-refresh --ignore-not-found >/dev/null 2>&1 || true
"${K[@]}" -n agent-kourier delete jobs -l batch.kubernetes.io/cronjob-name=agent-kourier-sandbox-token-refresh --ignore-not-found >/dev/null 2>&1 || true

# The alert source: the three fields of the Alertmanager that up.sh added come off first (the operator rolls it back to the tier's own
# configuration, whose sink the E2E tests need), so that it is never left naming a Secret that is gone; then the two Secrets go.
step "the alert source (Alertmanager's receiver and its webhook Secret)"
# Only when the Alertmanager is there and is the alert source (an optional part of up.sh): nothing is patched or waited for otherwise.
if [ "$("${K[@]}" -n monitoring get alertmanager agent-kourier --ignore-not-found -o 'jsonpath={.spec.configSecret}')" = alertmanager-kind-config ]; then
  "${K[@]}" -n monitoring patch alertmanager agent-kourier --type=merge -p '{"spec":{"configSecret":null,"secrets":null,"externalUrl":null}}' >/dev/null
  deadline=$((SECONDS + 180))
  until [[ $("${K[@]}" -n monitoring get pod alertmanager-agent-kourier-0 -o 'jsonpath={.spec.containers[*].args}' 2>/dev/null || echo unknown) != *--web.external-url=http://alertmanager.kind.local* ]] &&
    "${K[@]}" -n monitoring wait --for=condition=Ready pod/alertmanager-agent-kourier-0 --timeout=5s >/dev/null 2>&1; do
    [ "${SECONDS}" -lt "${deadline}" ] || die "the full tier's Alertmanager did not come back to its own configuration in 180 s"
    sleep 3
  done
fi
"${K[@]}" -n monitoring delete secret alertmanager-slack-secret alertmanager-kind-config --ignore-not-found >/dev/null

step "the agent, its model and Secret, and the tool server"
"${K[@]}" delete -f "${AGENT}/agent.yaml" --ignore-not-found >/dev/null
"${K[@]}" delete -f "${AGENT}/agent-prompt.yaml" --ignore-not-found >/dev/null # after the agent, which selects it
"${K[@]}" -n kagent delete modelconfig.api.kagent.dev/sandbox-model secret/sandbox-model --ignore-not-found >/dev/null
if [ -n "$("${H[@]}" list --namespace kagent --filter '^sandbox-tools$' --short)" ]; then
  "${H[@]}" uninstall sandbox-tools --namespace kagent --wait >/dev/null
fi
"${K[@]}" delete -f "${AGENT}/tools-rbac.yaml" --ignore-not-found >/dev/null

step "the seeded faults (namespace shop)"
"${K[@]}" delete namespace shop --ignore-not-found --wait >/dev/null

step "the full tier's own Agent Kourier"
"${K[@]}" -n agent-kourier scale deploy/agent-kourier --replicas=1 >/dev/null
"${K[@]}" -n agent-kourier rollout status deploy/agent-kourier --timeout=180s
echo "sandbox down in ${SECONDS}s: the full tier's Agent Kourier is back; make e2e-full works again"

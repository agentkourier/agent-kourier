#!/usr/bin/env bash
# Take the sandbox tier away and give the full tier its own courier back (the testhooks image on the fake Slack), so that
# `make e2e-full` works again: the sandbox courier, its volume, config and Slack Secret; the token refresh; the agent, its
# model Secret and its tool server; the seeded faults (namespace shop). The full tier's cluster, and its Binding token
# Secret, stay. Needs no secrets file. Safe to re-run, and to run when the sandbox was never up.
# Every call names --context kind-courier-full.
set -eEuo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd) # hack/kind
SBX=${HERE}/sandbox
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

step "the sandbox courier, its volume, config and Slack Secret, and the token refresh"
"${K[@]}" -n courier delete deploy/courier-sandbox --ignore-not-found --wait >/dev/null
"${K[@]}" -n courier delete pvc/courier-sandbox-data cm/courier-sandbox-config secret/sandbox-slack --ignore-not-found >/dev/null
"${K[@]}" delete -f "${SBX}/token-refresh.yaml" --ignore-not-found >/dev/null
"${K[@]}" -n courier delete jobs -l batch.kubernetes.io/cronjob-name=courier-token-refresh --ignore-not-found >/dev/null 2>&1 || true

step "the agent, its model and Secret, and the tool server"
"${K[@]}" delete -f "${SBX}/agent.yaml" --ignore-not-found >/dev/null
"${K[@]}" -n kagent delete modelconfig.api.kagent.dev/sandbox-model secret/sandbox-model --ignore-not-found >/dev/null
if [ -n "$("${H[@]}" list --namespace kagent --filter '^sandbox-tools$' --short)" ]; then
  "${H[@]}" uninstall sandbox-tools --namespace kagent --wait >/dev/null
fi
"${K[@]}" delete -f "${SBX}/tools-rbac.yaml" --ignore-not-found >/dev/null

step "the seeded faults (namespace shop)"
"${K[@]}" delete namespace shop --ignore-not-found --wait >/dev/null

step "the full tier's own courier"
"${K[@]}" -n courier scale deploy/courier --replicas=1 >/dev/null
"${K[@]}" -n courier rollout status deploy/courier --timeout=180s
echo "sandbox down in ${SECONDS}s: the full tier's courier is back; make e2e-full works again"

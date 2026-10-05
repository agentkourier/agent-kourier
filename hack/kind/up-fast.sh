#!/usr/bin/env bash
# Bring up the fast E2E tier from nothing: a kind cluster (courier-fast) with prometheus-operator
# and one Alertmanager, the fake Slack, the stub agent (-require-auth) and a sink webhook
# receiver, and courier's image when one exists (spec 12.1). Safe to re-run.
# The fast tier is its own cluster, so it runs beside the full tier; `make e2e-down` removes both
# (hack/kind/down.sh fast removes only this one).
#
# Every kubectl and helm call names --context kind-courier-fast and every kind call names
# --name courier-fast: the host kubeconfig may hold other clusters, production among them, and
# the current context is never relied on. assert_context aborts unless that context is the local
# kind cluster.
#
# The operator's CRDs are installed by helm on first install only; helm never upgrades them. To
# change OPERATOR_CHART_VERSION (in lib.sh), run `hack/kind/down.sh fast` first.
#
# Courier's testhooks image (courier-e2e/courier-testhooks:dev, built from the repo's Dockerfile) is
# built and loaded into the cluster, after a check that it carries its label, and run by courier's chart
# (charts/courier, with ci/fast-values.yaml, which the full tier uses too: lib.sh's deploy_courier and install_courier). On a
# cluster that still has the hand-made Deployment this tier used to have, re-running this replaces it, keeping the volume.
# The tier is up only when courier is ready and connected to the fake Slack.
set -eEuo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
use_tier fast
FAST=${HERE}/fast
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")

STEP=preflight
step() {
  STEP=$*
  echo "==> $*"
}
on_error() {
  echo "error: step '${STEP}' failed (line $1). Recover with: hack/kind/down.sh fast && make e2e-fast-up" >&2
}
trap 'on_error $LINENO' ERR
BUILD=
trap '[ -z "${BUILD}" ] || rm -rf "${BUILD}"' EXIT

for tool in docker kind kubectl helm go; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"
require_helm4

step "kind cluster ${CLUSTER}"
if cluster_exists; then
  assert_owned
  echo "reusing the cluster created earlier (from $(owner_checkout))"
else
  kind create cluster --name "${CLUSTER}" --config "${FAST}/kind-config.yaml" --image "${NODE_IMAGE}"
  assert_context fast
  "${K[@]}" annotate nodes -l "${OWNER_LABEL}" "${CHECKOUT_KEY}=${ROOT}" >/dev/null
fi
assert_context fast

step "prometheus-operator ${OPERATOR_CHART_VERSION}"
install_operator

step "test doubles image"
build_doubles_image

step "fake Slack, stub agent, sink, Alertmanager"
apply_doubles

step "courier image ${TESTHOOKS_IMAGE}"
load_courier_image

step "courier: namespace, config, Secrets, chart"
# Created and updated with apply, so a re-run changes what changed. The Secrets are fake constants made
# here and never committed: the Slack tokens carry the prefixes the fake Slack checks, and the agent token
# is an unsigned JWT with an email claim, which is all the stub agent (-require-auth) asks of a bearer.
"${K[@]}" create namespace courier --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
b64url() { printf %s "$1" | base64 | tr -d '=\n' | tr '/+' '_-'; }
"${K[@]}" -n courier create secret generic courier-agent-token \
  --from-literal=token="$(b64url '{"alg":"none"}').$(b64url '{"email":"courier-fast@example.test"}')." --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
# The rollout restart in install_courier picks up a rebuilt image (same tag) or a changed ConfigMap; verify.sh
# waits for the rollout and shows courier's log if it does not become ready.
deploy_courier "${FAST}/courier/config/agentbackend.yaml"

step "verify"
"${FAST}/verify.sh"
echo "fast tier up in ${SECONDS}s (context ${CTX})"

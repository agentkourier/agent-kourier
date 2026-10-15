#!/usr/bin/env bash
# Apply the disposable prompt probe to an existing full-tier cluster. This is
# deliberately standalone: it is a fixture for acceptance checks, not part of
# the production sandbox or the normal full-tier bring-up.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=../../lib.sh
. "${HERE}/../../lib.sh"
use_tier full
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")

cluster_exists || die "kind cluster ${CLUSTER} does not exist; bring it up with: make e2e-full-up"
assert_owned
assert_context full
refuse_while_sandbox_up

"${K[@]}" apply -f "${HERE}/prompt.yaml" -f "${HERE}/capture.yaml" -f "${HERE}/model.yaml" >/dev/null
"${K[@]}" -n kagent rollout status deploy/ta-config-model --timeout=180s >/dev/null
"${K[@]}" -n kagent wait --for=condition=Accepted modelconfig/ta-config-model --timeout=180s >/dev/null
"${K[@]}" apply -f "${HERE}/agent.yaml" >/dev/null
"${K[@]}" -n kagent wait --for=condition=Ready agent/ta-config --timeout=300s >/dev/null
echo "ta-config is Ready; inspect capture with: ${K[*]} -n kagent logs deploy/ta-config-model"

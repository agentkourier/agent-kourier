#!/usr/bin/env bash
# Apply the full tier's scenario agents (full/scenarios) to a running courier-full cluster, as up-full.sh does:
# for changing the scenario model or an agent without bringing the tier up again. Names --context
# kind-courier-full on every call.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd) # hack/kind, where lib.sh keeps its paths relative to
# shellcheck source-path=SCRIPTDIR source=../lib.sh
. "${HERE}/lib.sh"
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
assert_context full
deploy_scenario_agents
echo "scenario agents applied in ${CTX}"

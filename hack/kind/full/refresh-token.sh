#!/usr/bin/env bash
# Make a fresh Dex ID token for the Binding identity and store it in courier's Secret courier-agent-token (the full
# tier's; see lib.sh's refresh_dex_token). The token lasts 24 hours, so run this before using a full tier that has
# been up that long; e2e-run.sh full does. Names --context kind-courier-full on every call.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=../lib.sh
. "${HERE}/../lib.sh"
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
assert_context full
refresh_dex_token
echo "refreshed the Binding's Dex token in ${CTX}"

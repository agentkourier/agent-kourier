#!/usr/bin/env bash
# Check the sandbox secrets file and say whether it is usable: check-env.sh [FILE] (default $SANDBOX_ENV, or
# ~/.config/agent-courier/sandbox.env). Prints the path and the key names it found, and never a value; the
# rules, and the parser that never sources the file, are in env.sh. Touches no cluster.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source-path=SCRIPTDIR source=env.sh
. "${HERE}/env.sh"
file=${1:-${SANDBOX_ENV:-${HOME:?HOME is not set}/.config/agent-courier/sandbox.env}}
sandbox_env_load "${file}"
echo "ok: ${file} has ${SANDBOX_ENV_KEYS// /, }"

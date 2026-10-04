#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
source hack/kind/sandbox/env.sh
sandbox_env_load "${SANDBOX_ENV:-${HOME}/.config/agent-courier/sandbox.env}"
[[ ${MODEL_PROVIDER} == bedrock ]] || sandbox_env_fail "this smoke requires MODEL_PROVIDER=bedrock"
export MODEL_REGION MODEL_NAME
export AWS_BEARER_TOKEN_BEDROCK="${MODEL_API_KEY}"
export AWS_EC2_METADATA_DISABLED=true
unset MODEL_API_KEY SLACK_BOT_TOKEN SLACK_APP_TOKEN
cd examples/ax
go run ./cmd/smoke

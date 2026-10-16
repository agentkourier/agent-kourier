#!/usr/bin/env bash
# Owner-run: ask the docs agent, on loopback, with the real model named in the sandbox credentials file. It uses
# model quota and prints the answers, never a credential. The deterministic smoke (make docs-agent-smoke) needs none.
set -euo pipefail
cd "$(dirname "$0")/../../../.."
# shellcheck source=../../../../hack/kind/sandbox/env.sh
source hack/kind/sandbox/env.sh
sandbox_env_load "${SANDBOX_ENV:-${HOME}/.config/agent-kourier/sandbox.env}"
case "${MODEL_PROVIDER}" in
  openrouter | openai) ;;
  *) sandbox_env_fail "this smoke requires MODEL_PROVIDER=openrouter or openai" ;;
esac
export MODEL_PROVIDER MODEL_NAME MODEL_API_KEY
if [ -n "${MODEL_BASE_URL:-}" ]; then export MODEL_BASE_URL; fi
unset SLACK_BOT_TOKEN SLACK_APP_TOKEN SLACK_CHANNEL_ID MODEL_REGION
cd examples/recipes/docs-agent/agent
go run ./cmd/smoke -live

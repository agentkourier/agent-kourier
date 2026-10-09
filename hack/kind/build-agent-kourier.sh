#!/usr/bin/env bash
# Build an Agent Kourier image from the repo's Dockerfile: build-agent-kourier.sh builds agent-kourier:dev, the production
# image; build-agent-kourier.sh testhooks builds agent-kourier-e2e/agent-kourier-testhooks:dev, the image the kind tiers
# run. The pinned base images and the names are in lib.sh. Behind `make image` and `make image-testhooks`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
case "${1:-}" in
  "" | testhooks) ;;
  *) die "usage: build-agent-kourier.sh [testhooks]" ;;
esac
command -v docker >/dev/null || die "docker is required"
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"
build_agentkourier_image "${1:-}"
if [ "${1:-}" = testhooks ]; then echo "built ${TESTHOOKS_IMAGE}"; else echo "built ${AGENTKOURIER_IMAGE}"; fi

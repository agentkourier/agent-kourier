#!/usr/bin/env bash
# Build a courier image from the repo's Dockerfile: build-courier.sh builds courier:dev, the production
# image; build-courier.sh testhooks builds courier-e2e/courier-testhooks:dev, the image the kind tiers
# run. The pinned base images and the names are in lib.sh. Behind `make image` and `make image-testhooks`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
case "${1:-}" in
  "" | testhooks) ;;
  *) die "usage: build-courier.sh [testhooks]" ;;
esac
command -v docker >/dev/null || die "docker is required"
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"
build_courier_image "${1:-}"
if [ "${1:-}" = testhooks ]; then echo "built ${TESTHOOKS_IMAGE}"; else echo "built ${COURIER_IMAGE}"; fi

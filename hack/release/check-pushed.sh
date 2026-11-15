#!/usr/bin/env bash
# check-pushed.sh PUSHED_REF PLATFORM LOCAL_IMAGE: die unless PLATFORM's image in the pushed multi-platform image
# PUSHED_REF has exactly the layers (rootfs diff IDs) of LOCAL_IMAGE, the image release.sh checked and scanned for that
# platform. The push is a second build that should come from the cache the checks filled; an agent's Dockerfile resolves
# Wolfi's packages when it runs, so a cache miss would push layers nobody scanned. Run by release.sh after a push and
# before the release workflow signs anything.
set -euo pipefail

die() {
  echo "error: $*" >&2
  exit 1
}

[ $# -eq 3 ] || die "usage: check-pushed.sh PUSHED_REF PLATFORM LOCAL_IMAGE"
PUSHED_REF=$1 PLATFORM=$2 LOCAL_IMAGE=$3

# For a multi-platform reference, .Image is a map from platform to image config; for one platform, the config itself,
# which must then be PLATFORM's.
pushed=$(docker buildx imagetools inspect "${PUSHED_REF}" --format '{{json .Image}}' |
  jq -c --arg p "${PLATFORM}" '
    if has("rootfs") then (if (.os + "/" + .architecture) == $p then .rootfs.diff_ids else null end)
    else .[$p].rootfs.diff_ids end // empty') ||
  die "could not read the layers of ${PUSHED_REF}"
[ -n "${pushed}" ] && [ "${pushed}" != null ] || die "${PUSHED_REF} has no ${PLATFORM} image"
checked=$(docker image inspect --format '{{json .RootFS.Layers}}' "${LOCAL_IMAGE}" | jq -c .) ||
  die "could not read the layers of ${LOCAL_IMAGE}"
[ "${pushed}" = "${checked}" ] ||
  die "${PUSHED_REF} ${PLATFORM} is not the image that was checked and scanned (${LOCAL_IMAGE}): its layers differ"
echo "    ok: ${PLATFORM} of ${PUSHED_REF} is the checked image"

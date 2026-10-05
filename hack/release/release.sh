#!/usr/bin/env bash
# Build the multi-architecture release image of courier, and with PUSH=1 push it. Behind
# `make release-image` and .github/workflows/release.yml, so a human and the workflow build the same artifact.
#
#   VERSION   the release tag, vX.Y.Z or vX.Y.Z-rc.1 (required); it becomes main.version and the image tag
#   REGISTRY  the image repository, e.g. ghcr.io/agentkourier/agent-courier; required to push, no default
#   PUSH      1 to push; anything else (the default) builds to a local OCI archive and pushes nothing
#   PLATFORMS comma-separated, default linux/amd64,linux/arm64
#   CHART_REGISTRY  where the chart is pushed, e.g. oci://ghcr.io/agentkourier/charts; required to push, no default
#   OUT       the output directory, default bin/release
#   BUILDER   a buildx builder name, if the current one cannot build multi-platform images with attestations
#
# After the image it packages charts/courier with the version stamped and the image's digest as the default
# image (package-chart.sh), and with PUSH=1 pushes the chart; the digests are written to OUT/digest and
# OUT/chart-digest.
#
# The test hooks never ship. The Dockerfile's verify stage fails the build of any platform whose binary has
# a hook (internal/testhooks, the same traces `make image-check` looks for) or the wrong version or
# architecture, so no image exists to push; this script then checks each platform's image (no test-hooks
# label, non-root, the right platform) before the one push, which builds all platforms from the same cache.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=../kind/lib.sh
. "${ROOT}/hack/kind/lib.sh"

VERSION=${VERSION:-}
REGISTRY=${REGISTRY:-}
CHART_REGISTRY=${CHART_REGISTRY:-}
PUSH=${PUSH:-}
PLATFORMS=${PLATFORMS:-linux/amd64,linux/arm64}
OUT=${OUT:-${ROOT}/bin/release}
SOURCE_URL=https://github.com/agentkourier/agent-kourier

[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] ||
  die "VERSION must be a version tag such as v1.2.3 (got '${VERSION}')"
case "${PUSH}" in "" | 0 | 1) ;; *) die "PUSH must be 1 or unset (got '${PUSH}')" ;; esac
command -v docker >/dev/null || die "docker is required"
command -v helm >/dev/null || die "helm is required"
command -v jq >/dev/null || die "jq is required"

REVISION=$(git -C "${ROOT}" rev-parse HEAD)
DIRTY=$(git -C "${ROOT}" status --porcelain)
if [ "${PUSH}" = 1 ]; then
  [ -n "${REGISTRY}" ] || die "PUSH=1 needs an explicit REGISTRY (there is no default)"
  [[ "${REGISTRY}" =~ ^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+$ ]] ||
    die "REGISTRY must be a repository without a tag or scheme, such as ghcr.io/agentkourier/agent-courier (got '${REGISTRY}')"
  [[ "${CHART_REGISTRY}" =~ ^oci://[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+$ ]] ||
    die "PUSH=1 needs an explicit CHART_REGISTRY such as oci://ghcr.io/agentkourier/charts (got '${CHART_REGISTRY}')"
  [ -z "${DIRTY}" ] || die "refusing to push from a working tree with uncommitted or untracked files"
  [ "$(git -C "${ROOT}" rev-parse -q --verify "refs/tags/${VERSION}^{commit}" || true)" = "${REVISION}" ] ||
    die "refusing to push: the tag ${VERSION} does not exist at HEAD (${REVISION})"
fi
[ -z "${DIRTY}" ] || REVISION="${REVISION}-dirty"
# The commit's own time, not the clock: the label is the same on every build of a commit.
CREATED=$(TZ=UTC git -C "${ROOT}" show -s --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd HEAD)
NAME=${REGISTRY:-courier-release}:${VERSION}

# Passed to every build: the pinned bases, the version, the OCI labels. TAGS is never passed, so the binary
# is the production build, and no test-hooks label is ever added here.
LABELS=(
  org.opencontainers.image.source="${SOURCE_URL}"
  org.opencontainers.image.revision="${REVISION}"
  org.opencontainers.image.version="${VERSION}"
  org.opencontainers.image.created="${CREATED}"
)
BUILD=(docker buildx build -f "${ROOT}/Dockerfile"
  --build-arg "GO_IMAGE=${GO_IMAGE}" --build-arg "RUNTIME_IMAGE=${RUNTIME_IMAGE}" --build-arg "VERSION=${VERSION}")
[ -z "${BUILDER:-}" ] || BUILD+=(--builder "${BUILDER}")
# The same four as annotations on the index, which is where GHCR reads a multi-platform image's source repository
# (a single-platform build cannot take them, so only a multi-platform release build gets them).
ANNOTATIONS=()
IFS=, read -r -a PLATFORM_LIST <<<"${PLATFORMS}"
for l in "${LABELS[@]}"; do
  BUILD+=(--label "${l}")
  [ ${#PLATFORM_LIST[@]} -lt 2 ] || ANNOTATIONS+=(--annotation "index:${l}")
done

CHECKS=()
cleanup() {
  [ ${#CHECKS[@]} -eq 0 ] || docker rmi -f "${CHECKS[@]}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# check_platform PLATFORM: build that platform alone into the local image store (this is where the Dockerfile's
# verify stage runs for it: no test hook in the binary, an ELF of this architecture, main.version set to
# VERSION), then die unless the image is the one to release: no test-hooks label, non-root, this platform.
check_platform() {
  local platform=$1 ref="courier-release-check:${1//\//-}"
  CHECKS+=("${ref}")
  echo "==> ${platform}: build and check"
  "${BUILD[@]}" --platform "${platform}" --load -t "${ref}" "${ROOT}"
  [ -z "$(docker image inspect --format "{{index .Config.Labels \"${TESTHOOKS_LABEL}\"}}" "${ref}")" ] ||
    die "${platform}: the image carries the ${TESTHOOKS_LABEL} label; refusing to release it"
  [ "$(docker image inspect --format '{{.Config.User}}' "${ref}")" = 65532:65532 ] ||
    die "${platform}: the image does not run as 65532:65532"
  [ "$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "${ref}")" = "${platform}" ] ||
    die "${platform}: the image was built for $(docker image inspect --format '{{.Os}}/{{.Architecture}}' "${ref}")"
  echo "    ok: no hooks, no label, non-root, ${platform}, version ${VERSION}"
}

for p in "${PLATFORM_LIST[@]}"; do check_platform "${p}"; done

mkdir -p "${OUT}"
rm -f "${OUT}/metadata.json" "${OUT}/digest" "${OUT}/chart-digest"
# "${ANNOTATIONS[@]+...}": an empty array is unset to bash 3.2.
if [ "${PUSH}" = 1 ]; then
  echo "==> pushing ${NAME} (${PLATFORMS})"
  "${BUILD[@]}" --platform "${PLATFORMS}" --provenance=mode=max --sbom=true ${ANNOTATIONS[@]+"${ANNOTATIONS[@]}"} \
    --metadata-file "${OUT}/metadata.json" --push -t "${NAME}" "${ROOT}"
else
  archive="${OUT}/courier-${VERSION}.oci.tar"
  echo "==> building ${NAME} (${PLATFORMS}) to ${archive}; nothing is pushed"
  "${BUILD[@]}" --platform "${PLATFORMS}" --provenance=mode=max --sbom=true ${ANNOTATIONS[@]+"${ANNOTATIONS[@]}"} \
    --metadata-file "${OUT}/metadata.json" --output "type=oci,dest=${archive}" -t "${NAME}" "${ROOT}"
  echo "built ${archive}"
fi
digest=$(jq -r '."containerimage.digest" // empty' "${OUT}/metadata.json")
[[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "the build did not report an image digest"
echo "${digest}" >"${OUT}/digest"

echo "==> packaging the chart with image ${REGISTRY:-courier-release}@${digest}"
chart=$("${HERE}/package-chart.sh" "${VERSION}" "${REGISTRY:-courier-release}" "${digest}" "${OUT}")
if [ "${PUSH}" = 1 ]; then
  pushed=$(helm push "${chart}" "${CHART_REGISTRY}" 2>&1) || die "helm push failed: ${pushed}"
  chart_digest=$(sed -n 's/^Digest: //p' <<<"${pushed}")
  [[ "${chart_digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "helm push did not report a chart digest: ${pushed}"
  echo "${chart_digest}" >"${OUT}/chart-digest"
  echo "pushed ${CHART_REGISTRY#oci://}/courier@${chart_digest} (version ${VERSION#v})"
  echo "pushed ${REGISTRY}@${digest} (tag ${VERSION})"
else
  echo "packaged ${chart}; nothing is pushed"
fi

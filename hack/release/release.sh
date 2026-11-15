#!/usr/bin/env bash
# Build the multi-architecture release images of Agent Kourier and of the recipes' agents, and with PUSH=1 push them.
# Behind `make release-image` and .github/workflows/release.yml, so a human and the workflow build the same artifacts.
#
#   VERSION   the release tag, vX.Y.Z or vX.Y.Z-rc.1 (required); it becomes main.version and the images' tag
#   REGISTRY  the image repository, e.g. ghcr.io/agentkourier/agent-kourier; required to push, no default. Each agent
#             image goes to REGISTRY-<agent>, e.g. ghcr.io/agentkourier/agent-kourier-prompt-agent
#   PUSH      1 to push; anything else (the default) builds to a local OCI archive and pushes nothing
#   PLATFORMS comma-separated, default linux/amd64,linux/arm64
#   CHART_REGISTRY  where the chart is pushed, e.g. oci://ghcr.io/agentkourier/charts; required to push, no default
#   OUT       the output directory, default bin/release
#   BUILDER   a buildx builder name, if the current one cannot build multi-platform images with attestations
#   SCAN_IMAGES  1 to scan local platform images too; PUSH=1 always scans before it pushes
#   SCAN_IMAGE_SCRIPT  scanner path, default hack/security/scan-image.sh
#
# The agent images are AGENTS, each built from examples/recipes/<agent>/agent/Dockerfile, which needs no build argument.
# After the images it packages charts/agent-kourier with the version stamped and Agent Kourier's digest as the default
# image (package-chart.sh), and with PUSH=1 pushes the chart. The digests are written to OUT/digest (Agent Kourier),
# OUT/<agent>-digest and OUT/chart-digest.
#
# The test hooks never ship. The Dockerfile's verify stage fails the build of any platform whose binary has
# a hook (internal/testhooks, the same traces `make image-check` looks for) or the wrong version or
# architecture, so no image exists to push; this script then checks each platform's image (no test-hooks
# label, non-root, the right platform) before the one push, which builds all platforms from the same cache. Every
# platform of every image, the agents' included, is built, checked and scanned before the first push, so an agent image
# that fails leaves nothing pushed.
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
SCAN_IMAGES=${SCAN_IMAGES:-}
SCAN_IMAGE_SCRIPT=${SCAN_IMAGE_SCRIPT:-${ROOT}/hack/security/scan-image.sh}
SOURCE_URL=https://github.com/agentkourier/agent-kourier
# The recipes' agents whose images a release publishes: examples/recipes/<agent>/agent.
AGENTS=(prompt-agent echo-agent)

[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] ||
  die "VERSION must be a version tag such as v1.2.3 (got '${VERSION}')"
case "${PUSH}" in "" | 0 | 1) ;; *) die "PUSH must be 1 or unset (got '${PUSH}')" ;; esac
case "${SCAN_IMAGES}" in "" | 0 | 1) ;; *) die "SCAN_IMAGES must be 1 or unset (got '${SCAN_IMAGES}')" ;; esac
command -v docker >/dev/null || die "docker is required"
command -v helm >/dev/null || die "helm is required"
command -v jq >/dev/null || die "jq is required"

if [ "${PUSH}" = 1 ] || [ "${SCAN_IMAGES}" = 1 ]; then
  [ -f "${SCAN_IMAGE_SCRIPT}" ] || die "image scanning is required, but ${SCAN_IMAGE_SCRIPT} is missing"
fi

REVISION=$(git -C "${ROOT}" rev-parse HEAD)
DIRTY=$(git -C "${ROOT}" status --porcelain)
if [ "${PUSH}" = 1 ]; then
  [ -n "${REGISTRY}" ] || die "PUSH=1 needs an explicit REGISTRY (there is no default)"
  [[ "${REGISTRY}" =~ ^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+$ ]] ||
    die "REGISTRY must be a repository without a tag or scheme, such as ghcr.io/agentkourier/agent-kourier (got '${REGISTRY}')"
  [[ "${CHART_REGISTRY}" =~ ^oci://[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+$ ]] ||
    die "PUSH=1 needs an explicit CHART_REGISTRY such as oci://ghcr.io/agentkourier/charts (got '${CHART_REGISTRY}')"
  [ -z "${DIRTY}" ] || die "refusing to push from a working tree with uncommitted or untracked files"
  [ "$(git -C "${ROOT}" rev-parse -q --verify "refs/tags/${VERSION}^{commit}" || true)" = "${REVISION}" ] ||
    die "refusing to push: the tag ${VERSION} does not exist at HEAD (${REVISION})"
fi
[ -z "${DIRTY}" ] || REVISION="${REVISION}-dirty"
# The commit's own time, not the clock: the label is the same on every build of a commit.
CREATED=$(TZ=UTC git -C "${ROOT}" show -s --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd HEAD)
REPOSITORY=${REGISTRY:-agent-kourier-release}
NAME=${REPOSITORY}:${VERSION}

# Passed to every build: the OCI labels; to Agent Kourier's, also the pinned bases and the version. TAGS is never
# passed, so the binary is the production build, and no test-hooks label is ever added here.
LABELS=(
  org.opencontainers.image.source="${SOURCE_URL}"
  org.opencontainers.image.revision="${REVISION}"
  org.opencontainers.image.version="${VERSION}"
  org.opencontainers.image.created="${CREATED}"
)
BUILDX=(docker buildx build)
[ -z "${BUILDER:-}" ] || BUILDX+=(--builder "${BUILDER}")
# The same four as annotations on the index, which is where GHCR reads a multi-platform image's source repository
# (a single-platform build cannot take them, so only a multi-platform release build gets them).
ANNOTATIONS=()
IFS=, read -r -a PLATFORM_LIST <<<"${PLATFORMS}"
for l in "${LABELS[@]}"; do
  BUILDX+=(--label "${l}")
  [ ${#PLATFORM_LIST[@]} -lt 2 ] || ANNOTATIONS+=(--annotation "index:${l}")
done
BUILD=("${BUILDX[@]}" -f "${ROOT}/Dockerfile"
  --build-arg "GO_IMAGE=${GO_IMAGE}" --build-arg "RUNTIME_IMAGE=${RUNTIME_IMAGE}" --build-arg "VERSION=${VERSION}")

CHECKS=()
mkdir -p "${OUT}"
cleanup() {
  [ ${#CHECKS[@]} -eq 0 ] || docker rmi -f "${CHECKS[@]}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# scan_platform PLATFORM IMAGE [AGENT]: scan the local image with the shared scanner. PUSH=1 always scans; SCAN_IMAGES=1
# opts into the same scan for local release-image builds. The output is retained beside the release metadata, as
# trivy-<platform>.sarif for Agent Kourier and trivy-<agent>-<platform>.sarif for an agent.
scan_platform() {
  local platform=$1 ref=$2 agent=${3:-}
  local outfile="${OUT}/trivy-${agent:+${agent}-}${platform//\//-}.sarif"
  echo "==> ${agent:+${agent} }${platform}: vulnerability scan"
  bash "${SCAN_IMAGE_SCRIPT}" "${ref}" "${platform}" "${outfile}"
  [ -s "${outfile}" ] || die "${agent:+${agent} }${platform}: image scanner did not write ${outfile}"
}

# check_image REF PLATFORM WHAT: die unless the local image has no test-hooks label, runs as 65532:65532, and is
# PLATFORM. WHAT names the image in the message.
check_image() {
  local ref=$1 platform=$2 what=$3
  [ -z "$(docker image inspect --format "{{index .Config.Labels \"${TESTHOOKS_LABEL}\"}}" "${ref}")" ] ||
    die "${what}: the image carries the ${TESTHOOKS_LABEL} label; refusing to release it"
  [ "$(docker image inspect --format '{{.Config.User}}' "${ref}")" = 65532:65532 ] ||
    die "${what}: the image does not run as 65532:65532"
  [ "$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "${ref}")" = "${platform}" ] ||
    die "${what}: the image was built for $(docker image inspect --format '{{.Os}}/{{.Architecture}}' "${ref}")"
}

# check_platform PLATFORM: build that platform alone into the local image store (this is where the Dockerfile's
# verify stage runs for it: no test hook in the binary, an ELF of this architecture, main.version set to
# VERSION), then die unless the image is the one to release: no test-hooks label, non-root, this platform.
check_platform() {
  local platform=$1 ref="agent-kourier-release-check:${1//\//-}"
  CHECKS+=("${ref}")
  echo "==> ${platform}: build and check"
  "${BUILD[@]}" --platform "${platform}" --load -t "${ref}" "${ROOT}"
  check_image "${ref}" "${platform}" "${platform}"
  if [ "${PUSH}" = 1 ] || [ "${SCAN_IMAGES}" = 1 ]; then
    scan_platform "${platform}" "${ref}"
  fi
  echo "    ok: no hooks, no label, non-root, ${platform}, version ${VERSION}"
}

# check_agent_platform AGENT PLATFORM: build that agent's image for that platform alone into the local image store,
# check it as check_platform does (the agent's Dockerfile has no verify stage: its image holds no Go binary), and scan
# it when check_platform would.
check_agent_platform() {
  local agent=$1 platform=$2 ref="agent-kourier-release-check:${1}-${2//\//-}"
  CHECKS+=("${ref}")
  echo "==> ${agent} ${platform}: build and check"
  "${BUILDX[@]}" --platform "${platform}" --load -t "${ref}" "${ROOT}/examples/recipes/${agent}/agent"
  check_image "${ref}" "${platform}" "${agent} ${platform}"
  if [ "${PUSH}" = 1 ] || [ "${SCAN_IMAGES}" = 1 ]; then
    scan_platform "${platform}" "${ref}" "${agent}"
  fi
  echo "    ok: no label, non-root, ${platform}"
}

# image_digest METADATA: print the image digest a build wrote to its metadata file, or die.
image_digest() {
  local digest
  digest=$(jq -r '."containerimage.digest" // empty' "$1")
  [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "the build did not report an image digest in $1"
  echo "${digest}"
}

for p in "${PLATFORM_LIST[@]}"; do check_platform "${p}"; done
for a in "${AGENTS[@]}"; do
  for p in "${PLATFORM_LIST[@]}"; do check_agent_platform "${a}" "${p}"; done
done

rm -f "${OUT}/metadata.json" "${OUT}/digest" "${OUT}/chart-digest"
for a in "${AGENTS[@]}"; do rm -f "${OUT}/${a}-metadata.json" "${OUT}/${a}-digest"; done
# "${ANNOTATIONS[@]+...}": an empty array is unset to bash 3.2.
if [ "${PUSH}" = 1 ]; then
  echo "==> pushing ${NAME} (${PLATFORMS})"
  "${BUILD[@]}" --platform "${PLATFORMS}" --provenance=mode=max --sbom=true ${ANNOTATIONS[@]+"${ANNOTATIONS[@]}"} \
    --metadata-file "${OUT}/metadata.json" --push -t "${NAME}" "${ROOT}"
else
  archive="${OUT}/agent-kourier-${VERSION}.oci.tar"
  echo "==> building ${NAME} (${PLATFORMS}) to ${archive}; nothing is pushed"
  "${BUILD[@]}" --platform "${PLATFORMS}" --provenance=mode=max --sbom=true ${ANNOTATIONS[@]+"${ANNOTATIONS[@]}"} \
    --metadata-file "${OUT}/metadata.json" --output "type=oci,dest=${archive}" -t "${NAME}" "${ROOT}"
  echo "built ${archive}"
fi
digest=$(image_digest "${OUT}/metadata.json")
echo "${digest}" >"${OUT}/digest"

# Each agent image as one multi-platform build, like Agent Kourier's, from the cache its checks filled.
for a in "${AGENTS[@]}"; do
  agent_name="${REPOSITORY}-${a}:${VERSION}"
  agent_build=("${BUILDX[@]}" --platform "${PLATFORMS}" --provenance=mode=max --sbom=true
    ${ANNOTATIONS[@]+"${ANNOTATIONS[@]}"} --metadata-file "${OUT}/${a}-metadata.json" -t "${agent_name}")
  if [ "${PUSH}" = 1 ]; then
    echo "==> pushing ${agent_name} (${PLATFORMS})"
    "${agent_build[@]}" --push "${ROOT}/examples/recipes/${a}/agent"
  else
    archive="${OUT}/agent-kourier-${a}-${VERSION}.oci.tar"
    echo "==> building ${agent_name} (${PLATFORMS}) to ${archive}; nothing is pushed"
    "${agent_build[@]}" --output "type=oci,dest=${archive}" "${ROOT}/examples/recipes/${a}/agent"
    echo "built ${archive}"
  fi
  image_digest "${OUT}/${a}-metadata.json" >"${OUT}/${a}-digest"
done

echo "==> packaging the chart with image ${REPOSITORY}@${digest}"
chart=$("${HERE}/package-chart.sh" "${VERSION}" "${REPOSITORY}" "${digest}" "${OUT}")
if [ "${PUSH}" = 1 ]; then
  pushed=$(helm push "${chart}" "${CHART_REGISTRY}" 2>&1) || die "helm push failed: ${pushed}"
  chart_digest=$(sed -n 's/^Digest: //p' <<<"${pushed}")
  [[ "${chart_digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "helm push did not report a chart digest: ${pushed}"
  echo "${chart_digest}" >"${OUT}/chart-digest"
  echo "pushed ${CHART_REGISTRY#oci://}/agent-kourier@${chart_digest} (version ${VERSION#v})"
  echo "pushed ${REGISTRY}@${digest} (tag ${VERSION})"
  for a in "${AGENTS[@]}"; do echo "pushed ${REGISTRY}-${a}@$(cat "${OUT}/${a}-digest") (tag ${VERSION})"; done
else
  echo "packaged ${chart}; nothing is pushed"
fi

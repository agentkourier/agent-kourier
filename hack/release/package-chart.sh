#!/usr/bin/env bash
# package-chart.sh VERSION REPOSITORY DIGEST CMA_DIGEST AX_DIGEST OUTDIR: package charts/agent-kourier as
# OUTDIR/agent-kourier-<VERSION without v>.tgz, version VERSION without the v and appVersion VERSION, with the chart's
# default image set to REPOSITORY@DIGEST (values.yaml leaves image.repository empty and image.digest unset until a release
# stamps them) and each add-on's default image set to its pushed one: addons.cma.image to REPOSITORY-cma@CMA_DIGEST and
# addons.ax.image to REPOSITORY-ax@AX_DIGEST (values.yaml names each published repository and leaves its digest empty),
# so a released chart pulls all three by digest.
# It works on a copy, so the tree is untouched, and prints the path of the package. Run by release.sh. CHART_DIR, default
# charts/agent-kourier, is the chart to package; hack/release/test.sh points it at a fixture.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=../kind/lib.sh
. "${ROOT}/hack/kind/lib.sh"

[ $# -eq 6 ] || die "usage: package-chart.sh VERSION REPOSITORY DIGEST CMA_DIGEST AX_DIGEST OUTDIR"
VERSION=$1 REPOSITORY=$2 DIGEST=$3 CMA_DIGEST=$4 AX_DIGEST=$5 OUTDIR=$6
CHART_DIR=${CHART_DIR:-${ROOT}/charts/agent-kourier}
CMA_REPOSITORY="${REPOSITORY}-cma"
AX_REPOSITORY="${REPOSITORY}-ax"
[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] || die "VERSION must be a version tag such as v1.2.3 (got '${VERSION}')"
[[ "${DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "DIGEST must be a sha256 digest (got '${DIGEST}')"
[[ "${CMA_DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "CMA_DIGEST must be a sha256 digest (got '${CMA_DIGEST}')"
[[ "${AX_DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "AX_DIGEST must be a sha256 digest (got '${AX_DIGEST}')"
command -v helm >/dev/null || die "helm is required"

WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT
cp -R "${CHART_DIR}" "${WORK}/agent-kourier"

# Only the top-level image: block (the token-refresh image further down has its own repository and digest), and
# addons.cma.image and addons.ax.image: the keys are found by their indentation, two spaces a level, comments skipped.
# An add-on's repository is stamped whatever it holds, so that it is the repository its digest was pushed to; its tag
# stays empty and the digest wins over it. blocks sets image, cmaimage and aximage on the lines of those three blocks,
# for the stamp and the read back; the stamp exits 2 when the top-level block lacks its empty keys, 3 when the CMA
# add-on's lacks its keys, 4 when the AX add-on's does.
# shellcheck disable=SC2016 # an awk program: $0 is awk's
blocks='
  /^[^ #]/ { image = ($0 ~ /^image:/); addons = ($0 ~ /^addons:/); addon = ""; cmaimage = 0; aximage = 0 }
  addons && /^  [^ #]/ { addon = ($0 ~ /^  cma:/) ? "cma" : ($0 ~ /^  ax:/) ? "ax" : ""; cmaimage = 0; aximage = 0 }
  addons && addon != "" && /^    [^ #]/ { cmaimage = (addon == "cma" && $0 ~ /^    image:/); aximage = (addon == "ax" && $0 ~ /^    image:/) }
'
awk -v repo="${REPOSITORY}" -v digest="${DIGEST}" -v cmarepo="${CMA_REPOSITORY}" -v cmadigest="${CMA_DIGEST}" \
  -v axrepo="${AX_REPOSITORY}" -v axdigest="${AX_DIGEST}" "${blocks}"'
  image && /^  repository: ""$/ { print "  repository: " repo; r++; next }
  image && /^  digest: ""$/ { print "  digest: " digest; d++; next }
  cmaimage && /^      repository: / { print "      repository: " cmarepo; cr++; next }
  cmaimage && /^      digest: ""$/ { print "      digest: " cmadigest; cd++; next }
  aximage && /^      repository: / { print "      repository: " axrepo; ar++; next }
  aximage && /^      digest: ""$/ { print "      digest: " axdigest; ad++; next }
  { print }
  END { if (r != 1 || d != 1) exit 2; if (cr != 1 || cd != 1) exit 3; if (ar != 1 || ad != 1) exit 4 }
' "${WORK}/agent-kourier/values.yaml" >"${WORK}/values.yaml" || {
  status=$?
  [ "${status}" -ne 2 ] || die "the chart's values.yaml no longer has exactly one empty image.repository and image.digest to stamp"
  [ "${status}" -ne 3 ] || die "the chart's values.yaml no longer has exactly one addons.cma.image.repository and empty addons.cma.image.digest to stamp"
  [ "${status}" -ne 4 ] || die "the chart's values.yaml no longer has exactly one addons.ax.image.repository and empty addons.ax.image.digest to stamp"
  die "could not stamp the chart's values.yaml (awk exit ${status})"
}
mv "${WORK}/values.yaml" "${WORK}/agent-kourier/values.yaml"

mkdir -p "${OUTDIR}"
helm package "${WORK}/agent-kourier" --version "${VERSION#v}" --app-version "${VERSION}" --destination "${OUTDIR}" >/dev/null
pkg="${OUTDIR}/agent-kourier-${VERSION#v}.tgz"
[ -f "${pkg}" ] || die "helm package did not write ${pkg}"

# Read back what shipped: the default images (sorted, so the add-ons' blocks may come in either order) and the versions.
shown=$(helm show values "${pkg}" | awk "${blocks}"'
  image && /^  (repository|digest):/ { print "image" $0 }
  cmaimage && /^      (repository|digest):/ { print "cma" $0 }
  aximage && /^      (repository|digest):/ { print "ax" $0 }
' | LC_ALL=C sort)
want=$(printf 'image  repository: %s\nimage  digest: %s\ncma      repository: %s\ncma      digest: %s\nax      repository: %s\nax      digest: %s' \
  "${REPOSITORY}" "${DIGEST}" "${CMA_REPOSITORY}" "${CMA_DIGEST}" "${AX_REPOSITORY}" "${AX_DIGEST}" | LC_ALL=C sort)
[ "${shown}" = "${want}" ] ||
  die "the package's default images are not ${REPOSITORY}@${DIGEST}, ${CMA_REPOSITORY}@${CMA_DIGEST} and ${AX_REPOSITORY}@${AX_DIGEST}"
meta=$(helm show chart "${pkg}")
grep -q -x "version: ${VERSION#v}" <<<"${meta}" || die "the package's version is not ${VERSION#v}"
grep -q -x "appVersion: ${VERSION}" <<<"${meta}" || die "the package's appVersion is not ${VERSION}"
echo "${pkg}"

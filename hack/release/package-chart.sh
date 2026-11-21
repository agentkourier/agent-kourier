#!/usr/bin/env bash
# package-chart.sh VERSION REPOSITORY DIGEST CMA_DIGEST OUTDIR: package charts/agent-kourier as
# OUTDIR/agent-kourier-<VERSION without v>.tgz, version VERSION without the v and appVersion VERSION, with the chart's
# default image set to REPOSITORY@DIGEST (values.yaml leaves image.repository empty and image.digest unset until a release
# stamps them) and the CMA add-on's default image, addons.cma.image, set to REPOSITORY-cma@CMA_DIGEST (values.yaml names
# the published repository and leaves the digest empty), so a released chart pulls both by digest.
# It works on a copy, so the tree is untouched, and prints the path of the package. Run by release.sh. CHART_DIR, default
# charts/agent-kourier, is the chart to package; hack/release/test.sh points it at a fixture.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=../kind/lib.sh
. "${ROOT}/hack/kind/lib.sh"

[ $# -eq 5 ] || die "usage: package-chart.sh VERSION REPOSITORY DIGEST CMA_DIGEST OUTDIR"
VERSION=$1 REPOSITORY=$2 DIGEST=$3 CMA_DIGEST=$4 OUTDIR=$5
CHART_DIR=${CHART_DIR:-${ROOT}/charts/agent-kourier}
CMA_REPOSITORY="${REPOSITORY}-cma"
[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] || die "VERSION must be a version tag such as v1.2.3 (got '${VERSION}')"
[[ "${DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "DIGEST must be a sha256 digest (got '${DIGEST}')"
[[ "${CMA_DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "CMA_DIGEST must be a sha256 digest (got '${CMA_DIGEST}')"
command -v helm >/dev/null || die "helm is required"

WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT
cp -R "${CHART_DIR}" "${WORK}/agent-kourier"

# Only the top-level image: block (the token-refresh image further down has its own repository and digest), and
# addons.cma.image: the keys are found by their indentation, two spaces a level, comments skipped. The add-on's
# repository is stamped whatever it holds, so that it is the repository its digest was pushed to; its tag stays empty and
# the digest wins over it. blocks sets image and cmaimage on the lines of those two blocks, for the stamp and the read
# back; the stamp exits 2 when the top-level block lacks its empty keys, 3 when the add-on's lacks its keys.
# shellcheck disable=SC2016 # an awk program: $0 is awk's
blocks='
  /^[^ #]/ { image = ($0 ~ /^image:/); addons = ($0 ~ /^addons:/); cma = 0; cmaimage = 0 }
  addons && /^  [^ #]/ { cma = ($0 ~ /^  cma:/); cmaimage = 0 }
  addons && cma && /^    [^ #]/ { cmaimage = ($0 ~ /^    image:/) }
'
awk -v repo="${REPOSITORY}" -v digest="${DIGEST}" -v cmarepo="${CMA_REPOSITORY}" -v cmadigest="${CMA_DIGEST}" "${blocks}"'
  image && /^  repository: ""$/ { print "  repository: " repo; r++; next }
  image && /^  digest: ""$/ { print "  digest: " digest; d++; next }
  cmaimage && /^      repository: / { print "      repository: " cmarepo; cr++; next }
  cmaimage && /^      digest: ""$/ { print "      digest: " cmadigest; cd++; next }
  { print }
  END { if (r != 1 || d != 1) exit 2; if (cr != 1 || cd != 1) exit 3 }
' "${WORK}/agent-kourier/values.yaml" >"${WORK}/values.yaml" || {
  status=$?
  [ "${status}" -ne 2 ] || die "the chart's values.yaml no longer has exactly one empty image.repository and image.digest to stamp"
  [ "${status}" -ne 3 ] || die "the chart's values.yaml no longer has exactly one addons.cma.image.repository and empty addons.cma.image.digest to stamp"
  die "could not stamp the chart's values.yaml (awk exit ${status})"
}
mv "${WORK}/values.yaml" "${WORK}/agent-kourier/values.yaml"

mkdir -p "${OUTDIR}"
helm package "${WORK}/agent-kourier" --version "${VERSION#v}" --app-version "${VERSION}" --destination "${OUTDIR}" >/dev/null
pkg="${OUTDIR}/agent-kourier-${VERSION#v}.tgz"
[ -f "${pkg}" ] || die "helm package did not write ${pkg}"

# Read back what shipped: the default images and the versions.
shown=$(helm show values "${pkg}" | awk "${blocks}"'
  image && /^  (repository|digest):/ { print "image" $0 }
  cmaimage && /^      (repository|digest):/ { print "cma" $0 }
')
want=$(printf 'image  repository: %s\nimage  digest: %s\ncma      repository: %s\ncma      digest: %s' \
  "${REPOSITORY}" "${DIGEST}" "${CMA_REPOSITORY}" "${CMA_DIGEST}")
[ "${shown}" = "${want}" ] ||
  die "the package's default images are not ${REPOSITORY}@${DIGEST} and ${CMA_REPOSITORY}@${CMA_DIGEST}"
meta=$(helm show chart "${pkg}")
grep -q -x "version: ${VERSION#v}" <<<"${meta}" || die "the package's version is not ${VERSION#v}"
grep -q -x "appVersion: ${VERSION}" <<<"${meta}" || die "the package's appVersion is not ${VERSION}"
echo "${pkg}"

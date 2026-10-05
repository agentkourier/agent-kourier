#!/usr/bin/env bash
# package-chart.sh VERSION REPOSITORY DIGEST OUTDIR: package charts/courier as OUTDIR/courier-<VERSION without v>.tgz,
# version VERSION without the v and appVersion VERSION, with the chart's default image set to
# REPOSITORY@DIGEST (values.yaml leaves image.repository empty and image.digest unset until a release stamps them).
# It works on a copy, so the tree is untouched, and prints the path of the package. Run by release.sh.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=../kind/lib.sh
. "${ROOT}/hack/kind/lib.sh"

[ $# -eq 4 ] || die "usage: package-chart.sh VERSION REPOSITORY DIGEST OUTDIR"
VERSION=$1 REPOSITORY=$2 DIGEST=$3 OUTDIR=$4
[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] || die "VERSION must be a version tag such as v1.2.3 (got '${VERSION}')"
[[ "${DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "DIGEST must be a sha256 digest (got '${DIGEST}')"
command -v helm >/dev/null || die "helm is required"

WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT
cp -R "${ROOT}/charts/courier" "${WORK}/courier"

# Only the top-level image: block (the token-refresh image further down has its own repository and digest).
awk -v repo="${REPOSITORY}" -v digest="${DIGEST}" '
  /^[^ #]/ { inimage = ($0 ~ /^image:/) }
  inimage && /^  repository: ""$/ { print "  repository: " repo; r++; next }
  inimage && /^  digest: ""$/ { print "  digest: " digest; d++; next }
  { print }
  END { if (r != 1 || d != 1) exit 1 }
' "${WORK}/courier/values.yaml" >"${WORK}/values.yaml" ||
  die "charts/courier/values.yaml no longer has exactly one empty image.repository and image.digest to stamp"
mv "${WORK}/values.yaml" "${WORK}/courier/values.yaml"

mkdir -p "${OUTDIR}"
helm package "${WORK}/courier" --version "${VERSION#v}" --app-version "${VERSION}" --destination "${OUTDIR}" >/dev/null
pkg="${OUTDIR}/courier-${VERSION#v}.tgz"
[ -f "${pkg}" ] || die "helm package did not write ${pkg}"

# Read back what shipped: the default image and the versions.
shown=$(helm show values "${pkg}" | awk '/^[^ #]/ { i = ($0 ~ /^image:/) } i && /^  (repository|digest):/')
want=$(printf '  repository: %s\n  digest: %s' "${REPOSITORY}" "${DIGEST}")
[ "${shown}" = "${want}" ] || die "the package's default image is not ${REPOSITORY}@${DIGEST}"
meta=$(helm show chart "${pkg}")
grep -q -x "version: ${VERSION#v}" <<<"${meta}" || die "the package's version is not ${VERSION#v}"
grep -q -x "appVersion: ${VERSION}" <<<"${meta}" || die "the package's appVersion is not ${VERSION}"
echo "${pkg}"

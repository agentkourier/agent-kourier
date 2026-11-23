#!/usr/bin/env bash
# package-chart.sh VERSION REPOSITORY DIGEST OUTDIR NAME=DIGEST...: package charts/agent-kourier as
# OUTDIR/agent-kourier-<VERSION without v>.tgz, version VERSION without the v and appVersion VERSION, with the chart's
# default image set to REPOSITORY@DIGEST (values.yaml leaves image.repository empty and image.digest unset until a release
# stamps them) and each add-on's default image set to its pushed one: addons.NAME.image to REPOSITORY-NAME@DIGEST for
# each NAME=DIGEST (values.yaml names each published repository and leaves its digest empty), so a released chart pulls
# every image by digest. The NAMEs must be the add-ons of hack/addons.txt (hack/addons.sh), each once, in any order.
# It works on a copy, so the tree is untouched, and prints the path of the package. Run by release.sh. CHART_DIR, default
# charts/agent-kourier, is the chart to package; hack/release/test.sh points it at a fixture.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=../kind/lib.sh
. "${ROOT}/hack/kind/lib.sh"

usage="usage: package-chart.sh VERSION REPOSITORY DIGEST OUTDIR NAME=DIGEST..."
[ $# -ge 4 ] || die "${usage}"
VERSION=$1 REPOSITORY=$2 DIGEST=$3 OUTDIR=$4
shift 4
CHART_DIR=${CHART_DIR:-${ROOT}/charts/agent-kourier}
[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] || die "VERSION must be a version tag such as v1.2.3 (got '${VERSION}')"
[[ "${DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "DIGEST must be a sha256 digest (got '${DIGEST}')"
command -v helm >/dev/null || die "helm is required"

# ADDONS and ADDON_DIGESTS: each add-on of the list, in its order, and the digest its pair gave it.
list=$("${ROOT}/hack/addons.sh") || die "could not read the add-ons of hack/addons.txt"
ADDONS=()
while IFS= read -r a; do ADDONS+=("${a}"); done <<<"${list}"
ADDON_DIGESTS=()
for pair in "$@"; do
  [[ "${pair}" == *=* ]] || die "'${pair}': want NAME=DIGEST (${usage})"
  name=${pair%%=*} digest=${pair#*=} at=-1
  for i in "${!ADDONS[@]}"; do [ "${ADDONS[i]}" != "${name}" ] || at=${i}; done
  [ "${at}" -ge 0 ] || die "${name} is not an add-on of hack/addons.txt (${ADDONS[*]})"
  [ -z "${ADDON_DIGESTS[at]:-}" ] || die "${name} has two digests"
  [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "the digest of ${name} must be a sha256 digest (got '${digest}')"
  ADDON_DIGESTS[at]=${digest}
done
for i in "${!ADDONS[@]}"; do
  [ -n "${ADDON_DIGESTS[i]:-}" ] || die "no digest for the add-on ${ADDONS[i]} (${usage})"
done

WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT
cp -R "${CHART_DIR}" "${WORK}/agent-kourier"

# Only the top-level image: block (the token-refresh image further down has its own repository and digest), and each
# listed add-on's addons.NAME.image: the keys are found by their indentation, two spaces a level, comments skipped. An
# add-on's repository is stamped whatever it holds, so that it is the repository its digest was pushed to; its tag stays
# empty and the digest wins over it. blocks sets image on the lines of the top-level block, and aimage to the add-on
# whose image block a line is in ("" outside them), for the stamp and the read back. names is the add-ons, space
# separated (hack/addons.sh allows no space or regular expression character in a name).
# shellcheck disable=SC2016 # an awk program: $0 is awk's
blocks='
  BEGIN { n = split(names, list, " "); for (i = 1; i <= n; i++) listed[list[i]] = 1 }
  /^[^ #]/ { image = ($0 ~ /^image:/); addons = ($0 ~ /^addons:/); addon = ""; aimage = "" }
  addons && /^  [^ #]/ { key = substr($0, 3); sub(/:.*/, "", key); addon = (key in listed) ? key : ""; aimage = "" }
  addons && addon != "" && /^    [^ #]/ { aimage = ($0 ~ /^    image:/) ? addon : "" }
'
# The stamp exits 2 when the top-level block lacks its empty keys, and 3 when an add-on's block lacks its keys, having
# printed that add-on's name to the file missing.
digests=''
for i in "${!ADDONS[@]}"; do digests+="${digests:+ }${ADDON_DIGESTS[i]}"; done
awk -v repo="${REPOSITORY}" -v digest="${DIGEST}" -v names="${ADDONS[*]}" -v digests="${digests}" \
  -v missing="${WORK}/missing" "${blocks}"'
  BEGIN { split(digests, d, " "); for (i = 1; i <= n; i++) adigest[list[i]] = d[i] }
  image && /^  repository: ""$/ { print "  repository: " repo; r++; next }
  image && /^  digest: ""$/ { print "  digest: " digest; d0++; next }
  aimage != "" && /^      repository: / { print "      repository: " repo "-" aimage; ar[aimage]++; next }
  aimage != "" && /^      digest: ""$/ { print "      digest: " adigest[aimage]; ad[aimage]++; next }
  { print }
  END {
    if (r != 1 || d0 != 1) exit 2
    for (i = 1; i <= n; i++) if (ar[list[i]] != 1 || ad[list[i]] != 1) { print list[i] >missing; exit 3 }
  }
' "${WORK}/agent-kourier/values.yaml" >"${WORK}/values.yaml" || {
  status=$?
  [ "${status}" -ne 2 ] || die "the chart's values.yaml no longer has exactly one empty image.repository and image.digest to stamp"
  if [ "${status}" -eq 3 ]; then
    a=$(cat "${WORK}/missing")
    die "the chart's values.yaml no longer has exactly one addons.${a}.image.repository and empty addons.${a}.image.digest to stamp"
  fi
  die "could not stamp the chart's values.yaml (awk exit ${status})"
}
mv "${WORK}/values.yaml" "${WORK}/agent-kourier/values.yaml"

mkdir -p "${OUTDIR}"
helm package "${WORK}/agent-kourier" --version "${VERSION#v}" --app-version "${VERSION}" --destination "${OUTDIR}" >/dev/null
pkg="${OUTDIR}/agent-kourier-${VERSION#v}.tgz"
[ -f "${pkg}" ] || die "helm package did not write ${pkg}"

# Read back what shipped: the default images (sorted, so the add-ons' blocks may come in any order) and the versions.
shown=$(helm show values "${pkg}" | awk -v names="${ADDONS[*]}" "${blocks}"'
  image && /^  (repository|digest):/ { print "image" $0 }
  aimage != "" && /^      (repository|digest):/ { print aimage $0 }
' | LC_ALL=C sort)
want=$({
  printf 'image  repository: %s\nimage  digest: %s\n' "${REPOSITORY}" "${DIGEST}"
  for i in "${!ADDONS[@]}"; do
    printf '%s      repository: %s\n%s      digest: %s\n' "${ADDONS[i]}" "${REPOSITORY}-${ADDONS[i]}" "${ADDONS[i]}" "${ADDON_DIGESTS[i]}"
  done
} | LC_ALL=C sort)
shipped="${REPOSITORY}@${DIGEST}"
for i in "${!ADDONS[@]}"; do shipped+=", ${REPOSITORY}-${ADDONS[i]}@${ADDON_DIGESTS[i]}"; done
[ "${shown}" = "${want}" ] || die "the package's default images are not ${shipped}"
meta=$(helm show chart "${pkg}")
grep -q -x "version: ${VERSION#v}" <<<"${meta}" || die "the package's version is not ${VERSION#v}"
grep -q -x "appVersion: ${VERSION}" <<<"${meta}" || die "the package's appVersion is not ${VERSION}"
echo "${pkg}"

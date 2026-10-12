#!/usr/bin/env bash
# Select the latest stable release and the latest prerelease image references
# from SEC-2's GitHub Release metadata. Only immutable image digests are
# emitted; moving tags are deliberately ignored.
#
# Usage: select-release-images.sh RELEASES_JSON OUTFILE
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

[ "$#" -eq 2 ] || die "usage: $0 RELEASES_JSON OUTFILE"
RELEASES_JSON=$1
OUTFILE=$2
[ -f "${RELEASES_JSON}" ] || die "release metadata does not exist: ${RELEASES_JSON}"
[ -n "${OUTFILE}" ] || die "OUTFILE must not be empty"
command -v jq >/dev/null || die "jq is required"

mkdir -p "$(dirname -- "${OUTFILE}")"
candidate_file=$(mktemp)
trap 'rm -f "${candidate_file}"' EXIT

# -e makes malformed JSON, a non-array API response, or a non-string body a
# hard failure. A valid empty release list is a supported no-release state.
jq -e '
  type == "array" and all(.[];
    type == "object" and
    ((.draft // false) == true or
      ((.tag_name | type) == "string" and
       (.tag_name | test("^v[0-9]+\\.[0-9]+\\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$")) and
       ((.body // "" | type) == "string"))))
' "${RELEASES_JSON}" >/dev/null || die "GitHub Release metadata has invalid published entries"

jq -r '
  .[]
  | select((.draft // false) == false)
  | (.tag_name // "") as $tag
  | select($tag | test("^v[0-9]+\\.[0-9]+\\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$"))
  | [$tag, (if ((.prerelease // false) or ($tag | contains("-"))) then "prerelease" else "stable" end)]
  | @tsv
' "${RELEASES_JSON}" | sort -u >"${candidate_file}" || die "could not parse GitHub Release metadata"

: >"${OUTFILE}"
stable=$(awk -F '\t' '$2 == "stable" {print $1}' "${candidate_file}" | sort -V | tail -n 1)
for kind in stable prerelease; do
  tag=$(awk -F '\t' -v want="${kind}" '$2 == want {print $1}' "${candidate_file}" | sort -V | tail -n 1)
  [ -n "${tag}" ] || continue
  if [ "${kind}" = prerelease ] && [ -n "${stable}" ]; then
    # Only a candidate for a later release needs monitoring once stable ships.
    if ! jq -en --arg stable "${stable}" --arg candidate "${tag}" '
      def base: split("-")[0] | ltrimstr("v") | split(".") | map(tonumber);
      ($candidate | base) > ($stable | base)
    ' >/dev/null; then
      continue
    fi
  fi
  refs=$(jq -r --arg tag "${tag}" '
    .[]
    | select((.draft // false) == false and .tag_name == $tag)
    | (.body // "")
    | scan("ghcr\\.io/agentkourier/agent-kourier@sha256:[0-9a-f]{64}")
  ' "${RELEASES_JSON}") || die "could not parse image metadata for release ${tag}"
  [ -n "${refs}" ] || die "release ${tag} does not publish an immutable Agent Kourier image digest"
  printf '%s\n' "${refs}" >>"${OUTFILE}"
done

sort -u -o "${OUTFILE}" "${OUTFILE}"

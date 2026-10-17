#!/usr/bin/env bash
# Upload film renders to the site's media bucket and check that the public URL serves them.
#
#   upload-film.sh MP4 ...
#
# Each file goes to <name>.<first 12 hex digits of its SHA-256>.mp4 in the R2 bucket
# agentkourier-media, which is public only at https://media.agentkourier.dev. A key names one set of
# bytes, so a re-cut never replaces a published cut: it gets a new key, a page built against an older cut
# keeps playing it, and the object can be cached for a year. Uploading the same file again rewrites the
# same bytes. The script does not probe the URL before the upload: Cloudflare can cache a 404 for a few
# minutes, and that would fail the checks that follow.
#
# The owner runs this by hand after `npx --yes wrangler@<WRANGLER_VERSION> login`; CI holds no R2 token.
# It prints each public URL, for docs/index.md and docs/demo.md.
set -euo pipefail

WRANGLER_VERSION=4.147.0
BUCKET=agentkourier-media
MEDIA_URL=https://media.agentkourier.dev
CACHE_CONTROL='public, max-age=31536000, immutable'

usage() {
  echo "usage: upload-film.sh MP4 ..." >&2
  exit 2
}

die() {
  echo "error: $*" >&2
  exit 1
}

# The value of response header $2 in the header dump $1, without the trailing CR.
header() {
  grep -i "^$2:" "$1" | tail -n 1 | cut -d: -f2- | sed 's/^ *//; s/\r$//'
}

[ $# -ge 1 ] || usage
for tool in ffprobe curl shasum npx; do
  command -v "${tool}" >/dev/null || die "${tool} is not installed"
done

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

for file in "$@"; do
  [ -f "${file}" ] || die "${file}: no such file"
  case "${file}" in
    *.mp4) ;;
    *) die "${file}: not an .mp4" ;;
  esac

  # A video whose moov atom follows the media data cannot start playing until it has fully downloaded.
  first=$(ffprobe -v trace -i "${file}" 2>&1 | grep -o -m 1 "type:'\(moov\|mdat\)'" || true)
  [ "${first}" = "type:'moov'" ] || die "${file}: not faststart (re-encode with -movflags +faststart)"

  size=$(wc -c <"${file}" | tr -d ' ')
  hash=$(shasum -a 256 "${file}" | cut -c1-12)
  name=$(basename "${file}" .mp4)
  key=${name}.${hash}.mp4
  url=${MEDIA_URL}/${key}

  npx --yes "wrangler@${WRANGLER_VERSION}" r2 object put "${BUCKET}/${key}" --remote \
    --file "${file}" --content-type video/mp4 --cache-control "${CACHE_CONTROL}"

  # The public URL serves the whole object with the headers set above, and serves byte ranges, which
  # Safari needs to play a video at all.
  curl -sS -f -o /dev/null -I -D "${TMP}/head" "${url}" || die "${url}: HEAD failed"
  [ "$(header "${TMP}/head" content-length)" = "${size}" ] || die "${url}: Content-Length is not ${size}"
  [ "$(header "${TMP}/head" content-type)" = video/mp4 ] || die "${url}: Content-Type is not video/mp4"
  [ "$(header "${TMP}/head" cache-control)" = "${CACHE_CONTROL}" ] || die "${url}: Cache-Control is not '${CACHE_CONTROL}'"
  range=$(curl -sS -o /dev/null -D "${TMP}/range" -H 'Range: bytes=0-1' -w '%{http_code}' "${url}")
  [ "${range}" = 206 ] || die "${url}: a Range request returned ${range}, not 206"
  [ "$(header "${TMP}/range" content-range)" = "bytes 0-1/${size}" ] || die "${url}: wrong Content-Range"

  echo "${url}"
done

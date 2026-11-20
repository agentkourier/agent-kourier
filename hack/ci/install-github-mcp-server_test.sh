#!/usr/bin/env bash
# Tests for install-github-mcp-server.sh. Offline: each case sources the installer in a subshell and replaces its download function with one
# that copies a local fixture archive and records the URLs it was asked for, so no case reaches the network.
# Run as `hack/ci/install-github-mcp-server_test.sh`.
# shellcheck disable=SC2031 # run_installer's subshell re-sources the pins; the ones read here are the top-level copy
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=${HERE}/install-github-mcp-server.sh
DEPLOYMENT=${HERE}/../../examples/recipes/webhook-github/agent/manifests/deployment.yaml

# A CI job's own GITHUB_ENV must not reach the installer: each case sets what it needs.
unset GITHUB_ENV

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# The pins and sha256_of, for the checks below; main runs only when the installer is executed.
# shellcheck source=hack/ci/install-github-mcp-server.sh
source "${SCRIPT}"

# The fixture: a release archive that holds a fake binary, beside the README and LICENSE the real one has.
FIX=${TMP}/fixtures
mkdir -p "${FIX}/archive"
printf '#!/bin/sh\necho fake-github-mcp-server\n' >"${FIX}/archive/github-mcp-server"
printf 'readme\n' >"${FIX}/archive/README.md"
printf 'license\n' >"${FIX}/archive/LICENSE"
tar -czf "${FIX}/archive.tar.gz" -C "${FIX}/archive" github-mcp-server README.md LICENSE
sha256_of "${FIX}/archive.tar.gz" >"${FIX}/archive.sha256"

# run_installer LOG [ARG...]: the installer for linux/amd64 with the fixture behind its download, its digest as the pin (BAD set: a wrong one),
# and every requested URL appended to LOG. Its environment is the caller's, so a GITHUB_ENV set for one call reaches it.
run_installer() {
  local log=$1
  shift
  (
    # shellcheck source=hack/ci/install-github-mcp-server.sh
    source "${SCRIPT}"
    detect_platform() { printf 'linux-amd64\n'; }
    download() { # URL OUT
      printf '%s\n' "$1" >>"${log}"
      cp "${FIX}/archive.tar.gz" "$2"
    }
    pinned_sha256() { # PLATFORM
      if [ -n "${BAD:-}" ]; then
        printf '%064d\n' 0
        return
      fi
      cat "${FIX}/archive.sha256"
    }
    main "$@"
  )
}

echo "== a good checksum installs the binary alone, executable, from the release URL, and prints its path"
dir=${TMP}/good/bin
log=${TMP}/good.urls
out=$(run_installer "${log}" "${dir}")
[ "${out}" = "${dir}/github-mcp-server-${VERSION}" ] || fail "stdout was [${out}], want [${dir}/github-mcp-server-${VERSION}]"
[ "$("${out}")" = fake-github-mcp-server ] || fail "${out} is not the archive's binary"
[ "$(find "${dir}" -mindepth 1 | wc -l | tr -d ' ')" = 1 ] || fail "bin dir holds more than the binary: $(ls -A "${dir}")"
want_url="https://github.com/github/github-mcp-server/releases/download/${VERSION}/github-mcp-server_Linux_x86_64.tar.gz"
[ "$(cat "${log}")" = "${want_url}" ] || fail "downloaded from [$(cat "${log}")], want [${want_url}]"
echo "ok"

echo "== an installed binary of the pinned version is kept, with no download"
run_installer "${TMP}/rerun.urls" "${dir}" >/dev/null
[ ! -e "${TMP}/rerun.urls" ] || fail "a rerun downloaded again: $(cat "${TMP}/rerun.urls")"
echo "ok"

echo "== GITHUB_ENV gets GITHUB_MCP_SERVER_BIN"
gh_env=${TMP}/github_env
printf 'ALREADY=there\n' >"${gh_env}"
GITHUB_ENV=${gh_env} run_installer "${TMP}/env.urls" "${dir}" >/dev/null
[ "$(cat "${gh_env}")" = "ALREADY=there
GITHUB_MCP_SERVER_BIN=${dir}/github-mcp-server-${VERSION}" ] || fail "GITHUB_ENV holds [$(cat "${gh_env}")]"
echo "ok"

echo "== a bad checksum fails, names both digests, and installs nothing"
dir=${TMP}/bad/bin
gh_env=${TMP}/bad.github_env
: >"${gh_env}"
if err=$(BAD=1 GITHUB_ENV=${gh_env} run_installer "${TMP}/bad.urls" "${dir}" 2>&1 >/dev/null); then
  fail "a bad checksum was accepted"
fi
case "${err}" in
  *"$(printf '%064d' 0)"*"$(cat "${FIX}/archive.sha256")"*) ;;
  *) fail "the mismatch error does not name the pinned and the actual digest: ${err}" ;;
esac
[ -z "$(ls -A "${dir}" 2>/dev/null)" ] || fail "a bad checksum left files in ${dir}: $(ls -A "${dir}")"
[ ! -s "${gh_env}" ] || fail "a bad checksum still appended to GITHUB_ENV"
echo "ok"

echo "== every supported platform has a pinned sha256 and an archive URL the release names"
for host in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  sum=$(pinned_sha256 "${host}")
  [[ ${sum} =~ ^[0-9a-f]{64}$ ]] || fail "no sha256 pinned for ${host}: [${sum}]"
  [[ $(asset_url "${host}") =~ /github-mcp-server_(Linux|Darwin)_(x86_64|arm64)\.tar\.gz$ ]] || fail "no archive URL for ${host}: $(asset_url "${host}")"
done
echo "ok"

echo "== the pin is the version the recipe's sidecar runs"
grep -q "image: ghcr.io/github/github-mcp-server:${VERSION}@sha256:" "${DEPLOYMENT}" ||
  fail "${DEPLOYMENT} does not run ghcr.io/github/github-mcp-server:${VERSION} by digest: keep the two equal"
echo "ok"

printf 'install-github-mcp-server tests passed\n'

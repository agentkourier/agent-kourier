#!/usr/bin/env bash
# Install the pinned github-mcp-server binary, the sidecar of the webhook-github recipe's agent, for the prompt agent's tests that run it
# (examples/recipes/prompt-agent/agent/test_webhook_github.py, which read its path from GITHUB_MCP_SERVER_BIN). Usage:
# install-github-mcp-server.sh [BIN_DIR]; BIN_DIR defaults to bin/ at the repository root, which git ignores.
#
# The release archive for this host is downloaded from the project's GitHub release and its sha256 checked against the pin below before
# anything is installed: a mismatch exits non-zero naming both digests, and leaves BIN_DIR as it was. The binary is installed as
# BIN_DIR/github-mcp-server-<version>, and its path printed on stdout; when GITHUB_ENV is set, GITHUB_MCP_SERVER_BIN=<path> is also appended
# there, so the job's later steps run the tests. An installed binary of the pinned version is kept, with no download: it was checked when it was
# installed. Hosts: linux and darwin, amd64 and arm64.
#
# hack/ci/install-github-mcp-server_test.sh sources this file (main runs only when it is executed): it reads the pins and sha256_of, and
# replaces detect_platform, download and pinned_sha256.
set -euo pipefail

# The pin. To bump it, change the version here and replace the four sha256 values in pinned_sha256 with those of the release's
# github-mcp-server_<version>_checksums.txt; a version-only edit fails every check. Keep it at the version that the recipe's Deployment
# (examples/recipes/webhook-github/agent/manifests/deployment.yaml) runs, which the test script holds it to.
VERSION=v2.2.0

# The staging directory of a run, inside BIN_DIR so that installing is a rename; removed on exit.
STAGE=

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# pinned_sha256 PLATFORM: the pinned sha256 of the release archive for PLATFORM (linux-amd64 and the like), from the release's checksums file.
pinned_sha256() {
  case "$1" in
    linux-amd64) echo 157c5664b273e5070126d414f88cbab765630acde588f75dafdf9998c2235c51 ;;
    linux-arm64) echo d21e4fca8603b59d99380f1c332aaea36d73033d52b389867adf2124ee150983 ;;
    darwin-amd64) echo a42e667b99915cf368b92aa419fd2f753549ea7133cb3572a3e7f3e2d4a8675b ;;
    darwin-arm64) echo 5e46b05b87989d23267f0a69ee60a3acd1de51a72512ee5fcc2786ca60c54a6a ;;
    *) return 1 ;;
  esac
}

# asset_url PLATFORM: the release URL of the archive for PLATFORM. The project names its archives Linux or Darwin, x86_64 or arm64.
asset_url() {
  local os=${1%-*} arch=${1#*-}
  case "${os}" in linux) os=Linux ;; darwin) os=Darwin ;; esac
  case "${arch}" in amd64) arch=x86_64 ;; esac
  echo "https://github.com/github/github-mcp-server/releases/download/${VERSION}/github-mcp-server_${os}_${arch}.tar.gz"
}

# detect_platform: this host as OS-ARCH.
detect_platform() {
  local os arch
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "unsupported host OS: $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported host architecture: $(uname -m)" ;;
  esac
  printf '%s-%s\n' "${os}" "${arch}"
}

# download URL OUT: fetch URL into OUT over HTTPS only, redirects included.
download() {
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent --show-error --location \
    --retry 3 --output "$2" "$1"
}

# sha256_of FILE: FILE's sha256 in hex.
sha256_of() {
  if command -v sha256sum >/dev/null; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

main() {
  local bin=${1:-} platform url want got target
  if [ -z "${bin}" ]; then
    bin=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/bin
  fi
  command -v curl >/dev/null || die "curl is required"
  command -v tar >/dev/null || die "tar is required"
  command -v sha256sum >/dev/null || command -v shasum >/dev/null || die "sha256sum or shasum is required"
  platform=$(detect_platform)
  mkdir -p "${bin}" 2>/dev/null || die "cannot create ${bin}"
  [ -w "${bin}" ] || die "${bin} is not writable by this user"
  bin=$(cd "${bin}" && pwd)
  target=${bin}/github-mcp-server-${VERSION}

  if [ ! -x "${target}" ]; then
    url=$(asset_url "${platform}")
    want=$(pinned_sha256 "${platform}") || die "no sha256 pinned for ${platform}"
    STAGE=$(mktemp -d "${bin}/.install-github-mcp-server.XXXXXX")
    trap 'rm -rf "${STAGE}"' EXIT
    download "${url}" "${STAGE}/archive.tar.gz" || die "downloading ${url} failed"
    got=$(sha256_of "${STAGE}/archive.tar.gz")
    [ "${got}" = "${want}" ] || die "github-mcp-server sha256 mismatch for ${platform}: pinned ${want}, downloaded ${got} (${url}); nothing installed"
    tar -xzf "${STAGE}/archive.tar.gz" -C "${STAGE}" github-mcp-server
    chmod 0755 "${STAGE}/github-mcp-server"
    mv -f "${STAGE}/github-mcp-server" "${target}"
    rm -rf "${STAGE}"
    printf 'installed github-mcp-server %s (%s) as %s\n' "${VERSION}" "${platform}" "${target}" >&2
  fi
  if [ -n "${GITHUB_ENV:-}" ]; then
    printf 'GITHUB_MCP_SERVER_BIN=%s\n' "${target}" >>"${GITHUB_ENV}"
  fi
  printf '%s\n' "${target}"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi

#!/usr/bin/env bash
# Install the exact Gitleaks release used by CI into a caller-owned directory
# and print the resulting executable path.
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# When this version changes, refresh every asset checksum below from the same release.
GITLEAKS_VERSION=v8.30.1
TOOL_DIR=${1:-${RUNNER_TEMP:-${TMPDIR:-/tmp}/agent-kourier-security-tools}}
OS=$(uname -s)
ARCH=$(uname -m)

case "${OS}/${ARCH}" in
  Linux/x86_64) ASSET="gitleaks_8.30.1_linux_x64.tar.gz"; CHECKSUM=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb ;;
  Linux/aarch64 | Linux/arm64) ASSET="gitleaks_8.30.1_linux_arm64.tar.gz"; CHECKSUM=e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080 ;;
  Darwin/arm64) ASSET="gitleaks_8.30.1_darwin_arm64.tar.gz"; CHECKSUM=b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5 ;;
  Darwin/x86_64) ASSET="gitleaks_8.30.1_darwin_x64.tar.gz"; CHECKSUM=dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709 ;;
  *) die "unsupported Gitleaks host: ${OS}/${ARCH}" ;;
esac

command -v curl >/dev/null || die "curl is required"
command -v tar >/dev/null || die "tar is required"
mkdir -p "${TOOL_DIR}"
archive="${TOOL_DIR}/${ASSET}"
url="https://github.com/gitleaks/gitleaks/releases/download/${GITLEAKS_VERSION}/${ASSET}"
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location --output "${archive}" "${url}"

if command -v sha256sum >/dev/null; then
  printf '%s  %s\n' "${CHECKSUM}" "${archive}" | sha256sum --check --strict - >/dev/null
else
  command -v shasum >/dev/null || die "sha256sum or shasum is required"
  printf '%s  %s\n' "${CHECKSUM}" "${archive}" | shasum -a 256 --check --strict - >/dev/null
fi

tar -xzf "${archive}" -C "${TOOL_DIR}" gitleaks
chmod 0755 "${TOOL_DIR}/gitleaks"
printf '%s\n' "${TOOL_DIR}/gitleaks"

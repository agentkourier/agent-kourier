#!/usr/bin/env bash
# Install the exact zizmor release used by CI into a caller-owned directory
# and print the resulting executable path.
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# When this version changes, refresh every asset checksum below from the same release. zizmor's asset names
# carry no version, so a version-only edit downloads the new release and fails its checksum here.
ZIZMOR_VERSION=v1.30.1
TOOL_DIR=${1:-${RUNNER_TEMP:-${TMPDIR:-/tmp}/agent-kourier-security-tools}}
OS=$(uname -s)
ARCH=$(uname -m)

case "${OS}/${ARCH}" in
  Linux/x86_64) ASSET="zizmor-x86_64-unknown-linux-gnu.tar.gz"; CHECKSUM=e65324f4430c2717591937edcec90ccbefaf14c174f8ec9415e03ca875b46e1a ;;
  Linux/aarch64 | Linux/arm64) ASSET="zizmor-aarch64-unknown-linux-gnu.tar.gz"; CHECKSUM=7ff1dce33bdd18fd2a4affe63bdd47efcccca97b2cec1c1863ec26e9e2647540 ;;
  Darwin/arm64) ASSET="zizmor-aarch64-apple-darwin.tar.gz"; CHECKSUM=e28d22b087f9ebb8d99da6e740d348c930f559961c7c3f12badda54f882195a2 ;;
  Darwin/x86_64) ASSET="zizmor-x86_64-apple-darwin.tar.gz"; CHECKSUM=10e6b18b11ea07e515a16f0f0518c7b07527bc9977c1fd5698181ce7f3554202 ;;
  *) die "unsupported zizmor host: ${OS}/${ARCH}" ;;
esac

command -v curl >/dev/null || die "curl is required"
command -v tar >/dev/null || die "tar is required"
mkdir -p "${TOOL_DIR}"
archive="${TOOL_DIR}/${ASSET}"
url="https://github.com/zizmorcore/zizmor/releases/download/${ZIZMOR_VERSION}/${ASSET}"
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location --output "${archive}" "${url}"

if command -v sha256sum >/dev/null; then
  printf '%s  %s\n' "${CHECKSUM}" "${archive}" | sha256sum --check --strict - >/dev/null
else
  command -v shasum >/dev/null || die "sha256sum or shasum is required"
  printf '%s  %s\n' "${CHECKSUM}" "${archive}" | shasum -a 256 --check --strict - >/dev/null
fi

tar -xzf "${archive}" -C "${TOOL_DIR}" zizmor
chmod 0755 "${TOOL_DIR}/zizmor"
printf '%s\n' "${TOOL_DIR}/zizmor"

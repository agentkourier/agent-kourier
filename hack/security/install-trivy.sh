#!/usr/bin/env bash
# Install the exact Trivy release used by CI into a caller-owned directory and
# print the resulting executable path.
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# When this version changes, refresh every asset checksum below from the same release.
TRIVY_VERSION=v0.75.0
TOOL_DIR=${1:-${RUNNER_TEMP:-${TMPDIR:-/tmp}/agent-kourier-security-tools}}
OS=$(uname -s)
ARCH=$(uname -m)

case "${OS}/${ARCH}" in
  Linux/x86_64) ASSET="trivy_0.75.0_Linux-64bit.tar.gz"; CHECKSUM=c6e65abddb348e25f10549df887045629cf28cc72453cd1c63acb717316b3f3f ;;
  Linux/aarch64 | Linux/arm64) ASSET="trivy_0.75.0_Linux-ARM64.tar.gz"; CHECKSUM=a1ee9f6ffb7d112b64ff726a2a0717c21175c1114361391f4a132956751a13b3 ;;
  Darwin/arm64) ASSET="trivy_0.75.0_macOS-ARM64.tar.gz"; CHECKSUM=4a77108cccf8e55c8d6823e1e759939a622277e66cd0daa3c1fc621ed69e4568 ;;
  Darwin/x86_64) ASSET="trivy_0.75.0_macOS-64bit.tar.gz"; CHECKSUM=291edaa9778acbe4693d067b5ad60ee11570e5ac68296e85595417528ca641e4 ;;
  *) die "unsupported Trivy host: ${OS}/${ARCH}" ;;
esac

command -v curl >/dev/null || die "curl is required"
command -v tar >/dev/null || die "tar is required"
mkdir -p "${TOOL_DIR}"
archive="${TOOL_DIR}/${ASSET}"
url="https://github.com/aquasecurity/trivy/releases/download/${TRIVY_VERSION}/${ASSET}"
curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location --output "${archive}" "${url}"

if command -v sha256sum >/dev/null; then
  printf '%s  %s\n' "${CHECKSUM}" "${archive}" | sha256sum --check --strict - >/dev/null
else
  command -v shasum >/dev/null || die "sha256sum or shasum is required"
  printf '%s  %s\n' "${CHECKSUM}" "${archive}" | shasum -a 256 --check --strict - >/dev/null
fi

tar -xzf "${archive}" -C "${TOOL_DIR}" trivy
chmod 0755 "${TOOL_DIR}/trivy"
printf '%s\n' "${TOOL_DIR}/trivy"

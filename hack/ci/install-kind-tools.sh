#!/usr/bin/env bash
# Install the pinned kind, kubectl, Helm 4 and jq binaries the kind tiers (hack/kind) need, for a CI runner that
# has none of them. Usage: install-kind-tools.sh [BIN_DIR]; BIN_DIR defaults to $RUNNER_TEMP/bin.
#
# Each tool is downloaded from its project's release URL and its sha256 checked against the pin below before
# anything is installed: a mismatch exits non-zero naming the tool and both digests, and leaves BIN_DIR as it was.
# When all four match they are moved into BIN_DIR, executable, and BIN_DIR is printed on stdout; when GITHUB_PATH
# is set it is also appended there, so the job's later steps find the tools. It refuses to run as root and never
# uses sudo: BIN_DIR must be a directory this user can write. Hosts: linux and darwin, amd64 and arm64.
#
# hack/ci/test.sh sources this file (main runs only when it is executed) and replaces download and pinned_sha256.
set -euo pipefail

# The pins. To bump a tool, change its version here and replace its four sha256 values in pinned_sha256 with the
# ones from that release's published checksum files (named there); a version-only edit fails every check. Keep
# KIND_VERSION at a kind release that publishes NODE_IMAGE in hack/kind/lib.sh, KUBECTL_VERSION within one minor
# of that image's Kubernetes (v1.37), and HELM_VERSION on Helm 4 (require_helm4 in hack/kind/lib.sh).
KIND_VERSION=v0.33.0
KUBECTL_VERSION=v1.37.1
HELM_VERSION=v4.3.1
JQ_VERSION=1.8.2

# The staging directory of a run, inside BIN_DIR so that installing is a rename; removed on exit.
STAGE=

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# pinned_sha256 TOOL PLATFORM: the pinned sha256 of TOOL's download for PLATFORM (linux-amd64 and the like).
pinned_sha256() {
  case "$1/$2" in
    # kind-<platform>.sha256sum, assets of the kind release.
    kind/linux-amd64) echo aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d ;;
    kind/linux-arm64) echo 20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2 ;;
    kind/darwin-amd64) echo 5a99f26f57246dc9319dd294803313197a0f34d33c525b3ea8b655db5916ece0 ;;
    kind/darwin-arm64) echo 0c8c7dbe5e23594a198b786c4bc13dacc101fa6196b0cb0b23a1ca44e61f4b4f ;;
    # kubectl.sha256, next to each binary on dl.k8s.io.
    kubectl/linux-amd64) echo 65691ff77eb6fa44c908b77a1082c9f092c3b9733b5cefabec0d1104890e21a8 ;;
    kubectl/linux-arm64) echo ff749f4b78d9c4f1ec87307df9b50119ed819e2094aa9810cb9acffc3286c8c7 ;;
    kubectl/darwin-amd64) echo 6851381c486ff6edd691623e3d65c87cb9a5b02887ff8fbbb38d8a031b748387 ;;
    kubectl/darwin-arm64) echo fd65982c97ddad3106754b69ffa196d0e543aa591930ae52aed1adfb92f8c77f ;;
    # helm-<version>-<platform>.tar.gz.sha256sum, next to each archive on get.helm.sh.
    helm/linux-amd64) echo 0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b ;;
    helm/linux-arm64) echo 8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88 ;;
    helm/darwin-amd64) echo 34bec4eba761ba5c16d1965ee6b8f5b0abe5993974a802274e2a4f0418893d89 ;;
    helm/darwin-arm64) echo 6b4e61c8cd27d99e5a801e63ff25dd5c70b36875c2cd659d3e9ce6b76fc23198 ;;
    # sha256sum.txt, an asset of the jq release.
    jq/linux-amd64) echo b1c22172dd303f3be49e935aa56aa48a8b7a46e0bc838b4997d3bb451495870f ;;
    jq/linux-arm64) echo 8b85c817833814ddca00a144c33705546355afccf0cf39b188f3cdb48b852309 ;;
    jq/darwin-amd64) echo e94b266e3c26690550006abe63152b782280f4e14374accdf04cbde844f00bc0 ;;
    jq/darwin-arm64) echo 2d75340ba57a4b4b4c8708a21c2dc8e958a48aaa8bba13b27f77f6e4c0eca07e ;;
    *) return 1 ;;
  esac
}

# asset_url TOOL PLATFORM: the release URL of TOOL's download for PLATFORM.
asset_url() {
  local os=${2%-*} arch=${2#*-}
  case "$1" in
    kind) echo "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-$2" ;;
    kubectl) echo "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${os}/${arch}/kubectl" ;;
    helm) echo "https://get.helm.sh/helm-${HELM_VERSION}-$2.tar.gz" ;;
    # jq names its darwin builds macos.
    jq) echo "https://github.com/jqlang/jq/releases/download/jq-${JQ_VERSION}/jq-${os/darwin/macos}-${arch}" ;;
    *) return 1 ;;
  esac
}

# detect_platform: this host as OS-ARCH, in the release assets' terms.
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
  local bin=${1:-} platform tool url want got
  if [ -z "${bin}" ]; then
    [ -n "${RUNNER_TEMP:-}" ] || die "usage: $0 BIN_DIR (or set RUNNER_TEMP to install into \$RUNNER_TEMP/bin)"
    bin=${RUNNER_TEMP}/bin
  fi
  [ "$(id -u)" != 0 ] || die "refusing to run as root: install into a directory this user owns"
  command -v curl >/dev/null || die "curl is required"
  command -v tar >/dev/null || die "tar is required"
  command -v sha256sum >/dev/null || command -v shasum >/dev/null || die "sha256sum or shasum is required"
  platform=$(detect_platform)
  mkdir -p "${bin}" 2>/dev/null || die "cannot create ${bin}; choose a directory this user can write (this script never uses sudo)"
  [ -w "${bin}" ] || die "${bin} is not writable by this user; choose one that is (this script never uses sudo)"
  bin=$(cd "${bin}" && pwd)

  STAGE=$(mktemp -d "${bin}/.install-kind-tools.XXXXXX")
  trap 'rm -rf "${STAGE}"' EXIT
  for tool in kind kubectl helm jq; do
    url=$(asset_url "${tool}" "${platform}")
    want=$(pinned_sha256 "${tool}" "${platform}") || die "no sha256 pinned for ${tool} on ${platform}"
    download "${url}" "${STAGE}/${tool}.download" || die "downloading ${tool} from ${url} failed"
    got=$(sha256_of "${STAGE}/${tool}.download")
    [ "${got}" = "${want}" ] || die "${tool} sha256 mismatch for ${platform}: pinned ${want}, downloaded ${got} (${url}); nothing installed"
  done

  tar -xzf "${STAGE}/helm.download" -C "${STAGE}" "${platform}/helm"
  mv "${STAGE}/${platform}/helm" "${STAGE}/helm"
  for tool in kind kubectl jq; do
    mv "${STAGE}/${tool}.download" "${STAGE}/${tool}"
  done
  for tool in kind kubectl helm jq; do
    chmod 0755 "${STAGE}/${tool}"
    mv -f "${STAGE}/${tool}" "${bin}/${tool}"
  done
  rm -rf "${STAGE}"

  printf 'installed kind %s, kubectl %s, helm %s and jq %s (%s) into %s\n' \
    "${KIND_VERSION}" "${KUBECTL_VERSION}" "${HELM_VERSION}" "${JQ_VERSION}" "${platform}" "${bin}" >&2
  if [ -n "${GITHUB_PATH:-}" ]; then
    printf '%s\n' "${bin}" >>"${GITHUB_PATH}"
  fi
  printf '%s\n' "${bin}"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi

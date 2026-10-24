#!/usr/bin/env bash
# Tests for install-kind-tools.sh. Offline: each case sources the installer in a subshell and replaces its download
# function with one that copies local fixtures and records the URLs it was asked for, so no case reaches the network.
# Run as `hack/ci/test.sh`.
# shellcheck disable=SC2031 # run_installer's subshell re-sources the pins; the ones read here are the top-level copy
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=${HERE}/install-kind-tools.sh

# A CI job's own GITHUB_PATH and RUNNER_TEMP must not reach the installer: each case sets what it needs.
unset GITHUB_PATH RUNNER_TEMP

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# The pins and sha256_of, for the checks below; main runs only when the installer is executed.
# shellcheck source=hack/ci/install-kind-tools.sh
source "${SCRIPT}"

# Fixtures: a fake binary for kind, kubectl and jq, and a helm release tarball that holds one under linux-amd64/.
FIX=${TMP}/fixtures
mkdir -p "${FIX}/linux-amd64"
for name in kind kubectl jq helm; do
  printf '#!/bin/sh\necho fake-%s\n' "${name}" >"${FIX}/${name}"
done
mv "${FIX}/helm" "${FIX}/linux-amd64/helm"
tar -czf "${FIX}/helm.tar.gz" -C "${FIX}" linux-amd64/helm
rm -r "${FIX}/linux-amd64"
for name in kind kubectl jq helm.tar.gz; do
  sha256_of "${FIX}/${name}" >"${FIX}/${name}.sha256"
done

# run_installer LOG [ARG...]: the installer for linux/amd64 with the fixtures behind its downloads, their digests
# as its pins (BAD_TOOL, when set, gets a wrong one) and every requested URL appended to LOG. Its environment is
# the caller's, so a RUNNER_TEMP or GITHUB_PATH set for one call reaches it.
run_installer() {
  local log=$1
  shift
  (
    # shellcheck source=hack/ci/install-kind-tools.sh
    source "${SCRIPT}"
    detect_platform() { printf 'linux-amd64\n'; }
    download() { # URL OUT
      printf '%s\n' "$1" >>"${log}"
      case "$1" in
        */kind-linux-amd64) cp "${FIX}/kind" "$2" ;;
        */kubectl) cp "${FIX}/kubectl" "$2" ;;
        */helm-*-linux-amd64.tar.gz) cp "${FIX}/helm.tar.gz" "$2" ;;
        */jq-linux-amd64) cp "${FIX}/jq" "$2" ;;
        *) printf 'unexpected download %s\n' "$1" >&2 && return 1 ;;
      esac
    }
    pinned_sha256() { # TOOL PLATFORM
      if [ "$1" = "${BAD_TOOL:-}" ]; then
        printf '%064d\n' 0
        return
      fi
      case "$1" in
        helm) cat "${FIX}/helm.tar.gz.sha256" ;;
        *) cat "${FIX}/$1.sha256" ;;
      esac
    }
    main "$@"
  )
}

echo "== good checksums install every tool, executable, from the official URLs"
dir=${TMP}/good/bin
log=${TMP}/good.urls
out=$(run_installer "${log}" "${dir}")
[ "${out}" = "${dir}" ] || fail "stdout was [${out}], want the bin dir [${dir}]"
for name in kind kubectl helm jq; do
  [ -x "${dir}/${name}" ] || fail "${name} is not an executable in ${dir}"
  [ "$("${dir}/${name}")" = "fake-${name}" ] || fail "${dir}/${name} is not the downloaded ${name}"
done
[ "$(find "${dir}" -mindepth 1 | wc -l | tr -d ' ')" = 4 ] || fail "bin dir holds more than the four tools: $(ls -A "${dir}")"
want_urls="https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-linux-amd64
https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl
https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz
https://github.com/jqlang/jq/releases/download/jq-${JQ_VERSION}/jq-linux-amd64"
[ "$(cat "${log}")" = "${want_urls}" ] || fail "downloaded from [$(cat "${log}")], want [${want_urls}]"
echo "ok"

echo "== the pins agree with the rest of the repository"
case "${HELM_VERSION}" in v4.*) ;; *) fail "helm pin ${HELM_VERSION} is not Helm 4 (require_helm4 in hack/kind/lib.sh)" ;; esac
for wf in ci.yml release.yml; do
  # The version azure/setup-helm installs for the chart tests and the release.
  got=$(awk '/azure\/setup-helm@/ {f=1} f && /version:/ {print $2; exit}' "${HERE}/../../.github/workflows/${wf}")
  [ "${got}" = "${HELM_VERSION}" ] || fail "${wf} installs helm [${got}], the kind tools pin ${HELM_VERSION}: keep them equal"
done
# kubectl is supported within one minor of the cluster; the cluster is NODE_IMAGE, which Renovate bumps on its own.
node_minor=$(sed -n 's/^NODE_IMAGE=kindest\/node:v1\.\([0-9]*\)\..*/\1/p' "${HERE}/../kind/lib.sh")
kubectl_minor=$(printf '%s' "${KUBECTL_VERSION}" | sed -n 's/^v1\.\([0-9]*\)\..*/\1/p')
[ -n "${node_minor}" ] && [ -n "${kubectl_minor}" ] || fail "cannot read the minor of NODE_IMAGE [${node_minor}] or KUBECTL_VERSION [${kubectl_minor}]"
[ $((kubectl_minor - node_minor)) -le 1 ] && [ $((node_minor - kubectl_minor)) -le 1 ] ||
  fail "kubectl ${KUBECTL_VERSION} is more than one minor from NODE_IMAGE's v1.${node_minor} in hack/kind/lib.sh"
echo "ok"

echo "== a rerun replaces the tools in place"
run_installer "${TMP}/rerun.urls" "${dir}" >/dev/null
[ "$(find "${dir}" -mindepth 1 | wc -l | tr -d ' ')" = 4 ] || fail "rerun left more than the four tools: $(ls -A "${dir}")"
echo "ok"

echo "== GITHUB_PATH gets the bin dir appended; RUNNER_TEMP/bin is the default dir"
gh_path=${TMP}/github_path
printf '/already/there\n' >"${gh_path}"
mkdir -p "${TMP}/runner"
GITHUB_PATH=${gh_path} RUNNER_TEMP=${TMP}/runner run_installer "${TMP}/ghpath.urls" >/dev/null
[ "$(cat "${gh_path}")" = "/already/there
${TMP}/runner/bin" ] || fail "GITHUB_PATH holds [$(cat "${gh_path}")]"
[ -x "${TMP}/runner/bin/kind" ] || fail "RUNNER_TEMP/bin was not the default bin dir"
echo "ok"

echo "== a bad checksum fails, names the tool and both digests, and installs nothing"
for bad in kind kubectl helm jq; do
  dir=${TMP}/bad-${bad}/bin
  gh_path=${TMP}/bad-${bad}.github_path
  : >"${gh_path}"
  if err=$(BAD_TOOL=${bad} GITHUB_PATH=${gh_path} run_installer "${TMP}/bad-${bad}.urls" "${dir}" 2>&1 >/dev/null); then
    fail "a bad ${bad} checksum was accepted"
  fi
  case "${bad}" in helm) got=$(cat "${FIX}/helm.tar.gz.sha256") ;; *) got=$(cat "${FIX}/${bad}.sha256") ;; esac
  case "${err}" in
    *"${bad}"*"$(printf '%064d' 0)"*"${got}"*) ;;
    *) fail "the ${bad} mismatch error does not name the tool, the pinned and the actual digest: ${err}" ;;
  esac
  [ -z "$(ls -A "${dir}" 2>/dev/null)" ] || fail "a bad ${bad} checksum left files in ${dir}: $(ls -A "${dir}")"
  [ ! -s "${gh_path}" ] || fail "a bad ${bad} checksum still appended to GITHUB_PATH"
done
echo "ok"

echo "== no bin dir argument and no RUNNER_TEMP is a usage error"
if run_installer "${TMP}/nodir.urls" 2>/dev/null; then
  fail "ran with no bin dir"
fi
[ ! -e "${TMP}/nodir.urls" ] || fail "downloaded with no bin dir"
echo "ok"

echo "== an unwritable bin dir and root are refused before any download"
ro=${TMP}/readonly
mkdir -p "${ro}"
chmod 0555 "${ro}"
if [ -w "${ro}" ]; then
  echo "skip: ${ro} is writable here (running as root?)"
elif run_installer "${TMP}/ro.urls" "${ro}" 2>/dev/null; then
  fail "installed into an unwritable dir"
fi
chmod 0755 "${ro}"
[ ! -e "${TMP}/ro.urls" ] || fail "downloaded for an unwritable dir"
if (id() { printf '0\n'; } && run_installer "${TMP}/root.urls" "${TMP}/root/bin") 2>/dev/null; then
  fail "ran as root"
fi
[ ! -e "${TMP}/root.urls" ] || fail "downloaded as root"
echo "ok"

echo "== every supported platform has a pinned sha256 for every tool"
for host in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  for name in kind kubectl helm jq; do
    sum=$(pinned_sha256 "${name}" "${host}")
    [[ ${sum} =~ ^[0-9a-f]{64}$ ]] || fail "no sha256 pinned for ${name} on ${host}: [${sum}]"
  done
done
echo "ok"

printf 'install-kind-tools tests passed\n'

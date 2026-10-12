#!/usr/bin/env bash
# The single-quoted lines below are source text for temporary fake scanner
# scripts; expanding their variables in this test process would be incorrect.
# shellcheck disable=SC2016
# Policy tests for the standalone security wrappers. The fake scanners record
# arguments and return controlled statuses, so the tests prove argument policy
# and failure propagation without downloading a database or image.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -r "${tmp}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

fake_trivy="${tmp}/trivy"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -eu' \
  ': "${FAKE_ARGS_FILE:?}"' \
  'printf "%s\\n" "$*" >"${FAKE_ARGS_FILE}"' \
  'output=' \
  'previous=' \
  'for argument in "$@"; do' \
  '  if [ "${previous}" = --output ]; then output=${argument}; fi' \
  '  previous=${argument}' \
  'done' \
  '[ -n "${output}" ]' \
  'printf "%s\\n" "{\\"version\\":\\"2.1.0\\",\\"runs\\":[]}" >"${output}"' \
  'exit "${FAKE_EXIT:-0}"' >"${fake_trivy}"
chmod 0755 "${fake_trivy}"

trivy_args="${tmp}/trivy.args"
trivy_report="${tmp}/trivy.sarif"
FAKE_ARGS_FILE="${trivy_args}" TRIVY_BIN="${fake_trivy}" \
  "${HERE}/scan-image.sh" ghcr.io/example/image@sha256:deadbeef linux/amd64 "${trivy_report}"
grep -F -- '--platform linux/amd64' "${trivy_args}" >/dev/null || fail "image platform was not passed to Trivy"
grep -F -- '--scanners vuln' "${trivy_args}" >/dev/null || fail "image scanner policy was not passed to Trivy"
grep -F -- '--severity HIGH,CRITICAL' "${trivy_args}" >/dev/null || fail "image severity policy was not passed to Trivy"
grep -F -- '--ignore-unfixed=false' "${trivy_args}" >/dev/null || fail "unfixed vulnerabilities were not included"
grep -F -- '--exit-code 1' "${trivy_args}" >/dev/null || fail "image exit policy was not passed to Trivy"
test -s "${trivy_report}" || fail "successful image scan did not write SARIF"

if TRIVY_BIN="${fake_trivy}" FAKE_ARGS_FILE="${trivy_args}" \
  "${HERE}/scan-image.sh" example linux/s390x "${trivy_report}"; then
  fail "unsupported image platform was accepted"
fi

if TRIVY_BIN="${fake_trivy}" FAKE_ARGS_FILE="${trivy_args}" FAKE_EXIT=2 \
  "${HERE}/scan-image.sh" example linux/amd64 "${trivy_report}"; then
  fail "Trivy scanner error was swallowed"
fi

if TRIVY_BIN="${fake_trivy}" FAKE_ARGS_FILE="${trivy_args}" FAKE_EXIT=1 \
  "${HERE}/scan-image.sh" example linux/amd64 "${trivy_report}"; then
  fail "high-severity Trivy result was swallowed"
fi

fake_gitleaks="${tmp}/gitleaks"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -eu' \
  ': "${FAKE_ARGS_FILE:?}"' \
  'printf "%s\\n" "$*" >"${FAKE_ARGS_FILE}"' \
  'exit "${FAKE_EXIT:-0}"' >"${fake_gitleaks}"
chmod 0755 "${fake_gitleaks}"

gitleaks_args="${tmp}/gitleaks.args"
GITLEAKS_BIN="${fake_gitleaks}" FAKE_ARGS_FILE="${gitleaks_args}" \
  "${HERE}/scan-secrets.sh" . "${tmp}/gitleaks.sarif"
grep -F -- 'git' "${gitleaks_args}" >/dev/null || fail "Gitleaks git mode was not selected"
grep -F -- '--redact' "${gitleaks_args}" >/dev/null || fail "Gitleaks redaction was not enabled"
grep -F -- '--config ' "${gitleaks_args}" >/dev/null || fail "Gitleaks policy config was not selected"
grep -F -- '--log-opts=--all' "${gitleaks_args}" >/dev/null || fail "Gitleaks history scope was not enabled"
grep -F -- '--report-format sarif' "${gitleaks_args}" >/dev/null || fail "Gitleaks SARIF output was not requested"

if GITLEAKS_BIN="${fake_gitleaks}" FAKE_ARGS_FILE="${gitleaks_args}" FAKE_EXIT=2 \
  "${HERE}/scan-secrets.sh" . "${tmp}/gitleaks.sarif"; then
  fail "Gitleaks scanner error was swallowed"
fi

empty_releases="${tmp}/empty-releases.json"
printf '%s\n' '[]' >"${empty_releases}"
selected="${tmp}/selected-images"
"${HERE}/select-release-images.sh" "${empty_releases}" "${selected}"
test ! -s "${selected}" || fail "empty release list produced image digests"

release_fixture="${tmp}/releases.json"
printf '%s\n' \
  '[' \
  '  {"tag_name":"v1.0.0","draft":false,"prerelease":false,"body":"Image: ghcr.io/agentkourier/agent-kourier@sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},' \
  '  {"tag_name":"v1.1.0","draft":false,"prerelease":false,"body":"Image: ghcr.io/agentkourier/agent-kourier@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},' \
  '  {"tag_name":"v1.2.0-rc.1","draft":false,"prerelease":true,"body":"Image: ghcr.io/agentkourier/agent-kourier@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},' \
  '  {"tag_name":"v1.2.0-rc.2","draft":false,"prerelease":true,"body":"Image: ghcr.io/agentkourier/agent-kourier@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},' \
  '  {"tag_name":"v9.0.0","draft":true,"prerelease":false,"body":"Image: ghcr.io/agentkourier/agent-kourier@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}' \
  ']' >"${release_fixture}"
# The first body intentionally has a malformed digest and is older than the
# selected stable release; only the latest stable and a newer prerelease are emitted.
"${HERE}/select-release-images.sh" "${release_fixture}" "${selected}"
grep -F 'ghcr.io/agentkourier/agent-kourier@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' "${selected}" >/dev/null ||
  fail "latest stable release digest was not selected"
grep -F 'ghcr.io/agentkourier/agent-kourier@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd' "${selected}" >/dev/null ||
  fail "latest prerelease digest was not selected"
grep -F 'aaaaaaaa' "${selected}" >/dev/null && fail "older stable digest was selected"
grep -F 'eeeeeeee' "${selected}" >/dev/null && fail "draft release digest was selected"

missing_digest="${tmp}/missing-digest.json"
printf '%s\n' '[{"tag_name":"v2.0.0","draft":false,"prerelease":false,"body":"release notes without an image"}]' >"${missing_digest}"
if "${HERE}/select-release-images.sh" "${missing_digest}" "${selected}"; then
  fail "published release without image digest was accepted"
fi

invalid_releases="${tmp}/invalid-releases.json"
printf '%s\n' '{' >"${invalid_releases}"
if "${HERE}/select-release-images.sh" "${invalid_releases}" "${selected}"; then
  fail "invalid release metadata was accepted"
fi

# A candidate is no longer supported once its stable release has shipped.
sed 's/v1.2.0-rc/v1.1.0-rc/g' "${release_fixture}" >"${tmp}/superseded.json"
"${HERE}/select-release-images.sh" "${tmp}/superseded.json" "${selected}"
[ "$(wc -l <"${selected}" | tr -d ' ')" = 1 ] || fail "superseded prerelease was selected"

printf '%s\n' '[{"tag_name":"nightly","draft":false,"body":"invalid metadata"}]' >"${invalid_releases}"
if "${HERE}/select-release-images.sh" "${invalid_releases}" "${selected}"; then
  fail "invalid published release was treated as an empty release list"
fi

# A version-only installer edit must never silently use older assets.
for tool in trivy gitleaks; do
  installer=${HERE}/install-${tool}.sh
  version=$(sed -n "s/^$(printf '%s' "${tool}" | tr '[:lower:]' '[:upper:]')_VERSION=v//p" "${installer}")
  [ -n "${version}" ] || fail "installer has no pinned version"
  while IFS= read -r asset; do
    case "${asset}" in
      *"${tool}_${version}_"*) ;;
      *) fail "installer asset does not match its pinned version" ;;
    esac
  done < <(grep 'ASSET=' "${installer}")
done

printf 'security script policy tests passed\n'

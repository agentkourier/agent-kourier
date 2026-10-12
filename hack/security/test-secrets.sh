#!/usr/bin/env bash
# Run with the actual pinned Gitleaks binary, after installing it in CI.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT
repo=${tmp}/repo
git init -q "${repo}"
git -C "${repo}" config user.name scanner-test
git -C "${repo}" config user.email scanner-test@example.invalid
mkdir -p "${repo}/internal/render"
fixture=${repo}/internal/render/prompt_test.go
printf '%s\n' 'package render' >"${fixture}"
git -C "${repo}" add .
git -C "${repo}" commit -qm 'test: clean fixture'
"${HERE}/scan-secrets.sh" "${repo}" "${tmp}/clean.sarif" >"${tmp}/clean.log" 2>&1

# Assemble a synthetic value in the temporary repository, so this regression
# test's own source contains no complete credential-shaped canary.
payload=$(printf '%s%s' 'Zy83hcAks892Aqj34Qpa' 'PszU72yNsrw62iO13bZa')
printf 'aws_secret_access_key = "%s"\n' "${payload}" >>"${fixture}"
git -C "${repo}" add .
git -C "${repo}" commit -qm 'test: synthetic new credential in formerly excluded file'
status=0
"${HERE}/scan-secrets.sh" "${repo}" "${tmp}/finding.sarif" >"${tmp}/finding.log" 2>&1 || status=$?
[ "${status}" -eq 1 ] || { echo 'FAIL: new fixture credential was not detected' >&2; exit 1; }
grep -F 'internal/render/prompt_test.go' "${tmp}/finding.sarif" >/dev/null || {
  echo 'FAIL: report did not identify the fixture' >&2
  exit 1
}
if grep -F "${payload}" "${tmp}/finding.sarif" "${tmp}/finding.log" >/dev/null; then
  echo 'FAIL: scanner output exposed a credential' >&2
  exit 1
fi
echo 'secret scanner regression passed'

#!/usr/bin/env bash
# Run the pinned Go vulnerability scanner from a module directory.
set -euo pipefail

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION=v1.9.0
MODULE_DIR=${1:-.}

[ -d "${MODULE_DIR}" ] || die "module directory does not exist: ${MODULE_DIR}"
command -v go >/dev/null || die "go is required"
cd "${MODULE_DIR}"
go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" ./...

#!/usr/bin/env bash
# Prints, one per line, the packages matching PATTERN (default ./internal/...) whose test binary the testhooks build
# tag can change: the packages whose own files the tag selects or drops, and those whose test build (their code, their
# tests and the tests' imports, transitively) reaches one of those anywhere in the module. Every other package compiles
# to the same test binary with the tag and without it, so the untagged race pass already ran it. `make test-testhooks`
# runs the packages this prints. Run from the module root as `hack/ci/testhooks-packages.sh [PATTERN...]`; it fails,
# printing nothing, when go list fails or when the selection is empty, so the pass never runs nothing and passes.
set -euo pipefail

TAG=testhooks

if [ "$#" -eq 0 ]; then
  set -- ./internal/...
fi

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

# The packages the tag changes: those whose file set differs with and without it, for a constraint either way
# (testhooks or !testhooks). Comparing what go list selects, not the text of the constraints, reads them as the build
# does. A tag selects files and nothing else, so a package with the same files under both builds the same.
files='{{.ImportPath}} {{.GoFiles}} {{.CgoFiles}} {{.TestGoFiles}} {{.XTestGoFiles}}'
go list -f "${files}" ./... >"${TMP}/files"
go list -tags "${TAG}" -f "${files}" ./... >>"${TMP}/files"
sort "${TMP}/files" | uniq -u | cut -d' ' -f1 | sort -u >"${TMP}/changed"

# Each package matching PATTERN, then one line per package and dependency of its test build under the tag. go list
# -test adds the package's test variants ("P [P.test]" and "P_test [P.test]", both with ForTest P), whose Deps carry
# the tests' imports, and the test main ("P.test"), which is no candidate and drops out. One build is enough: on any
# path of imports that only one build has, the first such import is in a file the tag selects or drops, so the package
# holding it is in changed and both builds reach that package the same way.
go list "$@" >"${TMP}/candidates"
# shellcheck disable=SC2016 # a go list template, not shell: $p is the template's variable
deps='{{$p := .ImportPath}}{{with .ForTest}}{{$p = .}}{{end}}{{$p}}	{{.ImportPath}}{{range .Deps}}
{{$p}}	{{.}}{{end}}'
go list -tags "${TAG}" -test -f "${deps}" "$@" >"${TMP}/deps"

awk -F'\t' '
  FILENAME == ARGV[1] { changed[$1] = 1; next }
  FILENAME == ARGV[2] { candidate[$1] = 1; next }
  {
    sub(/ \[.*\]$/, "", $2) # a test variant is the package recompiled; the tag changes it if it changes the package
    if (($1 in candidate) && ($2 in changed)) print $1
  }
' "${TMP}/changed" "${TMP}/candidates" "${TMP}/deps" | sort -u >"${TMP}/selected"

if [ ! -s "${TMP}/selected" ]; then
  printf 'no package matching %s depends on a package the %s tag changes; refusing an empty %s pass\n' \
    "$*" "${TAG}" "${TAG}" >&2
  exit 1
fi
cat "${TMP}/selected"

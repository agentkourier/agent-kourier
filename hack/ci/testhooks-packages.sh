#!/usr/bin/env bash
# Prints, one per line, the packages matching PATTERN (default ./internal/...) whose test build the testhooks build
# tag can change, so the packages `make test-testhooks` runs. A package is selected when the tag selects or drops one of
# its own files, test files included, or when its test build reaches, in any module, a package whose importers see a
# different file set with the tag. Any other package compiles to the same packages with the tag and without it (only
# the linked binary's build info differs), so the untagged race pass already ran it.
#
# TESTHOOKS_BUILD_FLAGS (default -race) holds the build flags of the pass's go test, besides -tags: every go list here
# runs with them, since they can select files too (-race sets the race tag). Run from the module root as
# `hack/ci/testhooks-packages.sh [PATTERN...]`. It fails, printing nothing, when go list fails or the selection is
# empty, so the pass never runs nothing and passes.
set -euo pipefail

TAG=testhooks
read -r -a FLAGS <<<"${TESTHOOKS_BUILD_FLAGS--race}"

if [ "$#" -eq 0 ]; then
  set -- ./internal/...
fi

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

# golist TAGS ARG...: go list with the pass's build flags and the build tags TAGS ("" for none).
golist() {
  local tags=$1
  shift
  go list ${FLAGS[@]+"${FLAGS[@]}"} -tags "${tags}" "$@"
}

# The candidates, listed with the tag: a package whose files all need it matches PATTERN only then.
golist "${TAG}" "$@" >"${TMP}/candidates"

# Every package that either build of the candidates' tests compiles, in any module. Test variants, external test
# packages (both have ForTest) and test mains (package main) are left out: they are built from their package's files.
# Main packages cannot be imported, so the only ones a test build holds are candidates, added back from the list.
closure='{{if not .ForTest}}{{if ne .Name "main"}}{{.ImportPath}}{{end}}{{end}}'
{
  golist "" -deps -test -f "${closure}" "$@"
  golist "${TAG}" -deps -test -f "${closure}" "$@"
  cat "${TMP}/candidates"
} | sed '/^$/d' | sort -u >"${TMP}/closure"

# Each of those packages' files under each build: first the ones its importers compile, then its test files. -e lists
# a package whose files the build excludes, with none; the go list calls above, without -e, fail on a real error.
files='{{.ImportPath}}	{{.GoFiles}}{{.CgoFiles}}{{.CFiles}}{{.CXXFiles}}{{.HFiles}}{{.MFiles}}{{.FFiles}}{{.SFiles}}{{.SwigFiles}}{{.SwigCXXFiles}}{{.SysoFiles}}	{{.TestGoFiles}}{{.XTestGoFiles}}'
for tags in "" "${TAG}"; do
  xargs go list ${FLAGS[@]+"${FLAGS[@]}"} -e -tags "${tags}" -f "${files}" <"${TMP}/closure" >"${TMP}/files.${tags:-none}"
done

# One line per candidate and package its test build under the tag compiles: go list -test adds the test variants
# ("P [P.test]" and "P_test [P.test]", ForTest P), whose Deps carry the tests' imports. One build is enough: on any
# path of imports only one build has, the first such import is in a file the tag selects or drops, either a test file
# of the candidate, which selects it, or a file the importers of some package see, which both builds reach the same way.
# shellcheck disable=SC2016 # a go list template, not shell: $p is the template's variable
deps='{{$p := .ImportPath}}{{with .ForTest}}{{$p = .}}{{end}}{{$p}}	{{.ImportPath}}{{range .Deps}}
{{$p}}	{{.}}{{end}}'
golist "${TAG}" -test -f "${deps}" "$@" >"${TMP}/deps"

awk -F'\t' '
  FILENAME == ARGV[1] { candidate[$1] = 1; next }
  FILENAME == ARGV[2] { visible[$1] = $2; tests[$1] = $3; next }
  FILENAME == ARGV[3] {
    if (visible[$1] != $2) changed[$1] = 1 # its importers compile other files under the tag
    if (($1 in candidate) && tests[$1] != $3) selected[$1] = 1 # only its own test build sees its test files
    next
  }
  {
    sub(/ \[.*\]$/, "", $2) # a test variant is its package recompiled: built from the same files
    if (($1 in candidate) && ($2 in changed)) selected[$1] = 1
  }
  END { for (p in selected) print p }
' "${TMP}/candidates" "${TMP}/files.none" "${TMP}/files.${TAG}" "${TMP}/deps" | sort >"${TMP}/selected"

if [ ! -s "${TMP}/selected" ]; then
  printf 'no package matching %s depends on a package the %s tag changes; refusing an empty %s pass\n' \
    "$*" "${TAG}" "${TAG}" >&2
  exit 1
fi
cat "${TMP}/selected"

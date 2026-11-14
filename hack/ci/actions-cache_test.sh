#!/usr/bin/env bash
# Tests for actions-cache.sh. Offline: mark and trim run on caches under a temporary directory, one of them filled by
# the real go command, and prune runs against a fake gh that serves a listing and records deletions. Needs go and jq.
# Run as `hack/ci/actions-cache_test.sh`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=${HERE}/actions-cache.sh

# The fixture module is not this repository's, so neither a go.work above the temporary directory nor this job's
# GOFLAGS may reach it; the job's own caches stay out of reach too.
export GOWORK=off GOFLAGS='' GOTOOLCHAIN=local
unset CACHE_KEY CACHE_RESTORED_KEY

TMP=$(mktemp -d)
trap 'chmod -R u+w "${TMP}"; rm -rf "${TMP}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# fresh NAME: a new case directory under TMP, which holds the case's RUNNER_TEMP, GOCACHE and GOMODCACHE.
fresh() {
  CASE=${TMP}/$1
  mkdir -p "${CASE}/runner" "${CASE}/gocache" "${CASE}/modcache"
  export RUNNER_TEMP=${CASE}/runner GOCACHE=${CASE}/gocache GOMODCACHE=${CASE}/modcache
}

# entry DIR NAME: a cache entry file DIR/NAME, its directory created.
entry() {
  mkdir -p "$(dirname "$1/$2")"
  printf 'x\n' >"$1/$2"
}

# use PATH: what Go does to an entry it uses (it moves the mtime on only when it is over an hour old).
use() {
  touch "$1"
}

# trim keeps what the job used or wrote and deletes the rest.
fresh used
entry "${GOCACHE}" 00/used-a
entry "${GOCACHE}" 00/used-d
entry "${GOCACHE}" 01/unused-a
entry "${GOCACHE}" 01/unused-d
entry "${GOCACHE}" 02/exe-d/tool # a cached executable: Go marks the directory, not the file in it
entry "${GOCACHE}" 03/staleexe-d/tool
entry "${GOCACHE}" README
"${SCRIPT}" mark >/dev/null
[ "$(cat "${GOCACHE}/trim.txt")" -ge "$(($(date +%s) - 60))" ] || fail "mark did not write the time into trim.txt"
[ -n "$(find "${GOCACHE}/01/unused-a" -newer "${RUNNER_TEMP}/actions-cache/mark")" ] && fail "mark left an entry newer than the mark"
sleep 1
use "${GOCACHE}/00/used-a"
use "${GOCACHE}/00/used-d"
use "${GOCACHE}/02/exe-d"
entry "${GOCACHE}" 04/new-a
"${SCRIPT}" trim >/dev/null
for kept in 00/used-a 00/used-d 02/exe-d/tool 04/new-a README trim.txt; do
  [ -e "${GOCACHE}/${kept}" ] || fail "trim deleted ${kept}, which the job used or wrote, or is not an entry"
done
for gone in 01/unused-a 01/unused-d 03/staleexe-d; do
  [ ! -e "${GOCACHE}/${gone}" ] || fail "trim kept ${gone}, which the job did not use"
done

# An extra DIR is marked and trimmed like GOCACHE, and a DIR that does not exist is skipped by both.
fresh extra-dir
LINT=${CASE}/golangci-lint
entry "${GOCACHE}" 00/used-a
entry "${LINT}" 00/lintused-a
entry "${LINT}" 01/lintunused-a
"${SCRIPT}" mark "${LINT}" "${CASE}/absent" >/dev/null
[ -f "${LINT}/trim.txt" ] || fail "mark did not write the extra DIR's trim.txt"
sleep 1
use "${GOCACHE}/00/used-a"
use "${LINT}/00/lintused-a"
"${SCRIPT}" trim "${LINT}" "${CASE}/absent" >/dev/null
[ -e "${LINT}/00/lintused-a" ] || fail "trim deleted the extra DIR's used entry"
[ ! -e "${LINT}/01/lintunused-a" ] || fail "trim kept the extra DIR's unused entry"

# trim without mark fails and deletes nothing.
fresh no-mark
entry "${GOCACHE}" 00/old-a
touch -t 200001010000 "${GOCACHE}/00/old-a"
"${SCRIPT}" trim >/dev/null 2>&1 && fail "trim passed without a mark"
[ -e "${GOCACHE}/00/old-a" ] || fail "trim without a mark deleted an entry"

# The real go command: what a cached build reads survives trim, so building again compiles nothing.
fresh real-go
MOD=${CASE}/module
mkdir -p "${MOD}"
printf 'module example.com/fixture\n\ngo 1.21\n' >"${MOD}/go.mod"
printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("fixture") }\n' >"${MOD}/main.go"
(cd "${MOD}" && go build -o /dev/null .)
printf 'x\n' >"${CASE}/stray" && mkdir -p "${GOCACHE}/ff" && mv "${CASE}/stray" "${GOCACHE}/ff/stray-a"
"${SCRIPT}" mark >/dev/null
sleep 1
(cd "${MOD}" && go build -o /dev/null .)
"${SCRIPT}" trim >/dev/null
[ ! -e "${GOCACHE}/ff/stray-a" ] || fail "trim kept an entry go did not use"
compiles=$(cd "${MOD}" && go build -x -o /dev/null . 2>&1 | grep -c '/compile ' || true)
[ "${compiles}" -eq 0 ] || fail "after trim, a rebuild compiled ${compiles} packages; want 0"

# The real go command: when a run writes a new index entry (-a) whose output has the same bytes as a restored output
# (-d), go leaves that output file as it was, mtime and all, and trim must keep it. Here a change to a string the test
# binary holds changes the test's ID but not the list of inputs the test logs (it reads a testdata file), so the next
# run finds the result cached only if that output survived.
fresh real-go-same-output
MOD=${CASE}/module
mkdir -p "${MOD}/p/testdata"
printf 'module example.com/fixture\n\ngo 1.21\n' >"${MOD}/go.mod"
printf 'package p\n\nvar Label = "a"\n\nfunc One() int {\n\tif Label == "" {\n\t\treturn 0\n\t}\n\treturn 1\n}\n' \
  >"${MOD}/p/p.go"
printf 'one\n' >"${MOD}/p/testdata/one.txt"
printf '%s\n' 'package p' '' 'import (' '	"os"' '	"testing"' ')' '' 'func TestOne(t *testing.T) {' \
  '	if _, err := os.ReadFile("testdata/one.txt"); err != nil {' '		t.Fatal(err)' '	}' \
  '	if One() != 1 {' '		t.Fatal(One())' '	}' '}' >"${MOD}/p/p_test.go"
(cd "${MOD}" && go test ./p >/dev/null)
"${SCRIPT}" mark >/dev/null
sleep 1
printf 'package p\n\nvar Label = "b"\n\nfunc One() int {\n\tif Label == "" {\n\t\treturn 0\n\t}\n\treturn 1\n}\n' \
  >"${MOD}/p/p.go"
changed=$(cd "${MOD}" && go test ./p)
grep -F '(cached)' <<<"${changed}" >/dev/null && fail "the fixture's change left the test cached: ${changed}"
"${SCRIPT}" trim >/dev/null
again=$(cd "${MOD}" && go test ./p)
grep -F '(cached)' <<<"${again}" >/dev/null || fail "after trim, the test ran again instead of reporting (cached): ${again}"

# mark empties the module cache when the restored entry was saved for another go.sum or Go version, or when nothing
# was restored, and keeps it when the entry is of the same generation.
for restored in "go-test-Linux-go1.27.4-sumB-c1" "go-test-Linux-go1.27.3-sumA-c1" "go-test-Linux-go1.27.4-sumA" ""; do
  fresh "modules-${restored:-none}"
  entry "${GOMODCACHE}" cache/download/example.com/old/@v/v1.0.0.zip
  CACHE_KEY=go-test-Linux-go1.27.4-sumA-c2 CACHE_RESTORED_KEY=${restored} "${SCRIPT}" mark >/dev/null
  [ ! -e "${GOMODCACHE}/cache/download/example.com/old/@v/v1.0.0.zip" ] ||
    fail "mark kept the module cache after restoring ${restored:-nothing}"
done
for restored in "go-test-Linux-go1.27.4-sumA-c1" "go-test-Linux-go1.27.4-sumA-c2"; do
  fresh "modules-same-${restored}"
  entry "${GOMODCACHE}" cache/download/example.com/old/@v/v1.0.0.zip
  CACHE_KEY=go-test-Linux-go1.27.4-sumA-c2 CACHE_RESTORED_KEY=${restored} "${SCRIPT}" mark >/dev/null
  [ -e "${GOMODCACHE}/cache/download/example.com/old/@v/v1.0.0.zip" ] ||
    fail "mark emptied the module cache after restoring ${restored} for the same go.sum"
done
fresh modules-no-key
entry "${GOMODCACHE}" cache/download/example.com/old/@v/v1.0.0.zip
"${SCRIPT}" mark >/dev/null
[ -e "${GOMODCACHE}/cache/download/example.com/old/@v/v1.0.0.zip" ] || fail "mark without CACHE_KEY emptied the module cache"

# prune: a fake gh serves LISTING for `gh cache list` and records each `gh cache delete`.
FAKEBIN=${TMP}/bin
mkdir -p "${FAKEBIN}"
cat >"${FAKEBIN}/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${GH_LOG}"
case "$1 $2" in
  "cache list")
    for arg; do [ "${arg}" != "${GH_FAIL_LIST:-}" ] || exit 1; done
    cat "${LISTING}"
    ;;
  "cache delete") [ "$3" != "${GH_FAIL_ID:-}" ] || exit 1 ;;
  *) exit 2 ;;
esac
EOF
chmod +x "${FAKEBIN}/gh"
export PATH=${FAKEBIN}:${PATH} GITHUB_REPOSITORY=example/repo GH_LOG=${TMP}/gh.log LISTING=${TMP}/listing.json

# deleted: the ids the fake gh was asked to delete, one per line.
deleted() {
  sed -n 's/^cache delete \([0-9]*\) --repo example\/repo$/\1/p' "${GH_LOG}" | sort -n | tr '\n' ' '
}

# Every entry of the prefix but the newest created goes (it has the lowest id, so an order by id would delete it);
# another prefix's entry, which gh's prefix match also lists, stays.
cat >"${LISTING}" <<'EOF'
[
  {"id": 11, "key": "go-test-Linux-go1.27.4-sumA-c2", "createdAt": "2026-11-14T10:00:00Z"},
  {"id": 9, "key": "go-test-Linux-go1.27.4-sumA-c3", "createdAt": "2026-11-14T12:00:00Z"},
  {"id": 10, "key": "go-test-Linux-go1.27.4-sumOld", "createdAt": "2026-11-11T01:00:00Z"},
  {"id": 13, "key": "go-test-testhooks-Linux-go1.27.4-sumA-c3", "createdAt": "2026-11-14T13:00:00Z"}
]
EOF
: >"${GH_LOG}"
"${HERE}/actions-cache.sh" prune go-test-Linux- >/dev/null
[ "$(deleted)" = "10 11 " ] || fail "prune deleted ids $(deleted); want 10 11 (the newest created, id 9, stays)"
grep -F -- 'cache list --repo example/repo --ref refs/heads/main --key go-test-Linux-' "${GH_LOG}" >/dev/null ||
  fail "prune did not list main's entries by the prefix: $(cat "${GH_LOG}")"

# A single entry, or none, is left alone.
printf '[{"id": 12, "key": "go-test-Linux-go1.27.4-sumA-c3", "createdAt": "2026-11-14T12:00:00Z"}]\n' >"${LISTING}"
: >"${GH_LOG}"
"${SCRIPT}" prune go-test-Linux- >/dev/null
[ -z "$(deleted)" ] || fail "prune deleted the only entry"
printf '[]\n' >"${LISTING}"
"${SCRIPT}" prune go-test-Linux- >/dev/null || fail "prune failed on an empty listing"

# A prefix that does not end in "-", or none, is refused before gh runs for any prefix.
for prefix in go-test-Linux "" -; do
  : >"${GH_LOG}"
  "${SCRIPT}" prune go-lint-Linux- "${prefix}" >/dev/null 2>&1 && fail "prune accepted the prefix '${prefix}'"
  [ ! -s "${GH_LOG}" ] || fail "prune ran gh before refusing the prefix '${prefix}'"
done
"${SCRIPT}" prune >/dev/null 2>&1 && fail "prune passed with no prefix"

# A failed delete is a warning, and prune goes on to the next entry and prefix.
cat >"${LISTING}" <<'EOF'
[
  {"id": 10, "key": "go-test-Linux-go1.27.4-sumA-c1", "createdAt": "2026-11-14T09:00:00Z"},
  {"id": 11, "key": "go-test-Linux-go1.27.4-sumA-c2", "createdAt": "2026-11-14T10:00:00Z"},
  {"id": 12, "key": "go-test-Linux-go1.27.4-sumA-c3", "createdAt": "2026-11-14T12:00:00Z"}
]
EOF
: >"${GH_LOG}"
out=$(GH_FAIL_ID=11 "${SCRIPT}" prune go-test-Linux- go-test-Linux-go1.27.4-) || fail "prune failed on a failed delete"
grep -F '::warning::could not delete the superseded cache entry go-test-Linux-go1.27.4-sumA-c2' <<<"${out}" >/dev/null ||
  fail "prune did not warn of the failed delete: ${out}"
[ "$(deleted)" = "10 10 11 11 " ] || fail "prune tried to delete ids $(deleted); want 10 and 11 for each prefix"

# A failed listing is a warning too, and prune goes on to the next prefix.
: >"${GH_LOG}"
out=$(GH_FAIL_LIST=go-lint-Linux- "${SCRIPT}" prune go-lint-Linux- go-test-Linux-) || fail "prune failed on a failed listing"
grep -F '::warning::could not list the cache entries of go-lint-Linux-' <<<"${out}" >/dev/null ||
  fail "prune did not warn of the failed listing: ${out}"
[ "$(deleted)" = "10 11 " ] || fail "after a failed listing, prune deleted ids $(deleted); want 10 11 of the next prefix"

printf 'actions-cache.sh: all tests passed\n'

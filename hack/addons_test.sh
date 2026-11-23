#!/usr/bin/env bash
# Tests for addons.sh. Offline: each case writes a list of its own and points ADDONS_FILE at it; the last reads the
# repository's. Run as `hack/addons_test.sh`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=${HERE}/addons.sh

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# accepts NAME WANT LINE...: the list of the LINEs prints WANT (the names, one per line) and exits 0.
accepts() {
  local name=$1 want=$2 out
  shift 2
  printf '%s\n' "$@" >"${TMP}/${name}.txt"
  out=$(ADDONS_FILE="${TMP}/${name}.txt" "${SCRIPT}" 2>"${TMP}/stderr") || fail "${name}: refused ($(cat "${TMP}/stderr"))"
  [ "${out}" = "${want}" ] || fail "${name}: printed [${out}], want [${want}]"
  echo "ok: ${name}"
}

# refuses NAME WANT LINE...: the list of the LINEs fails with WANT on stderr and prints nothing.
refuses() {
  local name=$1 want=$2 out
  shift 2
  printf '%s\n' "$@" >"${TMP}/${name}.txt"
  if out=$(ADDONS_FILE="${TMP}/${name}.txt" "${SCRIPT}" 2>"${TMP}/stderr"); then
    fail "${name}: accepted, printed [${out}]"
  fi
  [ -z "${out}" ] || fail "${name}: printed [${out}] before it failed"
  grep -q -F -- "${want}" "${TMP}/stderr" || fail "${name}: stderr [$(cat "${TMP}/stderr")], want [${want}]"
  echo "ok: ${name}"
}

accepts one-per-line $'cma\nax' cma ax
accepts comments-and-blanks $'cma\nax' '# the add-ons' '' cma '  ' 'ax # the AX add-on' '#ax2'
accepts surrounding-space $'cma\nax' '  cma' $'ax\t'
accepts hyphen-and-digits $'my-addon2\nx1' my-addon2 x1
accepts no-final-newline cma cma
printf 'cma\nax' >"${TMP}/unterminated.txt"
[ "$(ADDONS_FILE="${TMP}/unterminated.txt" "${SCRIPT}")" = $'cma\nax' ] || fail "unterminated: the last line was dropped"
echo "ok: unterminated"

refuses empty 'lists no add-on' '# nothing yet' ''
refuses upper-case "line 1: 'Cma' is not an add-on name" Cma
refuses two-on-a-line "line 2: 'cma ax' is not an add-on name" '# x' 'cma ax'
refuses path "line 1: 'addons/cma' is not an add-on name" addons/cma
refuses leading-hyphen "line 1: '-x' is not an add-on name" -x
refuses trailing-hyphen "line 1: 'x-' is not an add-on name" x-
refuses double-hyphen "line 1: 'a--b' is not an add-on name" a--b
refuses leading-digit "line 1: '2x' is not an add-on name" 2x
refuses dot "line 1: 'a.b' is not an add-on name" a.b
refuses duplicate "line 3: cma is listed twice" cma ax cma
refuses kit "line 2: kit is the add-ons' shared module, not an add-on" cma kit
if ADDONS_FILE="${TMP}/no-such-file.txt" "${SCRIPT}" >"${TMP}/out" 2>"${TMP}/stderr"; then
  fail "missing: accepted a list that does not exist"
fi
grep -q 'cannot read' "${TMP}/stderr" || fail "missing: stderr [$(cat "${TMP}/stderr")]"
echo "ok: missing"

# The repository's list: each name has its directory under addons/, with a Dockerfile, and each directory there with a
# Dockerfile is listed (the kit has none).
names=$("${SCRIPT}") || fail "the repository's list is refused"
[ -n "${names}" ] || fail "the repository's list is empty"
while IFS= read -r a; do
  [ -f "${HERE}/../addons/${a}/Dockerfile" ] || fail "hack/addons.txt lists ${a}, which has no addons/${a}/Dockerfile"
done <<<"${names}"
for dockerfile in "${HERE}"/../addons/*/Dockerfile; do
  a=$(basename "$(dirname "${dockerfile}")")
  grep -q -x -F -- "${a}" <<<"${names}" || fail "addons/${a} has a Dockerfile but hack/addons.txt does not list it"
done
echo "ok: the repository's list"

echo 'ok: addons.sh'

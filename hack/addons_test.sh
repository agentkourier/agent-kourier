#!/usr/bin/env bash
# Tests for addons.sh and the Makefile's add-on aliases. Offline: each case writes a list of its own and points
# ADDONS_FILE at it; the last reads the repository's. Run as `hack/addons_test.sh`.
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
taken='is taken: a workflow already gives the name to a job, a check, a cache or an image'
refuses taken-job "line 2: lint ${taken}" cma lint
refuses taken-image "line 1: prompt-agent ${taken}" prompt-agent
refuses taken-module "line 1: root ${taken}" root
refuses taken-output "line 1: chart ${taken}" chart
refuses taken-test "line 1: test ${taken}" test
refuses taken-test-family "line 1: test-shard ${taken}" test-shard
refuses taken-test-cache "line 1: test-3 ${taken}" test-3
refuses taken-govulncheck-cache "line 1: govulncheck-cma ${taken}" govulncheck-cma
accepts not-a-family $'tester\ngovulnchecker' tester govulnchecker
refuses crlf "line 1: the line ends in a carriage return (CRLF): save the list with LF line endings" $'cma\r' $'ax\r'
refuses crlf-comment "line 1: the line ends in a carriage return (CRLF)" $'# the add-ons\r' cma
if ADDONS_FILE="${TMP}/no-such-file.txt" "${SCRIPT}" >"${TMP}/out" 2>"${TMP}/stderr"; then
  fail "missing: accepted a list that does not exist"
fi
grep -q 'cannot read' "${TMP}/stderr" || fail "missing: stderr [$(cat "${TMP}/stderr")]"
echo "ok: missing"

# The Makefile: each add-on <name> in a list has the aliases <name>-test, <name>-lint and <name>-image, which run what
# addon-test-<name>, addon-lint-<name> and addon-image-<name> run, and make help prints a line for each. The probe list
# adds probe-x1, which has no directory, so that only the list can give it its targets. make -n prints the commands
# without running them; MAKEFLAGS is emptied so that a make running this test passes none of its flags on.
command -v make >/dev/null || fail "make: not found"
printf '%s\n' cma ax probe-x1 >"${TMP}/probe.txt"
probe_make() { MAKEFLAGS='' ADDONS_FILE="${TMP}/probe.txt" make --no-print-directory -C "${HERE}/.." "$@"; }
help=$(probe_make help) || fail "make help failed with the probe list"
for a in cma ax probe-x1; do
  for kind in test lint image; do
    want=$(probe_make -n "addon-${kind}-${a}" 2>&1) || fail "make -n addon-${kind}-${a}: ${want}"
    got=$(probe_make -n "${a}-${kind}" 2>&1) || fail "make -n ${a}-${kind}: ${got}"
    [ -n "${got}" ] && [ "${got}" = "${want}" ] ||
      fail "make -n ${a}-${kind} printed [${got}], want what addon-${kind}-${a} prints [${want}]"
    line=$(grep -E -- "^  ${a}-${kind} +[^ ]" <<<"${help}") || fail "make help has no line for ${a}-${kind}"
    case ${line} in *'<name>'*) fail "make help left <name> in [${line}]" ;; esac
  done
done
echo "ok: the Makefile's add-on aliases"

# The repository's list: each name has its directory under addons/, with a Dockerfile, and each directory there with a
# Dockerfile is listed (the kit has none). No rule the Makefile writes by hand has a listed add-on's alias among its
# targets (an add-on named release would make release-image one too): make would merge the two rules without a word.
names=$("${SCRIPT}") || fail "the repository's list is refused"
[ -n "${names}" ] || fail "the repository's list is empty"
while IFS= read -r a; do
  [ -f "${HERE}/../addons/${a}/Dockerfile" ] || fail "hack/addons.txt lists ${a}, which has no addons/${a}/Dockerfile"
  if rule=$(grep -E -- "^([^#[:space:]:=][^:=]* )?${a}-(test|lint|image)( [^:=]*)?:([^=]|$)" "${HERE}/../Makefile"); then
    fail "hack/addons.txt lists ${a}, whose alias the Makefile also writes by hand: [${rule}]"
  fi
done <<<"${names}"
for dockerfile in "${HERE}"/../addons/*/Dockerfile; do
  a=$(basename "$(dirname "${dockerfile}")")
  grep -q -x -F -- "${a}" <<<"${names}" || fail "addons/${a} has a Dockerfile but hack/addons.txt does not list it"
done
echo "ok: the repository's list"

echo 'ok: addons.sh'

#!/usr/bin/env bash
# Prints the add-ons that hack/addons.txt lists, one name per line, in its order. Run as `hack/addons.sh`; ADDONS_FILE
# names another list (the tests use it). The Makefile, hack/release/release.sh, package-chart.sh and check-ci.sh read
# the list through it.
#
# A line holds one name, or nothing; # starts a comment, and space around a name is dropped. A name is lower-case
# letters and digits, starting with a letter, in parts joined by single hyphens: it becomes a make target, a directory,
# an image repository's suffix and a chart key, so nothing else is allowed. It fails, printing nothing, when the list
# cannot be read, holds a line that is not such a name, names one twice, names the kit, or names none.
set -euo pipefail

FILE=${ADDONS_FILE:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/addons.txt}
[ -f "${FILE}" ] && [ -r "${FILE}" ] || {
  printf 'addons.sh: cannot read %s\n' "${FILE}" >&2
  exit 1
}

awk -v file="${FILE}" '
  function refuse(msg) { printf "addons.sh: %s line %d: %s\n", file, FNR, msg >"/dev/stderr"; bad = 1 }
  {
    line = $0
    sub(/#.*/, "", line)
    gsub(/^[ \t]+|[ \t]+$/, "", line)
    if (line == "") next
    if (line !~ /^[a-z][a-z0-9]*(-[a-z0-9]+)*$/) { refuse("'\''" line "'\'' is not an add-on name"); next }
    if (line == "kit") { refuse("kit is the add-ons'\'' shared module, not an add-on"); next }
    if (line in seen) { refuse(line " is listed twice"); next }
    seen[line] = 1
    names[n++] = line
  }
  END {
    if (bad) exit 1
    if (n == 0) { printf "addons.sh: %s lists no add-on\n", file >"/dev/stderr"; exit 1 }
    for (i = 0; i < n; i++) print names[i]
  }
' "${FILE}"

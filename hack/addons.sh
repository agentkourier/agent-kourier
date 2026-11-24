#!/usr/bin/env bash
# Prints the add-ons that hack/addons.txt lists, one name per line, in its order. Run as `hack/addons.sh`; ADDONS_FILE
# names another list (the tests use it). The Makefile, hack/release/release.sh, package-chart.sh, check-ci.sh and
# release.yml's digests step read the list through it.
#
# A line holds one name, or nothing; # starts a comment, and space around a name is dropped. A name is lower-case
# letters and digits, starting with a letter, in parts joined by single hyphens: it becomes a make target, a directory,
# an image repository's suffix and a chart key, so nothing else is allowed. A name the workflows already give to a job,
# a check, a cache or an image is taken (TAKEN below; hack/ci/addonlist's test fails when a workflow adds one this list
# lacks), and so is kit, the add-ons' shared module. It fails, printing nothing, when the list cannot be read, holds a
# line that is not such a name or ends in a carriage return (CRLF), names one twice, names one that is taken, or names
# none.
set -euo pipefail

FILE=${ADDONS_FILE:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/addons.txt}
[ -f "${FILE}" ] && [ -r "${FILE}" ] || {
  printf 'addons.sh: cannot read %s\n' "${FILE}" >&2
  exit 1
}

# The names an add-on cannot take, as its leg's check, its govulncheck and trivy checks, its caches or its image would
# then be another job's: the workflows' job IDs and check names, govulncheck's modules, trivy's images and the caches'
# names; and release.yml's outputs, which name the release's other artifacts and addons, the add-ons' images. test
# and test-*, and govulncheck-*, are taken as families: the test jobs' caches are go-test-<leg>- and
# go-test-testhooks-<leg>-, and govulncheck's are go-govulncheck-<module>-, which an add-on's cache, go-<name>-, would
# match.
TAKEN='addon addons analyze build cache-prune chart codeql-go crd-corpus deploy-preview deploy-production docs
docs-agent docs-build echo-agent gate gitleaks govulncheck image label links lint prompt-agent publish
released-image-rescan root secrets sign verify'

awk -v file="${FILE}" -v taken="${TAKEN//$'\n'/ }" '
  function refuse(msg) { printf "addons.sh: %s line %d: %s\n", file, FNR, msg >"/dev/stderr"; bad = 1 }
  BEGIN { split(taken, t, " "); for (i in t) reserved[t[i]] = 1 }
  {
    line = $0
    if (line ~ /\r$/) { refuse("the line ends in a carriage return (CRLF): save the list with LF line endings"); next }
    sub(/#.*/, "", line)
    gsub(/^[ \t]+|[ \t]+$/, "", line)
    if (line == "") next
    if (line !~ /^[a-z][a-z0-9]*(-[a-z0-9]+)*$/) { refuse("'\''" line "'\'' is not an add-on name"); next }
    if (line == "kit") { refuse("kit is the add-ons'\'' shared module, not an add-on"); next }
    if (line in reserved || line == "test" || line ~ /^(test|govulncheck)-/) {
      refuse(line " is taken: a workflow already gives the name to a job, a check, a cache or an image"); next
    }
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

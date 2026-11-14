#!/usr/bin/env bash
# Splits a go test pass's packages over the legs of a CI matrix job, so that each package runs in exactly one leg and
# the legs take about as long as each other. Run from the repository root:
#
#   hack/ci/test-shard.sh pick WEIGHTS I/N PKG...   prints the packages of leg I of N (1 <= I <= N), one per line
#   hack/ci/test-shard.sh refresh WEIGHTS           rewrites WEIGHTS from go test's output on stdin
#
# WEIGHTS holds one line "<seconds> <import path>" per package: how long its tests took in CI. Lines starting with #
# and blank lines are comments. hack/ci/test-weights has one file per pass; a package it does not name weighs
# DEFAULT_WEIGHT seconds.
#
# pick drops repeated packages, sorts the rest by weight, longest first, then by path, and gives each in turn to the leg
# that has the least weight so far (the lowest-numbered on a tie). It prints leg I's packages in the order the leg got
# them, longest first: go test starts packages in the order it is given them, as far as their builds allow, so the
# longest do not start last. The split depends only on the set of packages and WEIGHTS, so legs that list the same
# packages from the same commit get disjoint parts whose union is the list. It fails, printing nothing, on a malformed
# I/N or WEIGHTS, on no packages, and when leg I gets none (fewer packages than legs): go test given no package tests
# the current directory instead, so a leg never runs it with none.
#
# refresh reads go test's output, or a GitHub Actions log of it (gh run view RUN --log: each line starts with the job,
# the step and a timestamp), and rewrites WEIGHTS: each package with a time ("ok") gets that time, rounded to a whole
# second and at least 1; one reported "(cached)" keeps its weight, or stays unlisted; one with no test files weighs 0;
# a package the output does not name is dropped. The comment lines at the top of WEIGHTS stay. It refuses, changing
# nothing, output with a FAIL, with a package twice (two runs' logs, say) or with no result at all. To refresh after
# a push run on main that ran every package of a pass (one that changed go.sum runs them all; otherwise unchanged
# packages report "(cached)" and keep their weights):
#
#   gh run view RUN --log | grep '^test-shard (' | hack/ci/test-shard.sh refresh hack/ci/test-weights/race.txt
#   gh run view RUN --log | grep '^test-testhooks-shard (' | hack/ci/test-shard.sh refresh hack/ci/test-weights/testhooks.txt
set -euo pipefail

DEFAULT_WEIGHT=10

die() {
  printf 'test-shard: %s\n' "$*" >&2
  exit 1
}

# read_weights FILE: FILE's entries as "<seconds>\t<import path>" lines; fails, naming the line, on a malformed one or a
# package listed twice.
read_weights() {
  [ -f "$1" ] && [ -r "$1" ] || die "cannot read the weights file $1"
  awk '
    /^#/ || /^[ \t]*$/ { next }
    NF != 2 || $1 !~ /^[0-9]+(\.[0-9]+)?$/ {
      printf "test-shard: %s:%d: want \"<seconds> <import path>\", got \"%s\"\n", FILENAME, FNR, $0 >"/dev/stderr"
      bad = 1
      next
    }
    $2 in seen {
      printf "test-shard: %s:%d: %s is listed twice\n", FILENAME, FNR, $2 >"/dev/stderr"
      bad = 1
      next
    }
    { seen[$2] = 1; print $1 "\t" $2 }
    END { exit bad }
  ' "$1"
}

pick() {
  [ "$#" -ge 2 ] || die "usage: test-shard.sh pick WEIGHTS I/N PKG..."
  local weights=$1 leg=$2 table pkgs picked
  shift 2
  [[ ${leg} =~ ^([1-9][0-9]*)/([1-9][0-9]*)$ ]] && [ "${BASH_REMATCH[1]}" -le "${BASH_REMATCH[2]}" ] ||
    die "leg '${leg}': want I/N with 1 <= I <= N"
  local i=${BASH_REMATCH[1]} n=${BASH_REMATCH[2]}
  table=$(read_weights "${weights}") || exit 1
  pkgs=$(printf '%s\n' "$@" | sed '/^$/d' | LC_ALL=C sort -u)
  [ -n "${pkgs}" ] || die "no packages to split"
  picked=$(
    awk -F'\t' -v def="${DEFAULT_WEIGHT}" 'FNR == NR { w[$2] = $1; next } { print (($1 in w) ? w[$1] : def) "\t" $1 }' \
      <(printf '%s\n' "${table}") <(printf '%s\n' "${pkgs}") |
      LC_ALL=C sort -t "$(printf '\t')" -k1,1nr -k2,2 |
      awk -F'\t' -v i="${i}" -v n="${n}" '
        {
          best = 1
          for (l = 2; l <= n; l++) if (load[l] < load[best]) best = l
          load[best] += $1
          if (best == i) print $2
        }
      '
  )
  [ -n "${picked}" ] || die "leg ${i} of ${n} has no package: there are fewer packages than legs"
  printf '%s\n' "${picked}"
}

refresh() {
  [ "$#" -eq 1 ] || die "usage: test-shard.sh refresh WEIGHTS <GO_TEST_OUTPUT"
  local weights=$1 table entries
  table=$(read_weights "${weights}") || exit 1
  entries=$(
    awk -F'\t' '
      FNR == NR { old[$2] = $1; next }
      # A GitHub Actions log line: the job, the step and a timestamp, then the line go test printed.
      { sub(/^[^\t]*\t[^\t]*\t[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9:.]+Z /, "") }
      $1 == "FAIL" && NF >= 2 {
        printf "test-shard: %s failed: refresh from a run that passed\n", $2 >"/dev/stderr"
        bad = 1
        next
      }
      $1 == "ok  " && NF >= 3 {
        split($3, result, " ")
        if (result[1] == "(cached)") {
          if (!($2 in old)) next
          w = old[$2]
        } else if (result[1] ~ /^[0-9]+(\.[0-9]+)?s$/) {
          w = int(substr(result[1], 1, length(result[1]) - 1) + 0.5)
          if (w < 1) w = 1
        } else next
        record($2, w)
        next
      }
      $1 == "?   " && NF >= 3 && $3 == "[no test files]" { record($2, 0) }
      function record(pkg, weight) {
        if (pkg in got) {
          printf "test-shard: %s has two results: refresh from one run of a pass\n", pkg >"/dev/stderr"
          bad = 1
        }
        got[pkg] = weight
        found = 1
      }
      END {
        if (!found && !bad) print "test-shard: no go test results on stdin" >"/dev/stderr"
        if (!found || bad) exit 1
        for (pkg in got) print got[pkg] " " pkg
      }
    ' <(printf '%s\n' "${table}") - | LC_ALL=C sort -k2,2
  ) || exit 1
  tmp=$(mktemp "${weights}.XXXXXX")
  trap 'rm -f "${tmp}"' EXIT
  {
    awk '/^#/ || /^[ \t]*$/ { print; next } { exit }' "${weights}"
    printf '%s\n' "${entries}"
  } >"${tmp}"
  mv "${tmp}" "${weights}"
}

[ "$#" -gt 0 ] || die "usage: test-shard.sh pick WEIGHTS I/N PKG... | refresh WEIGHTS <GO_TEST_OUTPUT"
command=$1
shift
case "${command}" in
  pick) pick "$@" ;;
  refresh) refresh "$@" ;;
  *) die "unknown command ${command}: want pick or refresh" ;;
esac

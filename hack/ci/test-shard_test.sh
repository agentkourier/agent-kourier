#!/usr/bin/env bash
# Tests for test-shard.sh. Offline: the cases split made-up package lists over weights files of their own, and check
# the repository's weights files only for their form. Run as `hack/ci/test-shard_test.sh`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=${HERE}/test-shard.sh

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# The weights: a few packages with the times the real ones had, so that the split is the shape CI sees.
W=${TMP}/weights.txt
cat >"${W}" <<'EOF'
# A comment, and a blank line after it.

185 example.com/m/render
130 example.com/m/audit
120	example.com/m/slack
113 example.com/m/broker
87.4 example.com/m/interact
85 example.com/m/sqlite
60 example.com/m/live
12 example.com/m/telemetry
2 example.com/m/chat
0 example.com/m/notests
EOF
KNOWN=(example.com/m/render example.com/m/audit example.com/m/slack example.com/m/broker example.com/m/interact
  example.com/m/sqlite example.com/m/live example.com/m/telemetry example.com/m/chat example.com/m/notests)
# Packages the weights do not name: each weighs the default.
UNKNOWN=(example.com/m/new1 example.com/m/new2 example.com/m/new3)

# legs N PKG...: every leg of N, one package per line, each leg's lines prefixed with its number and a tab.
legs() {
  local n=$1 i
  shift
  for ((i = 1; i <= n; i++)); do
    "${SCRIPT}" pick "${W}" "${i}/${n}" "$@" | sed "s|^|${i}	|"
  done
}

echo "== every package lands in exactly one leg, for any leg count, the weights' own and unknown packages alike"
all=("${KNOWN[@]}" "${UNKNOWN[@]}")
want=$(printf '%s\n' "${all[@]}" | LC_ALL=C sort)
for n in 1 2 3 4 5 13; do
  got=$(legs "${n}" "${all[@]}")
  [ "$(cut -f2 <<<"${got}" | LC_ALL=C sort)" = "${want}" ] ||
    fail "${n} legs: their union [$(cut -f2 <<<"${got}" | LC_ALL=C sort | tr '\n' ' ')] is not the packages"
  [ -z "$(cut -f2 <<<"${got}" | LC_ALL=C sort | uniq -d)" ] ||
    fail "${n} legs: a package is in more than one leg: $(cut -f2 <<<"${got}" | LC_ALL=C sort | uniq -d | tr '\n' ' ')"
  for ((i = 1; i <= n; i++)); do
    grep -q "^${i}	" <<<"${got}" || fail "${n} legs: leg ${i} has no package"
  done
done
echo "ok"

echo "== longest first, each to the lightest leg so far: the split and each leg's order"
got=$(legs 2 "${KNOWN[@]}")
# render 185 -> 1; audit 130 -> 2; slack 120 -> 2 (250); broker 113 -> 1 (298); interact 87.4 -> 2 (337.4);
# sqlite 85 -> 1 (383); live 60 -> 2 (397.4); telemetry 12 -> 1 (395); chat 2 -> 1 (397); notests 0 -> 1 (397).
want_split="1	example.com/m/render
1	example.com/m/broker
1	example.com/m/sqlite
1	example.com/m/telemetry
1	example.com/m/chat
1	example.com/m/notests
2	example.com/m/audit
2	example.com/m/slack
2	example.com/m/interact
2	example.com/m/live"
[ "${got}" = "${want_split}" ] || fail "split [${got}], want [${want_split}]"
echo "ok"

echo "== an unknown package weighs the default, 10 seconds; equal weights go in path order, a tie of legs to the lower"
# telemetry 12 -> 1 (both legs empty); new1 10 -> 2 (10); new2 10 -> 2 (20, leg 2 being lighter); chat 2 -> 1 (14).
got=$(legs 2 example.com/m/telemetry example.com/m/new2 example.com/m/new1 example.com/m/chat)
want_unknown="1	example.com/m/telemetry
1	example.com/m/chat
2	example.com/m/new1
2	example.com/m/new2"
[ "${got}" = "${want_unknown}" ] || fail "split [${got}], want [${want_unknown}]"
echo "ok"

echo "== the split does not depend on the order of the packages, and a repeated package runs once"
a=$(legs 3 "${all[@]}")
reversed=()
while IFS= read -r p; do reversed+=("${p}"); done < <(printf '%s\n' "${all[@]}" | LC_ALL=C sort -r)
b=$(legs 3 "${reversed[@]}" example.com/m/render example.com/m/new1)
[ "${a}" = "${b}" ] || fail "a reordered list with repeats split as [${b}], not [${a}]"
echo "ok"

# refused WANT_MESSAGE ARG...: the script fails with ARG..., prints nothing on stdout and says WANT_MESSAGE.
refused() {
  local want=$1 out
  shift
  if out=$("${SCRIPT}" "$@" 2>"${TMP}/err"); then
    fail "$* exited 0 with [${out}]"
  fi
  [ -z "${out}" ] || fail "$* printed [${out}] on stdout"
  grep -qF -- "${want}" "${TMP}/err" || fail "$*: the error [$(cat "${TMP}/err")] does not say [${want}]"
}

echo "== a leg that would get no package fails: more legs than packages, or no packages"
refused "leg 3 of 3 has no package" pick "${W}" 3/3 example.com/m/render example.com/m/audit
refused "no packages" pick "${W}" 1/1
refused "no packages" pick "${W}" 1/1 ''
echo "ok"

echo "== a malformed leg is refused"
for leg in 0/2 3/2 1/0 2 1/2/3 a/2 '' ' 1/2' 01/2; do
  refused "want I/N" pick "${W}" "${leg}" example.com/m/render
done
echo "ok"

echo "== a missing or malformed weights file is refused"
refused "cannot read" pick "${TMP}/none.txt" 1/1 example.com/m/render
for line in 'x example.com/m/a' '-1 example.com/m/a' '1' '1 a b' '1.2.3 example.com/m/a' '.5 example.com/m/a'; do
  printf '%s\n' "${line}" >"${TMP}/bad.txt"
  refused "bad.txt:1" pick "${TMP}/bad.txt" 1/1 example.com/m/render
done
printf '1 example.com/m/a\n2 example.com/m/a\n' >"${TMP}/twice.txt"
refused "twice.txt:2: example.com/m/a is listed twice" pick "${TMP}/twice.txt" 1/1 example.com/m/render
echo "ok"

echo "== refresh takes each package's time from go test's output, in a CI log or not, and keeps the header"
cp "${W}" "${TMP}/refresh.txt"
# A gh run log's lines are the job, the step and a timestamp before go test's own. render, slack and chat are not in
# the output, so they are dropped; a line a test printed is not go test's, however it reads.
"${SCRIPT}" refresh "${TMP}/refresh.txt" <<'EOF'
test-shard (1)	UNKNOWN STEP	2026-11-14T16:02:50.0672809Z ok  	example.com/m/audit	96.250s
test-shard (1)	UNKNOWN STEP	2026-11-14T16:02:50.0672809Z ok  	example.com/m/broker	(cached)
test-shard (1)	UNKNOWN STEP	2026-11-14T16:02:50.0672809Z ok  	example.com/m/brandnew	(cached)
test-shard (2)	UNKNOWN STEP	2026-11-14T16:02:50.0672809Z ?   	example.com/m/notests	[no test files]
ok  	example.com/m/interact	0.4s
ok  	example.com/m/live	12.5s [no tests to run]
ok  	example.com/m/added	3.0s
--- PASS: TestSomething (0.00s)
    the test logs a line: ok  	example.com/m/sqlite	999s
ok  	example.com/m/sqlite	84.6s
ok  	example.com/m/telemetry	11.49s
EOF
want_refresh="# A comment, and a blank line after it.

3 example.com/m/added
96 example.com/m/audit
113 example.com/m/broker
1 example.com/m/interact
13 example.com/m/live
0 example.com/m/notests
85 example.com/m/sqlite
11 example.com/m/telemetry"
[ "$(cat "${TMP}/refresh.txt")" = "${want_refresh}" ] ||
  fail "refreshed to [$(cat "${TMP}/refresh.txt")], want [${want_refresh}]"
echo "ok"

echo "== refresh refuses a failed run's output, an empty one and a package twice, and leaves the file as it was"
for input in $'ok  \texample.com/m/audit\t9.0s\nFAIL\texample.com/m/broker\t50.1s' '' \
  $'ok  \texample.com/m/audit\t9.0s\nok  \texample.com/m/audit\t8.0s'; do
  cp "${W}" "${TMP}/kept.txt"
  if "${SCRIPT}" refresh "${TMP}/kept.txt" <<<"${input}" 2>/dev/null; then
    fail "refresh accepted [${input}]"
  fi
  cmp -s "${W}" "${TMP}/kept.txt" || fail "a refused refresh changed the file: [$(cat "${TMP}/kept.txt")]"
done
echo "ok"

echo "== the repository's weights files are well formed and name only this module's packages"
for f in "${HERE}"/test-weights/*.txt; do
  "${SCRIPT}" pick "${f}" 1/1 example.com/m/any >/dev/null || fail "${f} is not a weights file pick reads"
  bad=$(grep -v -e '^#' -e '^$' "${f}" | awk '$2 !~ /^github\.com\/agentkourier\/agent-kourier(\/|$)/' || true)
  [ -z "${bad}" ] || fail "${f} names packages of another module: ${bad}"
done
[ -f "${HERE}/test-weights/race.txt" ] && [ -f "${HERE}/test-weights/testhooks.txt" ] ||
  fail "a pass's weights file is missing from ${HERE}/test-weights"
echo "ok"

printf 'test-shard tests passed\n'

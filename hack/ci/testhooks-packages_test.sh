#!/usr/bin/env bash
# Tests for testhooks-packages.sh. Offline: each case builds a small module of its own under a temporary directory,
# with no requirements, and runs the selector there. Run as `hack/ci/testhooks-packages_test.sh`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=${HERE}/testhooks-packages.sh
MOD=example.com/fixture

# The fixture module is not this repository's, so neither a go.work above the temporary directory nor this job's
# GOFLAGS may reach it.
export GOWORK=off GOFLAGS=

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# write MODULE_DIR FILE CONTENT...: FILE (relative to MODULE_DIR), its directories created, holds the lines CONTENT.
write() {
  local dir=$1 file=$2
  shift 2
  mkdir -p "$(dirname "${dir}/${file}")"
  printf '%s\n' "$@" >"${dir}/${file}"
}

# new_module DIR: an empty module at DIR.
new_module() {
  mkdir -p "$1"
  write "$1" go.mod "module ${MOD}" '' 'go 1.21'
}

# select_in DIR [PATTERN...]: the selector's stdout run in DIR, its package paths relative to the module.
select_in() {
  local dir=$1
  shift
  (cd "${dir}" && "${SCRIPT}" "$@") | sed "s|^${MOD}/||"
}

# The fixture:
#   internal/hooked     has a file only the tag selects (as internal/testhooks/on.go)
#   internal/hookedoff  has a file the tag drops (a !testhooks file, as internal/testhooks/off.go) and none it selects
#   internal/direct     imports hooked in its code
#   internal/transitive imports direct, so reaches hooked through it
#   internal/viatest    imports hooked only from a _test.go file
#   internal/viaxtest   imports hookedoff only from an external (package x_test) test file
#   internal/viacmd     imports, from its test, a package outside the pattern that has a tagged test file
#   internal/bridge     imports leaf only from a file the tag selects
#   internal/user       imports bridge from its test, so its test build under the tag reaches leaf
#   internal/leaf       has no tagged file and imports nothing: a tag-only import of it does not select it
#   internal/base       imports, from an external test, hookuser, which imports base: go list shows hookuser in base's
#                       test build only as the variant recompiled against base's test ("hookuser [base.test]")
#   internal/hookuser   has a tagged file and imports base
#   internal/plain      imports nothing of these, though it has tests
#   other/hookedtest    has a tagged _test.go file (as internal/outbox) and is outside the pattern
M=${TMP}/module
new_module "${M}"
write "${M}" internal/hooked/hooked.go 'package hooked' '' 'func Name() string { return "hooked" }'
write "${M}" internal/hooked/on.go '//go:build testhooks' '' 'package hooked' '' 'const On = true'
write "${M}" internal/hookedoff/hookedoff.go 'package hookedoff' '' 'const Name = "hookedoff"'
write "${M}" internal/hookedoff/off.go '//go:build !testhooks' '' 'package hookedoff' '' 'const Off = true'
write "${M}" internal/direct/direct.go 'package direct' '' "import \"${MOD}/internal/hooked\"" '' \
  'func Name() string { return hooked.Name() }'
write "${M}" internal/transitive/transitive.go 'package transitive' '' "import \"${MOD}/internal/direct\"" '' \
  'func Name() string { return direct.Name() }'
write "${M}" internal/viatest/viatest.go 'package viatest' '' 'func Name() string { return "viatest" }'
write "${M}" internal/viatest/viatest_test.go 'package viatest' '' \
  'import (' '	"testing"' '' "	\"${MOD}/internal/hooked\"" ')' '' \
  'func TestName(t *testing.T) { _ = hooked.Name() }'
write "${M}" internal/viaxtest/viaxtest.go 'package viaxtest' '' 'func Name() string { return "viaxtest" }'
write "${M}" internal/viaxtest/viaxtest_test.go 'package viaxtest_test' '' \
  'import (' '	"testing"' '' "	\"${MOD}/internal/hookedoff\"" ')' '' \
  'func TestName(t *testing.T) { _ = hookedoff.Name }'
write "${M}" other/hookedtest/hookedtest.go 'package hookedtest' '' 'func Name() string { return "hookedtest" }'
write "${M}" other/hookedtest/on_test.go '//go:build testhooks' '' 'package hookedtest' '' \
  'import "testing"' '' 'func TestOn(t *testing.T) {}'
write "${M}" internal/viacmd/viacmd.go 'package viacmd' '' 'func Name() string { return "viacmd" }'
write "${M}" internal/viacmd/viacmd_test.go 'package viacmd' '' \
  'import (' '	"testing"' '' "	\"${MOD}/other/hookedtest\"" ')' '' \
  'func TestName(t *testing.T) { _ = hookedtest.Name() }'
write "${M}" internal/leaf/leaf.go 'package leaf' '' 'const Leaf = "leaf"'
write "${M}" internal/leaf/leaf_test.go 'package leaf' '' 'import "testing"' '' 'func TestLeaf(t *testing.T) {}'
write "${M}" internal/bridge/bridge.go 'package bridge' '' 'func Name() string { return "bridge" }'
write "${M}" internal/bridge/bridge_hooks.go '//go:build testhooks' '' 'package bridge' '' \
  "import \"${MOD}/internal/leaf\"" '' 'const Leaf = leaf.Leaf'
write "${M}" internal/user/user.go 'package user'
write "${M}" internal/user/user_test.go 'package user' '' \
  'import (' '	"testing"' '' "	\"${MOD}/internal/bridge\"" ')' '' \
  'func TestName(t *testing.T) { _ = bridge.Name() }'
write "${M}" internal/base/base.go 'package base' '' 'const Name = "base"'
write "${M}" internal/base/base_internal_test.go 'package base' '' 'const internal = true'
write "${M}" internal/base/base_test.go 'package base_test' '' \
  'import (' '	"testing"' '' "	\"${MOD}/internal/hookuser\"" ')' '' \
  'func TestName(t *testing.T) { _ = hookuser.Name }'
write "${M}" internal/hookuser/hookuser.go 'package hookuser' '' "import \"${MOD}/internal/base\"" '' \
  'const Name = base.Name'
write "${M}" internal/hookuser/on.go '//go:build testhooks' '' 'package hookuser'
write "${M}" internal/plain/plain.go 'package plain' '' 'import "strings"' '' \
  'func Name() string { return strings.ToUpper("plain") }'
write "${M}" internal/plain/plain_test.go 'package plain' '' 'import "testing"' '' \
  'func TestName(t *testing.T) { _ = Name() }'

echo "== the packages the tag changes, and those that reach one from code or tests, are selected; leaf and plain are not"
got=$(select_in "${M}" ./internal/...)
want="internal/base
internal/bridge
internal/direct
internal/hooked
internal/hookedoff
internal/hookuser
internal/transitive
internal/user
internal/viacmd
internal/viatest
internal/viaxtest"
[ "${got}" = "${want}" ] || fail "selected [${got}], want [${want}]"
echo "ok"

echo "== the default pattern is ./internal/..."
[ "$(select_in "${M}")" = "${want}" ] || fail "with no pattern selected [$(select_in "${M}")], want [${want}]"
echo "ok"

echo "== a tag-changed package outside the pattern is not selected itself"
got=$(select_in "${M}" ./...)
case "${got}" in *other/hookedtest*) ;; *) fail "./... did not select other/hookedtest: [${got}]" ;; esac
case "$(select_in "${M}" ./internal/...)" in *other/*) fail "./internal/... selected a package outside it" ;; esac
echo "ok"

echo "== a package that gains a test-only import of a tag-changed package joins the selection"
write "${M}" internal/plain/hook_test.go 'package plain' '' \
  'import (' '	"testing"' '' "	\"${MOD}/internal/transitive\"" ')' '' \
  'func TestHook(t *testing.T) { _ = transitive.Name() }'
select_in "${M}" ./internal/... | grep -qx internal/plain || fail "plain did not join after importing transitive"
rm "${M}/internal/plain/hook_test.go"
echo "ok"

echo "== an empty selection fails, prints nothing on stdout and says why"
E=${TMP}/empty
new_module "${E}"
write "${E}" internal/plain/plain.go 'package plain'
write "${E}" internal/plain/plain_test.go 'package plain' '' 'import "testing"' '' 'func TestNothing(t *testing.T) {}'
if out=$(cd "${E}" && "${SCRIPT}" ./internal/... 2>"${TMP}/empty.err"); then
  fail "an empty selection exited 0 with [${out}]"
fi
[ -z "${out}" ] || fail "an empty selection printed [${out}] on stdout"
grep -q testhooks "${TMP}/empty.err" || fail "the empty-selection error does not name the tag: $(cat "${TMP}/empty.err")"
echo "ok"

echo "== tagged packages outside the pattern only still make an empty selection fail"
write "${E}" other/hookedtest/hookedtest.go 'package hookedtest'
write "${E}" other/hookedtest/on_test.go '//go:build testhooks' '' 'package hookedtest'
if out=$(cd "${E}" && "${SCRIPT}" ./internal/... 2>/dev/null); then
  fail "a selection with no ./internal/... package exited 0 with [${out}]"
fi
echo "ok"

echo "== a go list failure fails the selector"
write "${E}" internal/broken/broken.go 'package broken' '' 'import "example.com/does/not/exist"'
write "${E}" internal/broken/on.go '//go:build testhooks' '' 'package broken'
if out=$(cd "${E}" && "${SCRIPT}" ./internal/... 2>/dev/null); then
  fail "a package that does not build was selected without error: [${out}]"
fi
echo "ok"

printf 'testhooks-packages tests passed\n'

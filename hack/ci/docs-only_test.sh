#!/usr/bin/env bash
# Tests for docs-only.sh. Offline: each case commits to a branch of a throwaway repository, merges it into the base
# with a merge commit, as GitHub makes a pull request's, and runs the script on that commit with the branch as
# PR_HEAD_SHA. Run as `hack/ci/docs-only_test.sh`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=${HERE}/docs-only.sh

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

# The fixture repository is not this one: no user or system git config (commit signing, hooks, a default branch) may
# reach it.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
# No automatic maintenance or gc: git may start one in the background after a commit, and a copy of the repository
# (the broken-tree cases below) would race its lock files.
export GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=maintenance.auto GIT_CONFIG_VALUE_0=false GIT_CONFIG_KEY_1=gc.auto \
  GIT_CONFIG_VALUE_1=0
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.com
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.com

# Every class the script knows, in the order of the columns of the page cases below.
CLASSES='tests scan crd-corpus docs-agent echo-agent prompt-agent'

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# The base: a file at each kind of path the cases change, so a case can modify, delete or rename one. The Go files
# make Go package trees: internal/broker, the docs agent's corpus package, and test/fixtures/pkg with a page two
# directories below it.
REPO=${TMP}/repo
git init -q -b main "${REPO}"
for path in AGENTS.md README.md spec.md docs/index.md docs/how-to/install.md docs/assets/clip/team.gif \
  docs/spikes/probe.md internal/broker/run.go internal/broker/README.md go.mod Makefile \
  examples/recipes/docs-agent/README.md examples/recipes/docs-agent/agent/corpus/load.go \
  examples/recipes/docs-agent/agent/corpus/testdata/docs/index.md examples/recipes/echo-agent/agent/agent.py \
  examples/recipes/echo-agent/agent/README.md examples/recipes/prompt-agent/agent/agent.py \
  examples/recipes/prompt-agent/agent/README.md charts/agent-kourier/README.md test/fixtures/pkg/pkg.go \
  test/fixtures/pkg/sub/deeper/page.md; do
  mkdir -p "$(dirname "${REPO}/${path}")"
  printf 'base %s\n' "${path}" >"${REPO}/${path}"
done
git -C "${REPO}" add -A
git -C "${REPO}" commit -q -m base
BASE=$(git -C "${REPO}" rev-parse HEAD)

CASE=0

# merge_case COMMAND: on a new branch from BASE, runs COMMAND in the repository (sh -c), commits what it left, and
# merges the branch into BASE with a merge commit, which it leaves checked out (detached). PR_HEAD_SHA is then the
# branch's tip, the pull request's head.
merge_case() {
  CASE=$((CASE + 1))
  git -C "${REPO}" checkout -q --detach "${BASE}"
  git -C "${REPO}" checkout -q -b "case-${CASE}"
  (cd "${REPO}" && sh -c "$1")
  git -C "${REPO}" add -A
  git -C "${REPO}" commit -q --allow-empty -m "case ${CASE}"
  PR_HEAD_SHA=$(git -C "${REPO}" rev-parse HEAD)
  export PR_HEAD_SHA
  git -C "${REPO}" checkout -q --detach "${BASE}"
  git -C "${REPO}" merge -q --no-ff --no-edit "case-${CASE}"
}

# expect WANT NAME [DIR [ARG...]]: the script, run in DIR (default the repository) with the ARGs (the class, and the
# commit), prints exactly the line WANT and exits 0. Its stderr is left in ${TMP}/stderr.
expect() {
  local want=$1 name=$2 dir=${3:-${REPO}} out status=0
  out=$(cd "${dir}" && "${SCRIPT}" "${@:4}" 2>"${TMP}/stderr") || status=$?
  [ "${status}" -eq 0 ] || fail "${name}: exit ${status}, want 0 ($(cat "${TMP}/stderr"))"
  [ "${out}" = "${want}" ] || fail "${name}: printed '${out}', want '${want}' ($(cat "${TMP}/stderr"))"
}

# expect_each WANT NAME: expect WANT for every class, on the merge checked out.
expect_each() {
  local class
  for class in ${CLASSES}; do
    expect "$1" "$2 (${class})" "${REPO}" "${class}"
  done
}

# expect_classes WANTS NAME: WANTS is one letter per class, in the order of CLASSES: s for skip=true, r for skip=false.
expect_classes() {
  local wants=$1 class want
  for class in ${CLASSES}; do
    case "${wants}" in
      s*) want=skip=true ;;
      r*) want=skip=false ;;
      *) fail "$2: WANTS '$1' has no letter for ${class}" ;;
    esac
    wants=${wants#?}
    expect "${want}" "$2 (${class})" "${REPO}" "${class}"
  done
  [ -z "${wants}" ] || fail "$2: WANTS '$1' has more letters than classes"
}

# Docs only: each kind of path on the list selects skip, for every class.
for change in \
  'echo x >>AGENTS.md' \
  'echo x >>spec.md' \
  'printf "x\n" >RELEASING.md; printf "x\n" >CONTRIBUTING.md; printf "x\n" >CODE_OF_CONDUCT.md; printf "x\n" >SECURITY.md' \
  'echo x >>docs/assets/clip/team.gif' \
  'chmod +x docs/spikes/probe.md' \
  'mkdir -p docs/assets/film && echo WEBVTT >docs/assets/film/captions.vtt' \
  'mkdir -p docs/brand/film && echo "{}" >docs/brand/film/package.json' \
  'echo x >>docs/spikes/probe.md' \
  'git rm -q docs/spikes/probe.md' \
  'mkdir -p docs/brand && git mv docs/spikes/probe.md docs/brand/probe.md' \
  'printf "x\n" >"docs/assets/a name with spaces.png"' \
  'printf "x\n" >"docs/assets/new
line.png"' \
  'echo x >>AGENTS.md; echo x >>docs/assets/clip/team.gif; echo x >>docs/spikes/probe.md'; do
  merge_case "${change}"
  expect_each skip=true "docs only: ${change}"
done

# Off every list, for every class: code, the chart, workflows, build files, hack/, and files that are not Markdown.
for change in \
  'echo "// x" >>internal/broker/run.go' \
  'git rm -q internal/broker/run.go' \
  'mkdir -p charts/agent-kourier && echo x >charts/agent-kourier/values.yaml' \
  'mkdir -p .github/workflows && echo x >.github/workflows/ci.yml' \
  'echo x >>Makefile' \
  'echo x >>go.mod' \
  'echo x >go.sum' \
  'echo x >Dockerfile' \
  'mkdir -p hack/ci && echo x >hack/ci/docs-only.sh' \
  'echo x >mkdocs.yml' \
  'mkdir -p docs/stylesheets && echo x >docs/stylesheets/extra.css' \
  'mkdir -p docs/brand/film && echo "package film" >docs/brand/film/film.go' \
  'echo "module x" >docs/spikes/go.mod' \
  'echo "package docs" >docs/page.md.go' \
  'echo x >LICENSE' \
  'echo x >README' \
  'echo x >docs/upper-case.MD' \
  'git mv internal/broker/run.go docs/spikes/run.go' \
  'git mv Makefile docs/spikes/Makefile' \
  'printf "x\n" >"internal/a name with spaces.go"' \
  'echo x >>AGENTS.md; echo "// x" >>internal/broker/run.go' \
  'echo x >>docs/assets/clip/team.gif; echo x >>go.mod' \
  'echo x >>README.md; echo x >>docs/index.md; echo "// x" >>internal/broker/run.go' \
  'echo x >>README.md; echo x >mkdocs.yml'; do
  merge_case "${change}"
  expect_each skip=false "off the list: ${change}"
done

# Markdown off the docs-only list: each class skips what it does not read. Columns: tests, scan, crd-corpus,
# docs-agent, echo-agent, prompt-agent. The tests read pages anywhere; the docs agent reads docs/ and its recipe's
# directory; the echo and prompt agents' jobs read their agent directories; no class but tests skips a page in a Go
# package's tree, at any depth below it.
while IFS='|' read -r wants change; do
  merge_case "${change}"
  expect_classes "${wants}" "page: ${change}"
done <<'EOF'
rsssss|echo x >>README.md
rssrss|echo x >>docs/index.md
rssrss|echo x >>docs/how-to/install.md
rssrss|mkdir -p docs/reference && echo x >docs/reference/new-page.md
rssrss|git rm -q docs/how-to/install.md
rssrss|echo x >docs/assets/clip/notes.md
rsssss|echo x >>charts/agent-kourier/README.md
rsssss|mkdir -p examples/recipes/echo-agent && echo x >examples/recipes/echo-agent/README.md
rsssss|mkdir -p examples/recipes/prompt-agent && echo x >examples/recipes/prompt-agent/README.md
rsssrs|echo x >>examples/recipes/echo-agent/agent/README.md
rssssr|echo x >>examples/recipes/prompt-agent/agent/README.md
rssrss|echo x >>examples/recipes/docs-agent/README.md
rsssss|mkdir -p .github && echo x >.github/pull_request_template.md
rrrrrr|echo x >>internal/broker/README.md
rrrrrr|mkdir -p internal/broker/testdata && echo x >internal/broker/testdata/page.md
rrrrrr|echo x >>examples/recipes/docs-agent/agent/corpus/testdata/docs/index.md
rrrrrr|echo x >>test/fixtures/pkg/sub/deeper/page.md
rssrss|echo x >>README.md; echo x >>docs/index.md; echo x >>AGENTS.md; echo x >>docs/assets/clip/team.gif
rssrss|git mv docs/index.md docs/start.md
rrrrrr|git mv internal/broker/README.md docs/broker.md
rrrrrr|echo x >>README.md; echo x >>internal/broker/README.md
EOF

# A Go file at the root puts every path in a Go package's tree, whose package could embed README.md: then no class
# skips it. The base of these cases has the Go file.
git -C "${REPO}" checkout -q --detach "${BASE}"
echo 'package root' >"${REPO}/root.go"
git -C "${REPO}" add root.go
git -C "${REPO}" commit -q -m 'a package at the root'
SAVED_BASE=${BASE}
BASE=$(git -C "${REPO}" rev-parse HEAD)
merge_case 'echo x >>README.md'
expect_each skip=false 'a page with a Go package at the root'
merge_case 'echo x >>AGENTS.md'
expect_each skip=true 'the docs-only list with a Go package at the root (control)'
BASE=${SAVED_BASE}

# A listed path or a page that is not a regular file selects run, for every class: a walk of docs/ sees a symlink
# whatever its name, and the docs agent's corpus refuses one. So does a submodule (a gitlink; its directory is left
# empty, as git leaves it).
for change in \
  'ln -s AGENTS.md CLAUDE.md' \
  'ln -s ../../brand/mark.png docs/assets/clip/mark.png' \
  'mkdir -p docs/brand && ln -s ../spikes/probe.md docs/brand/probe.md' \
  'rm docs/spikes/probe.md && ln -s ../index.md docs/spikes/probe.md' \
  'rm README.md && ln -s AGENTS.md README.md' \
  'ln -s ../README.md charts/README.md' \
  "mkdir -p docs/brand/sub && git update-index --add --cacheinfo 160000,${BASE},docs/brand/sub"; do
  merge_case "${change}"
  for class in ${CLASSES}; do
    expect skip=false "not a regular file: ${change} (${class})" "${REPO}" "${class}"
    grep -q 'has mode 1[26]0000' "${TMP}/stderr" ||
      fail "not a regular file: ${change} (${class}): refused for another reason: $(cat "${TMP}/stderr")"
  done
done

# The class must be one the script knows: a missing, empty or unknown one runs, even on a docs-only merge.
merge_case 'echo x >>AGENTS.md; echo x >>README.md'
expect skip=true 'a merge every class but tests skips (control)' "${REPO}" scan
expect skip=false 'no class'
grep -q 'is not a class' "${TMP}/stderr" || fail "no class: refused for another reason: $(cat "${TMP}/stderr")"
expect skip=false 'an empty class' "${REPO}" ''
expect skip=false 'an unknown class' "${REPO}" docs
expect skip=false 'a class in another case' "${REPO}" SCAN
expect skip=false 'a class with a space' "${REPO}" 'scan '

# The merge commit's second parent must be the pull request's head. Here the head is itself a merge of the base, which
# gained a docs-only commit, into a branch with a Go change: against its first parent the head changes only AGENTS.md,
# but it is not a pull request's merge commit, and its second parent is not the head.
git -C "${REPO}" checkout -q --detach "${BASE}"
git -C "${REPO}" checkout -q -b reversed
echo '// x' >>"${REPO}/internal/broker/run.go"
git -C "${REPO}" commit -q -am 'a Go change'
git -C "${REPO}" checkout -q --detach "${BASE}"
echo x >>"${REPO}/AGENTS.md"
git -C "${REPO}" commit -q -am 'a docs-only commit on the base'
DOCS_ON_BASE=$(git -C "${REPO}" rev-parse HEAD)
git -C "${REPO}" checkout -q reversed
git -C "${REPO}" merge -q --no-ff --no-edit "${DOCS_ON_BASE}"
PR_HEAD_SHA=$(git -C "${REPO}" rev-parse HEAD) expect skip=false 'the head checked out, a merge of the base into it' "${REPO}" scan
PR_HEAD_SHA=${DOCS_ON_BASE} expect skip=true 'the same merge read with its second parent as the head (control)' "${REPO}" scan

# PR_HEAD_SHA unset or empty selects run.
merge_case 'echo x >>AGENTS.md'
expect_each skip=true 'a docs-only merge (control for the cases below)'
(
  unset PR_HEAD_SHA
  expect skip=false 'PR_HEAD_SHA unset' "${REPO}" scan
)
PR_HEAD_SHA='' expect skip=false 'PR_HEAD_SHA empty' "${REPO}" scan

# A merge that changes nothing against its base (the branch's change is already there) lists no path: run.
merge_case 'true'
expect_each skip=false 'empty listing'

# The listing cannot be made: run, and exit 0.
merge_case 'echo x >>AGENTS.md'
expect skip=false 'a commit that does not exist' "${REPO}" scan 0000000000000000000000000000000000000000
expect skip=false 'not a merge commit' "${REPO}" scan HEAD^2
# checkout DIR DEPTH: a checkout of the merge commit at DIR fetched to DEPTH, as actions/checkout makes it.
checkout() {
  git init -q "$1"
  git -C "$1" fetch -q --depth "$2" "file://${REPO}" "$(git -C "${REPO}" rev-parse HEAD)"
  git -C "$1" checkout -q --detach FETCH_HEAD
}
checkout "${TMP}/shallow" 1
expect skip=false 'a shallow checkout without the parents' "${TMP}/shallow" scan
expect skip=false 'not a repository' "${TMP}" scan

# The same merge in a checkout as CI makes it, the merge commit and its parents, selects skip; so does a page there,
# for a class that reads the merge commit's tree to find the Go packages.
checkout "${TMP}/ci" 2
expect skip=true 'a checkout of depth 2' "${TMP}/ci" tests
merge_case 'echo x >>README.md; echo x >>docs/index.md'
checkout "${TMP}/ci-page" 2
expect skip=true 'a page in a checkout of depth 2' "${TMP}/ci-page" scan
expect skip=false 'a page in a checkout of depth 2, for the tests' "${TMP}/ci-page" tests

# The merge commit's tree cannot be listed in full (an object of a directory the merge does not change is missing):
# the diff succeeds, the listing of the Go packages fails, and a class that needs it runs. The tests' class, which
# does not, still skips the docs-only list.
merge_case 'echo x >>README.md; echo x >>AGENTS.md'
cp -R "${REPO}" "${TMP}/broken"
tree=$(git -C "${TMP}/broken" rev-parse HEAD:internal)
rm "${TMP}/broken/.git/objects/${tree:0:2}/${tree:2}"
expect skip=false 'a tree that cannot be listed' "${TMP}/broken" scan
grep -q 'git ls-tree' "${TMP}/stderr" || fail "a tree that cannot be listed: refused for another reason: $(cat "${TMP}/stderr")"
merge_case 'echo x >>AGENTS.md'
cp -R "${REPO}" "${TMP}/broken-list"
tree=$(git -C "${TMP}/broken-list" rev-parse HEAD:internal)
rm "${TMP}/broken-list/.git/objects/${tree:0:2}/${tree:2}"
expect skip=true 'a tree that cannot be listed, for the tests (control)' "${TMP}/broken-list" tests

echo 'ok: docs-only.sh'

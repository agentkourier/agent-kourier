#!/usr/bin/env bash
# Tests for docs-only.sh. Offline: each case commits to a branch of a throwaway repository, merges it into the base
# with a merge commit, as GitHub makes a pull request's, and runs the script on that commit. Run as
# `hack/ci/docs-only_test.sh`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=${HERE}/docs-only.sh

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

# The fixture repository is not this one: no user or system git config (commit signing, hooks, a default branch) may
# reach it.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.com
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.com

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# The base: a file at each kind of path the cases change, so a case can modify, delete or rename one.
REPO=${TMP}/repo
git init -q -b main "${REPO}"
for path in AGENTS.md README.md spec.md docs/index.md docs/how-to/install.md docs/assets/clip/team.gif \
  docs/spikes/probe.md internal/broker/run.go go.mod Makefile; do
  mkdir -p "$(dirname "${REPO}/${path}")"
  printf 'base %s\n' "${path}" >"${REPO}/${path}"
done
git -C "${REPO}" add -A
git -C "${REPO}" commit -q -m base
BASE=$(git -C "${REPO}" rev-parse HEAD)

CASE=0

# merge_case COMMAND...: on a new branch from the base, runs COMMAND in the repository (sh -c), commits what it left,
# and merges the branch into the base with a merge commit, which it leaves checked out (detached).
merge_case() {
  CASE=$((CASE + 1))
  git -C "${REPO}" checkout -q --detach "${BASE}"
  git -C "${REPO}" checkout -q -b "case-${CASE}"
  (cd "${REPO}" && sh -c "$1")
  git -C "${REPO}" add -A
  git -C "${REPO}" commit -q --allow-empty -m "case ${CASE}"
  git -C "${REPO}" checkout -q --detach "${BASE}"
  git -C "${REPO}" merge -q --no-ff --no-edit "case-${CASE}"
}

# expect WANT NAME [DIR [COMMIT]]: the script, run in DIR (default the repository) on COMMIT (default HEAD), prints
# exactly the line WANT and exits 0.
expect() {
  local want=$1 name=$2 dir=${3:-${REPO}} out status=0
  out=$(cd "${dir}" && "${SCRIPT}" "${@:4}" 2>"${TMP}/stderr") || status=$?
  [ "${status}" -eq 0 ] || fail "${name}: exit ${status}, want 0 ($(cat "${TMP}/stderr"))"
  [ "${out}" = "${want}" ] || fail "${name}: printed '${out}', want '${want}' ($(cat "${TMP}/stderr"))"
}

# Docs only: each kind of path on the list selects skip.
for change in \
  'echo x >>AGENTS.md' \
  'echo x >>spec.md' \
  'printf "x\n" >RELEASING.md; printf "x\n" >CONTRIBUTING.md; printf "x\n" >CODE_OF_CONDUCT.md; printf "x\n" >SECURITY.md' \
  'ln -s AGENTS.md CLAUDE.md' \
  'echo x >>docs/assets/clip/team.gif' \
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
  expect skip=true "docs only: ${change}"
done

# Any path off the list selects run: code, the chart, workflows, build files, hack/, and docs that tests read.
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
  'echo x >>README.md' \
  'echo x >>docs/index.md' \
  'echo x >>docs/how-to/install.md' \
  'echo x >mkdocs.yml' \
  'mkdir -p examples/recipes/echo-agent && echo x >examples/recipes/echo-agent/README.md' \
  'echo x >docs/assets/clip/notes.md' \
  'mkdir -p docs/brand/film && echo "package film" >docs/brand/film/film.go' \
  'echo "module x" >docs/spikes/go.mod' \
  'echo x >LICENSE' \
  'git mv internal/broker/run.go docs/spikes/run.go' \
  'git mv Makefile docs/spikes/Makefile' \
  'printf "x\n" >"internal/a name with spaces.go"' \
  'echo x >>AGENTS.md; echo "// x" >>internal/broker/run.go' \
  'echo x >>docs/assets/clip/team.gif; echo x >>go.mod' \
  'echo x >>spec.md; echo x >>docs/how-to/install.md'; do
  merge_case "${change}"
  expect skip=false "off the list: ${change}"
done

# A merge that changes nothing against its base (the branch's change is already there) lists no path: run.
merge_case 'true'
expect skip=false 'empty listing'

# The listing cannot be made: run, and exit 0.
merge_case 'echo x >>AGENTS.md'
expect skip=false 'a commit that does not exist' "${REPO}" 0000000000000000000000000000000000000000
expect skip=false 'not a merge commit' "${REPO}" HEAD^2
# checkout DIR DEPTH: a checkout of the merge commit at DIR fetched to DEPTH, as actions/checkout makes it.
checkout() {
  git init -q "$1"
  git -C "$1" fetch -q --depth "$2" "file://${REPO}" "$(git -C "${REPO}" rev-parse HEAD)"
  git -C "$1" checkout -q --detach FETCH_HEAD
}
checkout "${TMP}/shallow" 1
expect skip=false 'a shallow checkout without the parents' "${TMP}/shallow"
expect skip=false 'not a repository' "${TMP}"

# The same merge in a checkout as CI makes it, the merge commit and its parents, selects skip.
checkout "${TMP}/ci" 2
expect skip=true 'a checkout of depth 2' "${TMP}/ci"

echo 'ok: docs-only.sh'

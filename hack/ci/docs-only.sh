#!/usr/bin/env bash
# Prints skip=true when a pull request changes only paths whose content no step that the code jobs of ci.yml,
# security.yml and codeql.yml skip can depend on (see docs_only_path), so those jobs can skip their steps; skip=false
# otherwise.
# Run from the repository as `hack/ci/docs-only.sh [COMMIT]`, COMMIT (default HEAD) being the pull request's merge
# commit, as a pull_request run checks it out: its first parent is the base it merges into, and the paths compared are
# every path whose content, mode or type differs between the two, renames listed as a deletion and an addition.
#
# It fails open: skip=true is printed only when the listing succeeded, listed at least one path, and every path is on
# the list in docs_only_path. A COMMIT that cannot be read or is not a merge of two parents (a shallow checkout without
# the parents, a push), a listing git fails, an empty listing, or any path off the list prints skip=false, with the
# reason on stderr. The listing is git's own, so it has no cap on the number of paths. Exit status is 0 for both
# answers; any other status means the script itself failed, and the workflow then runs everything too.
set -euo pipefail

COMMIT=${1:-HEAD}

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

# docs_only_path PATH: whether PATH (relative to the repository root) is on the list. Each entry was checked against
# every reader in a step that skips: the Go tests of the three modules, which were also run with a log of every file
# they open or stat (test/chart and the docs-agent corpus read every *.md under docs/ but a brand or spikes directory,
# others read single pages and README.md; internal/limitsdoc walks the tree for _test.go files and go.mod), the script
# tests under hack/, the images (.dockerignore drops docs/, and go:embed reaches only a package's own directory),
# govulncheck and CodeQL (Go source). One exception: hack/docs/contentcheck's TestPublicDocsAreClean and
# hack/docs/namecheck's TestSiteSpellsTheName run their check over the tree, which reads the text files under
# docs/assets (SVG, captions) and stats the files README.md links to (docs/brand holds its banner). They call the
# functions `make docs-check` calls on the same tree, and ci.yml's lint job, which runs it, never skips.
docs_only_path() {
  case "$1" in
    # A Go file or module file under a listed directory would join ./... (and limitsdoc's walk).
    *.go | */go.mod | */go.work) return 1 ;;
    # Prose for contributors and maintainers that nothing reads. README.md is not here: tests read it.
    AGENTS.md | CLAUDE.md | spec.md | RELEASING.md | CONTRIBUTING.md | CODE_OF_CONDUCT.md | SECURITY.md) return 0 ;;
    # The brand kit and the spike notes: no test, image or the docs agent's corpus reads them.
    docs/brand/* | docs/spikes/*) return 0 ;;
    # The site's media. Markdown here would be a page the tests read.
    docs/assets/*.md) return 1 ;;
    docs/assets/*) return 0 ;;
  esac
  return 1
}

# run REASON: the answer that runs every job.
run() {
  printf 'docs-only: %s; every job runs\n' "$1" >&2
  echo skip=false
  exit 0
}

parents=$(git rev-list --parents -n 1 "${COMMIT}" 2>/dev/null) || run "cannot read commit ${COMMIT}"
read -r -a commits <<<"${parents}"
[ "${#commits[@]}" -eq 3 ] || run "${COMMIT} is not a merge commit of two parents"
git diff --no-renames --name-only -z "${COMMIT}^1" "${COMMIT}" >"${TMP}/paths" 2>"${TMP}/git.err" ||
  run "git diff of ${COMMIT}^1 and ${COMMIT} failed: $(head -n 1 "${TMP}/git.err")"
[ -s "${TMP}/paths" ] || run "${COMMIT} changes no path"

count=0
# git ends every path with a NUL; the test after || keeps a last one without it from being dropped.
while IFS= read -r -d '' path || [ -n "${path}" ]; do
  docs_only_path "${path}" || run "${path} is not on the docs-only list"
  count=$((count + 1))
done <"${TMP}/paths"

printf 'docs-only: all %d changed paths are on the docs-only list; the code jobs skip their steps\n' "${count}" >&2
echo skip=true

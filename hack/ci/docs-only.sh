#!/usr/bin/env bash
# Prints skip=true when a pull request changes only paths that one class of the gated jobs of ci.yml, security.yml and
# codeql.yml does not read, so a job of that class can skip its steps; skip=false otherwise. A path qualifies when it is
# on the docs-only list, which no gated job reads (docs_only_path), or when it is Markdown that the class does not read
# (class_skips). Run from the repository as `PR_HEAD_SHA=<sha> hack/ci/docs-only.sh CLASS [COMMIT]`, CLASS naming the
# job's class (below), COMMIT (default HEAD) being the pull request's merge commit, as a pull_request run checks it out,
# and PR_HEAD_SHA the pull request's head: the merge commit's second parent must be that head, and its first parent is
# then the base it merges into. The paths compared are every path whose content, mode or type differs between the base
# and the merge commit, renames listed as a deletion and an addition.
#
# The classes, and the jobs that pass each (hack/ci/docsonly's test holds every job to its class):
#   tests       the race and testhooks legs, test-scripts, test-postgres: the docs-only list alone. Their tests read
#               README.md and pages under docs/ and elsewhere at run time (internal/store/storetest reads
#               docs/reference/stored-data.md, cmd/agent-kourier's tests read pages under docs/how-to), which nothing but
#               running them shows, so no Markdown is added for them.
#   scan        govulncheck (all four modules), trivy (each image and platform) and CodeQL's analyze: also Markdown outside every
#               Go package's tree. govulncheck reads a module's Go packages (their files, and the files they embed).
#               CodeQL's Go autobuild runs make (whose default goal is help), checks that go list ./... loads, and
#               extracts the .go files, by GitHub's documentation of its build options and the Go extractor's
#               codeql-extractor.yml, whose only file type is .go. The image is the binary of ./cmd/agent-kourier on the
#               runtime image: .dockerignore drops docs/, the build stage reads only Go inputs, and its verify stage runs
#               two tests of internal/testhooks that read the binaries. An agent's image (examples/recipes/<agent>/agent)
#               copies only the files its .dockerignore lets in by name, none of them Markdown. The CMA add-on's image is
#               the binary of addons/cma's ./cmd/adapter on the same runtime image, built from Go inputs only.
#   crd-corpus  both crd-corpus jobs: also Markdown outside every Go package's tree. The job builds and runs
#               test/crdcorpus, applies charts/agent-kourier/files/crds, and the corpus walk reads only .yaml and .yml
#               files under its roots (test/crdcorpus/corpus.go). It then runs test/chart's
#               TestLeaderLabelPolicyOnAnAPIServer, which renders the chart with helm template: helm loads the chart's
#               README.md, but the templates read no file but files/crds/*.yaml (.Files in templates/crds.yaml), so
#               no Markdown changes what it renders.
#   docs-agent  docs-agent: also Markdown outside docs/, examples/recipes/docs-agent/ and every Go package's tree. Its
#               tests load the real docs/ as the corpus and read the recipe's evals; its lint reads Go only.
#   echo-agent  echo-agent: also Markdown outside examples/recipes/echo-agent/agent/ and every Go package's tree. pytest,
#               ruff, pip-audit and the image build all run in that directory and read nothing above it; the job reads
#               no Go, but every class that adds Markdown keeps the Go rule, which costs it nothing here.
#   prompt-agent
#               prompt-agent: also Markdown outside examples/recipes/prompt-agent/agent/,
#               examples/recipes/webhook-github/agent/ and every Go package's tree, as for echo-agent: its pytest, ruff,
#               pip-audit and image build run in the first, and its pytest also reads the second (the webhook-github
#               recipe's agent runs on the same image) and the YAML of both recipes' kourier/, no Markdown.
#   cma         cma: also Markdown outside docs/, addons/cma/ and every Go package's tree. Its tests (the add-on's module
#               and its livecheck module) may read the add-on's pages, under docs/, and its own README, as the root's
#               tests read theirs; its lint reads Go only, and the Postgres it starts reads nothing of the tree.
# A Go package's tree is a directory of the merge commit that holds a .go file, and everything under it: //go:embed
# reaches only the package's own directory and those below it, and testdata lives there too. It is computed from the
# merge commit on each run, so a page that a new package could embed stops qualifying with no list to edit. An unknown
# or missing CLASS runs everything.
#
# It fails open: skip=true is printed only when CLASS is a class above, COMMIT is a merge of two parents whose second is
# PR_HEAD_SHA, the listing succeeded and listed at least one path, every path qualifies for CLASS, and every path is a
# regular file (mode 100644 or 100755) or deleted. Anything else prints skip=false, with the reason on stderr: an unknown
# class, a COMMIT that cannot be read or is not such a merge (a shallow checkout without the parents, a push, a checkout
# of the head itself), PR_HEAD_SHA unset or empty, a listing git fails (the changed paths or, for a class that adds
# Markdown, the merge commit's tree), an empty listing, a path that does not qualify, or a symlink (mode 120000) or
# submodule (160000). The listings are git's own, so there is no cap on the number of paths. Exit status is 0 for both
# answers; any other status means the script itself failed, and the workflow then runs everything too.
set -euo pipefail

CLASS=${1:-}
COMMIT=${2:-HEAD}

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

# docs_only_path PATH: whether PATH (relative to the repository root) is on the list. Each entry was checked against
# every reader in a step that skips: the Go tests of the three modules, which were also run with a log of every file
# they open or stat (test/chart and the docs-agent corpus read every *.md under docs/ but a brand or spikes directory,
# others read single pages and README.md; internal/limitsdoc walks the tree for _test.go files and go.mod), the script
# tests under hack/, the images (.dockerignore drops docs/, and go:embed reaches only a package's own directory),
# govulncheck and CodeQL (Go source). Those walks still see every entry of the directories they read: the docs-agent
# corpus (make docs-agent-test loads the real docs/) refuses a symlink anywhere in it, whatever its name, so a listed
# path is safe only as a regular file, which the mode check below enforces for every listed path. One exception:
# hack/docs/contentcheck's TestPublicDocsAreClean and hack/docs/namecheck's TestSiteSpellsTheName run their check over
# the tree, which reads the text files under docs/assets (SVG, captions) and stats the files README.md links to
# (docs/brand holds its banner). They call the functions `make docs-check` calls on the same tree, and ci.yml's lint
# job, which runs it, never skips.
docs_only_path() {
  case "$1" in
    # A Go file or module file under a listed directory would join ./... (and limitsdoc's walk).
    *.go | */go.mod | */go.work) return 1 ;;
    # Prose for contributors and maintainers whose content nothing reads. README.md is not here: tests read it.
    AGENTS.md | CLAUDE.md | spec.md | RELEASING.md | CONTRIBUTING.md | CODE_OF_CONDUCT.md | SECURITY.md) return 0 ;;
    # The brand kit and the spike notes: no test, image or the docs agent's corpus reads their content.
    docs/brand/* | docs/spikes/*) return 0 ;;
    # The site's media. Markdown here would be a page the tests read.
    docs/assets/*.md) return 1 ;;
    docs/assets/*) return 0 ;;
  esac
  return 1
}

# class_skips PATH: whether PATH is Markdown that the jobs of CLASS do not read (the header says what each reads).
class_skips() {
  case "${CLASS}" in
    tests) return 1 ;;
    docs-agent)
      case "$1" in docs/* | examples/recipes/docs-agent/*) return 1 ;; esac
      ;;
    echo-agent)
      case "$1" in examples/recipes/echo-agent/agent/*) return 1 ;; esac
      ;;
    prompt-agent)
      case "$1" in examples/recipes/prompt-agent/agent/* | examples/recipes/webhook-github/agent/*) return 1 ;; esac
      ;;
    cma)
      case "$1" in docs/* | addons/cma/*) return 1 ;; esac
      ;;
  esac
  case "$1" in
    *.md) outside_go_trees "$1" ;;
    *) return 1 ;;
  esac
}

# GO_DIRS holds every directory of COMMIT's tree that holds a .go file, "." for the root, each with a newline before and
# after it; load_go_dirs fills it. A directory whose name has a newline in it can only make a lookup find more, never
# less: each is matched whole.
GO_DIRS=
load_go_dirs() {
  git ls-tree -r -z --name-only "${COMMIT}" >"${TMP}/tree" 2>"${TMP}/git.err" ||
    run "git ls-tree of ${COMMIT} failed: $(head -n 1 "${TMP}/git.err")"
  GO_DIRS=$'\n'
  local path dir
  while IFS= read -r -d '' path || [ -n "${path}" ]; do
    case "${path}" in
      *.go) ;;
      *) continue ;;
    esac
    case "${path}" in
      */*) dir=${path%/*} ;;
      *) dir=. ;;
    esac
    GO_DIRS+="${dir}"$'\n'
  done <"${TMP}/tree"
}

# outside_go_trees PATH: whether no directory that holds PATH, up to and including the root, holds a .go file.
outside_go_trees() {
  local dir=$1
  while :; do
    case "${dir}" in
      */*) dir=${dir%/*} ;;
      *) dir=. ;;
    esac
    [[ "${GO_DIRS}" != *$'\n'"${dir}"$'\n'* ]] || return 1
    [ "${dir}" != . ] || return 0
  done
}

# run REASON: the answer that runs every step of the job.
run() {
  printf 'docs-only (%s): %s; every step runs\n' "${CLASS:-no class}" "$1" >&2
  echo skip=false
  exit 0
}

case "${CLASS}" in
  tests | scan | crd-corpus | docs-agent | echo-agent | prompt-agent | cma) ;;
  *) run "'${CLASS}' is not a class" ;;
esac
[ -n "${PR_HEAD_SHA:-}" ] || run "PR_HEAD_SHA is not set"
parents=$(git rev-list --parents -n 1 "${COMMIT}" 2>/dev/null) || run "cannot read commit ${COMMIT}"
read -r -a commits <<<"${parents}"
[ "${#commits[@]}" -eq 3 ] || run "${COMMIT} is not a merge commit of two parents"
[ "${commits[2]}" = "${PR_HEAD_SHA}" ] ||
  run "the second parent of ${COMMIT} is ${commits[2]}, not the pull request's head ${PR_HEAD_SHA}"
# --raw -z: per path, ":<old mode> <new mode> <old blob> <new blob> <status>" and then the path, each ended by a NUL.
git diff --no-renames --raw -z "${COMMIT}^1" "${COMMIT}" >"${TMP}/paths" 2>"${TMP}/git.err" ||
  run "git diff of ${COMMIT}^1 and ${COMMIT} failed: $(head -n 1 "${TMP}/git.err")"
[ -s "${TMP}/paths" ] || run "${COMMIT} changes no path"
[ "${CLASS}" = tests ] || load_go_dirs

count=0
# git ends every field with a NUL; the tests after || keep a last one without it from being dropped.
while IFS= read -r -d '' meta || [ -n "${meta}" ]; do
  IFS= read -r -d '' path || [ -n "${path}" ] || run "the listing ends after '${meta}' with no path"
  read -r _ mode _ <<<"${meta}"
  # A symlink (120000) or a submodule (160000): a walk of docs/ sees it whatever the path, and the corpus refuses one.
  case "${mode}" in
    100644 | 100755 | 000000) ;;
    *) run "${path} has mode ${mode}, not a regular file or a deletion" ;;
  esac
  docs_only_path "${path}" || class_skips "${path}" ||
    run "${path} is neither on the docs-only list nor Markdown that this class does not read"
  count=$((count + 1))
done <"${TMP}/paths"

printf 'docs-only (%s): all %d changed paths are on the docs-only list or Markdown that this class does not read; the job skips its steps\n' "${CLASS}" "${count}" >&2
echo skip=true

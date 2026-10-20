#!/usr/bin/env bash
# Tests for pr-labels.sh. Offline: a fake gh answers the reads from canned JSON and records the writes.
# Run as `hack/github/test.sh`.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=${HERE}/pr-labels.sh
REPOSITORY=agentkourier/agent-kourier
SHA=0123456789abcdef0123456789abcdef01234567

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

check() { # DESCRIPTION WANT GOT
  if [ "$2" = "$3" ]; then
    echo "ok: $1"
  else
    printf 'FAIL: %s:\nwant [%s]\ngot  [%s]\n' "$1" "$2" "$3" >&2
    return 1
  fi
}

echo "== the labels a title calls for"
while IFS='|' read -r title want; do
  check "${title}" "${want}" "$("${SCRIPT}" labels "${title}" | paste -sd, -)"
done <<'CASES'
feat(dialect): add kagent-v0.9|enhancement
fix(slack): write pipe tables as code blocks|bug
fix: no scope|bug
docs(site): play the film|documentation
test(session): install the handler first|tests
ci: lint the workflows with zizmor|ci
build: pin the Go image|build
refactor(store): split the fencing query|refactor
perf(render): cache the block layout|performance
chore(github): add templates|chore
revert: feat(agent): undo it|revert
chore(deps): update redis docker tag to v8|dependencies
fix(deps): update module golang.org/x/net|dependencies
chore(release): v0.1.1|release
feat(api)!: rename the binding field|breaking-change,enhancement
feat!: drop v1alpha0|breaking-change,enhancement
feat(slack,render): two scopes|enhancement
style: reformat|
Update README.md|
Feat(agent): capital type|
feat(agent):missing space|
CASES

# The fake gh: GET of a pull request prints ${FAKE_DIR}/pr.json, GET of a commit's pull requests prints
# ${FAKE_DIR}/commit-pulls.json, and every write is appended to ${FAKE_DIR}/writes.
fake_gh=${TMP}/gh
cat >"${fake_gh}" <<'FAKE'
#!/usr/bin/env bash
set -eu
case "$*" in
  "api repos/"*"/commits/"*"/pulls") cat "${FAKE_DIR}/commit-pulls.json" ;;
  "api repos/"*"/pulls/"*) cat "${FAKE_DIR}/pr.json" ;;
  "api --method "*) shift; printf '%s\n' "$*" >>"${FAKE_DIR}/writes" ;;
  *) echo "fake gh: unexpected call: $*" >&2; exit 1 ;;
esac
FAKE
chmod 0755 "${fake_gh}"

sync() { # PR_JSON FORM ARG: run the pr or commit form and print the writes it made
  local dir=${TMP}/case
  rm -rf "${dir}" && mkdir "${dir}"
  printf '%s\n' "$1" >"${dir}/pr.json"
  printf '[{"number": 7, "merge_commit_sha": "%s"}, {"number": 8, "merge_commit_sha": null}]\n' "${SHA}" \
    >"${dir}/commit-pulls.json"
  touch "${dir}/writes"
  FAKE_DIR=${dir} GH=${fake_gh} "${SCRIPT}" "$2" "${REPOSITORY}" "$3" >/dev/null
  cat "${dir}/writes"
}

echo "== syncing a pull request's labels"
check "adds the label the title calls for" \
  "--method POST repos/${REPOSITORY}/issues/7/labels --raw-field labels[]=bug" \
  "$(sync '{"title": "fix(slack): x", "labels": []}' pr 7)"
check "leaves a matching label and unmanaged labels alone" \
  "" \
  "$(sync '{"title": "fix(slack): x", "labels": [{"name": "bug"}, {"name": "accessibility"}]}' pr 7)"
check "replaces a stale type label after a retitle" \
  "--method DELETE repos/${REPOSITORY}/issues/7/labels/bug
--method POST repos/${REPOSITORY}/issues/7/labels --raw-field labels[]=enhancement" \
  "$(sync '{"title": "feat(slack): x", "labels": [{"name": "bug"}, {"name": "good first issue"}]}' pr 7)"
check "removes managed labels from a title that calls for none" \
  "--method DELETE repos/${REPOSITORY}/issues/7/labels/chore" \
  "$(sync '{"title": "Update README.md", "labels": [{"name": "chore"}]}' pr 7)"
check "labels the pull request merged as the commit, not another that contains it" \
  "--method POST repos/${REPOSITORY}/issues/7/labels --raw-field labels[]=documentation" \
  "$(sync '{"title": "docs: x", "labels": []}' commit "${SHA}")"

echo "== refusals"
refuses() { # DESCRIPTION ARGS...
  local description=$1
  shift
  if GH=${fake_gh} FAKE_DIR=${TMP} "${SCRIPT}" "$@" >/dev/null 2>&1; then
    echo "FAIL: ${description}: the script accepted it" >&2
    return 1
  fi
  echo "ok: ${description}"
}
refuses "a pull request number that is not a number" pr "${REPOSITORY}" '7; rm -rf /'
refuses "a commit that is not a full SHA" commit "${REPOSITORY}" main
refuses "an unknown form" label "fix: x"

echo "== a commit no pull request merged"
dir=${TMP}/none
mkdir "${dir}"
echo '[]' >"${dir}/commit-pulls.json"
touch "${dir}/writes"
FAKE_DIR=${dir} GH=${fake_gh} "${SCRIPT}" commit "${REPOSITORY}" "${SHA}" >/dev/null
check "makes no write" "" "$(cat "${dir}/writes")"

echo "all pr-labels tests passed"

#!/usr/bin/env bash
# Keep a pull request's type labels in step with its Conventional Commits title, so the generated release notes
# (.github/release.yml) file each change under the right heading. The title decides only the labels in MANAGED: one is
# added when the title calls for it and removed when it no longer does. Every other label is left alone.
#
# Usage:
#   pr-labels.sh labels TITLE              print the labels TITLE calls for, one per line
#   pr-labels.sh pr REPOSITORY NUMBER      sync the labels of pull request NUMBER
#   pr-labels.sh commit REPOSITORY SHA     sync the labels of the merged pull request that SHA came from
# GH selects the gh binary (default gh); the pr and commit forms need jq.
set -euo pipefail

GH=${GH:-gh}

# The labels this script owns. .github/release.yml groups the notes by some of them.
MANAGED=(breaking-change enhancement bug documentation tests ci build refactor performance chore revert dependencies release)

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# labels_for TITLE: the type label (or dependencies, or release) and breaking-change for a `!` before the colon.
# A title that is not a Conventional Commit, or has a type not listed here, calls for no label.
labels_for() {
  local title=$1 type scope bang
  local re='^([a-z]+)(\(([^)]*)\))?(!)?: '
  [[ ${title} =~ ${re} ]] || return 0
  type=${BASH_REMATCH[1]}
  scope=${BASH_REMATCH[3]}
  bang=${BASH_REMATCH[4]}
  [ -z "${bang}" ] || echo breaking-change
  if [ "${scope}" = deps ]; then
    echo dependencies
    return 0
  fi
  if [ "${type}" = chore ] && [ "${scope}" = release ]; then
    echo release
    return 0
  fi
  case ${type} in
    feat) echo enhancement ;;
    fix) echo bug ;;
    docs) echo documentation ;;
    test) echo tests ;;
    ci) echo ci ;;
    build) echo build ;;
    refactor) echo refactor ;;
    perf) echo performance ;;
    chore) echo chore ;;
    revert) echo revert ;;
  esac
}

is_managed() {
  local label
  for label in "${MANAGED[@]}"; do
    [ "${label}" != "$1" ] || return 0
  done
  return 1
}

contains() { # NEEDLE LINES
  grep -Fx -- "$1" <<<"$2" >/dev/null
}

sync_pr() { # REPOSITORY NUMBER
  local repository=$1 number=$2 pr title current want label
  [[ ${number} =~ ^[0-9]+$ ]] || die "not a pull request number: ${number}"
  pr=$("${GH}" api "repos/${repository}/pulls/${number}") || die "could not read pull request #${number}"
  title=$(jq -r '.title' <<<"${pr}")
  current=$(jq -r '.labels[].name' <<<"${pr}")
  want=$(labels_for "${title}")
  printf '#%s %s\n' "${number}" "${title}"
  while IFS= read -r label; do
    if [ -z "${label}" ] || ! is_managed "${label}" || contains "${label}" "${want}"; then
      continue
    fi
    "${GH}" api --method DELETE "repos/${repository}/issues/${number}/labels/${label}" >/dev/null ||
      die "could not remove ${label} from #${number}"
    printf '  removed %s\n' "${label}"
  done <<<"${current}"
  while IFS= read -r label; do
    if [ -z "${label}" ] || contains "${label}" "${current}"; then
      continue
    fi
    "${GH}" api --method POST "repos/${repository}/issues/${number}/labels" --raw-field "labels[]=${label}" >/dev/null ||
      die "could not add ${label} to #${number}"
    printf '  added %s\n' "${label}"
  done <<<"${want}"
}

sync_commit() { # REPOSITORY SHA
  local repository=$1 sha=$2 pulls number
  [[ ${sha} =~ ^[0-9a-f]{40}$ ]] || die "not a commit SHA: ${sha}"
  pulls=$("${GH}" api "repos/${repository}/commits/${sha}/pulls") || die "could not list the pull requests of ${sha}"
  number=$(jq -r --arg sha "${sha}" '.[] | select(.merge_commit_sha == $sha) | .number' <<<"${pulls}")
  if [ -z "${number}" ]; then
    printf 'no pull request merged as %s; nothing to label\n' "${sha}"
    return 0
  fi
  sync_pr "${repository}" "${number}"
}

case ${1:-} in
  labels)
    [ $# -eq 2 ] || die "usage: $0 labels TITLE"
    labels_for "$2"
    ;;
  pr)
    [ $# -eq 3 ] || die "usage: $0 pr REPOSITORY NUMBER"
    sync_pr "$2" "$3"
    ;;
  commit)
    [ $# -eq 3 ] || die "usage: $0 commit REPOSITORY SHA"
    sync_commit "$2" "$3"
    ;;
  *)
    die "usage: $0 labels TITLE | pr REPOSITORY NUMBER | commit REPOSITORY SHA"
    ;;
esac

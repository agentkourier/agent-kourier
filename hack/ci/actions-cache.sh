#!/usr/bin/env bash
# Upkeep of the Actions caches that roll with main (the comment at the top of ci.yml's lint job says how they work).
# Run from the repository root:
#
#   hack/ci/actions-cache.sh mark [DIR...]     after the restore, before the job's first Go command
#   hack/ci/actions-cache.sh trim [DIR...]     after the job's own steps, before the save
#   hack/ci/actions-cache.sh prune PREFIX...   after the saves; needs gh, jq, GITHUB_REPOSITORY and a GH_TOKEN with
#                                              actions: write
#
# mark and trim work on Go's build cache (go env GOCACHE) and on each DIR, a cache with the same layout (golangci-lint's
# is a copy of Go's). Each entry is a file, or for a cached executable a directory, named <id>-a or <id>-d one level
# down: an index entry (-a, a line "v1 <action ID> <output ID> <size> <time>") names its output (-d,
# <output ID[0:2]>/<output ID>-d). Both tools set an entry's mtime to the current time when they read it, unless it is
# already less than an hour old, and write new entries with the current time; but an index entry they write for an
# output whose bytes are already in the cache leaves that output file as it was, mtime and all. mark sets every entry's
# mtime to 2000-01-01, so that any read in the job moves it on, and writes the current time into each cache's trim.txt:
# otherwise either tool may run its own trim, at most once a day, which deletes every entry unused for five days, and
# every backdated entry looks unused for longer. trim keeps every entry newer than the mark, and every output a kept
# index entry names, and deletes the rest: the cache a job saves holds what one run read or wrote, not what every run
# before it left behind. A deleted entry only costs time: the tool builds it again.
#
# With CACHE_KEY and CACHE_RESTORED_KEY set (the restore's primary and matched keys, whose last dash-separated part is
# the commit), mark also empties the module cache (go clean -modcache) when the restored entry's key differs before
# that part: it was saved for another go.sum or Go version, or nothing was restored. Nothing trims the module cache, so
# without this every module version the restores carried forward would stay in it; the job downloads what it needs.
#
# mark and trim keep their state in RUNNER_TEMP/actions-cache, so trim fails, deleting nothing, without mark.
#
# prune deletes, for each PREFIX, every cache entry of refs/heads/main whose key starts with it except the most
# recently created, which is the one a restore by that prefix returns. It looks at the 100 newest; the next prune
# reaches any beyond them. A PREFIX must end in "-", so that go-test-Linux- cannot reach go-test-testhooks-Linux-. A
# listing or a delete that fails (an API error, or another run's prune got there first) is a warning and prune goes on
# with the next: the release gate (hack/release/check-ci.sh) needs every job of main's push runs to pass, and the next
# prune tries again.
set -euo pipefail

die() {
  printf 'actions-cache: %s\n' "$*" >&2
  exit 1
}

# entries DIR: the entries of the Go-layout cache DIR, one path per line, sorted.
entries() {
  find "$1" -mindepth 2 -maxdepth 2 \( -name '*-a' -o -name '*-d' \) | LC_ALL=C sort
}

# build_caches [DIR...]: Go's build cache, then each DIR.
build_caches() {
  go env GOCACHE
  printf '%s\n' "$@"
}

state_dir() {
  printf '%s/actions-cache' "${RUNNER_TEMP:?RUNNER_TEMP is not set}"
}

mark() {
  local state caches dir
  state=$(state_dir)
  caches=$(build_caches "$@")
  mkdir -p "${state}"
  : >"${state}/restored"
  local restored=${CACHE_RESTORED_KEY:-}
  if [ -n "${CACHE_KEY:-}" ] && [ "${restored%-*}" != "${CACHE_KEY%-*}" ]; then
    printf 'restored %s, not an entry of %s-: emptying the module cache\n' "${restored:-nothing}" "${CACHE_KEY%-*}"
    go clean -modcache
  fi
  while IFS= read -r dir; do
    [ -d "${dir}" ] || continue
    find "${dir}" -mindepth 2 -maxdepth 2 \( -name '*-a' -o -name '*-d' \) -exec touch -t 200001010000 {} +
    date +%s >"${dir}/trim.txt"
    entries "${dir}" >>"${state}/restored"
  done <<<"${caches}"
  LC_ALL=C sort -o "${state}/restored" "${state}/restored"
  # Every backdated entry is older than the mark; every entry used or written after this is newer.
  touch "${state}/mark"
  printf 'marked %d restored cache entries\n' "$(wc -l <"${state}/restored")"
}

trim() {
  local state caches dir restored kept written
  state=$(state_dir)
  [ -f "${state}/mark" ] || die "no mark in ${state}: run mark after the restore"
  caches=$(build_caches "$@")
  : >"${state}/kept"
  while IFS= read -r dir; do
    [ -d "${dir}" ] || continue
    # The outputs the kept index entries name move past the mark too, so the deletion below spares them.
    # The first path, which never exists, keeps touch from running with none when no index entry is kept.
    {
      printf '%s\n' "${state}/none"
      find "${dir}" -mindepth 2 -maxdepth 2 -name '*-a' -newer "${state}/mark" -exec awk -v dir="${dir}" \
        '$1 == "v1" && length($3) == 64 { print dir "/" substr($3, 1, 2) "/" $3 "-d" }' {} +
    } | tr '\n' '\0' | xargs -0 touch -c --
    find "${dir}" -mindepth 2 -maxdepth 2 \( -name '*-a' -o -name '*-d' \) ! -newer "${state}/mark" -exec rm -rf -- {} +
    entries "${dir}" >>"${state}/kept"
  done <<<"${caches}"
  LC_ALL=C sort -o "${state}/kept" "${state}/kept"
  restored=$(wc -l <"${state}/restored")
  kept=$(wc -l <"${state}/kept")
  written=$(LC_ALL=C comm -13 "${state}/restored" "${state}/kept" | wc -l)
  printf 'kept %d of %d restored cache entries; the job wrote %d\n' "$((kept - written))" "${restored}" "${written}"
}

prune() {
  [ "$#" -gt 0 ] || die "prune needs at least one key prefix"
  : "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}"
  local prefix list superseded id key
  for prefix in "$@"; do
    case "${prefix}" in
      ?*-) ;;
      *) die "key prefix '${prefix}' does not end in -" ;;
    esac
  done
  for prefix in "$@"; do
    if ! list=$(gh cache list --repo "${GITHUB_REPOSITORY}" --ref refs/heads/main --key "${prefix}" \
      --sort created_at --order desc --limit 100 --json id,key,createdAt) ||
      ! superseded=$(jq -r --arg prefix "${prefix}" \
        'map(select(.key | startswith($prefix))) | sort_by(.createdAt) | reverse | .[1:][] | "\(.id)\t\(.key)"' \
        <<<"${list}"); then
      printf '::warning::could not list the cache entries of %s; the next prune tries again\n' "${prefix}"
      continue
    fi
    while IFS=$'\t' read -r id key; do
      [ -n "${id}" ] || continue
      if gh cache delete "${id}" --repo "${GITHUB_REPOSITORY}"; then
        printf 'deleted %s: superseded\n' "${key}"
      else
        printf '::warning::could not delete the superseded cache entry %s; the next prune tries again\n' "${key}"
      fi
    done <<<"${superseded}"
  done
}

[ "$#" -gt 0 ] || die "usage: actions-cache.sh mark|trim [DIR...] | prune PREFIX..."
command=$1
shift
case "${command}" in
  mark) mark "$@" ;;
  trim) trim "$@" ;;
  prune) prune "$@" ;;
  *) die "unknown command ${command}: want mark, trim or prune" ;;
esac

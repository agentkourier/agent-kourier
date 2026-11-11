#!/usr/bin/env bash
# Record the clip of the docs, docs/assets/clip/team-app.gif, typed by vhs against a kind cluster of its own, with the
# echo agent of examples/recipes/echo-agent as the team's agent: a broker with no Slack app of its own, and a team in
# team-a that brings its own. README.md, docs/how-to/install-with-helm.md and docs/how-to/team-slack-app.md show it; its
# tape, team.tape, follows the section "The whole path, recorded" of docs/how-to/team-slack-app.md.
#
#   record.sh up        make the cluster (kourier-clip) and start the echo agent in it
#   record.sh take      reset the cluster to no Agent Kourier, render the tape, record bin/clip/out/team-app.gif
#   record.sh practice  the same take with fake Slack tokens, into bin/clip/practice; it stops at the first
#                       `kubectl get akbinding` row, since a broker with a fake token never connects and the Binding never
#                       gets to Ready. It resets the cluster as take does, so it removes the broker of a real take
#   record.sh stitch STILL
#                       add STILL, a capture of the Slack thread pane with the first answer, to the end of the take
#                       and write the published GIF, docs/assets/clip/team-app.gif
#   record.sh down      delete the cluster
#
# take needs:
#   CLIP_ENV_FILE  a file of mode 600 with the lines SLACK_BOT_TOKEN=xoxb-... and SLACK_APP_TOKEN=xapp-..., in the
#                  format of `docker run --env-file` (no `export`, no quotes). The tokens reach the recorder's shell as
#                  its environment; the tape only names them, and this script never prints them.
#   CLIP_CHANNEL   the ID of the channel the clip's Binding names (it is typed on screen), with the bot a member.
#
# The cluster keeps its kubeconfig in bin/clip, so the host kubeconfig and its current context are never read or
# changed. A cluster of that name that this script did not make (it lacks the owner label) is refused, not reset.
# The recorder is the image of hack/clip/Dockerfile; its container joins kind's Docker network and reaches the API
# server by the cluster's internal address.
#
# The chart is the published one of the version in charts/agent-kourier/Chart.yaml, so a take for a new release is
# made after its tag is published. To record before the final tag, so that the tag's tree carries the clip, set
# CLIP_CHART_VERSION to a release candidate of that version built from the same code (0.2.1-rc.1 for 0.2.1): the tape
# still types the version in Chart.yaml, and a helm function in the recorder's hidden setup installs the candidate in
# its place and hides the two lines helm prints naming the pulled chart and its digest. Say both in the pull request
# that brings the clip. The files the tape opens are extracted at each take from the yaml blocks of those names in the
# page's section (TAPE and FILES below); the commands of the tape follow that section but are written in the tape, so a
# change to them on the page needs the same change there. The one change to a page command is the `-w` the tape adds to the page's
# `kubectl get akbinding`, to watch the Binding turn Ready.
set -euo pipefail
umask 077

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
STATE=${ROOT}/bin/clip
CLUSTER=kourier-clip
OWNER_LABEL=agentkourier.agent-kourier.dev/owner=install-clip
KCFG=${STATE}/kubeconfig
KCFG_INTERNAL=${STATE}/kubeconfig.internal
RECORDER=agent-kourier-clip-vhs:dev
ECHO_IMAGE=agent-kourier-echo-agent:dev
CHART=oci://ghcr.io/agentkourier/charts/agent-kourier
# The team clip's namespace, which the broker admits; it exists before that take starts, as the page says.
TEAM_NS=team-a

die() {
  echo "record.sh: $*" >&2
  exit 1
}

mkdir -p "${STATE}"

K=(kubectl --kubeconfig "${KCFG}" --context "kind-${CLUSTER}" --request-timeout=30s)
# For deletes that wait: --request-timeout would also cut the watch of the wait.
KW=(kubectl --kubeconfig "${KCFG}" --context "kind-${CLUSTER}")
H=(helm --kubeconfig "${KCFG}" --kube-context "kind-${CLUSTER}")

# exists: true when kind has a cluster of this name.
exists() {
  kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"
}

# owned: true when the cluster exists and its node carries this script's owner label. It dies when the cluster exists
# but its API server does not answer (a stopped node container, for one), rather than calling that someone else's.
owned() {
  local nodes
  exists || return 1
  kind get kubeconfig --name "${CLUSTER}" >"${KCFG}" 2>/dev/null ||
    die "kind lists the cluster ${CLUSTER} but could not give its kubeconfig; is Docker running?"
  nodes=$("${K[@]}" get nodes -l "${OWNER_LABEL}" -o name 2>/dev/null) ||
    die "the kind cluster ${CLUSTER} exists but its API server does not answer; is its node container running?"
  [ -n "${nodes}" ]
}

up() {
  local node_image
  node_image=$(sed -n 's/^NODE_IMAGE=//p' "${ROOT}/hack/kind/lib.sh")
  [[ ${node_image} =~ ^kindest/node:v[0-9.]+@sha256:[0-9a-f]{64}$ ]] ||
    die "could not read a pinned NODE_IMAGE from hack/kind/lib.sh (read '${node_image}')"
  if exists; then
    owned || die "a kind cluster named ${CLUSTER} exists and this script did not make it; refusing to use it"
  else
    kind create cluster --name "${CLUSTER}" --image "${node_image}" --kubeconfig "${KCFG}" --wait 120s --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    labels: {${OWNER_LABEL%%=*}: ${OWNER_LABEL#*=}}
EOF
  fi
  kind get kubeconfig --name "${CLUSTER}" >"${KCFG}"
  kind get kubeconfig --internal --name "${CLUSTER}" >"${KCFG_INTERNAL}"

  docker build -t "${ECHO_IMAGE}" "${ROOT}/examples/recipes/echo-agent/agent"
  kind load docker-image "${ECHO_IMAGE}" --name "${CLUSTER}"
  "${K[@]}" create namespace echo-agent --dry-run=client -o yaml | "${K[@]}" apply -f -
  "${K[@]}" apply -n echo-agent -f "${ROOT}/examples/recipes/echo-agent/agent/manifests/"
  "${K[@]}" -n echo-agent rollout status deploy/echo-agent --timeout=180s
}

# reset: remove what a take installs, so the next take starts as a new install does: no release, no namespace, no CRDs.
reset() {
  owned || die "no cluster ${CLUSTER} made by this script: run record.sh up"
  "${H[@]}" uninstall agent-kourier -n agent-kourier --wait --ignore-not-found >/dev/null
  "${KW[@]}" delete namespace agent-kourier "${TEAM_NS}" --ignore-not-found --wait=true --timeout=3m
  "${KW[@]}" delete crd bindings.agentkourier.dev agentbackends.agentkourier.dev chatconnections.agentkourier.dev \
    --ignore-not-found --wait=true --timeout=3m
}

# What the clip records. TAPE is in hack/clip; SECTION is the heading, matched whole, of the PAGE section whose yaml
# blocks titled FILES are extracted; GIF names the take's output (the tape's Output line) and the published GIF.
TAPE=team.tape PAGE=${ROOT}/docs/how-to/team-slack-app.md SECTION='### The whole path, recorded'
FILES=(my-values.yaml team-a.yaml) GIF=team-app PRACTICE_ROW='my-binding\s{3,}'

# extract NAME: the yaml block titled NAME in SECTION of PAGE. The section ends at the next heading of its level or
# above; a line in a code fence (a YAML or shell comment) is never a heading.
extract() {
  awk -v name="$1" -v heading="${SECTION}" '
    BEGIN { level = index(heading, " ") - 1 }
    !section && $0 == heading { section = 1; next }
    !section { next }
    grab && /^```/ { exit }
    grab { print; next }
    $0 == "```yaml title=\"" name "\"" { grab = 1; next }
    /^```/ { fence = !fence; next }
    !fence && match($0, /^#+ /) && RLENGTH - 1 <= level { exit }
  ' "${PAGE}"
}

# record ENV_FILE CHANNEL READY_REGEX OUT
record() {
  local env_file=$1 channel=$2 ready=$3 out=$4 work=${STATE}/work tape version installed
  version=$(sed -n 's/^version: //p' "${ROOT}/charts/agent-kourier/Chart.yaml")
  [ -n "${version}" ] || die "no version in charts/agent-kourier/Chart.yaml"
  installed=${CLIP_CHART_VERSION:-${version}}
  [[ ${installed} == "${version}" || ${installed} =~ ^${version//./\\.}-rc\.[0-9]+$ ]] ||
    die "CLIP_CHART_VERSION must be ${version} or a release candidate of it (${version}-rc.N), not ${installed}"
  # Before reset, so a take for an unpublished version leaves the cluster as it was.
  helm show chart "${CHART}" --version "${installed}" >/dev/null ||
    die "no chart ${CHART} ${installed} is published yet: a take is made after a tag publishes it"
  docker build -q -t "${RECORDER}" "${HERE}" >/dev/null
  reset
  rm -rf "${work}" && mkdir -p "${work}" "${out}"
  for f in "${FILES[@]}"; do
    extract "${f}" >"${work}/${f}"
    [ -s "${work}/${f}" ] || die "no block titled ${f} in the section '${SECTION}' of ${PAGE}"
  done
  "${K[@]}" create namespace "${TEAM_NS}" >/dev/null
  kind get kubeconfig --internal --name "${CLUSTER}" >"${KCFG_INTERNAL}"

  tape=$(<"${HERE}/${TAPE}")
  # Quoted replacements: bash 5.2's patsub_replacement would read & in them.
  tape=${tape//@VERSION@/"${version}"}
  tape=${tape//@CHANNEL@/"${channel}"}
  tape=${tape//@READY@/"${ready}"}
  printf '%s\n' "${tape}" >"${STATE}/clip.tape"
  # The hidden setup sources this file. It is empty unless a candidate stands in for the typed version. Then a helm
  # function swaps the version argument for the candidate and drops the two lines helm prints when it pulls a chart,
  # "Pulled: <ref>" and "Digest: <digest>", which would name the candidate under the typed version. helm install
  # prints them on stdout and helm template on stderr, so the function joins the two streams on stdout (the clip
  # only shows a terminal) and passes every other line through in order; it returns helm's exit status.
  : >"${STATE}/setup.sh"
  if [ "${installed}" != "${version}" ]; then
    printf '__clip_typed=%q __clip_installed=%q\n' "${version}" "${installed}" >"${STATE}/setup.sh"
    cat >>"${STATE}/setup.sh" <<'EOF'
helm() {
  local a=() x
  for x in "$@"; do [ "$x" = "${__clip_typed}" ] && x=${__clip_installed}; a+=("$x"); done
  command helm "${a[@]}" 2>&1 | grep -v -E '^(Pulled|Digest): '
  return "${PIPESTATUS[0]}"
}
EOF
    echo "record.sh: the tape types --version ${version}; the take installs ${installed} and hides helm's Pulled and Digest lines" >&2
  fi

  docker run --rm --network kind \
    -e KUBECONFIG=/kube/config -v "${KCFG_INTERNAL}:/kube/config:ro" \
    --env-file "${env_file}" \
    -v "${work}:/work" -v "${out}:/vhs/out" \
    -v "${STATE}/clip.tape:/vhs/clip.tape:ro" -v "${STATE}/setup.sh:/vhs/setup.sh:ro" \
    "${RECORDER}" clip.tape
  ls -l "${out}"
}

# stitch STILL: the published GIF, docs/assets/clip/team-app.gif: vhs's GIF of the take, then one frame of STILL (a
# capture of the Slack thread pane alone: the mention and the agent's answer, and none of the channel's earlier
# messages), cropped by CLIP_CROP (w:h:x:y in the still's pixels) if it is set, fitted into the take's 1320x760
# frame, and held for CLIP_HOLD whole seconds. It runs the recorder image's ffmpeg and gifsicle. The take's own GIF
# is kept, not re-encoded. The tape writes no MP4, which nothing uses: vhs renders each output from every frame at
# once (see the Framerate note in the tape).
stitch() {
  local still=$1 out=${STATE}/out crop=${CLIP_CROP:-} hold=${CLIP_HOLD:-6}
  [ -f "${still}" ] || die "${still}: no such file"
  [ -f "${out}/${GIF}.gif" ] || die "no take in ${out}: run record.sh take"
  [[ -z ${crop} || ${crop} =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ ]] || die "CLIP_CROP must be w:h:x:y in pixels"
  [[ ${hold} =~ ^[1-9][0-9]?$ ]] || die "CLIP_HOLD must be whole seconds, 1 to 99"
  docker build -q -t "${RECORDER}" "${HERE}" >/dev/null
  cp "${still}" "${out}/still"
  docker run --rm --entrypoint ffmpeg -v "${out}:/out" "${RECORDER}" -y -v error -i /out/still -filter_complex "
      ${crop:+crop=${crop},}scale=1264:704:force_original_aspect_ratio=decrease:flags=lanczos,
      pad=1320:760:(ow-iw)/2:(oh-ih)/2:color=0x0b1020,split[a][b];
      [a]palettegen=max_colors=256[p];
      [b][p]paletteuse=dither=sierra2_4a" -frames:v 1 /out/still.gif
  docker run --rm --entrypoint gifsicle -v "${out}:/out" "${RECORDER}" --no-warnings --merge --loopcount=forever \
    "/out/${GIF}.gif" --delay "$((hold * 100))" /out/still.gif -O2 -o "/out/${GIF}-final.gif"
  mkdir -p "${ROOT}/docs/assets/clip"
  cp "${out}/${GIF}-final.gif" "${ROOT}/docs/assets/clip/${GIF}.gif"
  ls -l "${ROOT}/docs/assets/clip/${GIF}.gif"
}

# mode_of FILE: the permission bits in octal, with GNU stat or BSD stat. GNU first: BSD stat has no -c and fails
# without output, while GNU stat reads BSD's -f as "file system" and prints a block before failing.
mode_of() {
  stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"
}


case "${1:-}" in
  up) up ;;
  take)
    : "${CLIP_ENV_FILE:?set CLIP_ENV_FILE to the env file with SLACK_BOT_TOKEN and SLACK_APP_TOKEN}"
    : "${CLIP_CHANNEL:?set CLIP_CHANNEL to the channel ID}"
    [ -f "${CLIP_ENV_FILE}" ] || die "${CLIP_ENV_FILE}: no such file"
    [ "$(mode_of "${CLIP_ENV_FILE}")" = 600 ] || die "${CLIP_ENV_FILE} must be mode 600"
    for key in SLACK_BOT_TOKEN SLACK_APP_TOKEN; do
      grep -q "^${key}=." "${CLIP_ENV_FILE}" || die "${CLIP_ENV_FILE} has no ${key}= line"
    done
    [[ ${CLIP_CHANNEL} =~ ^[CG][A-Z0-9]+$ ]] || die "CLIP_CHANNEL must be a channel ID (C... or G...)"
    record "${CLIP_ENV_FILE}" "${CLIP_CHANNEL}" 'True\s+OK' "${STATE}/out"
    ;;
  practice)
    # Values that Slack refuses; they are not shaped like tokens, so the secret scan has nothing to match.
    printf '%s=%s\n' SLACK_BOT_TOKEN not-a-token SLACK_APP_TOKEN not-a-token >"${STATE}/practice.env"
    record "${STATE}/practice.env" C0123456789 "${PRACTICE_ROW}" "${STATE}/practice"
    ;;
  stitch)
    [ $# -eq 2 ] || die "usage: record.sh stitch STILL"
    stitch "$2"
    ;;
  down)
    if exists; then
      owned || die "a kind cluster named ${CLUSTER} exists and this script did not make it; refusing to delete it"
      kind delete cluster --name "${CLUSTER}"
    fi
    rm -f "${KCFG}" "${KCFG_INTERNAL}"
    ;;
  *)
    sed -n '2,16p' "$0" >&2
    exit 2
    ;;
esac

#!/usr/bin/env bash
# Record the install clip that README.md and docs/how-to/install-with-helm.md show: a fresh install on the custom
# resource route, typed by vhs against a kind cluster of its own, with the echo agent of examples/recipes/echo-agent.
#
#   record.sh up        make the cluster (kourier-clip) and start the echo agent in it
#   record.sh take      reset the cluster to no Agent Kourier, render install.tape, record bin/clip/out/install.{gif,mp4}
#   record.sh practice  the same take with fake Slack tokens; it stops at the first `kubectl get akbinding` row, since a
#                       broker with a fake token never connects and the Binding never gets to Ready
#   record.sh stitch STILL
#                       add STILL, a screenshot of the first answer in Slack, to the end of the take and write the
#                       published GIF, docs/assets/clip/install.gif
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
# made after its tag is published. my-values.yaml and my-resources.yaml are extracted from the blocks of those names in
# the page's "Install on custom resources" section at each take; the commands of install.tape follow that section but
# are written in the tape, so a change to them on the page needs the same change there.
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
PAGE=${ROOT}/docs/how-to/install-with-helm.md

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
  kind get kubeconfig --name "${CLUSTER}" >"${KCFG}" 2>/dev/null || return 1
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
  "${KW[@]}" delete namespace agent-kourier --ignore-not-found --wait=true --timeout=3m
  "${KW[@]}" delete crd bindings.agentkourier.dev agentbackends.agentkourier.dev chatconnections.agentkourier.dev \
    --ignore-not-found --wait=true --timeout=3m
}

# extract NAME: the yaml block titled NAME in the page's "Install on custom resources" section.
extract() {
  awk -v name="$1" '
    /^## Install on custom resources/ { section = 1; next }
    section && /^## / { exit }
    section && $0 == "```yaml title=\"" name "\"" { grab = 1; next }
    grab && /^```/ { exit }
    grab { print }
  ' "${PAGE}"
}

# record ENV_FILE CHANNEL READY_REGEX
record() {
  local env_file=$1 channel=$2 ready=$3 work=${STATE}/work out=${STATE}/out tape version
  docker build -q -t "${RECORDER}" "${HERE}" >/dev/null
  reset
  rm -rf "${work}" && mkdir -p "${work}" "${out}"
  for f in my-values.yaml my-resources.yaml; do
    extract "${f}" >"${work}/${f}"
    [ -s "${work}/${f}" ] || die "no block titled ${f} in the Install on custom resources section of ${PAGE}"
  done
  kind get kubeconfig --internal --name "${CLUSTER}" >"${KCFG_INTERNAL}"

  version=$(sed -n 's/^version: //p' "${ROOT}/charts/agent-kourier/Chart.yaml")
  [ -n "${version}" ] || die "no version in charts/agent-kourier/Chart.yaml"
  tape=$(<"${HERE}/install.tape")
  # Quoted replacements: bash 5.2's patsub_replacement would read & in them.
  tape=${tape//@VERSION@/"${version}"}
  tape=${tape//@CHANNEL@/"${channel}"}
  tape=${tape//@READY@/"${ready}"}
  printf '%s\n' "${tape}" >"${STATE}/install.tape"

  docker run --rm --network kind \
    -e KUBECONFIG=/kube/config -v "${KCFG_INTERNAL}:/kube/config:ro" \
    --env-file "${env_file}" \
    -v "${work}:/work" -v "${out}:/vhs/out" -v "${STATE}/install.tape:/vhs/install.tape:ro" \
    "${RECORDER}" install.tape
  ls -l "${out}"
}

# stitch STILL: the published GIF, docs/assets/clip/install.gif: vhs's GIF of the take, then one frame of STILL (a
# screenshot of the mention and the agent's answer in Slack), cropped by CLIP_CROP (w:h:x:y in the still's pixels),
# fitted into the take's 1320x760 frame, and held for CLIP_HOLD whole seconds. It runs the recorder image's ffmpeg and
# gifsicle. The take's own GIF is kept rather than re-encoded from the MP4, whose compression noise would differ in
# every frame and triple the file.
stitch() {
  local still=$1 out=${STATE}/out crop=${CLIP_CROP:-1034:410:400:128} hold=${CLIP_HOLD:-6}
  [ -f "${still}" ] || die "${still}: no such file"
  [ -f "${out}/install.gif" ] || die "no take in ${out}: run record.sh take"
  [[ ${crop} =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ ]] || die "CLIP_CROP must be w:h:x:y in pixels"
  [[ ${hold} =~ ^[1-9][0-9]?$ ]] || die "CLIP_HOLD must be whole seconds, 1 to 99"
  docker build -q -t "${RECORDER}" "${HERE}" >/dev/null
  cp "${still}" "${out}/still"
  docker run --rm --entrypoint ffmpeg -v "${out}:/out" "${RECORDER}" -y -v error -i /out/still -filter_complex "
      crop=${crop},scale=1264:704:force_original_aspect_ratio=decrease:flags=lanczos,
      pad=1320:760:(ow-iw)/2:(oh-ih)/2:color=0x0b1020,split[a][b];
      [a]palettegen=max_colors=256[p];
      [b][p]paletteuse=dither=sierra2_4a" -frames:v 1 /out/still.gif
  docker run --rm --entrypoint gifsicle -v "${out}:/out" "${RECORDER}" --no-warnings --merge --loopcount=forever \
    /out/install.gif --delay "$((hold * 100))" /out/still.gif -O2 -o /out/install-final.gif
  mkdir -p "${ROOT}/docs/assets/clip"
  cp "${out}/install-final.gif" "${ROOT}/docs/assets/clip/install.gif"
  ls -l "${ROOT}/docs/assets/clip/install.gif"
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
    record "${CLIP_ENV_FILE}" "${CLIP_CHANNEL}" 'True\s+OK'
    ;;
  practice)
    # Values that Slack refuses; they are not shaped like tokens, so the secret scan has nothing to match.
    printf '%s=%s\n' SLACK_BOT_TOKEN not-a-token SLACK_APP_TOKEN not-a-token >"${STATE}/practice.env"
    record "${STATE}/practice.env" C0123456789 'agent-kourier\s+my-binding'
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
    sed -n '2,12p' "$0" >&2
    exit 2
    ;;
esac

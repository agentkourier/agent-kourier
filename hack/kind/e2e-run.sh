#!/usr/bin/env bash
# Run the E2E tests in a tier's cluster: cross-build the test binary (go test -c -tags e2e), put it
# in the pinned distroless image, load that into the kind cluster, and run it as a Job in namespace
# e2e (hack/kind/e2e/runner.yaml). The tests then reach the test doubles by their ClusterIP Service
# names, and nothing is published to the host: the fake Slack's /_control/ has no authentication.
# The Job's logs are printed, the Job is deleted, and the exit status is the test result.
# Usage: e2e-run.sh fast|full. The tier must be up (make e2e-fast-up or e2e-full-up); both tiers
# have the same doubles in namespaces e2e and monitoring, so the same tests run in both.
#
# Every kubectl call names --context kind-courier-<tier> and every kind call names
# --name courier-<tier>; assert_context aborts unless the context is that local kind cluster.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
tier=${1:-}
use_tier "${tier}"
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
IMAGE=courier-e2e/runner:dev
JOB=e2e-runner
# How long to wait for the Job; its own activeDeadlineSeconds (600) is the backstop.
WAIT_SECONDS=${E2E_WAIT_SECONDS:-420}
[[ "${WAIT_SECONDS}" =~ ^[0-9]+$ ]] || die "E2E_WAIT_SECONDS must be a whole number of seconds"

for tool in docker kind kubectl go; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"
cluster_exists || die "kind cluster ${CLUSTER} does not exist; bring it up with: make e2e-${tier}-up"
assert_owned
assert_context "${tier}"

BUILD=$(mktemp -d)
# On any exit: the scratch build directory and the Job (the cluster itself is never torn down here).
trap 'rm -rf "${BUILD}"; "${K[@]}" -n e2e delete job "${JOB}" --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT
trap 'exit 130' INT TERM

echo "==> build the test binary"
case "$(docker info --format '{{.Architecture}}')" in
  aarch64 | arm64) goarch=arm64 ;;
  x86_64 | amd64) goarch=amd64 ;;
  *) die "unsupported Docker architecture" ;;
esac
GOOS=linux GOARCH=${goarch} CGO_ENABLED=0 go test -C "${ROOT}" -c -tags e2e -o "${BUILD}/e2e.test" ./test/e2e
docker build -q -f "${HERE}/e2e/Dockerfile" --build-arg "RUNTIME_IMAGE=${RUNTIME_IMAGE}" -t "${IMAGE}" "${BUILD}" >/dev/null
kind load docker-image "${IMAGE}" --name "${CLUSTER}"

echo "==> run the Job in ${CTX}"
# A Job's pod template cannot change, so a leftover Job from an interrupted run goes first.
"${K[@]}" -n e2e delete job "${JOB}" --ignore-not-found >/dev/null
# The runner's Role in courier's namespace needs the namespace to exist.
"${K[@]}" get namespace courier >/dev/null 2>&1 || "${K[@]}" create namespace courier >/dev/null
"${K[@]}" apply -f "${HERE}/e2e/runner.yaml" >/dev/null
result=
deadline=$((SECONDS + WAIT_SECONDS))
while [ -z "${result}" ]; do
  # "<succeeded>/<failed>", each empty until it is 1.
  case "$("${K[@]}" -n e2e get job "${JOB}" -o 'jsonpath={.status.succeeded}/{.status.failed}')" in
    1/*) result=0 ;;
    */1) result=1 ;;
    *) [ "${SECONDS}" -lt "${deadline}" ] || result=124 ;;
  esac
  [ -n "${result}" ] || sleep 2
done

echo "==> Job logs"
"${K[@]}" -n e2e logs "job/${JOB}" ||
  "${K[@]}" -n e2e describe pods -l "job-name=${JOB}" >&2 || true
case "${result}" in
  0) echo "e2e ${tier}: passed in ${SECONDS}s" ;;
  124) echo "e2e ${tier}: timed out after ${WAIT_SECONDS}s" >&2 ;;
  *) echo "e2e ${tier}: FAILED" >&2 ;;
esac
exit "${result}"

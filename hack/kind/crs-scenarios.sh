#!/usr/bin/env bash
# The scenarios of the custom resources route that need kubectl or Helm, which the E2E Job (a distroless image with no tools) does not have
# (make e2e-crs-scenarios). The fast tier must be up on the custom resources route: E2E_CONFIG_SOURCE=crds make e2e-fast-up. Each check prints
# "ok: ..." or "FAIL: ..." (the run goes on and exits 1 at the end), and "measured: ..." for what was seen and is only reported.
# Usage: crs-scenarios.sh [unknown-fields] [kubectl-get] [stored-invalid] [wait]; with none, all of them.
#
#  unknown-fields  Spec decision 3: the API server prunes a field that the schema does not have, so a removed field does not "fail strict decoding" as it
#                  does in a file. What each client does with a Binding that has one: kubectl apply (the default, and server-side), with --validate=warn
#                  and with --validate=false; kubectl create and patch; and Helm, upgrading the release with a Binding of its own values that has one.
#                  Argo CD is not run here: there is none in the tier. What Argo does is left to the Flatcar validation, with the Argo version that
#                  the cluster runs.
#  kubectl-get     What `kubectl get` shows: the short names, the category, the columns READY and REASON, and that `kubectl get bindings` is not it.
#  stored-invalid  A Binding stored before a tightening of the schema: it is made under a CRD with a rule left out, and the rule is put back. A
#                  server-side apply to its status subresource, which is what the status writer does, is accepted on this node (Kubernetes 1.37), with a warning for a CEL
#                  rule and none for a schema pattern, and the leader writes Accepted=False on it.
#  wait            `kubectl wait --for=condition=Ready` on a Binding that was deleted and made again: it returns when the new object is Ready, not
#                  at once on the old one's status; and it times out on one whose backend is dead.
#
# Every kubectl and helm call names --context kind-agent-kourier-fast, as the tier's scripts do; assert_context aborts unless that is the local kind cluster.
# shellcheck disable=SC2015 # "A && pass || fail": pass and fail only print, so C running after a true A is harmless
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "${HERE}/lib.sh"
use_tier fast
CTX=kind-${CLUSTER}
K=(kubectl --context "${CTX}")
H=(helm --kube-context "${CTX}")
FAILED=0

for tool in kubectl helm jq; do
  command -v "${tool}" >/dev/null || die "${tool} is required"
done
cluster_exists || die "kind cluster ${CLUSTER} does not exist; bring it up with: E2E_CONFIG_SOURCE=crds make e2e-fast-up"
assert_owned
assert_context fast
require_helm4

pass() { echo "ok: $1"; }
fail() {
  echo "FAIL: $1" >&2
  FAILED=1
}
note() { echo "measured: $1"; }
skip() { echo "skip: $1"; }
# check DESCRIPTION COMMAND...: pass or fail by the command's status.
check() {
  local d=$1
  shift
  if "$@"; then pass "${d}"; else fail "${d}"; fi
}
contains() { case $1 in *"$2"*) return 0 ;; *) return 1 ;; esac; } # TEXT PART

[ -n "$("${K[@]}" -n agent-kourier get deploy/agent-kourier -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="AGENTKOURIER_CONFIG_SOURCE")].value}')" ] ||
  die "the tier is on the ConfigMap route: bring it up on custom resources with E2E_CONFIG_SOURCE=crds make e2e-fast-up"

WORK=$(mktemp -d)
RESTORE_PATH=
RESTORE_VALUE=
cleanup() {
  local rc=$?
  "${K[@]}" -n agent-kourier delete akbinding ck11-unknown ck11-stored-invalid --ignore-not-found >/dev/null 2>&1 || true
  # A rule left out of the CRD by a case that did not get to put it back goes back now; a restore that fails is said, and fails the run.
  if [ -n "${RESTORE_PATH:-}" ]; then
    "${K[@]}" patch crd bindings.agentkourier.dev --type json -p "[{\"op\":\"add\",\"path\":\"${RESTORE_PATH}\",\"value\":${RESTORE_VALUE}}]" >/dev/null ||
      {
        echo "FAIL: the CRD bindings.agentkourier.dev could not be put back (the rule at ${RESTORE_PATH} is left out): restore it from charts/agent-kourier/files/crds" >&2
        rc=1
      }
  fi
  rm -rf "${WORK}"
  exit "${rc}"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# binding NAME CHANNEL [EXTRA [IDENTITY]]: a Binding of the tier's namespace that the tier's own backend, token and connection serve, as YAML. EXTRA is
# a line of the chat section, with its indentation; IDENTITY is the identity as flow YAML, the tier's token by default.
binding() {
  local identity=${4:-'{userId: agent-kourier-fast, tokenSecretRef: {name: agent-kourier-agent-token}}'}
  cat <<EOF
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: {name: $1, namespace: agent-kourier}
spec:
  agent:
    backendRef: {name: agents}
    namespace: kagent
    name: test-agent
  identity: ${identity}
  chat:
    connectionRef: {name: slack-fast}
    channel: $2
    output: live
${3:-}
EOF
}

# stored FIELD: the value at spec.chat.FIELD of the stored ck11-unknown, or "absent".
stored() { "${K[@]}" -n agent-kourier get akbinding ck11-unknown -o json 2>/dev/null | jq -r --arg f "$1" '.spec.chat[$f] // "absent"'; }

unknown_fields() {
  echo "== unknown fields: what each client does with spec.chat.bogusField"
  local file=${WORK}/unknown.yaml out
  binding ck11-unknown CCRUNKNOWN '    bogusField: x' >"${file}"
  "${K[@]}" -n agent-kourier delete akbinding ck11-unknown --ignore-not-found >/dev/null

  # kubectl asks the API server to refuse an unknown field (--validate=strict is its default), for apply, create and server-side apply alike (the
  # last in other words: the server's own, "field not declared in schema").
  for args in "apply" "apply --validate=strict" "apply --server-side" "create"; do
    # shellcheck disable=SC2086 # the words of ARGS are the flags
    if out=$("${K[@]}" ${args} -f "${file}" 2>&1); then
      fail "kubectl ${args}: accepted a Binding with an unknown field (${out})"
      "${K[@]}" -n agent-kourier delete akbinding ck11-unknown --ignore-not-found >/dev/null
    elif contains "${out}" 'bogusField'; then
      pass "kubectl ${args}: refused, naming the field (${out//$'\n'/ | })"
    else
      fail "kubectl ${args}: refused, but not for the unknown field: ${out}"
    fi
  done
  check "none of those left the Binding behind" test -z "$("${K[@]}" -n agent-kourier get akbinding ck11-unknown --ignore-not-found -o name)"

  # With the validation turned down the field is pruned: stored without it. warn says so; false (ignore) says nothing.
  for mode in warn false; do
    out=$("${K[@]}" apply --validate=${mode} -f "${file}" 2>&1) || {
      fail "kubectl apply --validate=${mode}: refused (${out})"
      continue
    }
    if [ "$(stored bogusField)" = absent ]; then
      pass "kubectl apply --validate=${mode}: accepted, and the field is not in the stored object (pruned)"
    else
      fail "kubectl apply --validate=${mode}: the stored Binding has bogusField"
    fi
    if contains "${out}" 'unknown field'; then
      note "kubectl apply --validate=${mode}: warns (${out//$'\n'/ | })"
    else
      note "kubectl apply --validate=${mode}: no word of it (output: ${out:-none})"
    fi
    "${K[@]}" -n agent-kourier delete akbinding ck11-unknown >/dev/null
  done

  # kubectl patch and an update by a client with no strict mode: whatever the server's default is, as the broker's own client sees it.
  "${K[@]}" apply -f <(binding ck11-unknown CCRUNKNOWN) >/dev/null
  if out=$("${K[@]}" -n agent-kourier patch akbinding ck11-unknown --type merge -p '{"spec":{"chat":{"bogusField":"y"}}}' 2>&1); then
    note "kubectl patch --type merge with an unknown field: accepted (${out}); stored: $(stored bogusField)"
  else
    note "kubectl patch --type merge with an unknown field: refused (${out})"
  fi
  [ "$(stored bogusField)" = absent ] && pass "kubectl patch: the stored Binding has no bogusField" || fail "kubectl patch: the stored Binding has bogusField"
  out=$("${K[@]}" -n agent-kourier get akbinding ck11-unknown -o json | jq '.spec.chat.bogusField="z"' | "${K[@]}" replace -f - 2>&1) &&
    note "kubectl replace with an unknown field: accepted (${out}); stored: $(stored bogusField)" ||
    note "kubectl replace with an unknown field: refused (${out})"
  "${K[@]}" -n agent-kourier delete akbinding ck11-unknown >/dev/null

  # Helm: a chart-rendered Binding with an unknown field, in the values of the release (config.bindings), as a team that keeps its Bindings in
  # the chart's values would write it. The release is the tier's, upgraded with its own values and one more Binding, and put back after.
  local values original=${WORK}/helm-values.json
  "${H[@]}" get values agent-kourier --namespace agent-kourier -o json >"${original}" # the values the release has now, to put back exactly (a map merges, and null leaves a mark)
  values=$(jq -nc '{"ck11-unknown": {spec: {agent: {backendRef: {name: "agents"}, namespace: "kagent", name: "test-agent"}, identity: {userId: "ck11", tokenSecretRef: {name: "agent-kourier-agent-token"}}, chat: {connectionRef: {name: "slack-fast"}, channel: "CCRUNKNOWN", output: "live", bogusField: "x"}}}}')
  if out=$("${H[@]}" upgrade agent-kourier "${ROOT}/charts/agent-kourier" --namespace agent-kourier --reuse-values --force-conflicts --set-json "config.bindings=${values}" 2>&1); then
    if [ "$(stored bogusField)" = absent ] && [ -n "$("${K[@]}" -n agent-kourier get akbinding ck11-unknown --ignore-not-found -o name)" ]; then
      note "helm upgrade: accepted a rendered Binding with an unknown field, and stored it without the field (pruned); output: ${out//$'\n'/ | }"
      pass "helm upgrade: the stored Binding has no bogusField"
    else
      fail "helm upgrade: accepted, but the stored Binding is missing or has bogusField"
    fi
  else
    note "helm upgrade: refused a rendered Binding with an unknown field (Helm 4's default, server-side apply; not measured with another way to apply): ${out//$'\n'/ | }"
    if [ -z "$("${K[@]}" -n agent-kourier get akbinding ck11-unknown --ignore-not-found -o name)" ]; then
      pass "helm upgrade: refused, and no Binding ck11-unknown is in the cluster"
    else
      fail "helm upgrade: refused, but a Binding ck11-unknown is in the cluster"
    fi
  fi
  "${H[@]}" upgrade agent-kourier "${ROOT}/charts/agent-kourier" --namespace agent-kourier --reset-values -f "${original}" --force-conflicts >/dev/null ||
    fail "helm: the release could not be put back to the values it had"
  check "helm: the rendered Binding is gone after the values go back" test -z "$("${K[@]}" -n agent-kourier get akbinding ck11-unknown --ignore-not-found -o name)"
  skip "Argo CD: not run here, there is none in the tier; what Argo does with an unknown field (it applies with kubectl's own validation or server-side, by its sync options) is left to the Flatcar validation"
}

kubectl_get() {
  echo "== kubectl get: names, category, columns"
  local out
  out=$("${K[@]}" get akbinding,akbackend,akconnection -A 2>&1) || fail "kubectl get akbinding,akbackend,akconnection -A: ${out}"
  contains "${out}" "binding.agentkourier.dev/fast-chat" && pass "kubectl get akbinding,akbackend,akconnection -A lists the tier's resources" || fail "the tier's fast-chat Binding is not listed:
${out}"
  out=$("${K[@]}" get agentkourier -A 2>&1) && contains "${out}" "chatconnection.agentkourier.dev/slack-fast" && pass "kubectl get agentkourier -A (the category) lists all three kinds" || fail "kubectl get agentkourier -A: ${out}"
  out=$("${K[@]}" -n agent-kourier get akbinding fast-chat 2>&1)
  echo "${out}"
  for col in READY REASON CHANNEL AGE; do
    contains "${out%%$'\n'*}" "${col}" && pass "kubectl get akbinding has the column ${col}" || fail "kubectl get akbinding has no column ${col}"
  done
  if out=$("${K[@]}" get bindings -A 2>&1); then fail "kubectl get bindings -A answered: that is the core resource, which must not be what the docs say to type"; else pass "kubectl get bindings -A fails (the core resource of that name): ${out##*$'\n'}"; fi
  local ready
  ready=$("${K[@]}" -n agent-kourier get akbinding fast-chat -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}')
  check "fast-chat has a Ready condition (status written by the leader)" test -n "${ready}"
  check "kubectl get akbinding shows a value in READY, not an empty column" test -n "$("${K[@]}" -n agent-kourier get akbinding fast-chat --no-headers | awk '{print $2}' | grep -E '^(True|False|Unknown)$' || true)"
}

# stored_case TITLE SCHEMA_PATH BINDING_YAML EXPECT_WARNING: a Binding is stored under a CRD that has the rule at SCHEMA_PATH (a JSON pointer into the CRD) left
# out, the rule is put back, and the Binding, now invalid under the schema that is served, gets a server-side apply to its status, which is what the status
# writer does. The apply is accepted on a node that ratchets (Kubernetes 1.33 and later, by the chart's measurements on 1.33.1, 1.35.0 and 1.37.0; refused on
# 1.30.13, 1.31.0 and 1.32.x). EXPECT_WARNING says whether this kind of rule is held to answer a Warning (1) or whether what is seen is only reported (0).
stored_case() {
  local title=$1 path=$2 yaml=$3 expect=$4 crd=bindings.agentkourier.dev saved out node
  saved=$("${K[@]}" get crd ${crd} -o json | jq -c --arg p "${path}" 'getpath($p | ltrimstr("/") | split("/") | map(tonumber? // .))')
  [ -n "${saved}" ] && [ "${saved}" != null ] || die "the CRD has nothing at ${path} to leave out"
  RESTORE_PATH=${path} RESTORE_VALUE=${saved} # cleanup puts it back if this case dies before it does
  "${K[@]}" patch crd ${crd} --type json -p "[{\"op\":\"remove\",\"path\":\"${path}\"}]" >/dev/null
  # The CRD handler learns of the new schema within a second or so: retry the create until it takes.
  local made=
  for _ in $(seq 20); do
    if "${K[@]}" apply -f - >/dev/null 2>&1 <<<"${yaml}"; then
      made=1
      break
    fi
    sleep 1
  done
  [ -n "${made}" ] || die "${title}: the Binding was never accepted under the looser schema"
  pass "${title}: the Binding was stored under a schema without the rule"
  "${K[@]}" patch crd ${crd} --type json -p "[{\"op\":\"add\",\"path\":\"${path}\",\"value\":${saved}}]" >/dev/null || die "${title}: the rule at ${path} could not be put back"
  RESTORE_PATH=''
  RESTORE_VALUE=''
  local refused=
  for _ in $(seq 20); do
    if ! "${K[@]}" apply --dry-run=server -f - >/dev/null 2>&1 <<<"${yaml//ck11-stored-invalid/ck11-stored-invalid-new}"; then
      refused=1
      break
    fi
    sleep 1
  done
  [ -n "${refused}" ] && pass "${title}: with the rule back, the apiserver refuses the same Binding when it is new" || fail "${title}: the apiserver still accepts it after the rule was put back"
  check "${title}: the stored Binding is still there, invalid under the schema served now" test -n "$("${K[@]}" -n agent-kourier get akbinding ck11-stored-invalid -o name)"

  node=$("${K[@]}" get nodes -o 'jsonpath={.items[0].status.nodeInfo.kubeletVersion}')
  cat >"${WORK}/status.yaml" <<EOF
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: {name: ck11-stored-invalid, namespace: agent-kourier}
status:
  conditions:
    - {type: Accepted, status: "False", reason: InvalidSpec, message: "ck11 probe", lastTransitionTime: "2026-01-01T00:00:00Z", observedGeneration: 1}
EOF
  if out=$("${K[@]}" apply --server-side --subresource=status --field-manager=ck11-probe --force-conflicts -f "${WORK}/status.yaml" 2>&1); then
    pass "${title}: a server-side apply to the status of the stored-invalid Binding is accepted on ${node}"
    if contains "${out}" "Warning"; then
      note "${title}: with a warning: ${out//$'\n'/ | }"
    elif [ "${expect}" = 1 ]; then
      fail "${title}: accepted without the warning that the chart's README reports on 1.33 and later; output: ${out}"
    else
      note "${title}: no warning on ${node}; output: ${out}"
    fi
  else
    fail "${title}: a server-side apply to the status of the stored-invalid Binding was refused on ${node}: ${out}"
  fi
  local reason=
  for _ in $(seq 90); do
    reason=$("${K[@]}" -n agent-kourier get akbinding ck11-stored-invalid -o 'jsonpath={.status.conditions[?(@.type=="Accepted")].reason}' 2>/dev/null || true)
    [ "${reason}" = InvalidSpec ] && ! contains "$("${K[@]}" -n agent-kourier get akbinding ck11-stored-invalid -o 'jsonpath={.status.conditions[?(@.type=="Accepted")].message}')" "ck11 probe" && break
    sleep 2
  done
  [ "${reason}" = InvalidSpec ] && pass "${title}: the leader wrote Accepted=False/InvalidSpec on it" || fail "${title}: the leader wrote no Accepted=False/InvalidSpec on it (reason: ${reason:-none})"
  "${K[@]}" -n agent-kourier get akbinding ck11-stored-invalid -o 'jsonpath={range .status.conditions[*]}{.type}={.status}/{.reason}: {.message}{"\n"}{end}' | cut -c1-300 || true
  "${K[@]}" -n agent-kourier delete akbinding ck11-stored-invalid --ignore-not-found >/dev/null
}

stored_invalid() {
  echo "== a Binding stored before a tightening of the schema"
  local base=/spec/versions/0/schema/openAPIV3Schema/properties/spec/properties
  # A rule of the CRD's CEL (a Binding with no identity at all) and one of its schema (a channel that is not a channel ID).
  stored_case "CEL rule" "${base}/identity/x-kubernetes-validations" "$(binding ck11-stored-invalid CCRSTORED '' '{}')" 1
  stored_case "schema pattern" "${base}/chat/properties/channel/pattern" "$(binding ck11-stored-invalid not-a-channel)" 0
}


wait_ready() {
  echo "== kubectl wait on a Binding that was made again"
  # fast-verify-ready is the tier's Binding that can be served: the bot is in its channel (the verification scenario puts it there; this does too).
  local name=fast-verify-ready saved=${WORK}/saved.yaml pod=crs-members-${RANDOM}
  "${K[@]}" -n agent-kourier get akbinding "${name}" -o json | jq 'del(.status, .metadata.uid, .metadata.resourceVersion, .metadata.generation, .metadata.creationTimestamp, .metadata.managedFields)' >"${saved}"
  "${K[@]}" -n e2e run "${pod}" --rm -i --quiet --restart=Never --image="${CURL_IMAGE}" --command -- \
    curl -sf -m 10 -X POST -H 'Content-Type: application/json' -d '{"channel":"CVFYREADY","users":["UFAKEBOT"]}' http://fakeslack.e2e.svc.cluster.local:8080/_control/members >/dev/null
  "${K[@]}" -n agent-kourier wait --for=condition=Ready "akbinding/${name}" --timeout=240s >/dev/null || fail "${name} was never Ready before it was deleted"
  local old_uid new_uid started=${SECONDS}
  old_uid=$("${K[@]}" -n agent-kourier get akbinding "${name}" -o 'jsonpath={.metadata.uid}')
  "${K[@]}" -n agent-kourier delete akbinding "${name}" >/dev/null
  "${K[@]}" apply -f "${saved}" >/dev/null
  new_uid=$("${K[@]}" -n agent-kourier get akbinding "${name}" -o 'jsonpath={.metadata.uid}')
  [ "${old_uid}" != "${new_uid}" ] && pass "the Binding is a new object (uid changed)" || fail "the Binding kept its uid"
  # Right after the create there is no status, so a wait that returned at once would have read something that is not this object's.
  [ -z "$("${K[@]}" -n agent-kourier get akbinding "${name}" -o 'jsonpath={.status.conditions}')" ] && pass "the new object has no status at first" || note "the new object already had a status when read"
  if "${K[@]}" -n agent-kourier wait --for=condition=Ready "akbinding/${name}" --timeout=240s >/dev/null; then
    pass "kubectl wait --for=condition=Ready returned $((SECONDS - started))s after the delete, for the new object"
  else
    fail "kubectl wait --for=condition=Ready on the new object timed out"
  fi
  "${K[@]}" -n agent-kourier get akbinding "${name}"

  # A Binding whose backend is dead (fast-verify-no-agent: the stub has no such agent) never becomes Ready, so the wait must time out, not return.
  local dead=fast-verify-no-agent reason
  "${K[@]}" -n agent-kourier get akbinding "${dead}" -o json | jq 'del(.status, .metadata.uid, .metadata.resourceVersion, .metadata.generation, .metadata.creationTimestamp, .metadata.managedFields)' >"${WORK}/dead.yaml"
  "${K[@]}" -n agent-kourier delete akbinding "${dead}" >/dev/null
  "${K[@]}" apply -f "${WORK}/dead.yaml" >/dev/null
  if "${K[@]}" -n agent-kourier wait --for=condition=Ready "akbinding/${dead}" --timeout=90s >/dev/null 2>&1; then
    fail "kubectl wait --for=condition=Ready returned for ${dead}, whose agent the backend does not have"
  else
    pass "kubectl wait --for=condition=Ready on ${dead} times out after 90s, as it must for a dead backend"
  fi
  reason=$("${K[@]}" -n agent-kourier get akbinding "${dead}" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}')
  note "${dead} after the wait: Ready=${reason}"
  case "${reason}" in False/*) pass "${dead} says Ready=False, with its reason" ;; *) fail "${dead} says Ready=${reason}, want False" ;; esac
}

[ $# -gt 0 ] || set -- unknown-fields kubectl-get stored-invalid wait
for scenario in "$@"; do
  case "${scenario}" in
    unknown-fields) unknown_fields ;;
    kubectl-get) kubectl_get ;;
    stored-invalid) stored_invalid ;;
    wait) wait_ready ;;
    *) die "unknown scenario '${scenario}' (want unknown-fields, kubectl-get, stored-invalid or wait)" ;;
  esac
done
[ "${FAILED}" = 0 ] || exit 1
echo "all checks passed"

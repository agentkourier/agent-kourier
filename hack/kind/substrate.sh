# shellcheck shell=bash
# shellcheck disable=SC2153 # ROOT comes from the caller; root below is a CA certificate.
# Pinned Substrate bootstrap shared by kagent and AX. Needs lib.sh, CTX, K, H.
SUBSTRATE_VERSION=0.3.0-alpha1
SUBSTRATE_OCI=oci://ghcr.io/kagent-dev/substrate/helm
# kubectl-ate builds the Substrate CA and JWT pools. Fetched once, checksum-verified.
fetch_kubectl_ate() {
  local os arch sha bin
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)
  case "${arch}" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
  case "${os}-${arch}" in
    darwin-arm64) sha=398ee3cc884f278dbdd6bc8429c3249069ac71991083ea93c69c284a471bca79 ;;
    darwin-amd64) sha=1c99949ae2865ff65486a9475c99c95afc9ddadeb59e02950010fbf963cb8dbe ;;
    linux-arm64) sha=938d6804329439a4300c038bceea6f5e1142ce585ca3de96797dd7395ef4ef12 ;;
    linux-amd64) sha=2c074a8b63222fbd613c614bc75cf17798cdac818c2fba1aff9df32a500b3fbc ;;
    *) die "no kubectl-ate build for ${os}-${arch}" ;;
  esac
  bin=${ROOT}/bin/kubectl-ate-${SUBSTRATE_VERSION}-${os}-${arch}
  if [ ! -x "${bin}" ]; then
    mkdir -p "${ROOT}/bin"
    curl -fsSL -o "${bin}" "https://github.com/kagent-dev/substrate/releases/download/v${SUBSTRATE_VERSION}/kubectl-ate-${os}-${arch}"
    [ "$(openssl dgst -sha256 -r "${bin}" | cut -d' ' -f1)" = "${sha}" ] || { rm -f "${bin}"; die "kubectl-ate checksum mismatch"; }
    chmod +x "${bin}"
  fi
  echo "${bin}"
}

install_substrate() {
echo "==> Agent Substrate ${SUBSTRATE_VERSION}"
ATE=$(fetch_kubectl_ate)
SUBSTRATE_ARGS=(--namespace ate-system --version "${SUBSTRATE_VERSION}"
  --set-string 'ateApi.extraArgs[0]=--template-resync-interval=250ms'
  --set "credentialProvider.namespacePolicies[0].atespace=${SUBSTRATE_ATESPACE:-kagent}"
  --set "credentialProvider.namespacePolicies[0].allowedNamespaces[0]=${SUBSTRATE_ATESPACE:-kagent}")
if [ -n "${SUBSTRATE_REGISTRY:-}" ]; then
  SUBSTRATE_ARGS+=(--set-string "atelet.extraArgs[0]=--localhost-registry-replacement=${SUBSTRATE_REGISTRY}")
fi
"${H[@]}" upgrade --install substrate-crds "${SUBSTRATE_OCI}/substrate-crds" --namespace ate-system --create-namespace --version "${SUBSTRATE_VERSION}" >/dev/null
# Installed twice on purpose, as in kagent's CI. Substrate pods mount the pool Secrets below,
# so they sit in ContainerCreating until those exist and the first call cannot wait. The
# second call re-applies the same values and waits.
"${H[@]}" upgrade --install substrate "${SUBSTRATE_OCI}/substrate" "${SUBSTRATE_ARGS[@]}" >/dev/null
# Generated material is created only when absent, so a re-run never rotates keys.
ensure_pool() { # kind namespace name extra-args...
  "${K[@]}" -n "$2" get secret "$3" >/dev/null 2>&1 ||
    "${ATE}" --context "${CTX}" admin "make-$1-pool" --name="$3" --secret-namespace="$2" "${@:4}"
}
ensure_pool ca podcertificate-controller-system service-dns-ca-pool --ca-id=1
ensure_pool ca podcertificate-controller-system pod-identity-ca-pool --ca-id=1
ensure_pool jwt ate-system actor-id-jwt-pool --key-id=1
ensure_pool ca ate-system actor-id-ca-pool --ca-id=1
ensure_pool ca ate-system egress-mitm-ca-pool --ca-id=1 --key-type=ECDSAP256
if ! "${K[@]}" -n ate-system get secret actor-id-ca-certs >/dev/null 2>&1; then
  root=$("${K[@]}" -n ate-system get secret actor-id-ca-pool -o jsonpath='{.data.pool}' | base64 --decode |
    jq -r '.CAs[0].RootCertificateDER' | base64 --decode | openssl x509 -inform der -outform pem)
  "${K[@]}" -n ate-system create secret generic actor-id-ca-certs --from-literal=ca.crt="${root}"
fi
"${K[@]}" apply -f "${SUBSTRATE_AUTH_CONFIG:-${HERE}/full/ate-api-authentication.yaml}" >/dev/null
"${H[@]}" upgrade --install substrate "${SUBSTRATE_OCI}/substrate" "${SUBSTRATE_ARGS[@]}" --wait --timeout 8m >/dev/null

}

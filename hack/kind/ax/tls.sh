#!/usr/bin/env bash
# Install a private test root in the egress gateway; both TLS hops verify.
set -euo pipefail
K=(kubectl --context "$1")
scratch=$2
if [ -z "$("${K[@]}" -n agent-kourier get secret ax-results-tls --ignore-not-found -o name)" ]; then
  umask 077
  host=ax-results.agent-kourier.svc.cluster.local
  openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 30 -subj '/CN=Local AX fixture CA' \
    -keyout "${scratch}/ca.key" -out "${scratch}/ca.pem" 2>/dev/null
  openssl req -newkey rsa:2048 -nodes -sha256 -subj "/CN=${host}" -keyout "${scratch}/tls.key" \
    -out "${scratch}/tls.csr" 2>/dev/null
  printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth\n' "${host}" >"${scratch}/tls.ext"
  openssl x509 -req -in "${scratch}/tls.csr" -CA "${scratch}/ca.pem" -CAkey "${scratch}/ca.key" \
    -CAcreateserial -days 30 -sha256 -extfile "${scratch}/tls.ext" -out "${scratch}/tls.pem" 2>/dev/null
  "${K[@]}" -n agent-kourier create secret tls ax-results-tls --cert="${scratch}/tls.pem" --key="${scratch}/tls.key" >/dev/null
  "${K[@]}" -n agent-kourier create configmap ax-results-test-root --from-file="ca.pem=${scratch}/ca.pem" >/dev/null
else
  "${K[@]}" -n agent-kourier get cm ax-results-test-root -o 'jsonpath={.data.ca\.pem}' > "${scratch}/ca.pem"
fi
"${K[@]}" -n ate-system get configmap atenet-egress -o json |
  jq -e --rawfile ca "${scratch}/ca.pem" '
    .data["envoy.yaml"] as $envoy |
    select(($envoy | contains("auto_san_validation: true")) and ($envoy | test("filename: (/etc/ssl/certs/ca-certificates[.]crt|/etc/envoy/(proof-ca|ax-results-test-root)[.]pem)"))) |
    {"data":{"envoy.yaml": ($envoy | gsub("filename: (/etc/ssl/certs/ca-certificates[.]crt|/etc/envoy/(proof-ca|ax-results-test-root)[.]pem)"; "filename: /etc/envoy/ax-results-test-root.pem")),
      "ax-results-test-root.pem":$ca}}' >"${scratch}/egress-tls.json"
"${K[@]}" -n ate-system patch cm atenet-egress --type=merge --patch-file="${scratch}/egress-tls.json" >/dev/null
"${K[@]}" -n ate-system rollout restart deploy/atenet-egress >/dev/null
"${K[@]}" -n ate-system rollout status deploy/atenet-egress --timeout=120s

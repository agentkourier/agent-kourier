#!/usr/bin/env bash
# Runs the WP-02 spike against the stub agent (test/stubagent), the acceptance check for the
# stub. Twice: with an unsigned JWT carrying an email claim, when every case must pass and the
# identity case must run, and with -no-auth, when it must pass with the identity case skipped
# (and reported as skipped). The spike's output goes to scratch directories, never to
# test/fixtures/kagent. The stub is always stopped.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

addr="${STUB_ADDR:-127.0.0.1:18183}"
work="$(mktemp -d)"
stub_pid=""
cleanup() {
  if [ -n "${stub_pid}" ]; then
    kill "${stub_pid}" 2>/dev/null || true
    wait "${stub_pid}" 2>/dev/null || true
  fi
  rm -rf "${work}"
}
trap cleanup EXIT

go build -o "${work}/stubagent" ./test/stubagent/cmd/stubagent
go build -o "${work}/spike" ./spike/a2a

# Something already answering on the port is a leftover stub, or another server: the run would
# test that, not the stub built above.
if curl -s -o /dev/null "http://${addr}/"; then
  echo "stub-oracle: ${addr} already answers; stop what is running there, or set STUB_ADDR" >&2
  exit 1
fi

"${work}/stubagent" -addr "${addr}" >"${work}/stub.log" 2>&1 &
stub_pid=$!
# Ready is the stub's own "listening" line, printed once the port is bound, with the process
# still alive: a stub that could not bind exits, and the port's owner is not taken for it.
for _ in $(seq 100); do
  kill -0 "${stub_pid}" 2>/dev/null || { echo "stub-oracle: the stub exited:" >&2; cat "${work}/stub.log" >&2; exit 1; }
  grep -q '^stubagent: listening on ' "${work}/stub.log" && break
  sleep 0.1
done
grep -q '^stubagent: listening on ' "${work}/stub.log" && kill -0 "${stub_pid}" 2>/dev/null \
  || { echo "stub-oracle: the stub did not report that it is listening on ${addr}" >&2; exit 1; }

b64url() { base64 | tr '+/' '-_' | tr -d '=\n'; }
token="$(printf '{"alg":"none"}' | b64url).$(printf '{"email":"binding-test@example.test","sub":"stub-sub"}' | b64url)."

# checks prints the number of checks in the ok lines of a spike run.
checks() { awk '/^ok   / { n += $3 } END { print n + 0 }' "$1"; }

echo "== spike with a token"
"${work}/spike" -base "http://${addr}" -token "${token}" -out "$(mktemp -d "${work}/out.XXXXXX")" | tee "${work}/token.log"
grep -q '^ok   identity: ' "${work}/token.log" || { echo "stub-oracle: the identity case did not run and pass" >&2; exit 1; }
if grep -q '^SKIP\|^FAIL' "${work}/token.log"; then echo "stub-oracle: a case was skipped or failed" >&2; exit 1; fi

echo "== spike with -no-auth"
"${work}/spike" -base "http://${addr}" -no-auth -out "$(mktemp -d "${work}/out.XXXXXX")" | tee "${work}/noauth.log"
grep -q '^SKIP identity: ' "${work}/noauth.log" || { echo "stub-oracle: -no-auth did not report the identity case as skipped" >&2; exit 1; }
if grep -q '^FAIL' "${work}/noauth.log"; then echo "stub-oracle: a case failed" >&2; exit 1; fi

echo "stub-oracle: OK, $(checks "${work}/token.log") checks with a token (identity ran), $(checks "${work}/noauth.log") with -no-auth (identity skipped)"

#!/usr/bin/env bash
# Run the Postgres store's tests, the store conformance suite among them, and the broker's tests that Run serves on
# Postgres and fences its leader's writes, against a throwaway Postgres container (make test-postgres). Needs Docker. Extra arguments go to go test of
# the store's package, e.g. -run TestConformance/Audit. The container, and how its password is kept, are lib.sh's.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$here/lib.sh"

start_postgres
cd "$root"
# The broker's tests: Run reads the DSN from a Secret and serves on the store it opens, and two replicas elect a leader and fence its writes.
run_child go test -race -count=1 -timeout 5m ./internal/broker/ -run 'TestRun(ServesOn|On)Postgres'
run_child go test -race -count=1 -timeout 20m ./internal/store/postgres/ "$@"

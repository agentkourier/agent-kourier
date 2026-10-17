#!/usr/bin/env bash
# Run the Postgres store's tests, the store conformance suite among them, and the broker's tests that Run serves on
# Postgres and fences its leader's writes, against a throwaway Postgres container (make test-postgres). Needs Docker. Extra arguments go to go test of
# the store's package, e.g. -run TestConformance/Audit.
#
# The container listens on a random port of the loopback interface only. Its password is made here, for this
# run, and is never written anywhere that outlives the run: it goes to the container in a mode-0600 env
# file (not on a command line, where ps shows it), to the tests in the environment, and the file and the
# container are removed on every way out.
set -euo pipefail

# postgres:18 on Debian, by digest of its multi-arch index. To move to another major or a newer build:
# docker pull postgres:<major>, then docker image inspect it for the digest.
POSTGRES_IMAGE="postgres:18@sha256:74935e72241653ca55e0414067e6d8763aceb8a810eb51b452253ec3dcfc4336"

root=$(cd "$(dirname "$0")/../.." && pwd)
name="agent-kourier-pg-test-$$-$RANDOM"
envfile=$(mktemp)

cleanup() {
	docker rm -f -v "$name" >/dev/null 2>&1 || true
	rm -f "$envfile"
}
child=""
trap cleanup EXIT
# The tests run in the background and the script waits for them, so that a signal that reaches only the
# script (a kill, a CI job's cancel) is handled at once and not after the tests finish: bash runs a trap
# when the foreground command ends.
trap 'if [ -n "$child" ]; then kill "$child" 2>/dev/null || true; fi; exit 130' INT
trap 'if [ -n "$child" ]; then kill "$child" 2>/dev/null || true; fi; exit 143' TERM

if command -v openssl >/dev/null 2>&1; then
	password=$(openssl rand -hex 24)
else
	password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
fi
chmod 600 "$envfile"
printf 'POSTGRES_USER=agentkourier\nPOSTGRES_DB=agentkourier\nPOSTGRES_PASSWORD=%s\n' "$password" >"$envfile"

# The throwaway server trades durability for speed: nothing in it outlives the run. The data directory
# is a tmpfs, so no volume is left behind either.
docker run -d --name "$name" --label agent-kourier.test=postgres \
	--env-file "$envfile" \
	-p 127.0.0.1::5432 \
	--tmpfs /var/lib/postgresql:rw,size=1g \
	"$POSTGRES_IMAGE" \
	-c fsync=off -c synchronous_commit=off -c full_page_writes=off >/dev/null
rm -f "$envfile"

hostport=$(docker port "$name" 5432/tcp | head -n 1)
port=${hostport##*:}

# While the image initialises it runs a server that listens on its socket only, so a TCP check passes
# only for the server the tests use.
ready=0
for _ in $(seq 1 120); do
	if [ "$(docker inspect -f '{{.State.Running}}' "$name")" != true ]; then
		echo "the postgres container stopped:" >&2
		docker logs "$name" 2>&1 | tail -n 20 >&2
		exit 1
	fi
	if docker exec "$name" pg_isready -q -h 127.0.0.1 -U agentkourier -d agentkourier; then
		ready=1
		break
	fi
	sleep 0.5
done
if [ "$ready" != 1 ]; then
	echo "postgres was not ready after 60 s" >&2
	exit 1
fi

export AGENTKOURIER_POSTGRES_DSN="postgres://agentkourier:${password}@127.0.0.1:${port}/agentkourier?sslmode=disable"
cd "$root"
# The broker's tests: Run reads the DSN from a Secret and serves on the store it opens, and two replicas elect a leader and fence its writes.
go test -race -count=1 -timeout 5m ./internal/broker/ -run 'TestRun(ServesOn|On)Postgres' &
child=$!
wait "$child"
go test -race -count=1 -timeout 20m ./internal/store/postgres/ "$@" &
child=$!
wait "$child"

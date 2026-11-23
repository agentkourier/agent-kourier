# shellcheck shell=bash
# Shared by test.sh and run.sh (sourced, not executed): a throwaway Postgres container for tests, and the way to run
# a command against it. Needs Docker.
#
# The container listens on a random port of the loopback interface only. Its password is made here, for this
# run, and is never written anywhere that outlives the run: it goes to the container in a mode-0600 env
# file (not on a command line, where ps shows it), to the tests in the environment, and the file and the
# container are removed on every way out.

# postgres:19 on Debian, by digest of its multi-arch index. To move to another major or a newer build:
# docker pull postgres:<major>, then docker image inspect it for the digest.
POSTGRES_IMAGE="postgres:19@sha256:bb6e70add742cd868fb62cb3b2f67d1dd2bdd2b65a521e778420748753687c60"

pg_name="agent-kourier-pg-test-$$-$RANDOM"
pg_envfile=""
pg_child=""

pg_cleanup() {
	docker rm -f -v "$pg_name" >/dev/null 2>&1 || true
	if [ -n "$pg_envfile" ]; then rm -f "$pg_envfile"; fi
}

# start_postgres: start the container, wait until it accepts connections over TCP, and export
# AGENTKOURIER_POSTGRES_DSN, the variable every Postgres test of the repository reads. It sets the traps that remove
# the container on every way out, and that stop the command run_child started on INT or TERM.
start_postgres() {
	pg_envfile=$(mktemp)
	trap pg_cleanup EXIT
	# The command runs in the background and the script waits for it, so that a signal that reaches only the
	# script (a kill, a CI job's cancel) is handled at once and not after it finishes: bash runs a trap
	# when the foreground command ends.
	trap 'if [ -n "$pg_child" ]; then kill "$pg_child" 2>/dev/null || true; fi; exit 130' INT
	trap 'if [ -n "$pg_child" ]; then kill "$pg_child" 2>/dev/null || true; fi; exit 143' TERM

	local password hostport port ready
	if command -v openssl >/dev/null 2>&1; then
		password=$(openssl rand -hex 24)
	else
		password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
	fi
	chmod 600 "$pg_envfile"
	printf 'POSTGRES_USER=agentkourier\nPOSTGRES_DB=agentkourier\nPOSTGRES_PASSWORD=%s\n' "$password" >"$pg_envfile"

	# The throwaway server trades durability for speed: nothing in it outlives the run. The data directory
	# is a tmpfs, so no volume is left behind either.
	docker run -d --name "$pg_name" --label agent-kourier.test=postgres \
		--env-file "$pg_envfile" \
		-p 127.0.0.1::5432 \
		--tmpfs /var/lib/postgresql:rw,size=1g \
		"$POSTGRES_IMAGE" \
		-c fsync=off -c synchronous_commit=off -c full_page_writes=off >/dev/null
	rm -f "$pg_envfile"

	hostport=$(docker port "$pg_name" 5432/tcp | head -n 1)
	port=${hostport##*:}

	# While the image initialises it runs a server that listens on its socket only, so a TCP check passes
	# only for the server the tests use.
	ready=0
	for _ in $(seq 1 120); do
		if [ "$(docker inspect -f '{{.State.Running}}' "$pg_name")" != true ]; then
			echo "the postgres container stopped:" >&2
			docker logs "$pg_name" 2>&1 | tail -n 20 >&2
			exit 1
		fi
		if docker exec "$pg_name" pg_isready -q -h 127.0.0.1 -U agentkourier -d agentkourier; then
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
}

# run_child COMMAND...: run COMMAND in the background and wait for it, so that the INT and TERM traps stop it;
# return its status.
run_child() {
	"$@" &
	pg_child=$!
	wait "$pg_child"
}

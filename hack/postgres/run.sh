#!/usr/bin/env bash
# Run one command, from the repository's root, against a throwaway Postgres container with AGENTKOURIER_POSTGRES_DSN
# set to it: `hack/postgres/run.sh make cma-test` runs the CMA add-on's tests with its Postgres store tests included.
# Needs Docker. The container, and how its password is kept, are lib.sh's; the container is removed when the command
# ends, however it ends, and the script exits with the command's status.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$here/lib.sh"

if [ $# -eq 0 ]; then
	echo "usage: hack/postgres/run.sh COMMAND [ARG...]" >&2
	exit 2
fi

start_postgres
cd "$root"
run_child "$@"

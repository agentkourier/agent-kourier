# AX Task launcher

This module builds the Agent Kourier AX add-on and `agent-kourier-ax-result`, the command wrapper for agent images. See [Connect the AX add-on](../../docs/how-to/connect-ax.md) and the [AX reference](../../docs/reference/ax.md).

Run `make ax-test` and `make ax-lint` from the repository root. Run `hack/postgres/run.sh make ax-test` for the Postgres tests. The [AX kind tier](../../hack/kind/ax/README.md) runs the broker, add-on, AX and Substrate with fake Slack and a deterministic child command.

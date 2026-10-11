# Command line and environment

This page lists the `agent-kourier` binary's environment variables, flags, subcommand and exit codes, as
`cmd/agent-kourier` reads them.

The broker is configured by environment variables, which the Helm chart sets from its values. It takes one flag,
`--version`, and one subcommand, `dry-run`.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `AGENTKOURIER_CONFIG_DIR` | `/etc/agent-kourier` | The directory of resource files (`*.yaml`). |
| `AGENTKOURIER_STORE` | `sqlite` | `sqlite` (one replica) or `postgres`. |
| `AGENTKOURIER_DB_PATH` | `/data/agent-kourier.db` | The SQLite file, on the volume. A database from before the rename is in `courier.db`; point this at it to keep it (see [Upgrade](../how-to/upgrade.md#upgrade-across-the-rename)). |
| `AGENTKOURIER_POSTGRES_DSN_SECRET` | | With `postgres`: where the DSN is, as `namespace/name/key` of a Secret, read through the Secret caches. Its namespace needs the same list and watch Role as the config's. |
| `AGENTKOURIER_POSTGRES_TIMEOUTS` | `agent-kourier` | With `postgres`: `agent-kourier` sets the idle-in-transaction and statement timeouts on each connection; `role` leaves them to the database role, for a pooler that refuses startup parameters. |
| `AGENTKOURIER_LEASE_NAMESPACE` | | With `postgres`, required: the namespace of the leader-election Lease. |
| `AGENTKOURIER_LEASE_NAME` | | With `postgres`, required: the Lease's name, one for the Deployment. |
| `AGENTKOURIER_POD_NAME` | | With `postgres`, required: the pod's name, which names the replica as the holder. |
| `AGENTKOURIER_LEASE_DURATION` | `15s` | With `postgres`: a standby takes over about this long after the leader's last renewal. |
| `AGENTKOURIER_LEASE_RENEW_DEADLINE` | `10s` | With `postgres`: the leader gives up renewing, and exits, after this long. |
| `AGENTKOURIER_LEASE_RETRY_PERIOD` | `2s` | With `postgres`: how often the leader renews and a standby tries to take the Lease. |
| `AGENTKOURIER_LISTEN` | `:8080` | The address of `/healthz`, `/readyz` and `/metrics`. |
| `AGENTKOURIER_SYNC_TIMEOUT` | `30s` | How long to wait for the Secret caches before the start fails. |
| `AGENTKOURIER_KLOG_V` | `0` | client-go's verbosity, held below 8: from 8 up, client-go logs response bodies, Secret lists included. |
| `AGENTKOURIER_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `AGENTKOURIER_AUDIT_RETENTION` | `keep` | How long audit entries are kept: `keep` (nothing is purged) or a duration of at least `24h`, such as `8760h`. |
| `AGENTKOURIER_ENVIRONMENT` | | Optional `deployment.environment.name` on traces. |
| `AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS` | `false` | `true` accepts OTLP headers on an `http://` endpoint. |

The OpenTelemetry variables Agent Kourier reads are listed in [Logs and traces](logs-and-traces.md#trace-settings).
See also [High availability and failover](../explanation/high-availability.md) for what the `AGENTKOURIER_LEASE_*`
timings mean for a takeover.

## Health endpoints

| Path | Answers |
|---|---|
| `/healthz` | The process is up. The chart's startup and liveness probes use it. |
| `/readyz` | The config loaded, the Secret caches synced and the store answers. It does not say whether the chat platform is connected; `agentkourier_chat_connected` does. |
| `/metrics` | Prometheus metrics: see [Metrics](metrics.md). |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Stopped after a signal. |
| 1 | Slack rejected the tokens. A restart would be rejected again. |
| 2 | The config, the wiring, or an environment variable is wrong. |
| 3 | The API server denied access to Secrets. |
| 4 | Any other failure, such as the Secret caches not syncing. |
| 5 | It was the leader and lost the lease or met a stale epoch. It ends at once, and Kubernetes restarts it as a standby. |

## `agent-kourier dry-run`

Replays a channel's history through a Binding's chat triggers and prints what each message would become. It only reads
Slack (`conversations.history`), and posts, starts and writes nothing.

| Flag | Default | Meaning |
|---|---|---|
| `--config` | `$AGENTKOURIER_CONFIG_DIR` | The directory of resource files, loaded with the broker's loader. |
| `--binding` | | The Binding, as `namespace/name`. |
| `--channel` | the Binding's | Another channel to read. |
| `--token-file` | | A file with the bot token, alone or as a `SLACK_BOT_TOKEN=` line. Else `SLACK_BOT_TOKEN`. The file must be a regular file that the group and others cannot read. |
| `--since` | all | Read only messages newer than this duration. |
| `--limit` | all | Show only the newest this many of what was read. |
| `--json` | `false` | Print one JSON report. |

Each line's outcome is one of `matched`, `unmatched`, `excluded`, `other_bot`, `own`, `person` or `ignored`. Reading the
output is in [Tune triggers with a dry run](../how-to/tune-triggers.md).

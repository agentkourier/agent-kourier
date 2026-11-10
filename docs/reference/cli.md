# Command line and environment

This page lists the `agent-kourier` binary's environment variables, flags, subcommand and exit codes, as
`cmd/agent-kourier` reads them.

The broker is configured by environment variables, which the Helm chart sets from its values. It takes one flag,
`--version`, and two subcommands, `dry-run` and `init`.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `AGENTKOURIER_CONFIG_SOURCE` | `files` | Where the resources are read from: `files` (also the meaning of unset) or `crds`. `files` reads `AGENTKOURIER_CONFIG_DIR`, all or nothing. `crds` reads `Binding`, `AgentBackend` and `ChatConnection` custom resources from the cluster, one object at a time, ignores the directory, and has the leader write `Binding.status`. Any other value exits with code 2. The two are never merged: a broker on `files` ignores custom resources. See [Choose a config route](../explanation/config-routes.md). |
| `AGENTKOURIER_CONFIG_DIR` | `/etc/agent-kourier` | With `files`, the directory of resource files (`*.yaml`). |
| `AGENTKOURIER_CHAT_CONNECTION` | | With `crds`, required unless `AGENTKOURIER_CONNECTION_NAMESPACES` names a namespace: the broker's own ChatConnection, as `namespace/name`, a namespace that is a DNS-1123 label and a name that is a DNS-1123 subdomain: anything else is refused (exit code 2) with a message that does not show the value. A broker with one always serves it, and it serves the Bindings whose `connectionRef` names it and those of the ChatConnections that `AGENTKOURIER_CONNECTION_NAMESPACES` admits. Unset, the broker has no ChatConnection of its own and serves only the ChatConnections of the namespaces in `AGENTKOURIER_CONNECTION_NAMESPACES`; unset with that list empty too is refused (exit code 2). With `files` it is refused (exit code 2): the directory must hold exactly one ChatConnection. The chart sets it from `config.chatConnection`, or, when both that and `config.connectionNamespaces` are empty, from the one entry of `config.chatConnections`, and renders no variable when `config.chatConnection` is empty and `config.connectionNamespaces` is set. |
| `AGENTKOURIER_CONNECTION_NAMESPACES` | | With `crds`, required unless `AGENTKOURIER_CHAT_CONNECTION` is set: the namespaces whose ChatConnections this broker serves besides its own, comma-separated, each a DNS-1123 label (no wildcard). Spaces around an item are trimmed, and empty items and repeats are dropped. Unset, empty or only separators, there is no list and the broker serves its own connection alone; with no `AGENTKOURIER_CHAT_CONNECTION` either it is refused (exit code 2) with a message that names both variables, since the list is read at start and such a broker could serve nothing until a restart. At most 16 connections are served in all ([Limits](limits.md#run-the-broker)). It is read at start, so a change is a restart. An item that is not a namespace name is refused (exit code 2) with a message that names the item's position and not its value; with `files` the variable is refused (exit code 2). The chart sets it from `config.connectionNamespaces`. See [Let a team bring its own Slack app](../how-to/team-slack-app.md). |
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
| 1 | Slack rejected the tokens of the broker's own connection (with `files`, the one ChatConnection of its directory). A restart would be rejected again. A team ChatConnection whose tokens Slack rejects does not end the process: that connection stops, and its Bindings say `ChatCredentialsRejected`. With `crds`, a broker with no `AGENTKOURIER_CHAT_CONNECTION` has no own connection, so a rejected token never exits 1 there: every connection it serves is a team connection, and the broker stays up and Ready with `agentkourier_chat_connected` at 0 for each connection that stopped. |
| 2 | The config, the wiring, or an environment variable is wrong. With `crds` also: neither `AGENTKOURIER_CHAT_CONNECTION` nor `AGENTKOURIER_CONNECTION_NAMESPACES` names anything (a list of only separators counts as unset; the message names both); the CRDs are not installed; the broker's own ChatConnection is missing or invalid at start (a broker with no own connection has none to find); or the broker has no own connection and, as it starts to lead, the store holds outbox rows or button presses that were written before connections were recorded (the message gives their counts and says to set `config.chatConnection` to the ChatConnection that served them, let the broker start once, then unset it). |
| 3 | The API server denied access to Secrets, or, with `crds`, to the custom resources. |
| 4 | Any other failure, such as the Secret caches not syncing. |
| 5 | It was the leader and lost the lease, met a stale epoch, or had a chat write or an agent call refused because its lease was past the renew deadline. It ends at once, and Kubernetes restarts it as a standby. |

## `agent-kourier dry-run`

Replays a channel's history through a Binding's chat triggers and prints what each message would become. It only reads
Slack (`conversations.history`), and posts, starts and writes nothing.

| Flag | Default | Meaning |
|---|---|---|
| `--config` | `$AGENTKOURIER_CONFIG_DIR` | The directory of resource files, loaded with the broker's loader. It always reads files, whatever the route of the install: to check custom resources against the CRDs, use `kubectl apply --dry-run=server`. |
| `--binding` | | The Binding, as `namespace/name`. |
| `--channel` | the Binding's | Another channel to read. |
| `--token-file` | | A file with the bot token, alone or as a `SLACK_BOT_TOKEN=` line. Else `SLACK_BOT_TOKEN`. The file must be a regular file that the group and others cannot read. |
| `--since` | all | Read only messages newer than this duration. |
| `--limit` | all | Show only the newest this many of what was read. |
| `--json` | `false` | Print one JSON report. |

Each line's outcome is one of `matched`, `unmatched`, `excluded`, `other_bot`, `own`, `person` or `ignored`. Reading the
output is in [Tune triggers with a dry run](../how-to/tune-triggers.md).

## `agent-kourier init`

Writes a first-install Helm values file for the ConfigMap route (`config.source: files`), checks it, and prints the commands that create the Secrets and install the
chart. Releases before 0.1.3 do not have it. It posts nothing to Slack and changes nothing in a cluster. The step-by-step use
is in [Install with Helm](../how-to/install-with-helm.md#write-it-with-init).

Every value that no option gives is asked on standard input, with the default in brackets. The questions go to standard
error and the commands to standard output. If the input ends before a value without a default is given, the command
exits 2 and names the option. The question whether the agent verifies tokens is asked only when another question was.

| Flag | Default | Meaning |
|---|---|---|
| `--out` | `my-values.yaml` | The file to write. An existing file is not overwritten unless `--force` is set, and a symbolic link is never followed. |
| `--force` | `false` | Overwrite the file. |
| `--dialect` | asked, default `a2a` | The agent's dialect: `a2a` or `kagent-v1`. |
| `--agent-url` | asked | The agent's URL. |
| `--agent-namespace` | asked | The agent's namespace. |
| `--agent-name` | asked | The agent's name. |
| `--channel` | asked | The channel, by ID (`C...`, or `G...`), or by name, with or without a leading `#`. A name is looked up with `conversations.list`, which needs a bot token and the scopes `channels:read` and `groups:read`; the file holds the ID. |
| `--agent-token` | `false` (asked) | The agent verifies tokens: the Binding's `identity.tokenSecretRef` names the Secret `agent-kourier-agent-token`, key `token`, and the commands create it. |
| `--namespace` | `agent-kourier` | The release's namespace. Resources that name none are checked in it, and the commands name it. |
| `--token-file` | | A file with the bot token, as for `dry-run`. Else `SLACK_BOT_TOKEN`. |

The checks, in order: a channel name is looked up; the resources that the chart renders from the values (the
ChatConnection `slack`, the AgentBackend `agent` and the Binding `my-binding`) are loaded with the broker's loader, which
defaults and validates them; and, with a bot token, a `conversations.history` request for the channel shows that the
bot can read it (`channels:history`, or `groups:history` for a private channel, and the bot in the channel). The request
asks for messages newer than the moment it is made, so it fetches none. Without a bot token the Slack checks are skipped
and the command says so. Nothing is written unless every check passes. The loader does not check references between
resources or the Secrets: those errors show only when the pod starts.

The `helm install` command it prints names the chart version of the binary's own release, and none for a build that is
not a release (version `dev`), where Helm takes the newest chart.

Exit codes: 0 for a written file; 1 when Slack rejects the token; 2 for a wrong option, answer, token file or values, or an
existing file; 4 for any other failure, such as Slack refusing the channel read.

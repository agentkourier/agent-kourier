# Command line and environment

This page lists the `agent-kourier` binary's environment variables, flags, subcommand and exit codes, as
`cmd/agent-kourier` reads them.

The broker is configured by environment variables, which the Helm chart sets from its values. It takes one flag,
`--version`, and two subcommands, `dry-run` and `init`.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `AGENTKOURIER_CONFIG_SOURCE` | `files` | Where the resources are read from: `files` (also the meaning of unset) or `crds`. `files` reads `AGENTKOURIER_CONFIG_DIR`, all or nothing. `crds` reads `Binding`, `AgentBackend` and `ChatConnection` custom resources from the cluster, one object at a time, ignores the directory, and has the leader write `Binding.status`. Any other value exits with code 2. The two are never merged: a broker on `files` ignores custom resources. See [Choose a config route](../explanation/config-routes.md). |
| `AGENTKOURIER_CONFIG_DIR` | `/etc/agent-kourier` | With `files`, the directory of resource files (`*.yaml` and `*.yml`; a name that starts with a dot is skipped). |
| `AGENTKOURIER_CHAT_CONNECTION` | | With `crds`, optional: the broker's own ChatConnection, as `namespace/name`, a namespace that is a DNS-1123 label and a name that is a DNS-1123 subdomain: anything else is refused (exit code 2) with a message that does not show the value. A broker with one always serves it, and it serves the Bindings whose `connectionRef` names it and those of the ChatConnections of the namespaces it admits. Unset, the broker has no ChatConnection of its own: it serves the ChatConnections of the namespaces that `AGENTKOURIER_CONNECTION_NAMESPACES` and `AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR` admit, or, with neither set, of every namespace. Set with neither of them, it serves its own connection alone. With `files` it is refused (exit code 2): the directory must hold exactly one ChatConnection. The chart sets it from `config.chatConnection`, or, when that, `config.connectionNamespaces` and `config.connectionNamespaceSelector` are all empty, from the one entry of `config.chatConnections`, and renders no variable when `config.chatConnection` is empty and the list or the selector is set, or none of the three is set and `config.chatConnections` has no entry. |
| `AGENTKOURIER_CONNECTION_NAMESPACES` | | With `crds`, optional: the namespaces whose ChatConnections this broker serves besides its own, comma-separated, each a DNS-1123 label (no wildcard). Spaces around an item are trimmed, and empty items and repeats are dropped. Unset or empty, there is no list. A value that names no namespace (only commas and spaces) is refused (exit code 2): read as no list, on a broker with nothing else set, it would serve every namespace. A namespace is admitted if it is on the list or `AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR` matches it. At most 16 connections are served in all ([Limits](limits.md#run-the-broker)). It is read at start, so a change is a restart. An item that is not a namespace name is refused (exit code 2) with a message that names the item's position and not its value; with `files` the variable is refused (exit code 2). The chart sets it from `config.connectionNamespaces`. See [Let a team bring its own Slack app](../how-to/team-slack-app.md). |
| `AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR` | | With `crds`, optional: a Kubernetes label selector, as JSON (`{"matchLabels":{"agentkourier.dev/serve":"true"}}`, with `matchLabels` and `matchExpressions`, whose operators are `In`, `NotIn`, `Exists` and `DoesNotExist`). The broker also serves the ChatConnections of the namespaces whose labels match it. `{}`, or any selector that requires nothing, admits every namespace and reads none. Unset or empty, there is no selector. The broker reads the selector at start, so a change is a restart, and follows the namespaces' labels live with an informer on namespaces: a namespace that gains a matching label is admitted at the next read, every 10 seconds, and one that loses it stops being admitted then, with no restart. While the broker cannot reach the API server it keeps the labels it last read. A selector other than `{}` needs `list` and `watch` on `namespaces`; without them the start fails (exit code 3). The value must be one JSON object and nothing after it, with no unknown or repeated field, and keys, values and operators that Kubernetes accepts: anything else is refused (exit code 2) with a message that says what is wrong and does not show the value. With `files` it is refused (exit code 2). The `config route` log line shows the selector as Kubernetes writes it (`connection_namespace_selector`). The chart sets it from `config.connectionNamespaceSelector`, as compact JSON. |
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
| 2 | The config, the wiring, or an environment variable is wrong. With `crds` also: `AGENTKOURIER_CONNECTION_NAMESPACES` is set and names no namespace (only commas and spaces); `AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR` cannot be read as a label selector (the message does not show the value); the CRDs are not installed; the broker's own ChatConnection is missing or invalid at start (a broker with no own connection has none to find); or the broker has no own connection and, as it starts to lead, the store holds outbox rows or button presses that were written before connections were recorded (the message gives their counts and says to set `config.chatConnection` to the ChatConnection that served them, let the broker start once, then unset it). |
| 3 | The API server denied access to Secrets, or, with `crds`, to the custom resources, or, with a selector other than `{}`, to namespaces. |
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

Writes the files of a first install with the Helm chart, checks them, and prints the commands that install the chart, create the Secrets and,
on custom resources, grant them to the broker and apply the team's file. Releases before 0.1.3 do not have it, and releases up to 0.3.1 write only the ConfigMap route. It
posts nothing to Slack and changes nothing in a cluster. The step-by-step use is in
[Install with Helm](../how-to/install-with-helm.md#write-the-files-with-init).

On custom resources (`--source crds`, the default) the broker has no Slack app of its own and serves the team's, as in
[Let a team bring its own Slack app](../how-to/team-slack-app.md#the-whole-path-recorded). It writes two files:

| File | Holds |
|---|---|
| `--out` (`my-values.yaml`) | The chart's values, as comments only and no key: the broker has no `config.chatConnection` and serves the ChatConnections of every namespace. The comments show `config.connectionNamespaces: [<team namespace>]` and `config.connectionNamespaceSelector`, commented out. |
| `<team namespace>.yaml`, beside `--out` | In the team's namespace: the ChatConnection `<team namespace>-slack`, whose `credentialsSecretRef` names the Secret of the same name; the AgentBackend `agent`; and the Binding `my-binding`, whose `backendRef` and `connectionRef` name the two without a namespace. |

`--out` may not be the team's file, whatever the case of its name. With `--source files` it writes one values file for the ConfigMap route (`config.source: files`) that holds
the ChatConnection `slack`, the AgentBackend `agent` and the Binding `my-binding` under `config`, with no namespace of their own, so the chart
puts them in the release's.

Every value that no option gives is asked on standard input, with the default in brackets, in the order of the table below; the team's namespace
comes right after the agent's. The questions go to standard error and the commands to standard output. If the input ends before a value without a
default is given, the command exits 2 and names the option. The question whether the agent verifies tokens is asked only when another question
was. A value or an answer that looks like a Slack token is refused as soon as it is read, before a later question could show it as a default.

| Flag | Default | Meaning |
|---|---|---|
| `--source` | `crds` | The chart's config route: `crds` (custom resources) or `files` (a ConfigMap). |
| `--out` | `my-values.yaml` | The values file to write, trimmed of spaces. If it exists the command stops before its questions, and if the team's file exists it stops before them when `--team-namespace` is given and after them otherwise, before any check, unless `--force` is set. A symbolic link is never followed. Without `--force`, a file that cannot be written removes the files this run wrote. |
| `--force` | `false` | Overwrite the files. Only a regular file is overwritten: anything else at either path stops the command before it writes. |
| `--dialect` | asked, default `a2a` | The agent's dialect: `a2a` or `kagent-v1`. |
| `--agent-url` | asked | The agent's URL. |
| `--agent-namespace` | asked | The agent's namespace. |
| `--team-namespace` | asked, default the agent's namespace | `crds` only: the team's namespace, where its ChatConnection, AgentBackend and Binding go. It must exist: the commands create none. With `--source files` it is a usage error, and it is not asked. |
| `--agent-name` | asked | The agent's name. |
| `--channel` | asked | The channel, by ID (`C...`, or `G...`), or by name, with or without a leading `#`. A name is looked up with `conversations.list`, which needs a bot token and the scopes `channels:read` and `groups:read`; the Binding's file holds the ID. |
| `--agent-token` | `false` (asked) | The agent verifies tokens: the Binding's `identity.tokenSecretRef` names the Secret `agent-kourier-agent-token`, key `token`, in the Binding's namespace (the team's on custom resources, the release's with `files`), and the commands create it there. On custom resources the team's Role names it too. |
| `--namespace` | `agent-kourier` | The release's namespace, which the commands name. With `files` the resources are in it. |
| `--token-file` | | A file with the bot token, as for `dry-run`. Else `SLACK_BOT_TOKEN`. On custom resources it is the team's app's. |

The checks, in order: a channel name is looked up; the resources are loaded and checked; and, with a bot token, a `conversations.history`
request for the channel shows that the bot can read it (`channels:history`, or `groups:history` for a private channel, and the bot in the
channel). The request asks for messages newer than the moment it is made, so it fetches none. On custom resources the team's file is loaded with the broker's file loader, which defaults and validates each object, and the
Binding's references to the ChatConnection and the AgentBackend are resolved as the broker resolves them, with every Secret taken as present.
With `files`, what the chart renders from the values is loaded. Without a bot token the Slack
checks are skipped and the command says so. Nothing is written unless every check passes. With `files` the references between the resources are
not checked, and on either route the Secrets are not: those errors show when the pod starts or, on custom resources, in the Binding's status.

On custom resources the commands it prints are the platform team's, `kubectl create namespace` and `helm install`, then the team's, in its
namespace: with `--agent-token`, the token Secret; the Secret `<team namespace>-slack`, from the file `$HOME/.config/<team namespace>/slack-secret.env`
with `botToken=` and `appToken=` lines; a Role `agent-kourier-secrets-<team namespace>` that grants `list` and `watch` on those Secrets by name, and a
RoleBinding of it to the ServiceAccount `agent-kourier` in `--namespace`, the chart's for the release `agent-kourier`; a `kubectl apply --dry-run=server`
and a `kubectl apply` of the team's file; and `kubectl get akbinding`. With `files` they create the namespace, the Secret `slack` and, with `--agent-token`,
the token Secret in `--namespace`, and install the chart. The `helm install` command names the chart version of the binary's own release, and none for a build that
is not a release (version `dev`), where Helm takes the newest chart.

Exit codes: 0 when the files are written; 1 when Slack rejects the token; 2 for a wrong option, answer, token file, values or resources, or an
existing file; 4 for any other failure, such as Slack refusing the channel read.

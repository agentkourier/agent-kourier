# Release notes

What changed in each release, newest first, and what you must do about it. The steps for an upgrade are in
[Upgrade](how-to/upgrade.md), which has a section for each change that needs one; this page says what changed and points to it.


## Version 0.8.0

The experimental AX add-on is now a Task launcher. It no longer runs a model itself. Each turn starts one AX Task from a
template you supply, running your own agent image. The Task sends its result back to the add-on over authenticated
HTTPS, and Agent Kourier posts it in the thread. The add-on keeps its name, its chart block `addons.ax` and its A2A
address. The broker's CRDs, database schema and values are unchanged from 0.7.0, and so is the Claude Managed Agents
add-on's chart block. In threads, one notice is reworded (below).

The launcher is built and tested against AX v0.4.0-rc.2, a pre-release of AX v0.4.0. A patch release will move it to
AX v0.4.0 once that version is released.

### Breaking changes

- **For an install with the AX add-on on.** The ADK agent is gone, and with it the add-on's model settings:
  `addons.ax.model`, `addons.ax.bedrock` and `addons.ax.anthropicApiKeySecret` are removed, and so is
  `addons.ax.networkPolicy.model`. The values schema refuses them, so a values file that still sets them fails to
  render. The `ask_user` tool and ADK's session memory are gone too, and so are both of the old agent's routes: the
  agent beside Agent Kourier, and the image that served A2A from inside an AX Task.
- **The broker's Secret grants, with the AX add-on on.** The chart refuses `rbac.secretNamespaces` that names the
  release's own namespace, so that the add-on's callback Secret stays out of the broker's reach. With `config.source: files`,
  the broker's Role in the release namespace names single Secrets, those that `config.*` and `rbac.secretNames` name,
  instead of granting every Secret there; a Secret the broker reads that the chart cannot see goes in `rbac.secretNames`.
  [Replace the experimental ADK add-on](how-to/upgrade.md#replace-the-experimental-adk-add-on-with-the-ax-task-launcher)
  has the steps: what to keep, what to set, and what happens to turns that were running or waiting on a question.
- **None for an install without it**, unless its values file still carries the removed keys under `addons.ax`: the
  schema refuses them with the add-on off too. With `addons.ax` off, the chart renders what 0.7.0 rendered, apart from
  the version labels and the config checksums that include them, so the broker's pod restarts on the upgrade.

### AX add-on (experimental)

- **New settings**: the AX control plane's address (`addons.ax.server.address`), the Task template's ConfigMap
  (`addons.ax.taskTemplate.configMap`), and the result callback (`addons.ax.resultCallback`): the HTTPS address that
  Tasks post their results to, the Secret that holds its token, and, optionally, an Ingress for it. Also
  `atespace`, `launcherId`, `promptKey`, `history`, `maxRunning`, `pollInterval`, `turnTimeout` and `keepFailed`.
  The add-on's NetworkPolicy settings gain `addons.ax.networkPolicy.server`, for egress to the AX server, and
  `addons.ax.networkPolicy.callbackFrom`, for the callback's ingress.
- **The image** also contains the command wrapper that your agent image runs under in the Task. The wrapper claims
  the start of one command and reports its bounded output through the callback.
  [Connect the AX add-on](how-to/connect-ax.md) has the setup, including the Gateway the Tasks reach the callback through.
- **Upgrade**: completed tasks stay readable. On its first start after the upgrade, the add-on fails the turns that
  were running as interrupted, and fails questions that were waiting on an answer with a prompt to send a new message.

### Other changes

- **The notice for a message that waits behind an earlier task** now reads "The agent is still busy with an earlier
  task, so this message is waiting. I'll keep trying." It no longer says "in this thread".
- **The add-on kit no longer releases a context it has just accepted a message for.** The Claude Managed Agents
  add-on's idle sweep could archive a thread's session after accepting a new message for it. The message then started
  a session without the thread's history, or, if it answered a paused task, found no task to answer. The sweep now
  checks again under a lock that the message's intake shares.
- **The release** retries an image build that fails, up to four attempts, for registries that fail briefly. It takes
  the add-ons from one list, and stops unless each add-on image it signs is one whole reference by digest.

### Dependencies

- **Go modules:** the broker's `k8s.io` modules move from v0.37.2 to v0.37.3, and `google.golang.org/grpc` from v1.84.0
  to v1.85.0. The Claude Managed Agents add-on's
  `anthropic-sdk-go` moves from v2.13.0 to v2.15.0. The AX add-on drops ADK, the model SDKs and GORM, and builds on
  `github.com/google/ax` v0.4.0-rc.2.
- **Images:** the Go builder, `golang:1.27.4`, takes a new digest at the same version. The runtime base image is 0.7.0's.

## Version 0.7.0

Two add-ons arrive, both experimental: components that the chart runs beside Agent Kourier, each off until its block under
`addons` turns it on, and each built, scanned and signed by Agent Kourier's release under the same version.

- **Claude Managed Agents** (`addons.cma`), an A2A server in front of one agent in Claude Managed Agents, Anthropic's hosted
  agent runtime. A thread is one session of the agent, and a tool call that the agent's permission policy asks about waits on
  Approve and Deny in the thread. [Connect Claude Managed Agents](how-to/connect-claude-managed-agents.md) has the setup.
- **AX** (`addons.ax`), the agent that was `examples/ax`: an agent built with Google's Agent Development Kit (ADK), on Amazon
  Bedrock or the Anthropic API, that serves A2A and asks its questions with one tool, `ask_user`. The chart runs it beside
  Agent Kourier, and it can still run inside Google AX as a Task. [Connect the AX add-on](how-to/connect-ax.md) has both routes.

A Binding reaches either through an AgentBackend with the `a2a` dialect, as it reaches any A2A agent; [Add-ons](explanation/add-ons.md)
says what an add-on is. In threads, an answer to a paused task no longer repeats the paused turn's text, and the ending of an
answered tool approval shows one line for each tool. The CRDs, the database schema and the chart's values of 0.6.0 are unchanged.
[Upgrade to the release that ships the add-ons](how-to/upgrade.md#upgrade-to-the-release-that-ships-the-add-ons) has what to check,
the move of the AX example, and how to roll back.

### Breaking changes

- **None for an install of the chart.** The CRDs are 0.6.0's, no migration runs, and no metric, label, log line or values key of the
  broker changed. The chart's only new values are under `addons`, with every add-on off, and a values file without them renders the
  objects it rendered on 0.6.0.
- **For a deployment of 0.6.0's `examples/ax` agent only.** The agent is now `addons/ax`, and it does not start while `STATE_DIR` is
  set: set `SQLITE_PATH` to `STATE_DIR` plus `/agent.db`, and its first start imports the old tasks. The route inside Google AX also needs
  `LISTEN_ADDR`, `PUBLIC_URL` and `RUNNER_URL` in `task.yaml`, and its image is now built with `make ax-runner-image`. An A2A client
  other than Agent Kourier must now name the protocol version (below). [The upgrade](how-to/upgrade.md#move-from-the-ax-example-of-060)
  has the steps.

### Add-ons (experimental)

- **Turn one on** with `addons.<name>.enabled: true`, the caller token's Secret in `addons.<name>.auth.tokenSecret`, and its
  settings: for `cma`, `agentId`, `environmentId` and the API key's Secret,
  `anthropicApiKeySecret`; for `ax`, `model.name`, with `model.provider` `bedrock` (the default, which needs `model.region`) or
  `anthropic`. Each runs as the Deployment `<fullname>-<name>`, with a Service of its own on port 8090, and serves A2A at
  `http://<fullname>-<name>.<namespace>.svc:8090/a2a`, which the AgentBackend names. Its objects carry
  `app.kubernetes.io/name: agent-kourier-<name>`, so nothing that selects the broker's pods selects an add-on's.
- **Images**: `ghcr.io/agentkourier/agent-kourier-cma` and `ghcr.io/agentkourier/agent-kourier-ax`, for linux/amd64 and linux/arm64,
  tagged with the release's `vX.Y.Z`, with SBOM and provenance attestations, and signed by the same workflow identity as Agent Kourier's
  image. The release's chart pins each by digest. [RELEASING.md](https://github.com/agentkourier/agent-kourier/blob/main/RELEASING.md#verify-a-release)
  has the commands that verify them. The AX add-on's image for the route inside Google AX is not released: you build it.
- **The caller check.** A request to an add-on's `/a2a` or to its agent card must carry a token of the Secret in
  `addons.<name>.auth.tokenSecret`, as `Authorization: Bearer <token>`, which a Binding's `identity.tokenSecretRef` sends, or, with
  `addons.<name>.auth.header` set, as that header's whole value instead; any other request is answered 401. The add-on reads the token file again every
  5 seconds, so a rotated Secret takes effect without a restart. `/healthz`, `/readyz` and `/metrics` need no token, and the chart never
  turns the check off. The AX add-on also takes OIDC, for the route inside Google AX.
- **The store.** Each add-on keeps the A2A tasks, the message IDs that started them and when each context was last active, with the
  add-on's own state: for `cma`, each context's session of the agent; for `ax`, the ADK sessions. SQLite, the default, is a file on the
  claim `<fullname>-<name>-data`, which the chart keeps when the release is uninstalled unless `persistence.retainOnUninstall` is
  `false`; `store.type: postgres` takes a database of the add-on's own, named in `store.postgres.dsnSecret`. A task paused on an
  approval or a question survives a restart, and a turn that was running fails. A settled task is deleted after `taskRetention`
  (`720h` by default), and a context idle for `sessionIdleTTL` (`2160h`, the default of a Binding's `session.threadTTL`) is released:
  the CMA add-on archives its session, and the AX add-on deletes its ADK session and tasks.
- **Replicas.** On SQLite an add-on runs one replica, and the chart refuses `replicaCount` above 1. On Postgres several replicas elect a
  leader through a lease row in the add-on's database, so the add-on needs no RBAC; a standby forwards each request to the leader, and
  every write of the leader checks the lease in its own transaction. A leader that loses the lease exits with code 5. On SIGTERM a leader
  drains its requests for up to 5 seconds, cancels the turns still running and waits up to 5 seconds for them, and hands the lease over.
- **For an A2A client other than Agent Kourier.** A JSON-RPC request must name the protocol version in the header `A2A-Version`, as
  `1.0`: one that names none is read as A2A 0.3 and refused with the error `-32009`, version not supported. That is the check of
  a2a-go v2.9.0, which the add-ons are built with. Agent Kourier's client sends the version the agent card gives, so a Binding is not
  affected. A message sent again with the same message ID joins its first send's task instead of starting a second turn, and a
  non-streaming `SendMessage` is answered with the task once its turn ends or pauses. An answer to a paused task that the add-on
  cannot use is refused as invalid before a turn starts, and the task stays paused: for `ax`, one with a part that is not text, or
  with no text; for `cma`, one that does not decide each paused call once in a `tool_approval_response`. The CMA add-on also refuses
  a new message with no text, or with a part that is neither text nor data.
- **Telemetry**: metrics with the prefixes `agentkourier_cma_` and `agentkourier_ax_` on each add-on's `/metrics`, and a ServiceMonitor
  of its own with `serviceMonitor.enabled`; a span for each turn, `agentkourier.cma.turn` or `agentkourier.ax.turn`, under the trace
  that Agent Kourier sends; and JSON logs on standard error.
- **NetworkPolicy.** With `networkPolicy.enabled`, each enabled add-on gets a policy of its own, and Agent Kourier's policy also allows
  egress to the add-on's pods on port 8090.
- **Every limit**, the ones the add-ons share and each one's own, is on [Limits](reference/limits.md#add-ons). The reference pages,
  [Claude Managed Agents add-on](reference/claude-managed-agents.md) and [AX add-on](reference/ax.md), have every setting, endpoint,
  metric and exit code. [Write an add-on on the kit](how-to/write-an-add-on.md) builds a new one on the code the two share, `addons/kit`.
- **What was tested.** The repository's tests run each add-on, with the race detector, on SQLite and on Postgres. The add-ons were run
  through the chart on kind clusters with a Slack test workspace on 2026-11-22 and 2026-11-23, on builds from before this tag:
    - The CMA add-on, with a Haiku agent on a self-hosted environment: on SQLite, an alert posted by another app started a thread, and
      the agent asked to approve a bash call; the add-on's pod was deleted while the call waited, and after Approve the call ran and
      the agent posted its findings, and a reply in the thread went on in the same session. On Postgres with two replicas, the leader
      was killed while approvals waited, another replica took over, and the same task completed after the approvals. An idle
      timeout of 2 minutes archived the sessions. Cloud environments have not been tested; the add-on does not refuse them.
    - The AX add-on, with `claude-haiku-4-5` on the Anthropic API, on SQLite: an alert started a thread and the agent asked a question;
      the pod was deleted while it waited, and the answer resumed the same task on the new pod. On Postgres with two replicas, the
      leader was killed while a question waited, the standby took over, and the same task completed after the answer. Bedrock has been
      tested against a mock of its API, not called; the routes to Bedrock through an IAM role (IRSA, EKS Pod Identity) have not been run
      on EKS; and the route inside Google AX was not run end to end after the move to `addons/ax`.

### Chat

- **An answer no longer repeats the paused turn's text.** The task that an answer to a question or a tool approval goes on with keeps
  the artifacts of the turn that paused, and its last snapshot holds them again, so the answer's message ended with a line the paused
  turn had already shown, under the answer's own text. Before it sends an answer, Agent Kourier now notes the text of each artifact of
  the paused task, and a snapshot that repeats one unchanged adds nothing; a live chunk of that text, or a snapshot that changes it, is
  shown as before. An answer whose agent adds no text of its own therefore shows none, and the thread gets "The agent finished without
  any output." An answer that Agent Kourier follows after a lost stream or a restart is not covered, and can still show the paused turn's
  text and steps again ([Agent contract](reference/agent-contract.md#tool-calls-and-results)).
- **An answered tool approval shows one line for each tool**, in the form of a tool's status line: its name, a summary of its redacted
  arguments on one line, and its verdict, such as `bash(command="kubectl get pods -n shop") — Approved`, numbered when there are several.
  The summary of a JSON object is its `key=value` pairs in the order of the keys, cut and marked past 160 units; the prompt still shows
  the arguments whole. The ending showed each tool's verdict and then its full arguments in a preformatted block. A timed-out or
  cancelled ending shows the same lines without a verdict. This is on Slack and on Microsoft Teams, and the audit is unchanged.
- **The ending leaves out the arguments that a card shows.** When the reply the agent paused in shows the call on its card, with the same
  name and summary of its arguments, the ending names the tool alone, such as `bash — Approved`. That takes `output: live` with
  `progress` set to `steps` or `steps-and-activity`, and the card in the reply's last write: the approval's prompt now waits for that
  write, up to 5 seconds after the pause. The ending keeps the arguments in every other case, such as a sub-agent's approval, a tool
  with no arguments or whose summary was cut, or a reply whose cards the platform refused
  ([Tool approvals](how-to/agent-questions.md#tool-approvals) lists them), and an approval paused before the upgrade, which has no
  such mark.

### Other changes

- **Documentation.**
    - New pages: [Add-ons](explanation/add-ons.md), a how-to and a reference page for each add-on,
      [Write an add-on on the kit](how-to/write-an-add-on.md), and the add-ons' sections of [Limits](reference/limits.md#add-ons).
    - The README, the home page, the chart's description and its README lead with what Agent Kourier does: it carries messages between
      team chat and agents, one session per thread, and an alert is one example of what starts a session. The chart's keywords add
      `microsoft-teams` and `webhooks`.
    - The [Agent contract](reference/agent-contract.md), Tool approvals and What each platform supports say what an answer and an
      approval's ending show; the chart README lists the add-ons' Deployments and the NetworkPolicy's egress to them.

### Dependencies

- **Go modules:** `modernc.org/sqlite` from v1.62.0 to v1.63.0. The broker's a2a-go is v2.9.0, as in 0.6.0. The Go builder and runtime
  images are 0.6.0's.
- **The add-ons** are built with a2a-go v2.9.0; the AX add-on moves from 0.6.0's example with ADK from v2.5.0 to v2.8.0. The image of
  the AX route, which you build, takes new digests of its Go and Debian bases.
- **Postgres in tests only.** The repository's Postgres tests now run on `postgres:19`, from `postgres:18`, and the fast kind tier's
  `postgres:17` takes a new digest. The chart and the recipes run no Postgres, so nothing changes for an install.

## Version 0.6.0

Two new platforms arrive beside Slack, both experimental:

- **Microsoft Teams**, a second chat platform. A ChatConnection with `spec.platform: teams` serves a team's channels, in `http` mode
  on the chat listener or in `socket` mode, and reads other apps' posts, such as Alertmanager's alerts, through Microsoft Graph.
  [Configure Microsoft Teams](how-to/configure-teams.md) has the setup.
- **Webhooks.** A ChatConnection with `spec.platform: webhook` takes signed JSON webhooks from another service, such as GitHub, Jira
  Cloud, Bitbucket Cloud or Linear, on the chat listener, and starts sessions of an agent from them. Agent Kourier posts nothing on
  such a connection; the agent answers on the service with its own tools. [Start agents from webhooks](how-to/receive-webhooks.md)
  has the setup, a page for each of these four services, and [how to map another service](how-to/receive-webhooks.md#map-another-service) whose webhooks fit the same checks.

A config of Slack connections loads as it did in 0.5.0. Every install runs migrations 0031 and 0032 at its first start, which only
add tables, and an install that holds the CRDs applies the Binding and ChatConnection CRDs again.
[Upgrade to the release that takes webhooks](how-to/upgrade.md#upgrade-to-the-release-that-takes-webhooks) and
[Upgrade to the release that takes Microsoft Teams ChatConnections](how-to/upgrade.md#upgrade-to-the-release-that-takes-microsoft-teams-chatconnections)
have the upgrade, and how to roll each back.

### Breaking changes

- **Apply the CRDs again.** The ChatConnection CRD changes for the platforms `teams` and `webhook` and their blocks, `spec.teams` and
  `spec.webhook`. The Binding CRD changes for the trigger type `webhookEvent`, the output `none` and the channel of a webhook Binding,
  and to take Microsoft Teams' chat IDs: `chat.channel` takes a Teams channel ID and its cap goes from 32 bytes to 140, and a
  `botMessage` trigger's `from.id` and a `chat.mentions` entry's `user` take a Microsoft Entra ID, with caps of 36 bytes (from 32 and
  21). The AgentBackend CRD is unchanged. The chart applies them with the release when `crds.install` is true, or unset on
  `config.source: crds`. If you apply them yourself, apply `charts/agent-kourier/files/crds` of this release. Until then the API
  server refuses the new fields and values, and a broker on custom resources logs at WARN
  `the schema hash of a CRD is not the one this broker was built with` and sets `agentkourier_crd_schema_state{state="mismatch"}` to 1
  for the two CRDs.
- **A chat ID with neither platform's shape is refused with a new message**, which names both shapes, for example
  `spec.chat.channel: invalid channel "general": must be a channel ID such as C0123456789 (Slack) or 19:…@thread.tacv2 (Microsoft Teams), not a name`.
  A script that matches the earlier message needs the new one.
- **Migrations 0031 and 0032 run at the first start**, on SQLite and on Postgres. Each only adds a table, `teams_conversations` and
  `teams_poll_positions`, which a Microsoft Teams connection writes. A broker of 0.5.0, after a rollback, does not read them, and logs at WARN
  at its start that the database schema is newer than its build.

### Webhooks (experimental)

- **New platform `webhook`** on a ChatConnection, with `spec.webhook`: `identity` (the service's ID of the agent's own account, whose events
  are dropped), `verify` (HMAC-SHA256 of the raw body, or a fixed token, in a header, with an optional timestamp in the body), and JSON
  Pointers for the `channel`, the `actor`, the `thread` and the `fields` that triggers read, and the `delivery` header that names each
  delivery in the logs. The credentials Secret holds `webhookSecret` for the HMAC scheme or `token` for the token scheme; one with a control character, such as a trailing
  newline, is refused, and so is a token under 32 bytes. The connection takes `POST /chat/webhook/<namespace>/<name>` on the chat listener,
  which only the leader serves; a broker with no chat listener does not run it.
- **New trigger type `webhookEvent`**, the only one a webhook connection's Bindings take. It matches and excludes on the connection's
  fields, and its `promptTemplate` reads the id fields. Its `limits` are required. A start that a limit stops is answered 200, logged at WARN
  with the delivery ID, and lost unless the event is delivered again.
- **New output `none`**, the only output a webhook connection takes, and one that Slack and Microsoft Teams refuse. A Binding of a webhook connection must set
  `chat.output: none`, and its channel is the value the connection reads from an event, such as `acme/widgets` or `OPS`, not a chat platform's ID.
- **New metrics for webhook connections**: `agentkourier_webhook_requests_total`, by `outcome`, `reason` and `connection`, with every
  series at 0 from when the connection's run starts; `agentkourier_webhook_ack_duration_seconds`, how long each request took to answer,
  by `outcome` and `connection`; and `agentkourier_webhook_last_accepted_timestamp_seconds`, the time of the connection's last delivery
  answered `accepted`, for a rule that finds a service that stopped sending. Each verified delivery is logged at INFO, `webhook: delivery answered`.
- **A span for each verified webhook request**, `agentkourier.webhook.request`, with the connection, the outcome and reason, the event's
  ID and the delivery ID; a request that is not verified has none. The intake's span is its child and the turn it starts is in its trace, and the delivery's log
  lines carry its `trace_id`, so a delivery is found by its delivery ID in the trace backend.
- **`agentkourier_chat_listener_requests_total` has two new labels**: `platform` (`slack`, `teams`, `webhook` or `other`) and `connection` (the
  ChatConnection the path names, while the leader serves it, else `unknown`). A rule that sums the counter reads as before.
- **An edit of the broker's own webhook connection applies at a restart.** Until then the leader logs at WARN, with the connection,
  `chat connection: its spec.webhook was edited, and the running connection still uses the spec.webhook it started with; a restart of the broker applies the edit`,
  and at INFO once the spec is back to the one it runs on. A team's connection restarts by itself when its ChatConnection changes.
- **Four recipes**: `webhook-github`, `webhook-jira`, `webhook-bitbucket` and `webhook-linear`, each a ChatConnection, a Binding and an
  AgentBackend. `webhook-github` ships an agent: the prompt agent's image with an `agent.yaml` of its own and GitHub's github-mcp-server
  beside it, which reviews pull requests and replies to mentions, posting as a machine user with its fine-grained personal access
  token ([GitHub](how-to/webhooks/github.md)). The other three ship no agent yet: point the Binding at an agent that has the service's
  tools.
- **The prompt agent can leave out `ask_user`.** `PROMPT_AGENT_ASK_USER=0` does not offer the model the tool, for a connection that
  serves no questions, such as a webhook's; unset or `1` offers it, as before, and any other value stops the agent at start.
- **What was tested.** The repository's tests run each recipe's connection against a fake of its service, and the four setups were
  run end to end against the real services on 2026-11-17, on a pre-0.6.0 build of the broker with the same mapping.

### Microsoft Teams (experimental)

- **New platform `teams`** on a ChatConnection, with `spec.teams`: `tenantID` and `appID`, the bot's Microsoft Entra tenant and
  application, each a GUID in lowercase hex and required; `mode`, `http` (the default) or `socket`; and `notificationURL`, optional,
  an `https` URL whose path is the connection's own route, `/chat/teams/<namespace>/<name>`. The credentials Secret holds
  `clientSecret`, the client secret of the bot's application, in either mode. `spec.teams` on a Slack connection, and `spec.slack` on a
  Teams one, are refused. A Binding of a Teams connection names Teams' IDs: a channel `19:…@thread.tacv2`, and a Microsoft Entra ID
  for a sender or a mentioned person ([What each platform supports](reference/platforms.md)).
- **Two modes.** In `http` mode the leader takes Teams' requests at `POST /chat/teams/<namespace>/<name>` on the chat listener, which
  a Teams connection in that mode needs; with none, the broker's own connection stops the start, and a team's is not started. In
  `socket` mode the leader opens an outbound WebSocket to Teams in each region, `amer`, `emea` and `apac`, and needs no chat listener
  or public endpoint for Teams' activities. Microsoft calls Socket mode a public preview, and Teams does not deliver what it sends while no socket is
  ready, then or later. Either way the broker posts, replies and edits through the Bot Connector.
- **What a thread shows.** A live reply is one message that Agent Kourier posts and then edits, at most once every 2 seconds, with
  the agent's tool calls listed under the text by default. Questions, tool approvals and the useful and not-useful buttons are Adaptive Cards that
  a member of the channel's team presses.
- **Alerts through Microsoft Graph.** Teams sends a bot no other app's posts, so an alert that Alertmanager posts through a Workflows
  webhook reaches a `botMessage` trigger through Microsoft Graph, in either mode: by Graph's notifications at
  `spec.teams.notificationURL`, which needs the chat listener and a public HTTPS route to it, or, with none, by polling each bound
  channel every 10 seconds. A poll reads replies only in a channel's newest threads. Graph reads under the resource-specific
  permission `ChannelMessage.Read.Group`, which the Teams app's manifest asks for and its install in the team grants.
- **Not there yet.** No reactions: the acknowledgment is taken and shows nothing. `interactions.approverGroups` is refused,
  `UnsupportedOnPlatform`. No ephemeral messages and no live progress streams. `agent-kourier dry-run` reads only Slack, so pin a
  Teams trigger with the recipe's trigger evals. A post whose tag cannot be looked for after a crash is posted again, and may show
  twice ([Delivery guarantees](explanation/delivery.md)).
- **New metrics for Teams connections**, each by `connection`: `agentkourier_teams_requests_total`,
  `agentkourier_teams_tokens_refused_total`, `agentkourier_teams_activities_shed_total` and
  `agentkourier_teams_activities_lost_total` for what Teams sends; `agentkourier_teams_socket_envelopes_total`,
  `agentkourier_teams_socket_reconnects_total`, `agentkourier_teams_socket_ready` and `agentkourier_teams_socket_gap_seconds` for
  `socket` mode; and `agentkourier_teams_graph_notifications_total`, `agentkourier_teams_graph_polls_total`,
  `agentkourier_teams_graph_read_messages_total`, `agentkourier_teams_graph_catchups_total`,
  `agentkourier_teams_graph_throttled_total` and `agentkourier_teams_graph_channels` for Microsoft Graph ([Metrics](reference/metrics.md)).
- **A span for each verified Microsoft Teams activity request**, `agentkourier.teams.request`, on a connection in `http` mode, with the
  connection, the activity's type and ID, and the outcome; a request refused at its token, one whose body is no activity, and one of
  another tenant have none. The intake's span is its child, a message's too though its post is handed on after the response, and the turn
  it starts is in its trace; the lines logged for the request, on its channel's lane as well, carry its `trace_id`. Microsoft Graph's
  notifications and polls, and Socket mode, have no such span.
- **An edit of the broker's own Teams connection applies at a restart.** Until then the leader logs at WARN, with the connection,
  `chat connection: its spec.teams was edited, and the running connection still uses the spec.teams it started with; a restart of the broker applies the edit`,
  and at INFO once the spec is back to the one it runs on. `mode: http` written out where the mode was unset, or taken out, is the same mode and logs nothing.
- **A recipe**, `prompt-agent-teams`: the `prompt-agent` recipe's agent in a Teams channel, with Alertmanager's alerts, the Teams
  app's manifest and trigger evals.
- **What was tested.** The repository's tests run the adapter against fakes of Microsoft's services, the chat conformance suite
  among them. Parts of it were checked on one test tenant in 2026-11, in Teams' web client: `http` mode, Graph subscriptions,
  Graph polling and `socket` mode. A full install has not yet been run against Teams; the checks still open are listed in
  [issue 379](https://github.com/agentkourier/agent-kourier/issues/379).
- **Where to start.** [Configure Microsoft Teams](how-to/configure-teams.md) sets up the bot, the Teams app, the connection and the
  alert route; [Start from a recipe](how-to/recipes.md) has `prompt-agent-teams`; [What each platform supports](reference/platforms.md)
  has what Teams serves and how it reads alerts.

### Other changes

- **On the files route, a reload that changes the broker's one ChatConnection is refused.** A reload whose directory renames the
  connection the broker started with, adds a second or takes it away keeps the last good config, sets `agentkourier_config_stale` to 1
  and logs `config reload failed; keeping the last good snapshot`. An edit of the connection under the same name is taken, and applies
  at a restart ([Choose a config route](explanation/config-routes.md)).

## Version 0.5.0

A Slack ChatConnection can take its events and button presses over HTTP, as Events API and interactivity requests that Slack signs,
instead of over Socket Mode: `spec.slack.mode: http`, on a chat listener that only the leading replica serves. HTTP mode is experimental.
Socket Mode stays the default, and the chat listener is off unless the values turn it on. Every install runs migration 0030 at its first
start, which lets a button press that Slack delivers again be recorded once, and an install that holds the CRDs applies the Binding and
ChatConnection CRDs again, for two new `kubectl get` columns. The chart now refuses `podLabels` that set `agentkourier.dev/role`. The image
is built with Go 1.27.4, as 0.4.0's was. [Configure Slack](how-to/configure-slack.md) has the steps for HTTP mode, and
[Upgrade to the release that serves Slack over HTTP](how-to/upgrade.md#upgrade-to-the-release-that-serves-slack-over-http) and
[Upgrade to the release that shows each Binding's connection](how-to/upgrade.md#upgrade-to-the-release-that-shows-each-bindings-connection)
have the upgrade, and how to roll it back.

### Breaking changes

- **The chart refuses `podLabels` that set `agentkourier.dev/role`**, whether or not the chat listener is on:
  `podLabels must not set agentkourier.dev/role: the leader puts that label on its own pod, and the chat Service selects the leader by it, ...`.
  In the pod template the label would put every replica, the standby included, behind the chat Service. Take the key out of `podLabels`
  in the values you upgrade with.
- **`kubectl get akbinding` has a new column, `CONNECTION`, before `CHANNEL`**, so `CHANNEL` is one column later. `kubectl get akconnection`
  shows `PLATFORM` between `NAME` and `AGE`, where it showed `NAME` and `AGE` alone. A script that reads the columns by position needs the
  new order.
- **Apply the CRDs again.** The Binding CRD changes for its new column, and the ChatConnection CRD for its column and `spec.slack`; the
  AgentBackend CRD is unchanged. The chart applies them with the release when `crds.install` is true, or unset on `config.source: crds`. If
  you apply them yourself, apply `charts/agent-kourier/files/crds` of 0.5.0. Until then `kubectl get` shows the old columns, the API server
  refuses `spec.slack`, or drops it for a client that does not ask for strict field validation, and a broker on custom resources logs at WARN
  `the schema hash of a CRD is not the one this broker was built with` and sets `agentkourier_crd_schema_state{state="mismatch"}` to 1 for
  the two CRDs.
- **A Deployment of your own that sets `AGENTKOURIER_POD_NAMESPACE` without `AGENTKOURIER_CHAT_LISTEN` exits with code 2.** The variable is
  new, and turns the leader label on (below). The chart sets it only with `chatListener.enabled`, with the chat listener.

### Slack over HTTP (experimental)

- **New field `spec.slack.mode`** on a ChatConnection: `socket`, the default when it is unset, or `http`. Only a ChatConnection whose
  `spec.platform` is `slack` may set `spec.slack`: the CRD and the broker's load refuse it on any other. The mode sets the keys of the
  credentials Secret. Socket Mode needs `botToken` and `appToken`, as before; HTTP mode needs `botToken` and `signingSecret`, and does not
  read `appToken`. A missing key fails the load on the ConfigMap route, fails the start for the broker's own connection (exit code 2), and
  leaves a team's connection not started.
- **The chat listener**, set by the new variable `AGENTKOURIER_CHAT_LISTEN`, is a port of its own, apart from the metrics port. It has no
  default: a broker without it opens no such port. It takes `POST /chat/<platform>/<namespace>/<name>`, for Slack
  `/chat/slack/<namespace>/<name>`, answers 404 to any other path and 405 to any other method, and answers 503 with `Retry-After: 5` for a
  connection that takes no requests on this replica. A ChatConnection in HTTP mode needs it: without it the broker's own connection fails
  the start (exit code 2), and a team's connection is not started.
- **Only the leader serves.** Every replica opens the chat listener, but only the leader runs connections, so a standby answers 503. The
  leader labels its own pod `agentkourier.dev/role=leader` once it leads, and the chart's chat Service selects that label. Every replica
  takes the label off its own pod as it starts, and the leader takes it off at shutdown; a leader that loses its lease exits with the label
  on, and the pod's next container takes it off. The write is a merge patch of that one label, retried after 1 s, doubling to 30 s; a first
  write that the API server refuses with 403 or 404 fails the start with exit code 2.
- **New variables for the leader label.** `AGENTKOURIER_POD_NAMESPACE` turns the label on. It needs `AGENTKOURIER_CHAT_LISTEN` and
  `AGENTKOURIER_POD_NAME`, which the broker now requires with either store when the label is on (it required it with Postgres alone). The
  chart sets both from the downward API with `chatListener.enabled`.
- **`/readyz` has a new check, `leader-label`**, when the label is on. It fails until a write of the label has succeeded, so a pod that
  may still carry the label of an earlier process stays out of every Service until it has taken the label off.
- **New chart values `chatListener`**: `enabled` (false by default), `port` (8081), `service` and `ingress`. Turned on, the chart adds:
    - the container port `chat`;
    - the Service `<fullname>-chat`, which selects the pod that carries the leader label;
    - with `chatListener.ingress.enabled`, an Ingress of the same name that sends the path prefix `/chat/` to it, and nothing else;
    - with `rbac.create`, the Role `<fullname>-leader-label`, with `patch` on the pods of the release namespace (a Deployment's pods are
      named by its ReplicaSet, so the Role cannot name the pod), and its RoleBinding.
- **A ValidatingAdmissionPolicy holds that `patch` to the label.** With `chatListener.enabled` and `rbac.create` the chart installs a
  ValidatingAdmissionPolicy (`admissionregistration.k8s.io/v1`, `failurePolicy: Fail`) and its binding,
  `<fullname>-<namespace>-leader-label`. They let the broker's ServiceAccount change only the label `agentkourier.dev/role`, only to
  `leader` or away, and only on the pod its token is bound to, and deny any change to that pod's spec, annotations, finalizers or owners.
  Both are cluster-scoped, so whoever runs `helm install` or `helm upgrade` needs the right to create ValidatingAdmissionPolicies and
  ValidatingAdmissionPolicyBindings. The chart checks no Kubernetes version for them, and its tests run the policy on Kubernetes 1.33 and
  1.37 only: use 1.33 or later, which the custom resource route already requires. With `rbac.create: false` the chart makes neither the
  Role nor the policy ([Security model](explanation/security.md#the-chat-port)).
- **New value `networkPolicy.chatFrom`**: the NetworkPolicyPeers that may reach the chat port.
- **The chart refuses to render:**
    - `chatListener.port` equal to `listen.port`;
    - `networkPolicy.enabled` with the chat listener and no `networkPolicy.chatFrom`;
    - `chatListener.ingress.enabled` without `chatListener.enabled`;
    - `networkPolicy.chatFrom` without `chatListener.enabled`;
    - a `chatListener.service.type` other than `ClusterIP`, `NodePort` or `LoadBalancer` (the values schema).
- **The chat listener's bounds.** A body is capped at 1 MiB (413) and the headers at 16 KiB (431). The headers must arrive within 1 s and
  the whole request within 2 s, and the handler has 3 s. At most 32 handlers run at once; past that a request is answered 503 with
  `Retry-After`. Bodies are read in 4 KiB chunks from a budget of 32 MiB across every request, and a body is read whole before it takes a
  handler's place. A refusal made before the body is read closes the connection. At shutdown the listener waits 2 s for the requests in
  flight, then cuts them unanswered. [Limits](reference/limits.md#run-the-broker) has each value.
- **Slack's requests.** Agent Kourier checks a request's signature against the signing secret before it decodes anything in the body,
  and answers 401 to a missing or wrong signature (`bad_signature`) and to a timestamp more than 5 minutes off (`stale`). It answers a
  `url_verification` request with its challenge. It answers 200 once it has taken an event or press, and 503 when it has not taken it
  within 2 s or cannot take it, so that Slack sends it again.
- **New metrics:**
    - `agentkourier_chat_listener_requests_total{outcome}`;
    - `agentkourier_leader_pod_label_writes_total{result}`;
    - `agentkourier_slack_http_requests_total{outcome,connection}`;
    - `agentkourier_slack_http_retries_total{connection}`, the requests that carry Slack's `X-Slack-Retry-Num`.

    In HTTP mode `agentkourier_chat_connected` is 1 from when the connection's `auth.test` passes until its run ends, as there is no
    WebSocket, and `agentkourier_slack_ack_duration_seconds` is observed too ([Metrics](reference/metrics.md)).

- **What was tested.** The kind tiers run in HTTP mode against the end-to-end tests' fake Slack, and the mode was tried once against Slack
  itself, on one installation, as [Configure Slack](how-to/configure-slack.md) describes.
  [High availability](explanation/high-availability.md#slack-in-http-mode) says what Slack gets during a takeover.

### Fixes

- **A button press that Slack delivers again is recorded once**, in Socket Mode and in HTTP mode: an answer to a question or an approval,
  and a useful or not-useful vote. Each delivery used to be recorded, and its person told the outcome again. A press's key is a hash of
  who pressed, the channel, the message, the button and Slack's `action_ts`; a second delivery of the key is acknowledged and logged at
  info as processed before. A press with no `action_ts` is recorded each time it comes, and pressing the button again is a new press.
  Migration **0030** adds the table `press_deliveries`, on SQLite and on Postgres, which keeps each key for at least an hour and is purged
  every 10 minutes ([Stored data](reference/stored-data.md)). It only adds a table.
- **An answer is taken only on the question its task is paused on.** A press decided again (a redelivery after its key was purged, a press
  with no key, or a second press on a prompt that was never replaced) could answer a later question of the same task with the earlier
  answer. Now an answer to another question is dropped with no notice, and logged at info.
- **`agentkourier_trigger_decisions_total` counts a decision once its start is taken.** It was counted as soon as the decision was made,
  so a start that then failed counted `started`, and its redelivery counted again: `started` for a trigger with no thread rules, and `dedup`
  for a keyed one, though that delivery started the turn. A failed attempt now counts nothing here (it counts as
  `agentkourier_intake_total{disposition="error"}`, and logs at WARN `chat: trigger decision not taken; its redelivery decides again`),
  and the delivery that starts the turn counts `started`, so `started` counts the turns that started. Two exceptions remain:
    - a start with no thread rules keeps no record of itself, so its redelivery after a restart counts `started` again, though its turn is
      queued once;
    - a change of leader with an event in flight can count one start twice.

    [Metrics](reference/metrics.md#operations-intake-delivery-and-triggers) has the detail.

### A Binding checked against its platform

- **The broker checks a Binding once more against its ChatConnection's platform** when it reads the two together, and can refuse it with
  two new reasons: `InvalidPlatformID`, for a chat ID of another shape, and `UnsupportedOnPlatform`, for a feature the platform does not
  serve. Slack serves every feature that the check covers, and the schema already holds each chat ID to Slack's shape, so no Binding on
  Slack gets either. `agentkourier_config_binding_rejections` has a series for each, at 0. The chat IDs' patterns, length caps and
  messages are 0.4.0's. [What each platform supports](reference/platforms.md) is a new reference page.

### Other changes

- **No change on Slack: a prompt the chat platform reports as invalid is refused at once**, as one too large for a message was. It was retried in silence
  until the pause timed out. Now the task gets the unusable-pause refusal: the notice "The agent paused for input in a way Agent Kourier
  cannot use, so Agent Kourier is cancelling the task.", the interaction closed, the task cancelled, and an `unusable_pause` audit entry, then `unusable_pause_cancel`.
  On Slack it is not expected to show: the Slack adapter calls a prompt invalid only when it has no ID, both questions and tools or
  neither, or an ID over 2000 units, and the dialects build none of those.
- **Internal changes, with no change in behaviour on Slack:**
    - each chat platform supplies its own app identity;
    - code that is not Slack's names the chat platform rather than Slack;
    - reactions are a capability of the platform: a Binding on a platform without them would have the condition `AcknowledgmentShown`,
      which no Binding on Slack has;
    - a platform without ephemeral messages has an error of its own, which Slack never returns.

    The help text of `agentkourier_chat_connected` now speaks of the chat platform.

- **Documentation.**
    - Configure Slack has a tab for HTTP mode, with what the one run against Slack itself showed; High availability, the Security model's
      chat port, Troubleshoot and Upgrade cover the mode too.
    - The recipes' README says how to run any recipe over HTTP mode.
    - The README, the landing page and the glossary say Slack is the only platform today, and the README's integrations table lists
      a second chat platform as planned.

- **Before a tag, the release checklist** runs the fast kind tier in HTTP mode on the custom resource route twice: on one replica, and on Postgres with two
  replicas, where the failover tests check that a press sent while the leader is dead or frozen is sent again and decided by the new
  leader. For a release that changes HTTP mode, it also runs a probe against an install that Slack itself reaches.

### Dependencies

- None. `go.mod`, `go.sum`, the `Dockerfile`, the pinned images, and the recipes' and the AX example's dependencies are unchanged. The
  image is built with `golang:1.27.4`, at the digest 0.4.0 used.

## Version 0.4.0

On custom resources a broker can admit namespaces by their labels, and a broker that names nothing it serves serves the ChatConnections of every
namespace, where 0.3.2 refused to render the chart and its broker refused to start. A values file or an environment that worked with 0.3.2 keeps
its meaning, except for one value of `AGENTKOURIER_CONNECTION_NAMESPACES` that the chart never renders (below). The release also publishes the
images of two recipes' agents, and adds the `prompt-agent` recipe. The image is built with Go 1.27.4, as 0.3.2's was.
[Which ChatConnections a broker serves](explanation/config-routes.md#which-chatconnections-a-broker-serves) has the rule, and
[A team brings its own chat app](explanation/security.md#a-team-brings-its-own-chat-app) what each way of admitting means for who consents.
[Upgrade to the release that admits namespaces by label](how-to/upgrade.md#upgrade-to-the-release-that-admits-namespaces-by-label) has the
steps, and how to roll back.

### Breaking changes

- **`AGENTKOURIER_CONNECTION_NAMESPACES` that names no namespace is refused.** A value of only commas and spaces was read as no list: with
  `AGENTKOURIER_CHAT_CONNECTION` set the broker served its own connection alone, and without it the start failed. Read as no list now, with nothing
  else set, it would serve every namespace, so the broker exits with code 2 whatever else is set, with a message that says the variable names no
  namespace. A Deployment that sets the variable itself to only separators, beside an own connection, stops starting: unset the variable. The chart
  is not affected: it renders the variable from `config.connectionNamespaces`, whose items must be namespace names, and renders nothing when the
  list is empty.

### Namespaces admitted by label, or every namespace

- **New value `config.connectionNamespaceSelector`** (the environment variable `AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR`, a label selector as
  JSON), with custom resources only: the broker also serves the ChatConnections of the namespaces whose labels match it, beside its own and the
  list's, under the same cap of 16. `null`, the default, is no selector, and `{}` admits every namespace. The broker reads the selector at start and
  follows the namespaces' labels live, with an informer on namespaces: a namespace that gains a matching label is admitted at the next read, every
  10 seconds, and one that loses it stops being admitted, with no restart, its connections stopped and the Bindings on them told
  `ConnectionNotServed`. While the broker cannot reach the API server it keeps the labels it last read. A selector that cannot be read is refused
  (exit code 2) with a message that does not show the value; the chart's schema checks it first, and the chart refuses it with `config.source: files`.
  Whoever may set a namespace's labels decides whether its ChatConnections are admitted
  ([Security model](explanation/security.md#a-team-brings-its-own-chat-app)).
- **A selector other than `{}` adds a rule to the chart's ClusterRole:** `list` and `watch` on `namespaces` (no `get`). `{}`, no selector and a
  list add nothing. A broker given a selector without that rule exits with code 3 at start.
- **With nothing set, the broker serves every namespace.** With custom resources, no `config.chatConnection`, no `config.connectionNamespaces`, no
  `config.connectionNamespaceSelector` and no `config.chatConnections` entry, the chart renders none of the three variables, and the broker serves
  the ChatConnections of every namespace, up to 16 for the whole cluster (the oldest first). Both used to fail. The rule that the one
  `config.chatConnections` entry is the own connection when the other three are empty is unchanged, so adding one entry to such an install makes it
  serve that connection alone; two or more entries are still refused.
- **Every namespace is a setting for a cluster with one broker.** A broker that serves every namespace serves a second broker's connections too,
  that broker's own included, and the two open Socket Mode on the same Slack apps, which splits each app's events between them. Where the two
  write different statuses for the same Binding, `agentkourier_binding_status_writes_total{result="put_back"}` climbs and the leader logs that
  something else keeps changing the status ([Troubleshoot](how-to/troubleshoot.md#the-status-of-a-binding-keeps-changing)). With two brokers,
  admit with a list or a selector on each.
- **The `config route` log line says what the broker admits**: `connection_admission` is `all`, `list`, `selector`, `list+selector` or `own`
  (its own connection alone), with `connection_namespaces` for a list and `connection_namespace_selector` for a selector other than `{}`.

### Changes to `init`

- **`agent-kourier init` writes the platform team's values as comments only**, so the broker it installs serves every namespace; the comments show
  the list and the selector for a platform that serves fewer. A values file that an earlier `init` wrote keeps its list, and its meaning. See [Write the files with `init`](how-to/install-with-helm.md#write-the-files-with-init).

### The recipes' agent images

- **The release publishes the prompt agent's and the echo agent's images**, `ghcr.io/agentkourier/agent-kourier-prompt-agent` and
  `ghcr.io/agentkourier/agent-kourier-echo-agent`, tagged with the release, for `linux/amd64` and `linux/arm64`. They are built, scanned, signed
  and attested as Agent Kourier's image is, and the GitHub Release names their digests. The two recipes' manifests name them, so a team no longer
  builds an image to try either of them.
- **Both images are built on Wolfi**, with CPython 3.14 and no pip, and still run as user 65532. The images that `make prompt-agent-image` and
  `make echo-agent-image` build changed the same way: the entrypoint is `/opt/venv/bin/python agent.py`.

### Recipes

- **New recipe, [`prompt-agent`](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/prompt-agent/README.md)**: an agent
  that a team writes as one `agent.yaml` in Pydantic AI's agent spec format (a prompt, a model, and the MCP tools it may call), served over A2A by
  a small Python server and reached through the `a2a` dialect. The server refuses to start on anything in the file that could give the agent a
  tool the file does not list, and, unless it is told to accept every caller, answers 401 to a caller without its bearer token.
  [Recipes](how-to/recipes.md#define-an-agent-from-a-prompt) has it.
- **The `docs-agent` recipe runs under a kagent agent that owns the channel**, in one of two ways, with the files in its `kagent/` directory:
  as a sub-agent that the owning agent calls as a tool, or as the tools `search_docs` and `read_page`, served by `/docs-mcp`, which the image
  now also holds. For the sub-agent, `A2A_V03_URL` makes the docs agent also serve A2A 0.3 at its root, as kagent 0.9 calls it; the agent
  refuses to start when that URL's path is anything but empty or `/`. Without it nothing changes. The recipe's README has the steps.
- **The `docs-agent` recipe's prompt tells the model to open with the answer**, not a preamble about what it found.

### Other changes

- **On SQLite, a broker logs a warning when its database is newer than it is**, as one on Postgres already did: at start, when the database
  records a migration this build does not have, as after a rollback from a later release, it logs at WARN
  `the database schema is newer than this build of Agent Kourier`, with `newest_known_migration` and `newest_in_database`, and starts. This
  release adds no migration, and changes no CRD.
- **Redaction skips the patterns that cannot match.** Before each costly pattern, the redactor checks that the text holds what any match of it
  needs (a word such as `bearer` or `token`, or a character the patterns allow between letters), and skips it where it does not. What is
  redacted is unchanged.

### Dependencies

- **Go modules:** `github.com/jackc/pgx/v5` from v5.12.0 to v5.13.0, `github.com/slack-go/slack` from v0.35.0 to v0.36.0,
  `golang.org/x/sys` from v0.49.0 to v0.50.0 and `go.yaml.in/yaml/v3` from v3.0.5 to v3.0.6.
- **The Go builder image `golang:1.27.4` is pinned to a new digest**; the tag, and so Go 1.27.4, is unchanged, and so is the runtime base.
- **The recipes' and the AX example's dependencies:** the echo agent's `opentelemetry-api` moves from 1.45.1 to 1.45.2 (CVE-2026-126360), the
  new prompt agent holds `fastmcp-slim` at 4.1.2 or later (GHSA-7xgj-8392-5q4r, GHSA-vqqp-x5qm-5qp4), and
  `go.opentelemetry.io/otel/trace` in the `docs-agent` recipe and in `examples/ax` moves from v1.46.0 to v1.47.0 (GHSA-q7gw-24fr-5p69).

## Version 0.3.2

`agent-kourier init` writes the chart's default route, for a team that brings its own Slack app. The broker's code, the CRDs and the chart's
templates are unchanged by it. The image is built with Go 1.27.4.

### Breaking changes

- **`agent-kourier init` writes custom resources by default, for a broker with no Slack app of its own.** It wrote one values file for the
  ConfigMap route (`config.source: files`), on the broker's own app. It now writes the files of
  [the team-app path](how-to/team-slack-app.md#the-whole-path-recorded): `my-values.yaml`, whose `config.connectionNamespaces` admits the team's
  namespace, and `<team namespace>.yaml`, the team's ChatConnection, AgentBackend and Binding in that namespace. The commands it prints create the
  team's Slack Secret, and the agent token Secret with `--agent-token`, in the team's namespace, and grant them to the broker with a Role there.
  A script that wants the old file passes `--source files`, which writes it as before. On the default route, a run without `--team-namespace`
  asks for it right after the agent's namespace, so answers piped on standard input need one more line there (an empty line, or the end of
  the input, takes the agent's namespace). A command line that gave every other option is now also asked whether the agent verifies tokens,
  unless it passes `--agent-token`. See [Write the files with `init`](how-to/install-with-helm.md#write-the-files-with-init).

### Other changes to `init`

- **`init --force` refuses a path that holds anything but a regular file** (a symbolic link, a directory, a device), with exit code 2 and
  before it writes. It used to fail at the write with exit code 4, or write into a device, or block on a FIFO until it had a reader.

### Documentation

- **[Install on custom resources](how-to/install-with-helm.md#install-on-custom-resources) leads with a broker with no Slack app of its
  own**, and a team that brings its own, as `init` writes it. The install with an app of the broker's is
  [its own section](how-to/install-with-helm.md#a-broker-with-a-slack-app-of-its-own), and the chart's README follows the same order. The
  install clip is the clip of the team-app path.

### Dependencies

- **The Go builder image moves from `golang:1.27.3` to `golang:1.27.4`**, and `go.mod` with it. The Go project lists security fixes to
  `net/http`, the go command, the compiler and the linker in 1.27.4 ([Go release history](https://go.dev/doc/devel/release#go1.27.minor)).

## Version 0.3.1

A documentation release. The broker's code, the CRDs and the chart's templates are those of 0.3.0, so an upgrade from 0.3.0 needs no step. The image is rebuilt from that code and has a new digest, so `helm upgrade` replaces the pod as any upgrade does.

- **The overview pages say a team can bring its own chat app.** The README, the home page, [Why a broker](explanation/why-a-broker.md) and [Write a Binding](how-to/write-a-binding.md) described the platform team's chat app as the only one. They now say who owns which ChatConnection, and Write a Binding asks for an AgentBackend's `allowedNamespaces` only when the backend is in another namespace. The pages on setting one up were already right in 0.3.0.
- **[Install on custom resources](how-to/install-with-helm.md#install-on-custom-resources) splits the resources by owner.** `platform.yaml` holds the ChatConnection and the AgentBackend, each listing the team's namespace. `team.yaml` holds the team's Binding in its own namespace, and the Binding's token Secret goes there too. The recorded clip follows the same split.
- **[Let a team bring its own Slack app](how-to/team-slack-app.md#the-whole-path-recorded) has a recorded clip**: a broker with no app of its own serves a team's app, and the commands it types are on the page.

## Version 0.3.0

A team can now bring its own Slack app: the platform team lists the team's namespace, and the broker serves the ChatConnection the team
makes there beside its own, if it has one: a broker may now have none. The broker serves nothing new until that list admits a namespace; the migration, the metric labels, the new `ApproversReadable` condition and the other changes below apply to every install. Read [Let a team bring its own Slack app](how-to/team-slack-app.md),
and the warning on rolling back below, before you set it. [Upgrade to the release that serves team connections](how-to/upgrade.md#upgrade-to-the-release-that-serves-team-connections) has the checklist for every install: the CRDs, the queries and log alerts to change, what changes with the list empty, and the rollback. An AgentBackend on `dialect: kagent-v1` can also limit which agent namespaces its Bindings may name; nothing changes for one that
does not set the new field, and [the upgrade section](how-to/upgrade.md#upgrade-to-the-release-that-limits-the-agents-a-binding-may-name) says what to read before you set it.

The release candidates before it (`v0.3.0-rc.1`, `v0.3.0-rc.2` and `v0.3.0-rc.3`) are prereleases on GitHub; what they changed is in this entry.

### Breaking changes

Against 0.2.1, with `config.connectionNamespaces` empty and no new field set, nothing in the values or the resources has to change. These can still break an existing install, each described below:

- **Queries and alerts on six series.** They gain a `connection` label, and `agentkourier_chat_connected` stops reporting 0 for a connection that is removed or not served (see [Other changes](#other-changes)).
- **`Ready` can turn `False`.** A Binding that sets `interactions.approverGroups` with `interactions.toolApprovals: true`, and whose bot cannot read a group, reads `False` on the new `ApproversReadable` condition.
- **Error texts changed.** Log lines and status messages that quote a token, a Secret or a Slack call were reworded; the Socket Mode reconnect warning `slack: socket mode connection attempt failed; retrying` is now `slack connection ended; connecting again`; and the digest line says `messages` where it said `bot messages` (see [Other changes](#other-changes)). A log alert that matches the old text must change.
- **`token_expired` on a Socket Mode reconnect is fatal.** It was retried without end.
- **The CRDs must be new.** A Binding CRD of 0.2.1 refuses a `personMessage` trigger, and all three CRDs have new schema hashes.
- **A rollback to 0.2.1 needs steps first** if a team connection has an unposted row, a Binding has a `personMessage` trigger, an AgentBackend sets `spec.agents`, or the broker has no connection of its own (see the warnings below and [Upgrade](how-to/upgrade.md#roll-back-from-this-release)).
- **If you ran `v0.3.0-rc.1` with an empty `config.chatConnection`, check your values** ([below](#a-broker-with-no-connection-of-its-own)). An install that never ran rc.1 is not affected.

### Team-owned chat connections

- **New value `config.connectionNamespaces`** (the environment variable `AGENTKOURIER_CONNECTION_NAMESPACES`), the platform's admission list: the namespaces whose ChatConnections the
  broker serves besides its own, if it has one, with custom resources only (the chart refuses a non-empty list with `config.source: files`). Empty, the default, serves the own connection alone, as before. The broker
  reads the list at start, so a change restarts it. A team makes its ChatConnection and its Secret in its own namespace, and grants the Secret to the broker's
  ServiceAccount with a Role of its own; `rbac.secretNames` or `rbac.secretNamespaces` can grant it instead. Listing a namespace is the platform's consent: the broker then holds the team's app tokens in its memory, as it holds
  the Binding tokens ([Security model](explanation/security.md#a-team-brings-its-own-chat-app)).
- **A pause whose Binding or connection is gone ends.** An interaction that waited for an answer whose Binding was deleted, or whose ChatConnection is not served (deleted, off the list, past the 16, or a second connection of a Slack app), used to stay
  pending, with its session and the runner parked on it, for as long as the broker ran: ending it needs the Binding's agent. The sweep now ends it, posting nothing, `5m` after the first sweep that finds it both past its timeout and orphaned (the sweep runs every 10 seconds, and a restart or a takeover starts the count again; no setting changes the `5m` or the 10 seconds), which is not
  counted as a timeout but as the outcome `orphaned` of `agentkourier_approval_outcomes_total`, and which leaves the agent's task input-required at the agent when the Binding is gone. The session of a Binding that exists and is not served is still kept for as long as it is unserved. A connection that is served and not running
  (its credentials rejected, or not started yet) still keeps its interactions pending ([Limits](reference/limits.md#what-a-pause-may-ask)).
- **At most 16 connections are served,** the own one included, which counts first when there is one, then the others oldest first. No setting changes the 16. A connection past it is not run, and the Bindings it admits say `ConnectionNotServed`
  ([Limits](reference/limits.md#run-the-broker)). The adapter's bounds on the events it holds, and the cap on button presses waiting, are per connection.
- **Slack rejecting a team connection's token stops that connection only,** not the broker: a WARN names it and Slack's code,
  `agentkourier_chat_connected` is 0 for it, its Bindings read `ChannelJoined` `False` with `ChatCredentialsRejected`, and it starts again when its Secret or its
  ChatConnection changes. A changed Secret restarts a team connection by itself, at the next of the broker's looks, which come every 5 seconds. The broker's own connection is as before: a rejected token exits the process with code 1, and new tokens apply at a restart.
- **New `Accepted` reason `ConnectionNotServed`.** A Binding on a ChatConnection that exists and that this broker does not serve says so, once: the broker served the connection before and does not now (its namespace was taken off the list, or it was the broker's own connection and `config.chatConnection` now names another or is empty, and its namespace is not on the list), it is past the 16, or it is a second connection of a Slack app.
  It is written once and not put back, so that a broker that does serve the connection has the last word. The leader counts these Bindings in the new value `ConnectionNotServed` of the `reason` label of `agentkourier_config_binding_rejections`, so that
  `sum(agentkourier_config_binding_rejections) > 0` includes them; a standby reads 0 for it. This happens with the list empty too: after you change `config.chatConnection` to another ChatConnection, the Bindings of the old one, which still exists,
  are told and counted, so that `sum(agentkourier_config_binding_rejections) > 0` fires, until that ChatConnection is deleted or this broker serves it again, even when another release serves it. `ChatUnavailable` on `ChannelJoined` and `ApproversReadable` now also covers a
  connection that is not running, with no call made to Slack ([Binding status](reference/binding-status.md)).
- **A Slack app is served once.** The broker asks Slack (`auth.test`, then `bots.info`) which app a connection's bot belongs to, once per connection and again when its ChatConnection or Secret changes, and serves
  the older of two connections of one app. One app installed in two workspaces counts once. The call is made only when the broker serves a team connection, and the bot's app needs `users:read` for it (Slack's documentation; not verified here), which the manifest in [Configure Slack](how-to/configure-slack.md#what-the-app-is-allowed-to-do) has.
  A message that the bot of another connection of the same broker posts starts nothing unless a Binding's trigger names that bot, so two of its apps in one channel do not answer each other in a loop.
- **Two brokers that admit one namespace** both serve its connections and both open Socket Mode on its apps, so Slack gives each event to one of them and the other misses it. Give each broker its own connections and its own list
  ([Troubleshoot](how-to/troubleshoot.md#the-status-of-a-binding-keeps-changing)).
- **A database migration (0029).** The outbox and the button presses record the connection they belong to, and the broker records the connections it serves. The migration adds columns and a table, and replaces the index on the outbox's channel and thread with one that starts at the connection. Rows written before it, or by an
  older release after a rollback, belong to the own connection: the leader gives them to it when it starts. A broker with no own connection has none to give them to, and its leader refuses to start while any exist (see below).
  A rollback does not undo the index: 0.2.1 deletes the settled outbox rows of an expired session by channel and thread (`channel = ... AND thread_ts = ...`), which an index that starts at the connection does not serve, so on SQLite each
  of those deletions reads the whole `outbox` table until the broker is upgraded again. It costs time, not data (Postgres was not measured).
- **Do not roll back past 0.3.0 while a team connection has an unposted row.** The outbox of 0.2.1 posts every unposted row through its one chat platform, so a post waiting for any team's connection would go out through the platform's app,
  and its sweep can edit a team's prompt and post a timeout notice from the platform's app for an interaction whose session no longer records the task. The guard is the unposted rows, not whether a connection is served now: a team's
  posts stay in the outbox for up to 7 days after its namespace was taken off the list or its ChatConnection was deleted. While the connections are still served: let the team's pending interactions end
  (a pause ends at its deadline, up to 7 days), delete their Bindings, check again that none is pending (one left after the delete is ended by the sweep `5m` after the first sweep that finds it both past its deadline and orphaned, without cancelling the agent's task),
  check that no button press waits (the table `interaction_presses`, by `connection`), and wait until `agentkourier_outbox_pending{connection!="<own>"}` is 0 or absent on the leader while `agentkourier_outbox_pending{connection="<own>"}` is present (a failed scrape has no series at all), with `<own>` the broker's own connection as `namespace/name`, then take
  `config.connectionNamespaces` off the values in the same change as the rollback: the chart of 0.2.1 refuses a `config` key it does not have. To clear rows of a connection that is not served, admit its namespace again (and, if its ChatConnection was deleted, have the team make it again under the same name, for the same Slack app) so that they post, or wait out the 7 days.
  The steps are in [Let a team bring its own Slack app](how-to/team-slack-app.md#take-it-back).
- **Remove `personMessage` triggers before any rollback to 0.2.1 or earlier.** Those releases know the trigger types `mention` and `botMessage` only. On the files route one `personMessage` trigger fails the whole config: the broker exits with code 2 at start,
  and keeps the last good config on a reload. On custom resources the older broker refuses that Binding as invalid (`Accepted` `False`, `InvalidSpec`) and does not serve it.

### A broker with no connection of its own

- **`config.chatConnection` may be left empty with `config.connectionNamespaces` set.** The chart then renders no `AGENTKOURIER_CHAT_CONNECTION`, and the broker serves only the ChatConnections of the listed namespaces, each as a team connection.
  This is the case for a platform whose teams all bring their own Slack apps; nothing names a connection of the platform's. With the variable unset, the broker must have a non-empty list: with neither it exits with code 2 and an error that names both variables, since it reads the list at start and could serve nothing.
  The file route is unchanged: it still has exactly one ChatConnection and refuses both variables. A broker whose listed namespaces hold no ChatConnection yet starts, is ready and serves nothing until one appears. The chart refuses `config.chatConnectionSecret` in this shape (it names the Secret of an own connection, and there is none), and refuses a
  `config.chatConnections` entry whose namespace is not on the list, because the chart would render it and the broker would never serve it. [A broker with no connection of its own](how-to/team-slack-app.md#a-broker-with-no-connection-of-its-own) is the how-to, with the move from an own connection to none.
- **Values whose meaning changed since 0.3.0-rc.1.** Under rc.1 an empty `config.chatConnection` with exactly one `config.chatConnections` entry made that entry the broker's own connection, whether or not `config.connectionNamespaces` was set. Now, with the list non-empty,
  that entry is a team connection, and the render fails if its namespace is not on the list. With the list empty, or `config.chatConnection` set, nothing changed. **If you installed rc.1 with an empty `config.chatConnection`, one entry in `config.chatConnections` and a non-empty `config.connectionNamespaces`, set `config.chatConnection` to that entry before you upgrade**:
  otherwise it loses what only an own connection has (below), and the chart refuses to render if its namespace is not listed.
- **What an own connection has and a team connection does not.** A token that Slack rejects ends the process with code 1 for the own connection, and stops that connection alone for a team's. The own connection's ChatConnection is held when it is deleted, and a team's is dropped when the deletion settles.
  The own connection takes the first of the 16 places and the first claim of its Slack app; with none, the 16 places and every claim go by age (`creationTimestamp`, then namespace and name).
- **With no own connection, a rejected token never ends the process or the pod's readiness.** If Slack rejects every token, the broker stays up and `/readyz` says ready, `agentkourier_chat_connected` is 0 for each connection, and its Bindings read `ChannelJoined` `False` with `ChatCredentialsRejected`. Alert on the metric, with
  the label that your scrape gives the release's namespace in place of `namespace="agent-kourier"` ([Metrics](reference/metrics.md#alert-rules-for-a-broker-with-no-connection-of-its-own) says what each rule misses):

  ```yaml
  - alert: AgentKourierNoChatConnectionUp
    expr: max(agentkourier_chat_connected{namespace="agent-kourier"}) < 1
    for: 10m
  - alert: AgentKourierChatConnectionDown
    expr: agentkourier_chat_connected{namespace="agent-kourier"} == 0
    for: 10m
  ```
- **Rows with no connection stop a broker that has no own connection.** Outbox rows and button presses written before migration 0029, or by an older release after a rollback, have an empty connection. With no own connection to give them to, the leader exits with code 2 when it starts, naming the two counts and
  saying to set `config.chatConnection` to the connection that served them, let the broker start once, and unset it. Only the replica that becomes the leader makes the check.
- **A broker with no own connection cannot be rolled back in place.** 0.2.0, 0.2.1 and 0.3.0-rc.1 exit with code 2 on `config.source: crds` without `AGENTKOURIER_CHAT_CONNECTION` (read in their source, not run). [Upgrade](how-to/upgrade.md#roll-back-from-this-release) has the steps.

### Other changes

- **New trigger type `personMessage`.** A Binding can start a turn on each message a person posts at the top level of its channel, with no mention ([A person's message](reference/chat-triggers.md#a-persons-message)).
  It reads `match` and `exclude` on the field `text` alone, requires `promptTemplate` and `limits` (both `rateLimit` and `maxRunsPerDay`), and refuses `from`, `extract` and `thread`. Its limits count starts, and a mentioned reply in the thread is no start.
  The Slack app needs no scope or event it does not have, and `agent-kourier dry-run` replays the trigger. A Binding with no such trigger is unchanged. The Binding CRD must be new before one is applied: an older CRD refuses the type. Anyone who can post in the channel can start a turn,
  held to the trigger's limits ([Security model](explanation/security.md)).
- **New condition `ApproversReadable`.** A Binding that sets `interactions.toolApprovals: true` and `interactions.approverGroups` now has it: the leader reads each group with the connection's bot at each pass of Binding verification,
  and `Ready` is `True` only when this condition is. A Binding whose bot cannot read a group read `Ready` `True` on 0.2.1 and now reads `False` (`MissingScope` when the app lacks `usergroups:read`, `GroupNotFound`, `GroupsUnreadable`), and `agentkourier_binding_ready` is 0 for it
  (`Ready` is `Unknown` while Slack rate-limits the read). Turns still run: the probe only reports. A Binding with no approver groups has no such condition. The new series are `agentkourier_binding_condition{condition="ApproversReadable"}` and
  `agentkourier_binding_probe_duration_seconds{condition="ApproversReadable"}` ([Binding status](reference/binding-status.md), [Troubleshoot](how-to/troubleshoot.md#approversreadable)).
- **The digest line says `messages`.** The line posted when a trigger's limits stopped starts reads `7 messages not investigated in the last 10m (rate limit).` for every trigger, where it said `bot messages` ([Chat triggers](reference/chat-triggers.md#limits-and-the-digest-line)).
- **A fence after a list marker or a `>` is code.** A fenced block opened after `-`, `*`, `+`, a number and `.` or `)`, or `>` was prose before: a name in it notified, and a closing line with no mark of its own opened a block that hid the rest of the reply.
  Now it is a block, shown at column 0 with the mark dropped, and a name in it does not notify ([Let the agent notify people](how-to/write-a-binding.md#let-the-agent-notify-people)).
- **Slack reconnects are paced by the broker.** A Socket Mode connection that is lost and cannot be opened again used to be retried inside the Slack client library, from 100 ms doubling to 5 minutes with no jitter, so
  after a 3.5 minute outage every connection stayed down until about 409 seconds. Now a failed attempt ends the run and the broker waits 1 second, doubling to 60 seconds, each wait cut at random by up to half so that connections do not
  retry together; the first attempt after a loss, and Slack's refresh, still reconnect at once. A Retry-After from Slack is honoured up to 5 minutes. No setting changes the backoff or the 5-minute cap. `token_expired` on a reconnect is now fatal, where it used to be retried without end:
  the broker's own connection exits with code 1, and a team connection stops (`ChatCredentialsRejected`). The log line `slack connection ended; connecting again` replaces `connection attempt failed; retrying`, and
  `agentkourier_chat_connected` is 0 from when the connection is lost, which is when the library starts to connect again, until it is back ([Limits](reference/limits.md#run-the-broker), [Troubleshoot](how-to/troubleshoot.md#the-log-says-slack-connection-ended-connecting-again)).
- **Every Slack Web API call the broker makes is cut at 30 seconds at most,** the `apps.connections.open` that opens each Socket Mode connection included, so a reconnect on a connection that a network blip left dead fails and is retried instead of hanging for minutes. A call that is not answered in time fails with a client timeout error, which is a transient failure and not a rejection of the credentials. The Socket Mode WebSocket is not cut. No setting changes the 30 seconds ([Limits](reference/limits.md#run-the-broker)).
- **The Socket Mode log lines name their connection.** The `slack: socket mode` lines (connecting, connected, reconnecting) and the warning `slack connection ended; connecting again` carry a `connection` field, the ChatConnection as `namespace/name`, as the broker's other per-connection lines do, so that the lines of two connections can be told apart ([Troubleshoot](how-to/troubleshoot.md#the-log-says-slack-connection-ended-connecting-again)).
- **Some error texts were reworded so that redaction leaves them whole.** The redaction reads a secret word followed by a colon as a name and its value, and had hidden causes such as `token_revoked`. The texts now read, for example, `slack auth.test call: ...` (was `slack auth.test: ...`), `secret <namespace>/<name> cannot be read: ...` (was `secret <namespace>/<name>: ...`), `secret caches for [...] are not synced: ...` (was `secret caches not synced for [...]: ...`), and the Binding message `the Binding requires a token and none was set` (was `... requires a bearer token ...`). A log alert or a script that matches the old text must change.
- **New field `spec.agents` on AgentBackend.** `sameNamespace: true` lets a Binding name an agent in its own namespace, and `namespaces` lists others: at most 256: the API server refuses a longer list, and the broker refuses one at load (`has N items, maximum is 256`), with no setting to raise it. Left out, there is no limit, as before.
  A value that allows nothing is invalid, and so is the field on an `a2a` AgentBackend. All three CRDs change, and each has a new schema hash: the AgentBackend CRD gains `spec.agents`, the Binding CRD's trigger `type` enum gains `personMessage` with rules (CEL) that say which fields it takes, and the ChatConnection CRD's description says that a team may own one. Bring the CRDs and the image to the same release.
- **New `Accepted` reason `AgentNamespaceNotAllowed`.** On custom resources a Binding that names an agent namespace its AgentBackend does not allow is not served, and says so in `Accepted`
  ([Binding status](reference/binding-status.md)). On the files route the config does not resolve: the broker exits with code 2 at start, and keeps the last good config on a reload. The metric
  `agentkourier_config_binding_rejections` gains the reason.
- **An earlier broker ignores the field on custom resources,** and serves every Binding with no limit, so remove `agents` before a rollback.
- **Six series gain a `connection` label,** the ChatConnection as `namespace/name`, so that a broker that serves several connections keeps their numbers apart:
  `agentkourier_intake_total`, `agentkourier_chat_api_calls_total`, `agentkourier_slack_events_shed_total`, `agentkourier_slack_ack_duration_seconds`,
  `agentkourier_outbox_pending` and `agentkourier_outbox_oldest_age_seconds`. A query that sums over the series, or selects by the labels it names, gives what it did.
  An expression that matches one of these series one to one with a metric that has no `connection` label, such as `agentkourier_outbox_pending + agentkourier_reply_queue_depth`,
  stops matching at once and returns nothing: add `ignoring(connection)` or aggregate first. An `on(...)` list needs `connection` once the broker serves two connections.
  A served connection always has its two outbox gauges on the leader (0 with nothing unposted), and a connection that is no longer served has them only while it has an unposted row.
  A connection the broker stops serving has its counter and histogram series deleted, and its `agentkourier_chat_connected` series too, which used to stay at 0: an alert on `agentkourier_chat_connected == 0` no longer fires for a connection that was removed or is not served. [Metrics](reference/metrics.md#series-by-connection) has the rest.
- Dependencies and the Go builder image are updated, including `slack-go` from v0.34.0 to v0.35.0, `a2a-go` from v2.7.0 to v2.8.0 and `modernc.org/sqlite` from v1.61.1 to v1.62.0. The builder image keeps the tag `golang:1.27.3` and has a new digest.

## Version 0.2.1

The chart now grants the served ChatConnection's Secret by itself, so the install on custom resources needs `config.chatConnection` and no `rbac.secretNames` entry for that Secret
(a Secret that a Binding or AgentBackend you apply yourself names in the release namespace still needs an entry or a Role of your own), and a team grants its own Secrets in its own namespace. Read [the upgrade section](how-to/upgrade.md#upgrade-to-the-release-that-grants-the-chatconnections-secret) before you upgrade.

- **New value `config.chatConnectionSecret`.** With `config.source: crds` and `config.chatConnection` set, the chart grants `list` and `watch` on one Secret by name, in the
  connection's namespace: the Secret named like the connection, or the one `config.chatConnectionSecret` names. Before, the same install needed an `rbac.secretNames` entry for that Secret, or the pod
  exited with code 3 in a loop. An existing entry keeps working, and a Secret named twice is granted once. The value is ignored with `config.source: files`, when
  `config.chatConnection` is empty, and when `config.chatConnections` renders that connection (its `credentialsSecretRef` is granted instead).
- **One new named grant on upgrade.** An install on `crds` that sets `config.chatConnection` gains `list` and `watch` on one more Secret: the one named like the connection, or the one `config.chatConnectionSecret` names, unless an `rbac.secretNames` entry already names it or
  `config.chatConnections` renders that connection. When the connection reads a Secret of another name, the derived Secret may not exist: the broker never reads it, so the grant covers no Secret until one of that name is created
  in that namespace. Set `config.chatConnectionSecret` to the real name.
- **A Role in the connection's namespace.** With `config.chatConnection: other-ns/name`, the chart now renders the Role and RoleBinding `<fullname>-secrets` in `other-ns`. A Role of that
  name that someone made by hand there conflicts: the upgrade fails with `invalid ownership metadata` until you delete it or rename it.
- **A team grants its own Secrets.** The documentation now leads with the team's own Role and RoleBinding in its namespace, two `kubectl` commands, in
  [Write a Binding](how-to/write-a-binding.md#load-it) and [Let a team add its own Binding](how-to/migrate-config-source.md#let-a-team-add-its-own-binding). `rbac.secretNames` stays for Secrets the
  chart cannot see and the platform owns, such as a Binding's token in the release namespace.
- **The examples name the Slack Secret `slack`,** like the ChatConnection `slack`, so that the default derivation finds it.
- **`CredentialsUnreadable` says which cause it is.** The status message of a Binding whose token or header Secret cannot be read now says whether the Secret does not exist, lacks the key, holds an empty
  value or a control character, is not of type `Opaque`, or was denied to the ServiceAccount (and then what grant it needs), or that the Binding requires a token and has none. It names the Secret and the
  key only when the Secret is in the Binding's namespace: [Binding status](reference/binding-status.md).

## Version 0.2.0

Custom resources are now the recommended way to configure Agent Kourier on Kubernetes, and the chart's default: `Binding`,
`AgentBackend` and `ChatConnection` are CustomResourceDefinitions, and `Binding.status.conditions` shows in `kubectl get`. An install
that names no route (`config.source: crds` is the default) gets the CRDs and a broker that reads them.
The ConfigMap route still works. It was built for a quick proof of concept, and it stays for that use and for installs that cannot
add CRDs, but it is no longer the default. **A ConfigMap install that upgrades with its old values file must now name its route**:
read [Breaking changes](#breaking-changes_1) first.

The release candidates before it (`v0.2.0-rc.N`) are prereleases on GitHub; what they changed is in this entry.

### The custom resource route

Each team writes its own `Binding` in its own namespace, committed next to the agent's manifests, and sees what became of it with
`kubectl`. The broker's leader writes `Binding.status.conditions` (`Accepted`, `BackendReachable`, `ChannelJoined` and `Ready`), and
`kubectl get akbinding -A` shows them:

```text
NAMESPACE   NAME          READY   REASON                       CHANNEL   AGE
team-a      team-a-chat   True    OK                           C2        4m
team-a      late          False   Accepted:ChannelConflict     C1        1m
```

- **Short names.** `akbinding`, `akbackend` and `akconnection`, all in the category `agentkourier`.
- **A bad object does not stop the others.** The broker judges each object on its own: it keeps serving the last good generation of an
  object that is edited into an invalid state, and does not serve an invalid new one. [A bad object](explanation/config-routes.md#what-a-bad-object-does) says
  what each case does, and [Choose a config route](explanation/config-routes.md) says which route fits.
- **Secrets are followed one at a time.** The broker watches each Secret the config names, not a whole namespace, so a reload can add a
  Secret in a namespace it was not watching, with no restart, as long as its Role grants `list` and `watch`.
- **The start line names the route.** The first line of the log is an INFO `config route` with `config_source` (`crds` or `files`) and, with
  `crds`, the `connection` the broker is configured to serve, so a pod's log says which route it reads.
- **A bad ChatConnection name stops the start.** With `crds`, `AGENTKOURIER_CHAT_CONNECTION` must be a namespace that is a DNS-1123 label and a
  name that is a DNS-1123 subdomain. Anything else exits with code 2, with a message that does not show the value. The chart sets it from `config.chatConnection`.
- **Move between the routes.** [Move the config between a ConfigMap and custom resources](how-to/migrate-config-source.md) is a
  two-upgrade path in each direction. [Install with Helm](how-to/install-with-helm.md#install-on-custom-resources) is the path for a new install.
- **Needs** Kubernetes 1.33 or later, and a right to create CustomResourceDefinitions (they are cluster-scoped). The ConfigMap route has no minimum version.
  With `rbac.create`, the chart also makes a ClusterRole, bound to the ServiceAccount: `list` and `watch` on the three kinds in every namespace,
  `patch` on `bindings/status`, and `get` on the three CRDs by name (`charts/agent-kourier/templates/rbac.yaml`). With `rbac.create: false`, the operator creates it.
  The Secret Roles name single Secrets by `resourceNames`, so the Secrets of a team's own Binding, which the chart cannot see in `config.*`, need
  `rbac.secretNames`, `rbac.secretNamespaces` or a Role the team makes; a namespace in `rbac.secretNamespaces` keeps a namespace-wide Role ([Install with Helm](how-to/install-with-helm.md#install-on-custom-resources)).

The status is written only on this route. On the ConfigMap route, the leader still probes each Binding and reports the result in
metrics and logs: see [Binding verification](reference/metrics.md#binding-verification) and
[Binding status](reference/binding-status.md).

### Breaking changes

- **The chart's default is `config.source: crds`.** An install that reads a ConfigMap and upgrades with the values it has gets the new
  default. To keep the ConfigMap route, put this in the values file:

    ```yaml
    config:
      source: files
    ```

    With `files`, an unset `crds.install` is false, so the render is 0.1.6's plus the `GOMEMLIMIT` variable (below), with no CRD. To move to
    custom resources instead, follow [the migration](how-to/migrate-config-source.md).

    An unset `config.source` still reads as `files`, so the chart keeps the ConfigMap route for the values of a `helm upgrade --reuse-values`
    from 0.1.6, which have no such key. A chart test renders with each new key unset and gets the render of the ConfigMap route
    (`TestUpgradeFromAReleaseThatLacksTheNewValues`). What Helm hands the chart for a new chart's default under `--reuse-values` is Helm's
    behaviour and is unverified here.
- **An unset `crds.install` follows the route.** It is true with `crds` and false with `files`. An explicit `true` or `false` always
  wins, so `config.source: files` with `crds.install: true` installs the CRDs before any custom resource exists (step 1 of the
  migration), and `crds.install: false` on `crds` is for a platform that applied the CRDs itself.
- **Kubernetes 1.33 or later is needed for the custom resource route.** The ConfigMap route has no minimum.
- **The chart refuses these values on the default route.** The render fails before anything is applied, so nothing changes, and each
  message says how to keep the ConfigMap route or how to move:

    The messages that name the default say "the chart's default unless your values set it", since the chart sees the same value whether your values say `config.source: crds` or leave it out.

    | Your values | Why it is refused |
    |---|---|
    | `config.existingConfigMap`, and no `config.source` | The broker would read custom resources, mount no ConfigMap and ignore it. |
    | `config.agentBackends`, `config.chatConnections` or `config.bindings`, on a cluster that does not serve the CRDs | A release cannot install the CRDs and the custom resources of them in one apply, so the CRDs must be served first. |
    | A cluster older than Kubernetes 1.33 | The route needs 1.33. With `helm template`, pass `--kube-version`. |
    | `config.chatConnection` empty, and `config.chatConnections` without exactly one entry | The broker needs to know which ChatConnection it serves. |

    [Upgrade](how-to/upgrade.md#upgrade-to-the-release-that-makes-custom-resources-the-default) shows each message and what the chart does not
    refuse: an install that already serves the CRDs and upgrades with inline `config.*` has them rendered as custom resources, which is
    step 2 of the migration, done by the upgrade.
- **A config the broker accepted before can be refused now.** This release bounds what a config may hold, on both routes. On the
  ConfigMap route a config over a bound fails at start-up (the broker exits with code 2) and a reload keeps the last good config. On the
  custom resource route a Binding over a bound is not served after the upgrade and says `Accepted=False` with the reason
  `InvalidSpec`. Run `agent-kourier dry-run` of the new release against your config before you upgrade
  ([how](how-to/install-with-helm.md#check-the-values-before-you-install); a values file from 0.1.6 needs `config.source: files` first). What changed:
    - [Triggers, extract entries and headers](how-to/upgrade.md#upgrade-to-the-release-that-bounds-triggers-extract-entries-and-headers) are capped.
    - [A `promptTemplate`](how-to/upgrade.md#upgrade-to-the-release-that-limits-prompt-templates) may use only text, comments, `if`, `with`, the view's fields,
      `.Fields.<name>` and a short list of functions. `range`, `define`, `template`, `block`, variables, `print`, `printf`, `println`, `html`, `js`,
      `urlquery` and `call` are refused, and a template that writes more than 262144 bytes starts no turn and is counted as `render_error`, which a dry run cannot catch. A
      `promptTemplate` is capped at 8192 bytes, `spec.agent.name` at 253 and `spec.identity.userId` at 256.
    - [Each regular expression of a trigger, and all those of a Binding](how-to/upgrade.md#upgrade-to-the-release-that-bounds-what-a-triggers-regular-expressions-cost), are held to a size, and a match reads the first 16 KiB of its field.
    - [`interactions.timeout` and `session.threadTTL`](how-to/upgrade.md#upgrade-to-the-release-that-caps-the-pause-timeout-and-the-thread-ttl) have caps. Lowering a `threadTTL` can delete sessions.
    - [Namespaces, chat IDs, URLs, header values and durations](how-to/upgrade.md#upgrade-to-the-release-that-bounds-namespaces-chat-ids-urls-header-values-and-durations) are bounded, and an AgentBackend `url` may have no query or fragment.

    [Limits](reference/limits.md) lists every limit you can run into.
- **The upgrade restarts the pod.** The chart now sets `GOMEMLIMIT` at 80% of `resources.limits.memory`, which changes the pod
  template. With SQLite (`Recreate`) there is a gap while the new pod starts. See
  [the release that sets GOMEMLIMIT](how-to/upgrade.md#upgrade-to-the-release-that-sets-gomemlimit).
- **The broker bounds what it reads from an agent.** Release 0.1.6 had no bound of its own: only the A2A library's 10 MiB on each line of a
  stream, and a reply was shown cut at 12000 UTF-16 units. Now one event of a stream is read up to 2 MiB, the answer to a call that is not a
  stream up to 4 MiB, what a turn holds of the agent's reply is 341 KiB, and the agent card is 1 MiB (`internal/dialect/dialect.go`, `MaxCardBytes`).
  A turn past one of the first three ends with the new outcome `response_too_large`, the thread is told, and, when the agent had named a task,
  the task is cancelled once; a reply that named no task has none to cancel. An agent card over 1 MiB fails every call to that agent, so the Binding takes no turn and its probe
  reports it; a follow of a task that meets it cancels nothing. A message that is sent again, whose outcome is unknown, is not sent, for an agent that can list tasks, when the list cannot be read in
  full: the turn ends with the new outcome `lookup_incomplete`. See
  [the release that bounds what it reads](how-to/upgrade.md#upgrade-to-the-release-that-lowers-the-bounds-on-what-it-reads).
- **Another bot's mention now starts a turn.** `chat.botMentions` defaults to `any`, so every existing Binding with a `mention` trigger takes the
  mentions of Agent Kourier by other bots and incoming webhooks that can post in its channel, held to fixed limits. At 0.1.6 they were ignored. To keep
  that, set `chat.botMentions: none` in the config you upgrade with, in the same upgrade, since 0.1.6 refuses the field. See [the release that lets another bot's mention start a turn](how-to/upgrade.md#upgrade-to-the-release-that-lets-another-bots-mention-start-a-turn).

### Features

- **Binding verification.** The leader probes each Binding on a schedule: it fetches the agent's card on a fresh connection and reads
  the Slack channel, and reports the result as metrics and log lines (and, on custom resources, as status). The scopes it uses are
  `channels:read` and `groups:read`, which the Slack manifest already asks for.
  [A Binding is not ready](how-to/troubleshoot.md#a-binding-is-not-ready) lists each reason and what to do.
- **Working updates show as progress.** The status updates an agent sends in state `working` now title the `Working…` card or show as a
  progress line in `steps-and-activity`, and `activity` counts them. See
  [Working updates](reference/agent-contract.md#working-updates).
- **Agents can notify people.** A Binding declares people under `chat.mentions`, and an agent that writes `@name` for a declared name in
  its final reply notifies that person in a message of its own. `chat.maxMentionsPerTurn` (default 2) caps it. See
  [Write a Binding](how-to/write-a-binding.md).
- **Another bot's mention can start a turn,** which is how one agent calls another (see Breaking changes for the default). See
  [Let another agent start yours](how-to/any-bot-trigger.md#let-another-agent-start-yours).
- **Limits are documented.** [Limits](reference/limits.md) is a page of every cap, bound and timeout you can run into, with what happens
  past it, guarded against the code by a test.

### Fixes

- Two controls that end the same pause no longer both ask the agent to cancel its task.
- A Slack update that Slack refuses as `block_mismatch`, as a recovered turn can meet on a message it did not write, is no longer retried every 2 seconds.
- A vote pressed before the reply's message place is recorded is retried instead of ignored.
- A 403 at the agent card fetch is no longer treated as a rejected credential: kagent 1.0 answers it for an agent that does not exist, so the
  token is not re-read and the credential-rejection metric is not raised. A 401, and a 403 on an A2A call, stay rejected credentials.
- The events of one Slack channel are handled in order on a lane of their own, so a slow match no longer delays the acknowledgments of
  other channels. Past 16 events held for a channel, or 256 in all, an event is left unacknowledged and counted in
  `agentkourier_slack_events_shed_total`.
- Dependencies and the container base images are updated, including `slack-go` to v0.34.0.

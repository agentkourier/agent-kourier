# Release notes

What changed in each release, newest first, and what you must do about it. The steps for an upgrade are in
[Upgrade](how-to/upgrade.md), which has a section for each change that needs one; this page says what changed and points to it.

## Version 0.6.0 (unreleased)

A ChatConnection can take signed JSON webhooks from another service, such as GitHub, Jira Cloud, Bitbucket Cloud or Linear, and start
sessions of an agent from them: `spec.platform: webhook`, on the chat listener. The webhook platform is experimental. Agent Kourier posts
nothing on such a connection; the agent answers on the service with its own tools. [Start agents from webhooks](how-to/receive-webhooks.md)
has the setup, and a page for each of the four services. [Upgrade to the release that takes webhooks](how-to/upgrade.md#upgrade-to-the-release-that-takes-webhooks)
has the upgrade, and how to roll it back. The release also takes Microsoft Teams ChatConnections: [Upgrade to the release that takes
Microsoft Teams ChatConnections](how-to/upgrade.md#upgrade-to-the-release-that-takes-microsoft-teams-chatconnections) has its upgrade.

### Breaking changes

- **Apply the CRDs again.** The ChatConnection CRD changes for `spec.webhook` and the platform `webhook`, and the Binding CRD for the trigger
  type `webhookEvent`, the output `none` and the channel of a webhook Binding; the AgentBackend CRD is unchanged. The chart applies them with
  the release when `crds.install` is true, or unset on `config.source: crds`. If you apply them yourself, apply
  `charts/agent-kourier/files/crds` of this release. Until then the API server refuses the new fields and values, and a broker on custom
  resources logs at WARN `the schema hash of a CRD is not the one this broker was built with` and sets
  `agentkourier_crd_schema_state{state="mismatch"}` to 1 for the two CRDs. A config of Slack connections alone loads as before.

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
- **Four recipes**: `webhook-github`, `webhook-jira`, `webhook-bitbucket` and `webhook-linear`, each a ChatConnection, a Binding and an
  AgentBackend for an A2A agent of your own.

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

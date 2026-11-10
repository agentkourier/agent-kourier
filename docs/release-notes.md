# Release notes

What changed in each release, newest first, and what you must do about it. The steps for an upgrade are in
[Upgrade](how-to/upgrade.md), which has a section for each change that needs one; this page says what changed and points to it.

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

The release candidates before it (`v0.3.0-rc.1` and `v0.3.0-rc.2`) are prereleases on GitHub; what they changed is in this entry.

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

# Upgrade

For an operator running the Helm chart: at the end, Agent Kourier runs a newer release with its sessions, outbox and
audit log intact, and you know how to roll back.

Each release publishes a signed multi-arch image and a chart whose default image is that image by digest, as
[Install with Helm](install-with-helm.md#the-release) describes. The current release is `0.1.6`. To run a build that is not
released, see [Build from source](install-with-helm.md#build-from-source).

## Upgrade the chart

Pass the values file you installed with (`my-values.yaml` in [Install with Helm](install-with-helm.md#write-the-values)):
`helm upgrade` replaces the release's values with the ones you give it.

```sh
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <new version> \
  -n agent-kourier -f my-values.yaml
```

- **SQLite (the default).** The Deployment's strategy is `Recreate`: the old pod stops and releases the volume
  before the new one starts, so Agent Kourier is down for its start-up. Its volume carries the sessions, the outbox
  and the reply queue across, and recovery continues each conversation where it stopped.
- **Postgres with a standby.** The strategy is a rolling update. The leader hands the lease over when it stops, so
  the other replica takes over after the old leader's ordered shutdown (up to 25 s), at its next attempt.
- Agent Kourier applies its own database migrations at start.

Deploy by digest. A released chart pins its image by digest, so `--set image.tag=...` alone changes nothing: set
`image.digest`, or `image.digest=""` together with the tag.

## Roll back

Install the previous chart version, whose default image is that release's digest, or set `image.digest` back to the
previous release's digest. A bad release is not deleted or retagged: a new tag carries the fix.

**A Binding that sets `chat.progress` does not load on `0.1.2`.** That release has no `progress` field, and its loader
rejects a field it does not know, reporting `spec.chat.progress: unknown field` for the Binding. One Binding that fails
fails the whole config: at start-up the broker exits with [code 2](../reference/cli.md#exit-codes) and the pod never
becomes Ready, and a running broker that reloads keeps its last good config, as
[the release that writes triggers in full](#upgrade-to-the-release-that-writes-triggers-in-full) describes. Before you
roll back to `0.1.2`, remove `progress` from every Binding, in `config.bindings` or in the ConfigMap you name in
`config.existingConfigMap`. `0.1.2` then shows no `Working…` card or line and sets no assistant status.

## Verify a release before you deploy it

Deploy only digests that `cosign verify` accepts: an unsigned digest is not a release. The repository's
[RELEASING.md](https://github.com/agentkourier/agent-kourier/blob/main/RELEASING.md#verify-a-release) has the commands
for the image's signature, SBOM and provenance, and for the chart.

## Upgrade across the rename

<!-- name-check: off -->
The chart was called `courier` before the project became Agent Kourier. A release installed under that name
(`helm install courier ...`) gets the fullname `courier-agent-kourier` from the renamed chart, so an upgrade makes a
new Deployment and, with SQLite, a new and empty `<fullname>-data` claim beside the old one.
<!-- name-check: on -->

The old claim survives, because `persistence.retainOnUninstall` defaults to true. To carry the data over, upgrade with
`persistence.existingClaim: <old release>-data`, or keep the old names with `fullnameOverride: <old release>`.

**The SQLite file.** The default `persistence.dbFile` is now `agent-kourier.db`. A database from before the rename is in
`courier.db`, whatever the release is called: set `persistence.dbFile: courier.db` to keep it, or Agent Kourier starts
with an empty database. There is no fallback in the code.

**Postgres.** Nothing is stored in the chart's volume: migration 0024 drops the old purge function and setting, and
renames the stored system user and the auto-deny decider. The Deployment rolls with `maxSurge: 1` and
`maxUnavailable: 0` at any replica count, so the new pod runs 0024 while the old leader is still working, and an
alert's turn that the old leader enqueues or runs until it stops is read by the new version as a person's turn. Scaling to one replica
does not avoid that; stopping every old pod first does. Upgrade once with `replicaCount: 0` (the kill switch), wait
until the old pods have terminated, then upgrade again with `replicaCount` restored. Agent Kourier is down in between;
if `podDisruptionBudget` is enabled, switch it off or to `maxUnavailable` before scaling to zero (see the chart's
values).

## Upgrade to the release that verifies Bindings

This release has the leader check each Binding and report the result. No Binding fails to load for it, and it changes nothing about turns or `/readyz`.

- **New metrics and log lines.** See [Binding verification](../reference/metrics.md#binding-verification). To alert on it, see [Alert on what matters](export-telemetry.md#alert-on-what-matters).
- **New calls.** On each pass, the leader fetches each Binding's agent card and calls Slack's `conversations.info` for its channel. The scopes are `channels:read` and `groups:read`, which the manifest already asks for. An app without them reads `False/MissingScope`, and nothing else changes.
- **New log lines on the first start.** A Binding that was already broken logs a WARN. See [A Binding is not ready](troubleshoot.md#a-binding-is-not-ready).

## Upgrade to the release with step cards in alert threads

This release shows an alert thread's tool calls as step cards, as a chat thread does. No Binding fails to load for it.

- **An alert thread keeps a card for each tool call.** The reply's first post is unchanged: it shows the line of the latest
  tool call, or `Working…`. Every update after it shows a step card for each tool call above the text, in place of the
  line, and the final message keeps them, so an investigation ends with the trail of what it checked. With
  `steps-and-activity` (the default) a gap of 3 seconds is a `Working…` card, which the next tool call takes over, as in a
  chat thread. The reply shows the latest 20 cards, under a line that counts the earlier ones.
- **One more update.** A reply that has cards when its first post is made makes one more update, once the throttle allows,
  to show them, in place of the post's line if it shows one.
- **A reply with cards and no text** ends with the cards, and no longer says `(no output)`.
- **Unchanged:** `progress: activity` (one counting line that names no tool), `output: final`, and a platform without step
  cards, which keeps the line. If Slack refuses an update with cards, it is sent again without them, and the reply shows the
  line from then on.

See [Output](../explanation/threads-and-sessions.md#output).

## Upgrade to the release that writes triggers in full

A `botMessage` trigger is written in full in this release: nothing fills it in from a preset, it names its sender and the
message fields it reads in terms that do not belong to one chat platform, and it must set its limits. A build from
before these changes took a shorter trigger, so a trigger written for it meets them all together. A Binding
with a `botMessage` trigger fails to load until you change it, so change your Bindings before you upgrade. A Binding
with only `mention` triggers loads as before.

**What changed in a `botMessage` trigger.**

- **`preset:` is not a field.** Nothing reads or translates the old value. The trigger the preset stood for is written
  out in [Investigate Alertmanager alerts](investigate-alertmanager-alerts.md#add-the-trigger).
- **Limits are required.** A trigger without `limits.rateLimit` (all of `maxRuns`, `per` and `maxConcurrent`) or without
  `limits.maxRunsPerDay` fails to load. Before, such a trigger loaded and started with no limit. `maxRunsPerDay: 0` still
  means no daily cap, and now has to be written.
- **Three names changed, and their values did not.**

  | Before | Now | Where |
  |---|---|---|
  | `botId` | `id` | `from` |
  | `titleLink` | `link` | the keys of `match` and `exclude`, and `field` of an `extract` entry |
  | `attachmentText` | `body` | the keys of `match` and `exclude`, and `field` of an `extract` entry |

  `from.id` is still the bot ID (`B...`) on Slack. `link` still holds the first legacy attachment's `title_link` and
  `body` its `text`; `text`, `title`, `footer` and `fallback` keep their names.
- **`from.kind` is new and optional.** It is `bot`, the default and the only value. A trigger takes a message only when
  the sender's ID and kind both equal `from.id` and `from.kind`; every Slack bot is of kind `bot`.

**What an old trigger reports.** The loader names the file, the Binding and the field of each problem. A trigger written
for the last tag that sets only a bot ID (as `botId`) and a preset reports all of these, in one run:

```text
b.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].from.botId: unknown field
b.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].preset: unknown field
b.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].from.id: required
b.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].limits.rateLimit: required: set maxRuns, per and maxConcurrent; see the example in how-to/investigate-alertmanager-alerts (https://agentkourier.dev/how-to/investigate-alertmanager-alerts/)
b.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].limits.maxRunsPerDay: required: set the cap for a day, or 0 for no cap; see the example in how-to/investigate-alertmanager-alerts (https://agentkourier.dev/how-to/investigate-alertmanager-alerts/)
b.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].promptTemplate: required
```

A trigger that did not use a preset reports only some of these. One that still has a `match`, `exclude` or `extract` on
`titleLink` or `attachmentText` also reports `unsupported message field`, and the error lists the field names that exist.

**What the broker does with a Binding that does not load.** With the files route (the default, and the only one in this section's
releases), one Binding that fails fails the whole config, so none of them is served. With custom resources only that Binding is left out: see
[Choose a config route](../explanation/config-routes.md#what-a-bad-object-does).

- *At start-up*, the broker does not start. It prints the error to stderr and exits with
  [code 2](../reference/cli.md#exit-codes), and the pod never becomes Ready. With SQLite (the default) the Deployment's strategy is `Recreate`, so the old pod is already gone when the
  new one fails: no broker runs, and no alert is investigated, until you fix the Binding or [roll back](#roll-back).
  Alertmanager's own receivers still notify people. With Postgres the strategy is a rolling update with
  `maxUnavailable: 0`, so the new pod that never becomes Ready does not take the old pod away, and the old pod keeps its
  config.
- *On a reload*, when a running broker finds the files changed, it keeps the last good config. It logs `config reload
  failed; keeping the last good snapshot`, counts `agentkourier_config_reload_errors_total`, and sets
  `agentkourier_config_stale` to 1; `/readyz` does not fail for it. With a ConfigMap the chart writes and
  `config.rollOnChange` on (the default), a change to the Bindings rolls the pod, so a bad Binding shows as a failed
  start. A reload happens with `config.existingConfigMap`, or when the mounted files change in a running pod.

The broker's log line, at start-up and on a reload, cuts the error text after 300 bytes, so with several errors you may see
only the first, or part of it. Run `agent-kourier dry-run` (step 3 below) to see every error: it prints the
whole text.

**Migrate each Binding that has a `botMessage` trigger.**

1. If it sets `preset:`, replace the trigger with the one in
   [Investigate Alertmanager alerts](investigate-alertmanager-alerts.md#add-the-trigger). Apart from `match.link`, it is
   the old preset written out: the same regexes, a 4 hour cooldown, 5 starts in 10 minutes with 2 at once, 40 a day, and
   the prompt. Keep `match.link` only if your preset trigger matched the receiver in the title's link, set to your
   receiver. It is not part of the old preset, and the alerts of your other receivers start nothing if you leave it in.
   Then carry over what you overrode, under the new names: an `exclude`, `thread`, `limits` or `promptTemplate`, and a
   `match`, `exclude` or `extract` on `titleLink` or `attachmentText` becomes one on `link` or `body`.
2. If it does not set `preset:`, rename `botId`, `titleLink` and `attachmentText` as the table says, and add the
   `limits` you want.
3. Replay the channel's history with `agent-kourier dry-run` from the release you are upgrading to, against the edited
   Bindings ([Tune triggers with a dry run](tune-triggers.md)). It loads them with the broker's own loader, so it prints
   the same errors the broker would, and each message you expect to start an investigation should come out `matched`.
   The older release's binary proves nothing: it accepts `preset:` and `botId` and rejects the new keys.

**What else changes.**

- **The session source.** A trigger start records the source `botMessage`, in `sessions.origin_source` and in the
  prompt's `.Source`; a start from a preset trigger recorded `alertmanager`. A `promptTemplate` that tested `.Source`
  against `alertmanager` needs the new value. Sessions created before the upgrade keep `alertmanager`, and a vote on
  their messages still counts under the alert's name ([Stored data](../reference/stored-data.md#feedback)).
- **The first prompt names the fields differently.** The block of untrusted data that follows a trigger's prompt has an
  element for each field the message has, named by the field: `<link>` and `<body>` where `<titleLink>` and
  `<attachmentText>` were. An agent instruction that names those elements needs the new names. A prompt template reads
  extracted values through `.Fields.<name>`, which are your own extract names, so a template is unaffected.
- **The dry run names the new fields,** for example `trigger 1: match link`, and for a sender of the right ID and the
  wrong kind it says `sender kind`. Regexes are tried in the order of the field names, so a message that fails two of them
  can now show a different field first.
- **The acknowledgment reaction.** A Binding that left `chat.acknowledgment.emoji` out used to have
  `hourglass_flowing_sand` written into it when it loaded. It now stays empty, and Agent Kourier's Slack adapter supplies
  the same reaction, so nothing changes on Slack. An `emoji` you set is used as before.
- **Alerts that are firing keep their threads.** An alert thread opened before the upgrade is looked up by the same
  connection, channel, bot ID and key as before, so a repeat of an alert whose thread is open finds that thread and adds a
  "Fired again" note to it, instead of starting a second investigation.
- **The actor names the platform.** The `agentkourier.dev/actor` metadata of a turn a person wrote gains `platform`
  (`slack`). On Slack the text prefix is unchanged, `[Slack: Name (U123) <email>]`
  ([Agent contract](../reference/agent-contract.md)).

## Upgrade to the release with progress modes

This release adds `chat.progress` to the Binding, and its default changes what a Binding that does not set it shows. No
Binding fails to load for it.

- **A chat thread gains `Working…` cards.** When 3 seconds pass with no new text and no tool call under way, a card titled
  `Working…` begins in the stream. The next tool call takes that card over, so it carries the tool's name; if text comes
  first, the card is retitled `Wrote the answer`, and `Wrote an update` if a tool call follows that text; a card still open when the turn ends becomes `Finished`, or `Stopped` (an
  error) if the task failed or was cancelled. A thread that used to show one card for each tool call can now show one more,
  and each gap longer than 3 seconds costs at most three more writes to Slack (the card's begin; its end, when no tool call
  takes it over; and its retitle, when text ended it and a tool call follows that text). This needs a platform with step cards; an agent that marks no tool parts (see
  [the next section](#upgrade-to-the-release-that-reads-tool-steps-on-every-dialect)) gets `Working…` cards alone.
- **An alert thread gains a `Working…` line.** After the same 3 seconds, the message the investigation updates shows
  `Working…` as a line of small print in place of the line of a tool call that returned, under the text so far if there is
  some, and as the message's first post if nothing else has been said. The next text or tool call replaces it. Each gap
  costs one more message update. A turn that is silent for 3 seconds and then ends with nothing to show, or fails, now
  leaves a message saying `(no output)` where under `steps` it posted nothing.
- **An alert thread on Slack gains an assistant status.** Agent Kourier sets Slack's `is working…` status on the thread when
  the turn starts and again after each write but the last, because a reply in the thread may take it off, and clears it when the turn ends.
  If Slack answers that the app or workspace cannot use the method, Agent Kourier logs that once at info level and sets no
  status on that connection until it restarts; any other error stops it for that turn only. The manifest now asks for
  `assistant:write`, with which we have seen the status render; an app made from an older manifest may get the status anyway
  or be refused, and a refusal is only logged. Which scope the call needs is unverified; see [Configure Slack](configure-slack.md#what-the-app-is-allowed-to-do).
  Each alert turn makes one `assistant.threads.setStatus` call when it starts, at most one after each write of its message
  before the last, and one to clear the status when it ends.
- **`output: final` is unchanged.** It posts once when the turn ends, whatever `progress` says.

To keep the cards and lines as they were, set `progress: steps` on the Binding. `activity` shows one counting card or line
that never names a tool (a tool approval prompt still does). See [Output](../explanation/threads-and-sessions.md#output).

**Add `assistant:write` to an existing Slack app.** An app made from an older copy of the manifest lacks the scope: add
`assistant:write` to the app's bot token scopes and reinstall the app to the workspace. Without it, Slack may refuse the
call. A refusal whose code says the app, token or workspace cannot use the method (`missing_scope`,
`not_allowed_token_type`, `not_allowed`, `access_denied`, `feature_not_enabled`, `no_permission` or `unknown_method`) turns
the status off for that connection until the broker restarts, with one info log line. Any other error stops the status
for that turn, with a warning. Neither fails a reply. See
[An alert thread shows no "is working…" status](troubleshoot.md#an-alert-thread-shows-no-is-working-status).
`progress: steps` sets no status, and makes no call for it.

## Upgrade to the release that reads tool steps on every dialect

This release reads tool calls and results the same way on every dialect: a data part whose metadata holds exactly one of
`adk_type` (Google ADK's A2A converter), `kagent_type` (kagent 0.9.x) or `kagent.dev/a2a/part-type` (kagent 1.0), set to
`function_call` or `function_response`, and whose data has a non-empty `name`, is a tool part, in an artifact or in a
message from the agent ([Tool calls and results](../reference/agent-contract.md#tool-calls-and-results) lists where).
Before, only the `kagent-v1` dialect read tool parts, only those marked `kagent.dev/a2a/part-type`, and only in artifacts.
No Binding fails to load for it.

- **More threads show tool steps.** On the `a2a` dialect, an agent that marks its tool parts, such as kagent 0.9.x or an
  agent built on Google ADK's converter, now gets a step card for each tool call in a chat thread, and the line of the
  latest call in an alert thread, each with a summary of the call's arguments from which secrets are redacted. Such a
  thread makes more writes to Slack than it did. The same holds on `kagent-v1` for a part marked with either of the other
  keys, and for a marked part in a message.
- **Some parts that showed no longer do.** A marked part whose data has no `name`, or an empty one, is not a tool part, and
  neither is a part whose metadata holds two of the keys with a marker value. On `kagent-v1` such a part showed as a step
  before.

For fewer cards, set `progress: activity`: one card (or, in an alert thread, one line) for the wait before the first
text, which counts the calls and names none; a tool approval prompt still names the tool. `output: final` shows no cards
or lines, and posts once when the turn ends. See [Output](../explanation/threads-and-sessions.md#output).

## Upgrade to the release that reads kagent's extension on the a2a dialect

This release lets an `a2a` Binding use kagent's human-in-the-loop extension, `https://kagent.dev/extensions/hitl/v1`, when
the agent card lists it under `capabilities.extensions`, as kagent 1.0's does. No Binding fails to load for it. An agent
whose card does not list the extension sees no change. For one whose card does, such as a kagent 1.0 agent on the `a2a`
dialect:

- **Every A2A call carries `A2A-Extensions: https://kagent.dev/extensions/hitl/v1`**, and the agent sends its structured
  pauses.
- **A tool approval is an approval.** With `interactions.toolApprovals: true` it is posted with approval buttons, and
  `approverGroups` gate it. Without it the approval is denied through the dialect, with a notice in the thread, as on
  `kagent-v1`; it used to be posted as a text question (`askUser: true`) or cancelled as one (`askUser: false`).
- **A question with choices is shown with them**, and a structured question takes its answer from the prompt's controls
  only: a typed reply in the thread does not answer it, even a question with no choices.
- **A card that marks the extension `required` now connects**, where it failed with
  `agent requires an unsupported extension`.

See [Connect a generic A2A agent](connect-a2a-agent.md#what-works-on-this-dialect).

Before you roll back to an earlier release, let pending approvals and structured questions on `a2a` Bindings end. The
earlier release answers a pause on that dialect only with a plain text part, so for a pause this release stored:

- **A press on an approval, or on a question with choices, is not taken.** The person is told the decision could not be
  used, and the prompt stays pending until its timeout, when the agent's task is cancelled.
- **An answer to a question with no choices is sent as plain text**, without the extension's payload. kagent 1.0.0-alpha5
  takes it as an ordinary message, not as the question's answer: in the recorded case the task completed without it.

## Upgrade to the release that shows an agent's working updates

This release reads the status updates an agent sends in state `working` on every dialect, as
[Working updates](../reference/agent-contract.md#working-updates) describes. No Binding fails to load for it, and an
agent that sends no such update sees no change.

- **`steps-and-activity` shows the agent's progress line.** When no tool call is under way, the text of a working update
  titles the `Working…` card while it is open. In an alert thread with no card open it is a line of small print at the next
  write, where `Working…` used to wait 3 seconds, so a turn that then ends empty leaves `(no output)` where it might have
  posted nothing. Each change of line is one more write to Slack, by default at most
  one every 2 seconds for each reply. No card is begun for a line.
- **`activity` counts working updates.** Until the first tool call has its result, its card or line counts the updates
  (`Working… 5 updates`). An update does not begin it: it still begins at the first tool call or result, or after 3 seconds.
- **kagent 0.9.x** repeats its answer in a working update, so in `steps-and-activity` the start of the answer can title
  an open card, or be an alert thread's line, for a moment before the answer itself.

`progress: steps` shows no working update.

## Upgrade to the release that bounds triggers, extract entries and headers

Agent Kourier bounded none of these before. This release refuses a config that holds more than the bounds below, and
loads a config within them as before. Nothing else about a Binding or an AgentBackend changes. No example, recipe or
kind tier config in this repository has more than two triggers, three `extract` entries or one header, or a header name of
more than 16 characters, so the bounds are far above what they use. [Limits](../reference/limits.md) lists every limit.

| Where | Bound | The error |
|---|---|---|
| `spec.chat.triggers` of a Binding | 50 triggers | `spec.chat.triggers: has 51 items, maximum is 50` |
| `extract` of a `botMessage` trigger | 32 entries | `spec.chat.triggers[1].extract: has 33 entries, maximum is 32` |
| `spec.headers` of an AgentBackend | 32 headers | `spec.headers: has 33 items, maximum is 32` |
| The `name` of a header | 256 bytes | `spec.headers[0].name: is 257 bytes, longer than the 256 byte limit` |

A config over a bound is an invalid config, and the broker treats it as it treats the Binding that does not load in
[the release that writes triggers in full](#upgrade-to-the-release-that-writes-triggers-in-full): at start-up it exits
with [code 2](../reference/cli.md#exit-codes) and names the field, so the upgraded pod never becomes Ready, and on a
reload it keeps its last good config. To find one before you upgrade, run `agent-kourier dry-run` of the new release
against your config, as in [Check the values before you install](install-with-helm.md#check-the-values-before-you-install).
It loads the files before it reads a token, so it reports the error without one.

An earlier release has none of these four bounds, so after a [rollback](#roll-back) a config over one loads again.

## Upgrade to the release that serves custom resources

This release can read `Binding`, `AgentBackend` and `ChatConnection` from custom resources instead of files. Nothing changes until you ask:
the default `config.source: files` renders the objects it always has, and the route adds nothing to the pod's template (a release that also sets `GOMEMLIMIT` moves the
template for that reason: see [the release that sets GOMEMLIMIT](#upgrade-to-the-release-that-sets-gomemlimit)). Release 0.1.6 has none of it.

- **New values.** `config.source` (`files` or `crds`), `config.chatConnection`, `crds.install` and `rbac.secretNames`. To move, see
  [Move the config between a ConfigMap and custom resources](migrate-config-source.md). The route needs Kubernetes 1.33 or later, and only the
  route: the files route has no minimum.
- **`--reuse-values` from 0.1.6 works.** The chart reads the values it adds as unset when the old values lack them (a release made by
  the chart of 0.1.6 upgrades to this one with `--reuse-values`).
- **A reload can add a Secret in a namespace the broker was not watching, with no restart.** The broker follows each Secret the config names
  with an informer of its own, so a new namespace works as long as its Role grants `list` and `watch` on the Secret. With the files route, a Secret it may not read fails the
  reload, and the last good config stays, as before; with custom resources it makes that Binding's `BackendReachable` say `CredentialsUnreadable` and stops nothing.
- **New bounds on triggers, extract entries and headers** apply on both routes: see
  [the release that bounds them](#upgrade-to-the-release-that-bounds-triggers-extract-entries-and-headers), which also shows the check to run before you upgrade.
- **A `promptTemplate` may no longer loop, define a template or use a variable, and three strings are bounded.** See
  [the release that limits prompt templates](#upgrade-to-the-release-that-limits-prompt-templates).

After you move to custom resources, rolling back to a release without them leaves the CRDs and every custom resource in the cluster, unread. Move back
to the files route first, as in [Move back to the ConfigMap](migrate-config-source.md#move-back-to-the-configmap), then roll the chart back.

## Upgrade to the release that sets GOMEMLIMIT

The chart now gives the container `GOMEMLIMIT`, the Go runtime's soft memory limit, at 80% of `resources.limits.memory`, in bytes: the default
limit of 256Mi gives 214748364. The runtime then collects more often as its memory nears the limit. The variable is part of the pod template, so
**this upgrade restarts the pod**, as any change to the template does, with every values file that has a memory limit. With SQLite (`Recreate`) there is a gap
while the new pod starts; with Postgres (`RollingUpdate`) the new pod starts beside the old one and takes over when the old one stops.

- **It is soft.** It caps nothing. Memory that is live stays, so a number of large replies from agents read at the same time (each event of up to
  2 MiB takes from about four and a half times its size, for text, to about fifty, for many small objects, while it is decoded, and a stream that is open keeps buffers of about
  twice the largest event it has carried: see
  [the release that lowers the bounds](#what-the-lower-bounds-buy-and-what-they-do-not)) can still pass the container's limit and be OOM-killed.
  Raise `resources.limits.memory` for that.
- **Your own wins.** A `GOMEMLIMIT` in `extraEnv` is the only one rendered, as you wrote it. The runtime reads a number of bytes, optionally with `B`, `KiB`, `MiB`,
  `GiB` or `TiB` after it (`100MiB`), and 9223372036854775807, its default, turns the limit off.
- **No variable** is rendered without a memory limit, for a limit that is an exponent or in milli-bytes, or under 1Mi.

## Upgrade to the release that limits prompt templates

A `promptTemplate` is run when a Binding loads, and again for each message a trigger matches. This release refuses a template that could
run for ever or build a string without bound, and bounds three strings that had no limit. A config that does neither loads as before.
No example, recipe or kind tier config in this repository uses a construct below: they read `.Channel`, `.State`, `.Key` and `.Fields.<name>`.
The current values are on [Limits](../reference/limits.md#write-a-prompt-template).

**What a template may use.** Text and comments, `if` (with `else if` and `else`), `with`, the view's fields (`.Channel`, `.State`, `.Key`,
`.Link`, `.Source`), `.Fields.<name>` or `index .Fields "<name>"`, and the functions `and`, `eq`, `ge`, `gt`, `index`, `le`, `len`, `lt`, `ne`,
`not`, `or` and `slice`. A template with anything else fails to load, whatever the route.

| Refused | Example | Rewrite |
|---|---|---|
| `range` | `{{ range 3 }}x{{ end }}` | There is nothing to range over: `.Fields` cannot be read whole. Name each entry. |
| `define`, `template`, `block` | `{{ define "a" }}...{{ end }}{{ template "a" }}` | Write the text where you use it. |
| A variable, declared or assigned, also in `if` and `with` | `{{ $k := .Fields.alertname }}{{ $k }}` | Read the value where you use it: `{{ .Fields.alertname }}`. |
| `print`, `printf`, `println` | `{{ printf "%s/%s" .Channel .Fields.alertname }}` | `{{ .Channel }}/{{ .Fields.alertname }}` |
| `html`, `js`, `urlquery`, `call` | `{{ .Fields.alertname \| html }}` | Drop the call: a value is checked, by its characters, before the template sees it. |

The error names the position and what is refused, as `dry-run` prints it for a template that starts with `{{ range 9000000000000000 }}{{ end }}`:

```text
b.yaml#1 Binding agent-kourier/ci-failures: spec.chat.triggers[0].promptTemplate: invalid template: promptTemplate:1:9: range is not allowed: a prompt template has no loops
```

The broker refuses the template before it runs it, so a template that would never return is refused at once.

**What the template may write.** The load runs the template once over placeholder values of 64 bytes, and each render runs it over the message's values, through a
buffer of 262144 bytes. A template that writes more fails, where before its output was built and then cut: at load, the Binding is invalid; at render, the trigger
starts no turn, posts a note in the alert's thread, and is audited and counted in `agentkourier_trigger_refused_total` with the reason `render_error`. The prompt keeps
only the first 4000 UTF-16 units of the template's output, so a template near the bound writes mostly what is thrown away.

**Three strings are bounded.** They apply on both routes, and on the custom resource route the API server refuses a longer one when you create or
update the object, with the CRD's `maxLength`, which counts characters where the broker counts bytes.

| Where | Bound | The error |
|---|---|---|
| `promptTemplate` of a `botMessage` trigger | 8192 bytes | `spec.chat.triggers[1].promptTemplate: is 8193 bytes, longer than the 8192 byte limit` |
| `spec.agent.name` of a Binding | 253 bytes | `spec.agent.name: is 254 bytes, longer than the 253 byte limit` |
| `spec.identity.userId` of a Binding | 256 bytes | `spec.identity.userId: is 257 bytes, longer than the 256 byte limit` |

A template over its bound is refused without being parsed. No example, recipe or kind tier config here has a template over 250 bytes, an agent name over 25 characters
or a user ID over 25.

**Check before you upgrade.** Run `agent-kourier dry-run` of the new release against your files, as in
[the release that bounds triggers](#upgrade-to-the-release-that-bounds-triggers-extract-entries-and-headers). It loads them with the broker's loader and reports each of these
errors without a token. It reads files, never the cluster, so on the custom resource route give it the YAML you applied.
A custom resource that fails these checks after you upgrade is judged as
[a bad object is](../explanation/config-routes.md#what-a-bad-object-does): the broker does not serve a new object that is invalid, and keeps serving the last good generation of one that is.

## Upgrade to the release that lowers the bounds on what it reads

This release lowers what Agent Kourier reads from an agent, so that a backend that sends large events costs the broker less memory. It does not bound the
memory a backend can make it use: read [What the lower bounds buy](#what-the-lower-bounds-buy-and-what-they-do-not) before you rely on it. The three bounds that moved:

| What | Before | Now |
|---|---|---|
| One event of a stream (the lines of it, up to the blank line that ends it) | 10 MiB | 2 MiB |
| The answer to a call that is not a stream: `GetTask`, `CancelTask`, `ListTasks`, or a JSON answer to a call that asked for a stream | 16 MiB | 4 MiB |
| What one turn holds of the agent's reply (the text of its artifacts, tool results that wait for their call, the bookkeeping of each tool part) | 2 MiB | 341 KiB |

The agent card stays at 1 MiB. **There is no setting for any of them**: they are constants of the binary, and no chart value, file or custom resource field reaches them, so
a reply that is over them cannot be let through by configuration. Nothing in the chart's values changes. A backend that sends events of more than 2 MiB, or whose tasks
grow past 4 MiB, needs a build with other constants.
The current values, and what else the broker bounds, are on [Limits](../reference/limits.md#what-the-broker-reads-and-holds).

**What an agent that sends more sees.**

- **An event over 2 MiB** is not read. The read fails as the event passes the bound, never as a shortened event that parses, and the turn ends with the outcome
  `response_too_large`. A tool result sits in one event, so one of more than 2 MiB (a large log dump, say) ends the turn.
- **The tool output of a whole turn, on kagent 1.0, is limited to a little under 2 MiB, though no result is that large.** The final task of kagent 1.0 holds every tool call
  and result of the turn again in its artifacts (read in the recorded fixture `test/fixtures/kagent/approval/02-approve.response.sse`), and it arrives as one event. So what has
  to fit 2 MiB is the sum, and a turn that calls tools can pass it with a reply of a few bytes. Measured with 300 KiB results, in a test process: results adding up to 1800 KiB
  made a final task of 1907 KiB, which is read; 2400 KiB (eight results) made 2543 KiB, which is not, and the turn ended at its final event with the reply already
  shown. The `error` of the log line says `an event of more than 2097152 bytes`, as it does for one large result; that the text shown is short, and the tool steps before it
  are many, says it is the sum.
- **A reply that adds up to more than 341 KiB** in the text the turn holds ends the turn the same way, however many events it came in. Before, the same reply was held
  up to 2 MiB.
- **An answer over 4 MiB** is not read either. The answer to `GetTask` carries the task with its whole history, so a long task with large tool results can pass it. The
  turn ends in the same way wherever it reads its task: after a lost stream, after a stream that ended with the task settled and no final task, when it follows a
  task again after a restart, when a person's answer or text reply to a paused task reads the task, and when the read after an answer of unknown outcome meets it. The task is
  read once and not tried again, since the same answer would come again. A person's answer to a paused task also reads the task first, to tell what is new in the reply:
  when that read is too large, the answer is sent without it.
- **A cancel whose answer is over 4 MiB** is a cancel that took. An a2a-go server answers every cancel, and a repeat of one that already took, with the whole task, and
  only a refusal is a small error (read in a test against a real a2a-go handler, not against kagent). So Agent Kourier counts such a cancel as done, and the Stop control does
  too: the task is ended and the record cleared. The audit log still records the cancel as `failed response_too_large`, which says that the answer was not read.
- **A page of `ListTasks` over 4 MiB** is asked for again at half the size, down to three tasks, and the pages after it at the size that fit (each task of a page carries its
  history; whether a backend continues a page token with another page size was not measured). If the lookup still cannot read a page and an earlier page held the turn, that task is
  the answer. If none did, Agent Kourier does not know whether the agent already has the turn and cannot find out, so it **does not send the turn again**: it fails, and
  the thread is told. This is for an agent that can list tasks; an agent that cannot (A2A 0.3, kagent 0.9.x) is still sent the turn, which may run twice. Recovery after
  a restart follows the recorded task first, and the queued message then fails the same way; nothing is asked for again and again.

**What the thread shows.** When a turn stops, text that arrived before the bound was passed stays shown, cut to what a message holds as it always was (12000 UTF-16 units in the
Slack adapter, with a mark where it is cut). Then the thread gets one fixed sentence, `the agent sent more than Agent Kourier can read or hold, so the turn stopped`, and the
turn has no final answer. The sentence is the same for a reply of sixteen bytes after tool results that add up to megabytes and for a reply of megabytes; it names no host and no
size. The difference from before is for a reply between 341 KiB and 2 MiB: it used to end as any turn does, with the cut text, and now the turn stops with the sentence, and
Agent Kourier cancels the task at the agent, once and within 30 seconds, so the agent's later work is lost. The text past 341 KiB was never in the thread: what the lower
budget keeps out of it is the turn's ending.

For a message that was not sent because the lookup could not be read, the thread gets: `Agent Kourier could not check whether the agent already started this message, because the
agent's list of tasks is too large to read, so it did not send it again. The agent may have started it; post the message again to run it once more.` A person who posts it again
sends a new message, which is not looked up.

**What to do.** Make the agent send less in one piece: have its tools trim what they return to what the model needs (a result of 2 MiB is on the order of half a million tokens,
an estimate that is past the context of most models), and keep a reply to what a person will read. Keep a thread's tasks small if the lookup notice appears. There is no
counter of replies that are between the old bounds and the new ones before you upgrade. After it, three signals say what happened, and
[Troubleshoot](troubleshoot.md#the-thread-says-the-agent-sent-more-than-agent-kourier-can-read-or-hold) reads them:

- the turn outcome `response_too_large` (a turn that stopped) and `lookup_too_large` (a message not sent), in `agentkourier_turn_outcomes_total`, which you can alert on;
- the log lines of the turn, with the `error` that names the bound; and
- the audit row of the call that failed with `response_too_large`.

### What the lower bounds buy, and what they do not

The bounds are of the bytes read. The memory it takes to read them depends on the shape of the JSON, which the backend chooses, so
they do not bound the memory. **A backend that is hostile, or that sends a shape you did not test, can take the broker past the chart's default limit of 256Mi with two or three
responses at once.** Trust each AgentBackend as you trust code that runs in the broker's pod, or give a backend you do not trust a broker of its own: a restart recovers the sessions, but it interrupts every turn of
every tenant on that broker while it runs. A bound on memory, a token budget for what is decoded, is not built in this release.

All of this was measured in a test process, not a pod: the Go test binary on a laptop (macOS), with the fake agent and the client in one process and a baseline of about 40 MiB of
resident memory, through the broker's own client path (a `Pool`, the `generic` dialect and the transport that enforces the bounds). Each stream sent one event of exactly the bound and stayed open, all at
once; each call read an answer of exactly the bound, all at once. Three runs of each. `GOMEMLIMIT` is off, or at 214748364 bytes (80% of 256 MiB).

*Text, the shape the first table is of: one long string.* A pod's baseline and its cgroup accounting were not measured.

| | Before | Now |
|---|---|---|
| Heap one open stream keeps after one event of the bound | 20 MiB | 4 MiB |
| Heap at the peak of reading one event of the bound | about 50 MiB | about 9 MiB |
| Heap at the peak of reading one answer of the bound | about 56 MiB | about 18 MiB |
| Open streams that reach 256 MiB of resident memory, `GOMEMLIMIT` off | 5 | 33 to 40 |
| Open streams that reach 256 MiB, `GOMEMLIMIT` at 214748364 | 7 to 8 | 41 to 48 |
| Answers read at once that reach 256 MiB, `GOMEMLIMIT` off | 4 | 17 to 24 |
| Answers read at once that reach 256 MiB, `GOMEMLIMIT` at 214748364 | 5 to 8 | 25 to 32 |

*A data part of many small objects (`{"a":0},` repeated), at the bound: the shape that took the most memory of those tried.* Resident memory, over three runs:

| At once | 2 MiB event, now | 4 MiB answer, now | 10 MiB event, before | 16 MiB answer, before |
|---|---|---|---|---|
| 1 | 136 to 142 MiB | 232 to 254 MiB | 563 to 612 MiB | 853 to 900 MiB |
| 2 | 238 to 254 MiB | 434 to 457 MiB | 1095 to 1197 MiB | 1676 to 1798 MiB |
| 3 | 342 to 385 MiB | 638 to 704 MiB | | |
| 4 | 448 to 502 MiB | | | |

One 2 MiB event of that shape peaks at about 106 to 111 MiB of heap, about fifty times its size. Two of them, or one answer, reach the default limit; the bound on bytes does not stop that, and
streams that ended right after their event, instead of staying open, were no better in the text shape (some runs of 32 streams reached the limit, and others did not by 64, as the
garbage collector's timing decided). Nothing bounds how many streams or calls are open at once; the `rateLimit.maxConcurrent` of a `botMessage` trigger limits only the investigations
that trigger starts. Size `resources.limits.memory` for the number of agents that can answer at once, with the worst shape in mind, and see
[the release that sets GOMEMLIMIT](#upgrade-to-the-release-that-sets-gomemlimit).

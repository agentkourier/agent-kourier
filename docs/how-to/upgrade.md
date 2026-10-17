# Upgrade

For an operator running the Helm chart: at the end, Agent Kourier runs a newer release with its sessions, outbox and
audit log intact, and you know how to roll back.

Each release publishes a signed multi-arch image and a chart whose default image is that image by digest, as
[Install with Helm](install-with-helm.md#the-release) describes. The current release is `0.1.0`. To run a build that is not
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

**What the broker does with a Binding that does not load.** One Binding that fails fails the whole config, so none of
them is served.

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

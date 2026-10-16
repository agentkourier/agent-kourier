# Upgrade

For an operator running the Helm chart: at the end, Agent Kourier runs a newer build with its sessions, outbox and
audit log intact, and you know how to roll back.

Each release publishes a signed multi-arch image and a chart whose default image is that image by digest.

## Upgrade the chart

```sh
helm upgrade agent-kourier oci://<registry>/<repo>/charts/agent-kourier --version <new version> \
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

## Upgrade to the release without trigger presets

A `botMessage` trigger is no longer filled in from a preset, and it must set its limits. A Binding that loaded before the
upgrade can fail to load after it, so change your Bindings before you upgrade.

- **`preset:` is not a field.** A trigger that sets it fails to load. The loader names the file, the Binding and the
  field, for example `b.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].preset: unknown field`. Nothing reads or
  translates the old value.
- **Limits are required.** A `botMessage` trigger without `limits.rateLimit` (all of `maxRuns`, `per` and
  `maxConcurrent`) or without `limits.maxRunsPerDay` fails to load, and the error names the missing field. Before, such a
  trigger loaded and started with no limit. `maxRunsPerDay: 0` still means no daily cap, and now has to be written.

A trigger that sets only a bot ID and a preset gives several errors at once: the unknown field, each missing limit and the missing prompt.

**What the broker does with a Binding that does not load.** One Binding that fails fails the whole config, so none of
them is served.

- *At start-up*, the broker does not start. It prints the error to stderr and exits with a non-zero status, and the pod
  never becomes Ready. With SQLite (the default) the Deployment's strategy is `Recreate`, so the old pod is already gone when the
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
only the first, or part of it. Run `agent-kourier dry-run` (step 4 below) to see every error: it prints the
whole text.

**Migrate each Binding that sets `preset:`.**

1. Copy the trigger from [Investigate Alertmanager alerts](investigate-alertmanager-alerts.md#add-the-trigger) in place
   of the preset trigger. Apart from `match.link`, it is the old preset written out: the same regexes, a 4 hour
   cooldown, 5 starts in 10 minutes with 2 at once, 40 a day, and the prompt.
2. Handle `match.link`. The example's is not part of the old preset: it limits the trigger to one receiver. Keep it,
   set to your receiver, only if your preset trigger matched the title's link. Delete it otherwise, or the alerts of your
   other receivers start nothing.
3. Re-apply the rest of what you overrode, in the same fields: `exclude`, `thread`, `limits` or `promptTemplate`.
4. Replay the channel's history with `agent-kourier dry-run` from the release you are upgrading to, against the edited
   Bindings ([Tune triggers with a dry run](tune-triggers.md)). It loads them with the broker's own loader, so it prints
   the same errors the broker would, and each Alertmanager message should come out `matched`. The older release accepts
   `preset:`, so its binary proves nothing.

**The session source.** A trigger start now records the source `botMessage`, in `sessions.origin_source` and in the
prompt's `.Source`; a start from a preset trigger recorded `alertmanager`. A `promptTemplate` that tested `.Source`
against `alertmanager` needs the new value. Sessions created before the upgrade keep `alertmanager`, and a vote on
their messages still counts under the alert's name ([Stored data](../reference/stored-data.md#feedback)).

## Upgrade to the release with platform-neutral trigger fields

A trigger names its sender and the message fields it reads in terms that do not belong to one chat platform, and the
API no longer fills in the acknowledgment reaction. A Binding written for the earlier release fails to load, so change
your Bindings before you upgrade. If you also migrate off `preset:`, make both changes in one edit: the example in
[Investigate Alertmanager alerts](investigate-alertmanager-alerts.md#add-the-trigger), which the preset section points to,
already uses the new names.

| Before | Now | Where |
|---|---|---|
| `botId` | `id` | `from` of a `botMessage` trigger |
| `titleLink` | `link` | the keys of `match` and `exclude`, and `field` of an `extract` entry |
| `attachmentText` | `body` | the keys of `match` and `exclude`, and `field` of an `extract` entry |

- **The values do not change.** `from.id` is still the bot ID (`B...`) on Slack. `link` still holds the first legacy
  attachment's `title_link` and `body` its `text`; `text`, `title`, `footer` and `fallback` keep their names.
- **The old keys fail to load.** The loader names the file, the Binding and the field, for example
  `b.yaml#1 Binding sre/sre-alerts: spec.chat.triggers[0].from.botId: unknown field`, then `from.id: required`. A
  `match`, `exclude` or `extract` on `titleLink` or `attachmentText` fails with `unsupported message field`, and the
  error lists the field names that exist.
- **`from.kind` is new and optional.** It is `bot`, the default and the only value. A trigger takes a message only when
  the sender's ID and kind both equal `from.id` and `from.kind`; every Slack bot is of kind `bot`.
- **The first prompt names the fields differently.** The block of untrusted data that follows a trigger's prompt has an
  element for each field the message has, named by the field: `<link>` and `<body>` where `<titleLink>` and
  `<attachmentText>` were. An agent instruction that names those elements needs the new names. A prompt template reads
  extracted values through `.Fields.<name>`, which are your own extract names, so a template is unaffected.
- **The dry run names the new fields,** for example `trigger 1: match link`. Regexes are tried in the
  order of the field names, so a message that fails two of them can now show a different field first.
- **The acknowledgment reaction.** A Binding that left `chat.acknowledgment.emoji` out used to have
  `hourglass_flowing_sand` written into it when it loaded. It now stays empty, and Slack supplies the same reaction, so
  nothing changes on Slack. An `emoji` you set is used as before.
- **Alerts that are firing keep their threads.** An alert thread opened before the upgrade is looked up by the same
  connection, channel, bot ID and key as before, so a repeat of an alert whose thread is open finds that thread and
  adds a "Fired again" note to it, as it did before the upgrade, instead of starting a second investigation.

To migrate, rename the keys in the table, then replay the channel's history with `agent-kourier dry-run` from the
release you are upgrading to, against the edited Bindings ([Tune triggers with a dry run](tune-triggers.md)). It loads
them with the broker's own loader, so it prints the same errors the broker would, and each message you expect to start an
investigation should come out `matched`. The older release does not accept the new keys, so its binary proves nothing.

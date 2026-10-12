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

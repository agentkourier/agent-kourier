# Run on Postgres with a standby

For a platform engineer who needs Agent Kourier to survive a node loss: at the end, Agent Kourier keeps its state in
your Postgres database and runs as two replicas, one leading and one standing by.

SQLite, the default, is one file on a volume that one pod may use, so it is one replica. Postgres allows more: the
replicas elect a leader with a Kubernetes Lease, the leader does all the work, and every write is fenced by an epoch
in the database.

## Before you start

- A Postgres database you run. The chart installs none.
- A role that may create tables in its schema: Agent Kourier applies its own migrations at start.
- A direct connection, or a pooler in **session** mode. For a pooler in transaction mode, see the last section.

## 1. Store the DSN

In the release namespace, a Secret with a `postgres://` URL or a key/value string:

```sh
kubectl -n agent-kourier create secret generic agent-kourier-pg --from-literal=dsn="$DSN"
```

Percent-encode the reserved characters of a password in a URL. The DSN never goes in a values file: the render refuses
a `postgres://` URL with a password anywhere in the values.

## 2. Set the values

```yaml
store:
  type: postgres
  postgres:
    dsnSecret: {name: agent-kourier-pg, key: dsn}
replicaCount: 2
podDisruptionBudget:
  enabled: true
  minAvailable: 1 # (1)!
affinity:
  podAntiAffinity: # (2)!
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels: {app.kubernetes.io/name: agent-kourier}
        topologyKey: kubernetes.io/hostname
```

1. Keeps a node drain from taking both replicas.
2. Keeps the two replicas on different nodes. The chart does not do this for you.

With `networkPolicy.enabled`, also say where the database is, or the render refuses:

```yaml
networkPolicy:
  postgres:
    to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: db}}, podSelector: {matchLabels: {app: postgres}}}]
    port: 5432
```

Then `helm upgrade`. No volume is mounted and no claim is made with Postgres.

## 3. Check the leader

```sh
kubectl -n agent-kourier get lease agent-kourier-leader -o jsonpath='{.spec.holderIdentity}{"\n"}'
```

The metric `agentkourier_leader` is 1 on the leader and 0 on the standby, and `agentkourier_leader_takeovers_total`
counts takeovers. Both replicas are Ready: readiness never depends on the lease.

After a crash, a deleted pod or a lost node, work resumes in about 15 seconds plus Agent Kourier's start-up. A leader
that stops on a signal (a rolling update, a drain) hands the lease over, and the standby takes it within a retry
period, 2 seconds by default.

## Behind a pooler in transaction mode

Agent Kourier sends its idle-in-transaction and statement timeouts as startup parameters; they end a stalled
leader's transaction and free its locks. A pooler in transaction mode (PgBouncer's `pool_mode = transaction`) refuses
startup parameters, and listing them in `ignore_startup_parameters` silently drops the protection. Set them on the role
instead:

```sql
ALTER ROLE agentkourier SET idle_in_transaction_session_timeout = '10s';
ALTER ROLE agentkourier SET statement_timeout = '30s';
```

and set `store.postgres.timeouts: role`.

## Rotate the database password

The DSN is read once at start. Change the Secret and the database role, then restart the pods.

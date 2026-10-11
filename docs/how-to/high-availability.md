# Run two replicas (high availability)

For a platform engineer who needs Agent Kourier to survive a node loss, a drain and an upgrade: at the end, Agent
Kourier keeps its state in your Postgres database, runs as two replicas on different nodes with one leading and one
standing by, and you have watched it fail over.

How the leader is chosen and what a failover can and cannot do is explained in
[High availability and failover](../explanation/high-availability.md).

## Before you start

- A Postgres database you run. The chart installs none.
- A role that may create tables in its schema: Agent Kourier applies its own migrations at start.
- A direct connection, or a pooler in **session** mode. For a pooler in transaction mode, see the last section.
- At least two schedulable nodes, and three if the replicas must never share a node during an upgrade (step 3).

## 1. Store the DSN

SQLite, the default, is one file on a volume that one pod may use, so it is one replica; the chart refuses
`replicaCount` above 1 with it. Postgres is the prerequisite for a second replica.

In the release namespace, create a Secret with a `postgres://` URL or a key/value string:

```sh
kubectl -n agent-kourier create secret generic agent-kourier-pg --from-literal=dsn="$DSN"
```

Percent-encode the reserved characters of a password in a URL. The DSN never goes in a values file: the render refuses
a `postgres://` URL with a password anywhere in the values.

## 2. Set the store and the replicas

```yaml
store:
  type: postgres
  postgres:
    dsnSecret: {name: agent-kourier-pg, key: dsn}
replicaCount: 2
```

With `networkPolicy.enabled`, also say where the database is, or the render refuses:

```yaml
networkPolicy:
  postgres:
    to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: db}}, podSelector: {matchLabels: {app: postgres}}}]
    port: 5432
```

No volume is mounted and no claim is made with Postgres. The chart makes the Lease, `<fullname>-leader`, and a Role
that may `get` and `update` it.

Two replicas is the design: one leader does all the work. A third replica is one more standby and adds nothing else.

## 3. Spread the replicas and survive drains

Add these to the same values file:

```yaml
podDisruptionBudget:
  enabled: true
  minAvailable: 1 # (1)!
affinity:
  podAntiAffinity: # (2)!
    requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels: {app.kubernetes.io/name: agent-kourier, app.kubernetes.io/instance: agent-kourier}
        topologyKey: kubernetes.io/hostname
priorityClassName: agent-kourier # (3)!
```

1. A drain may take one replica and must wait for it to come back before it takes the other.
2. No two replicas on one node. The chart has no anti-affinity of its own; this goes in its free-form `affinity` value.
   `app.kubernetes.io/instance` is the Helm release's name (`agent-kourier` here): with it, two releases in one
   namespace do not keep each other's pods apart. The chart's own README selects on the name label alone.
3. A PriorityClass you create, so a node under pressure evicts other work first. Leave the line out to use the
   cluster's default priority.

**The budget.** Set `minAvailable` or `maxUnavailable`, not both. The render refuses a budget that would block every
voluntary eviction, a node drain included: `maxUnavailable: 0`, a `minAvailable` of every replica (`2` at two
replicas), and a percentage that rounds up to every replica (`100%`, or `50%` at one replica). At one replica the useful
budget is `maxUnavailable: 1`. Before you scale to zero with `minAvailable: 1`, switch the budget off or to
`maxUnavailable`: the zero renders, and the scale back to one is refused.

**Required or preferred.** `required` never places two replicas on one node, and a replica that has nowhere to go
stays Pending. An upgrade starts a new pod before it stops an old one (below), so with two replicas `required` needs
three schedulable nodes; on a two-node cluster the new pod stays Pending and the upgrade does not progress. There, use
`preferred`, which places them apart when it can:

```yaml
affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchLabels: {app.kubernetes.io/name: agent-kourier, app.kubernetes.io/instance: agent-kourier}
          topologyKey: kubernetes.io/hostname
```

**The PriorityClass**, if you use one:

```yaml
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: agent-kourier
value: 100000
globalDefault: false
description: Agent Kourier's replicas, so a busy node evicts other work before them.
```

`nodeSelector` and `tolerations` take Kubernetes' own form too, if the replicas must run on particular nodes.
The chart has no `topologySpreadConstraints` value; adding one is planned (CT-1, `tasks/plans/chart-topology-spread.md`).

**What an upgrade does.** With Postgres the Deployment's strategy is a rolling update with one surge and none
unavailable: a new pod starts, as a standby, before an old one stops. When the old leader stops on its signal, it
keeps renewing the Lease through its ordered shutdown (up to 25 s), then hands the Lease over, and a standby takes it at
its next attempt a few seconds later (a retry period, 2 s, plus up to 120% jitter). If the Lease passes to the other old pod first, that pod hands it over again when it stops. (With SQLite the
strategy is `Recreate`: the old pod releases the ReadWriteOnce volume before the new one starts.)

**A draining or frozen leader.** Kubernetes gives a stopping pod `terminationGracePeriodSeconds`, 40 seconds, and the
ordered shutdown takes up to 25. A frozen process answers nothing: the liveness probe (`livenessProbe.periodSeconds`
10, `failureThreshold` 3) restarts it after about 30 seconds, and the standby has taken the Lease about 15 seconds after
the frozen leader's last renewal.

Then `helm upgrade` with the values file.

## 4. Check the leader

```sh
kubectl -n agent-kourier get pods -l app.kubernetes.io/name=agent-kourier -o wide
kubectl -n agent-kourier get lease agent-kourier-leader -o jsonpath='{.spec.holderIdentity}{"\n"}'
```

The holder is the leading pod's name, an underscore and a random suffix for the process: `agent-kourier-6d9f-x2k4p_1a2b3c4d`.
The leader's log has a `leading` line with the `epoch` and the `holder`. Both pods are Ready: readiness never depends
on the Lease. On each pod, the metric `agentkourier_leader` is 1 on the leader and 0 on the standby:

```sh
kubectl -n agent-kourier port-forward pod/<pod> 8080:8080 &
curl -s localhost:8080/metrics | grep -E '^agentkourier_leader'
```

## 5. Drill a failover

Do this on a test install, with a bound channel you can use.

1. Find the leader's pod:

    ```sh
    holder=$(kubectl -n agent-kourier get lease agent-kourier-leader -o jsonpath='{.spec.holderIdentity}')
    leader=${holder%_*}
    echo "$leader"
    ```

2. In the bound channel, ask the agent something that takes a while, so a turn is running.

3. While it streams, delete the leader's pod, and watch the Lease change hands:

    ```sh
    kubectl -n agent-kourier delete pod "$leader" --wait=false
    kubectl -n agent-kourier get lease agent-kourier-leader -w \
      -o custom-columns=HOLDER:.spec.holderIdentity,TRANSITIONS:.spec.leaseTransitions
    ```

    The holder changes to the other pod, or to the new pod the Deployment starts. A deleted pod stops on its signal:
    after its ordered shutdown (up to 25 s, depending on what is in flight) it hands the Lease over, and a
    standby takes it at its next attempt a few seconds later (a retry period, 2 s, plus up to 120% jitter). A crashed or
    lost node's leader is replaced about a lease duration, 15 s, after its last renewal.

4. Watch the thread. The new leader recovers the turn: the half-written message is replaced with the whole answer, and
   the answer appears once. Reply in the thread: the same session continues.

5. Check the new leader counted its takeover. The earlier port-forward may point at the deleted pod, so
   port-forward to the new holder's pod, as in Check the leader, and read the counter:

    ```sh
    curl -s localhost:8080/metrics | grep -E '^agentkourier_leader_takeovers_total'
    ```

The project's own failover test, S16, does the same with a kill mid-turn, mid-post and mid-storm, and a leader frozen
past its Lease, and checks that every reply appears once.

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

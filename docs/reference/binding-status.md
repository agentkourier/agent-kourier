# Binding status

With the config read from custom resources (`config.source: crds`), the elected leader of the broker that serves a Binding
writes the Binding's `status.conditions`, so its owner can read what became of it with `kubectl`. With the ConfigMap route nothing writes a
status, and a Binding file that sets `status` is refused: `status: a file may not set it, remove it`. The field is in the
[Binding reference](binding.md#bindingstatus). This page says what is in it. To act on a reason, see
[Troubleshoot an install](../how-to/troubleshoot.md#a-binding-is-not-accepted).

## What `kubectl get` shows

```sh
kubectl get akbinding -A
```

```text
NAMESPACE   NAME          READY   REASON                       CHANNEL   AGE
team-a      team-a-chat   True    OK                           C2        4m
team-a      late          False   Accepted:ChannelConflict     C1        1m
```

`READY` and `REASON` are the status and reason of the `Ready` condition, `CHANNEL` is `spec.chat.channel`, and `AGE` is the
object's age. A Binding that has no status yet shows empty `READY` and `REASON`.

## The conditions

A Binding that the broker serves has four conditions, each with `type`, `status` (`True`, `False` or `Unknown`), `reason`,
`message`, `lastTransitionTime` and `observedGeneration`. A Binding that the broker does not serve has `Accepted` and `Ready`
only. No other type is written.

| `type` | What it says | Written from |
|---|---|---|
| `Accepted` | The broker took the Binding as written: its spec passes the broker's checks, the AgentBackend and ChatConnection it names exist, are valid and allow its namespace, and no older Binding holds its channel. | What the config source refused. |
| `BackendReachable` | A fresh fetch of the agent's card worked and the card is one Agent Kourier can use. | The leader's probe of the agent. |
| `ChannelJoined` | The bot is in the Binding's channel, which exists and is not archived. | The leader's read of the channel. |
| `Ready` | Every other condition is `True`. | Derived. |

- `message` is one line of at most 1024 bytes, redacted, with `…` where it was cut. It is empty when the condition is `True`.
  A probe's message is one fixed sentence for each reason: it never names a host, address, port or the text of another
  namespace's object, because the person who reads a status may not own what it probed. The log of the leader has the cause
  (a `cause` attribute), and a status points there: "the operator's log has the cause".
- `lastTransitionTime` is when `status` last changed. A new reason or message with the same status does not move it.
- There is no `lastProbeTime`. A status that changed every few minutes would be watch traffic for everyone who watches Bindings, so
  a condition does not say when it was last probed. That probes still run shows in
  [`agentkourier_binding_probe_duration_seconds`](metrics.md#binding-verification).

### `Accepted`

`True` has the reason `OK`. `False` has one reason, and its `message` holds the problems of every reason the Binding has, in the
order below, so the reason does not depend on the order the broker listed them in. A reason outside this set would read `Rejected`; none
is written today.

| `reason` | What it means |
|---|---|
| `InvalidSpec` | The Binding fails the broker's checks, which are a superset of the schema's: [Choose a config route](../explanation/config-routes.md#what-is-checked-and-where) lists what only the broker checks. `message` names each field and what is wrong with it. |
| `BackendNotFound` | `spec.agent.backendRef` names no AgentBackend. |
| `BackendRejected` | The AgentBackend it names is invalid. If it had a good generation, that generation is what runs, and `message` says which. |
| `ConnectionNotFound` | `spec.chat.connectionRef` names no ChatConnection. No broker owns such a Binding, so the leader of any broker that sees it writes this. |
| `ConnectionRejected` | The ChatConnection it names is invalid, as for `BackendRejected`. |
| `NamespaceNotAllowed` | The AgentBackend or ChatConnection is in another namespace and its `allowedNamespaces` does not list this Binding's. The answer is the same for a valid object and an invalid one, so it says nothing of the object but that it exists. |
| `ChannelConflict` | Another Binding on the same ChatConnection holds the channel. The older one, by `creationTimestamp`, then by namespace and name, holds it. |

A `message` about an AgentBackend or ChatConnection of another namespace never quotes that object's problems. A `ChannelConflict`
whose winner is in another namespace says `already bound by a Binding in another namespace`, without the name; the log has it.

`Ready` is `False` with the reason `Accepted:<reason>` for a Binding that is not accepted, with `Accepted`'s message and generation.
Otherwise it is the probes' result (below), with the reason `<type>:<reason>` of the first condition that holds it back, such as
`BackendReachable:AgentNotFound`.

### `BackendReachable` and `ChannelJoined`

Each has `OK` when `True`, `Pending` while its first probe has not ended, and a reason from a closed set. The reasons, what each
means and what to do are in [A Binding is not ready](../how-to/troubleshoot.md#a-binding-is-not-ready). `ProbeTimeout` and `ProbeFailed` are the
prober's own, and the same for both. When a probe could not tell (a rate limit, a timeout), the status is `Unknown` and not `False`.

How a probe is made, and how often, is under [Binding verification](metrics.md#binding-verification): when the leader starts, when the config or a
Secret the Binding refers to changes (at most once every 30 seconds), and every 5 minutes.

## `observedGeneration`

Each condition says which generation of the Binding it describes. `metadata.generation` goes up with each change of the spec, and
the status does not. A condition whose `observedGeneration` is lower than the Binding's `metadata.generation` is about an older spec.

- **`Accepted`** is the newest generation the broker looked at. For a refused edit that is the refused generation, and the broker keeps serving
  the last good one: `message` says `generation 2 is invalid, serving generation 1`. The probes stay at the generation served.
- **A probe's condition** is the generation the pass began with, and it moves with every probe, even if the result did not change.
  A change made during a pass is not claimed.
- **`Ready`** when accepted is the oldest generation of the conditions it derives from. It says nothing newer than its evidence.

### After an edit

An edit shows `Accepted` at the new generation within seconds, while `BackendReachable`, `ChannelJoined` and `Ready` still say what they said about the
old spec, and the `READY` column still says `True`. The probes catch up when the next pass runs. The leader does not start a pass within 30 seconds
of the start of the last one, looks for work every 5 seconds, and a probe may take up to 10, so by those timings the window is at most about 45 seconds. Measured on
a kind cluster: `Accepted` was at the new generation after 12 seconds and `Ready` after 23, when the edit came
right after a pass. A Binding that was edited into something that can no longer work shows `False` only after the window.

`kubectl wait --for=condition=Ready` waits for the condition to describe the generation it is waiting on. Measured with kubectl 1.37.1, it returned
13 seconds after an edit, when `Ready` reached the new generation; other kubectl versions were not tried, and one that ignores
`observedGeneration` would return at once on the old `True`.

### A Binding made again

A Binding that was deleted and created again with the same name is a new object, with generation 1 and no status. The broker picks it up at
its next look at the config, which is within 10 seconds, and writes the new status then. Until it does, the `READY` and `REASON` columns are empty
(measured: 4 seconds), and then `Unknown` with the reason `BackendReachable:Pending` until the probes end. The broker never carries the old object's status over: a
condition made for another object is `Pending` for this one.

## Who writes it

Every replica reads the three kinds, so a standby can take over, but only the leader writes status, with a server-side apply on the status subresource
under the field manager `agent-kourier`. With SQLite there is no election and the one replica writes. Notes:

- A write is made only when the status would change, at most once every 10 seconds for one Binding. A failed write is tried again after 10 seconds, doubling up to 5 minutes.
- If the status of a Binding the broker serves is changed by someone else, the broker writes its own words back. One edit is put back once. A status that is
  changed again and again is a fight: see [The status of a Binding keeps changing](../how-to/troubleshoot.md#the-status-of-a-binding-keeps-changing).
- A `status:` in a manifest that is applied to the Binding itself is ignored by the API server (measured with `kubectl apply`): it is a subresource.
- After a failover the new leader starts every probed condition at `Pending`, and does not write that over a good status of the generation it serves. Between
  one leader's last write and the next leader's first, the status is as it was. With no leader it goes stale.
- A Binding that was served by the ConfigMap route, or whose broker was rolled back to it, keeps the last status it had. See
  [Move the config between a ConfigMap and custom resources](../how-to/migrate-config-source.md#move-back-to-the-configmap).

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

A Binding that the broker serves has four conditions, or five if it asks for tool approvals from user groups (`interactions.toolApprovals: true` and a non-empty `interactions.approverGroups`), each with `type`, `status` (`True`, `False` or `Unknown`), `reason`,
`message`, `lastTransitionTime` and `observedGeneration`. A Binding that the broker does not serve has `Accepted` and `Ready`
only. No other type is written.

| `type` | What it says | Written from |
|---|---|---|
| `Accepted` | The broker took the Binding as written: its spec passes the broker's checks, the AgentBackend and ChatConnection it names exist, are valid and allow its namespace, and no older Binding holds its channel. | What the config source refused. |
| `BackendReachable` | A fresh fetch of the agent's card worked and the card is one Agent Kourier can use. | The leader's probe of the agent. |
| `ChannelJoined` | The bot is in the Binding's channel, which exists and is not archived. | The leader's read of the channel. |
| `ApproversReadable` | The bot can read every user group in `interactions.approverGroups`. A Binding that asks for no approvals, or for approvals from anyone, has no such condition. | The leader's read of the groups. |
| `Ready` | Every other condition is `True`. | Derived. |

- `message` is one line of at most 1024 bytes, redacted, with `…` where it was cut. It is empty when the condition is `True`.
  A probe's message is a fixed sentence that never names a host, address, port or the text of another
  namespace's object, because the person who reads a status may not own what it probed. The one that names anything is
  `CredentialsUnreadable`: it names a Secret and a key of the Binding's own namespace, and never a Secret of another. The other is `ApproversReadable`, whose message
  names a group of the Binding's own `approverGroups` by its place in the list and its ID. The log of the leader has the cause
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
| `ConnectionRejected` | The ChatConnection it names is invalid. If it had a good generation, that generation is what runs, and `message` says which. |
| `ConnectionNotServed` | `spec.chat.connectionRef` names a ChatConnection that exists and that this broker does not serve: it served it before and does not now (its namespace was taken off the list of namespaces it serves, or its own ChatConnection was changed), or it left it out because it serves at most 16 chat connections. `message` says which. The broker writes it when it first sees the Binding, or when its words change, and does not put it back if the status is changed, so that a broker that does serve the connection has the last word. It tells a Binding only if the ChatConnection admits the Binding's namespace (it is the connection's own, or listed in `allowedNamespaces`); to any other Binding it says nothing. Session retention keeps the sessions of the Binding, as it keeps those of a Binding that is not served for any other reason. A condition does not say which broker wrote it, so a broker cannot tell the words of another broker that serves the connection from the stale ones it exists to replace. After a restart or a change of leader, the broker that first sees a Binding of a connection it left out or stopped serving therefore writes `ConnectionNotServed` over a status that says anything else, another broker's `Ready=True` included, and `Ready` is `False` until that broker puts its own words back, which it does within about 10 seconds while it has a leader and its writes succeed (it checks each second, and waits 10 seconds after its last write to that Binding; after failed writes it backs off for up to 5 minutes). |
| `NamespaceNotAllowed` | The AgentBackend or ChatConnection is in another namespace and its `allowedNamespaces` does not list this Binding's. The answer is the same for a valid object and an invalid one, so it says nothing of the object but that it exists. |
| `AgentNamespaceNotAllowed` | The AgentBackend it names sets `agents`, and `spec.agent.namespace` is neither the Binding's own namespace, with `sameNamespace: true`, nor listed in `namespaces`. The Binding is not served. `message` names the AgentBackend and the agent namespace, and nothing else of the AgentBackend's spec. When the AgentBackend's newest generation is invalid, or it was deleted and is held, `message` adds that the limit is that of the generation being served, and which generation that is; the problems of an invalid generation are added only for a Binding in the AgentBackend's own namespace. |
| `ChannelConflict` | Another Binding on the same ChatConnection holds the channel. The older one, by `creationTimestamp`, then by namespace and name, holds it. |

A `message` about an AgentBackend or ChatConnection of another namespace never quotes that object's problems. A `ChannelConflict`
whose winner is in another namespace says `already bound by a Binding in another namespace`, without the name; the log has it.

`Ready` is `False` with the reason `Accepted:<reason>` for a Binding that is not accepted, with `Accepted`'s message and generation.
Otherwise it is the probes' result (below), with the reason `<type>:<reason>` of the first condition that holds it back, such as
`BackendReachable:AgentNotFound`. For about 15 seconds after a broker starts, before its prober has listed the Binding, `Ready` is `Unknown` with the bare reason
`Pending`; then it is `BackendReachable:Pending` until the first probes end. A Binding that an edit gives its first approver group is `Unknown` with `ApproversReadable:Pending`
until the next pass reads the group, unless `Ready` is already `False` for another reason, or is already `Unknown` and Pending on another condition (such as `BackendReachable:Pending`), so a Binding is never `Ready` on the word of a probe that did not ask about its groups. An edit that takes the
last group away removes `ApproversReadable` from the status at the next write. Until the prober has seen the edit, `Ready` reads `Unknown` with the reason `Pending` if the prober's `Ready` was held back
by `ApproversReadable`, rather than naming a group the Binding no longer has; after that, `Ready` does not count the condition.

### `BackendReachable`, `ChannelJoined` and `ApproversReadable`

Each has `OK` when `True`, `Pending` while its first probe has not ended, and a reason from a closed set. The reasons, what each
means and what to do are in [A Binding is not ready](../how-to/troubleshoot.md#a-binding-is-not-ready). `ProbeTimeout` and `ProbeFailed` are the
prober's own, and the same for all three. A probe that could not tell is `Unknown` and not `False`: the chat platform's rate limit or deadline, an agent's 408, 425 or 429,
and a probe that ran past the prober's own 10 seconds (`ProbeTimeout`). A backend that does not answer is `False` with `Unreachable`. A card of more
than 1 MiB is `False` with `InvalidAgentCard`, and the probe stops reading at that size.

A Binding on a team ChatConnection (one that is not the broker's own) is read as that connection's bot, and when the connection does not run the read is not made. If Slack rejected the connection's token, `ChannelJoined` is `False` with `ChatCredentialsRejected`, and so is `ApproversReadable` on a Binding that asks for approver groups, and the connection starts again by itself when its credentials Secret or the ChatConnection changes. If its Secret is gone, empty or of a type that is never read, they are `Unknown` with `ChatCredentialsUnusable` until the Secret can be used. Otherwise a connection that is not running (it has not started yet, or stopped) is `Unknown` with `ChatUnavailable`. For `ChatCredentialsChanged`, the broker's own connection takes the new tokens at a restart of Agent Kourier, and a team connection restarts by itself, within about 5 seconds of Agent Kourier seeing the change. The reasons are listed in [A Binding is not ready](../how-to/troubleshoot.md#a-binding-is-not-ready).

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

`Accepted` reaches the new generation within a few seconds, up to about 20, while `BackendReachable`, `ChannelJoined`, `ApproversReadable` and `Ready` still say what they said about
the old spec, and the `READY` column still says `True`. The probes catch up when the next pass runs, and that takes longer. A write of a Binding's status waits for 10 seconds
from the end of the last one, and the writer looks for work each second. A pass does not begin within 30 seconds of the start of the last, the prober looks for work each 5 seconds, and
it runs 4 probes at a time, two for each Binding (three for one with approver groups), each for up to 10 seconds, so a pass over many slow backends takes longer than one over a few. Add the write after the pass and
the delay is up to about a minute for one Binding, and more for many. It is often much less, when the last pass is long past. A Binding that was edited into something that can no longer work shows `False` only after that.

`kubectl wait --for=condition=Ready` waits for the condition to describe the generation it is waiting on, with kubectl 1.37.1. A kubectl that ignores
`observedGeneration` would return at once on the old `True`.

### A Binding made again

A Binding that was deleted and created again with the same name is a new object, with generation 1 and no status. The broker picks it up at
its next look at the config, which is within about 10 seconds, and writes the new status then. Until it does, the `READY` and `REASON` columns are empty,
and then `Unknown` with the reason `Pending` and `BackendReachable:Pending` until the probes end. The broker never carries the old object's status over: a
condition made for another object is `Pending` for this one.

## Who writes it

Every replica reads the three kinds, so a standby can take over, but only the leader writes status, with a server-side apply on the status subresource
under the field manager `agent-kourier`. With SQLite there is no election and the one replica writes. Notes:

- A write is made only when the status would change, at most once each gap for one Binding. A failed write is tried again after one gap, then after twice the wait before, up to the backoff cap, and then at that interval for as long as it fails.
  The writer looks for work each second. These values are on [Limits](limits.md#run-the-broker).
- If the status of a Binding the broker serves is changed by someone else, the broker writes its own words back. One edit is put back once. A status that is
  changed again and again is a fight: see [The status of a Binding keeps changing](../how-to/troubleshoot.md#the-status-of-a-binding-keeps-changing).
- The leader records the chat connections it serves in its store, when it starts and whenever the set changes. The record also keeps a connection
  it served before and that still exists. A leader that starts, on the same replica or on another, compares the record with the set it serves, and
  writes `ConnectionNotServed` once on the Bindings of a connection that is in the record, is not served now, and still exists, as it does for a
  Binding made on that connection later. The connection stays in the record, so every later start finds it stopped too, until it is served again or
  its ChatConnection is deleted. A deleted connection is not this: its Bindings say `ConnectionNotFound`.
- A `status:` in a manifest that is applied to the Binding itself is ignored by the API server: it is a subresource.
- After a failover the new leader starts every probed condition at `Pending`, and does not write that over a good status of the generation it serves. Between
  one leader's last write and the next leader's first, the status is as it was. With no leader it goes stale.
- A Binding that was served by the ConfigMap route, or whose broker was rolled back to it, keeps the last status it had. See
  [Move the config between a ConfigMap and custom resources](../how-to/migrate-config-source.md#move-back-to-the-configmap).

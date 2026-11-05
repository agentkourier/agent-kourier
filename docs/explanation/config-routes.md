# Choose a config route

Agent Kourier reads its `ChatConnection`, `AgentBackend` and `Binding` resources from one of two places, chosen when the
broker starts. This page says what each route does and how they differ when something is wrong, so that you can choose. Both are
supported. [Move the config between a ConfigMap and custom resources](../how-to/migrate-config-source.md) is the how-to.

## The two routes

| | Files (the default) | Custom resources |
|---|---|---|
| Set with | `AGENTKOURIER_CONFIG_SOURCE=files`, or unset | `AGENTKOURIER_CONFIG_SOURCE=crds` (the chart's `config.source: crds`) |
| Where the resources are | YAML files in `AGENTKOURIER_CONFIG_DIR`, usually a mounted ConfigMap | `Binding`, `AgentBackend` and `ChatConnection` objects of the group `agentkourier.dev` in the cluster |
| Who writes them | Whoever edits the ConfigMap or the values | Whoever Kubernetes RBAC lets write the objects in each namespace |
| One bad object | The whole config is refused | That object is refused; the others carry on |
| What a Binding's owner sees | The broker's log and metrics | `Binding.status`, in `kubectl get akbinding` |
| ChatConnections | Exactly one in the directory | Many in the cluster; the broker serves the one `AGENTKOURIER_CHAT_CONNECTION` names |
| Kubernetes | No minimum | 1.33 or later, which the chart checks |
| Extra grants | A Role for the Secrets in each namespace | A ClusterRole to read the three kinds and write `bindings/status`, and Roles that name the Secrets |
| Unknown fields | The loader refuses the file | The API server refuses them at apply, or drops them; the broker ignores one its own types lack (see below) |
| A change is read | At the next check of the files, every 10 seconds, and the chart rolls the pod when its ConfigMap changes | At the next check, every 10 seconds |

The two are never merged and there is no fallback. A broker on `files` ignores any custom resources in the cluster, and a broker on
`crds` has no config directory. The route is chosen at start: a reload cannot change it, so moving is an upgrade of the release.

### Which to choose

Use files when one team writes all the config. One reviewed ConfigMap is one artifact, a mistake is caught before anything serves it,
and nothing needs a cluster-scoped grant. This is the simple default.

Use custom resources when several teams each write their own Bindings. One team's typo then leaves the others serving, each team can
see what became of its Binding without the operator's log, and RBAC decides who may write what. The price is the grants above, a
Kubernetes of 1.33 or later, and the need to think about who may write which object (see
[What the people who write these objects can do](security.md#what-the-people-who-write-these-objects-can-do)).

## What a bad object does

Both routes run the same code on each object: the same defaults, the same validation, the same checks between objects. They differ on a field that the broker's types do not have
(the files refuse it, the broker ignores it on custom resources: see [Unknown fields](#unknown-fields)), and in what
they do with a failure.

**Files are all or nothing.** One resource that does not load fails the config. At start the broker exits with code 2 and names the
file and field. On a reload it keeps the last good config, logs `config reload failed; keeping the last good snapshot` and sets
`agentkourier_config_stale` to 1. One owner and one reviewed artifact make that an honest rule.

**Custom resources are judged one at a time**, because different teams own them and one team's typo must not freeze everyone:

| Event | What the broker does |
|---|---|
| An invalid edit of an object that had a good generation | Keeps serving the last good generation. A Binding's status says which: `Accepted=False` with the reason `InvalidSpec`, and the generation being served in the message. |
| An invalid new object | Does not serve it. A Binding has `Accepted=False`. |
| An object is deleted | Removes it. |
| A Binding names an AgentBackend or ChatConnection that does not exist, or that does not allow its namespace | Drops the Binding from routing, with a condition (`BackendNotFound`, `ConnectionNotFound`, `NamespaceNotAllowed`). |
| A Binding names an AgentBackend that is invalid | Serves the AgentBackend's last good generation, or drops the Binding if it has none, with `BackendRejected`. |
| The broker's own ChatConnection is edited into something invalid | Keeps serving its last good generation, and each of its Bindings says `ConnectionRejected`. |
| A Binding names a ChatConnection that exists and that no broker serves | Nothing reads the Binding and nothing writes its status: `READY` stays empty. Point `connectionRef` at a connection that a broker serves. |
| A Binding's Secret is missing, unreadable or lacks the key | Keeps the Binding served. `BackendReachable` says `CredentialsUnreadable`. |
| Two Bindings on one channel | The older holds it. The other has `ChannelConflict`. |
| An object is edited again and again | Serves each read as it was when the read began, and reads again at the next check, so an edit is served within one check (10 seconds) plus up to two reads: the one that is running when it lands, then its own. One team editing its Binding in a loop does not hold back another's. But each read that finds a change moves the config's revision, so the Binding verification pass, which probes every Binding of the connection, runs at most every 30 seconds for as long as the loop lasts, where it would run every 5 minutes (see [Binding verification](../reference/metrics.md#binding-verification)). Measured only in a test against a fake API server in the test process, not against a real API server: a Binding of about 0.4 MiB, edited with a 10 ms pause between edits (50 to 85 edits a second in the runs), took 35 to 50 ms to read, or about 0.5 s under the race detector, and a Binding that another team added during the loop was served 50 to 150 ms later with the test's 100 ms check (1.1 to 1.2 s under the race detector). |
| The caches have not synced, or the API server is out | Publishes nothing new. An empty or failed cache never reads as "everything was removed". |
| The CRDs are not installed at start | The broker exits with code 2. |
| The broker's own ChatConnection is missing or invalid at start | The broker exits with code 2. While it runs, it keeps the last good one and says so in its log and in a metric, including after the object is deleted. |

Only a Binding has a status. An invalid AgentBackend or ChatConnection shows in the status of each Binding that uses it, in the log
(`config: an object is refused`) and in `agentkourier_config_invalid_objects`. The reasons are in
[Binding status](../reference/binding-status.md) and what to do about each is in
[Troubleshoot an install](../how-to/troubleshoot.md#a-binding-is-not-accepted).

### Who holds a channel

On both routes one channel of a ChatConnection takes one Binding. On files two claims fail the whole config: the broker will not start, or a
reload is refused, and the error names the Binding that already holds the channel. On custom resources the older Binding, by `creationTimestamp` and then by
namespace and name, holds the channel, and the other is not served and says `ChannelConflict`. A later Binding cannot take a channel from an older one.

**Age alone decides.** An older Binding that is edited onto a younger one's channel displaces it, and so does an older Binding whose broken reference heals. A reply in a thread that the displaced Binding started, or one already queued, gets "This thread's agent is no longer set up, so I can't pass your messages on." A new mention in the channel is answered by the Binding that holds it now.

### What the custom resource route does not promise

- **The last good generation is kept in memory.** A broker that starts during an invalid generation never saw the good one, so it leaves that Binding
  out until it is fixed. With two replicas the replicas can then disagree: the one that has run since before the edit serves the last good generation,
  and the other serves nothing for it. A failover during such a time takes the Binding out of routing, and can change which Binding holds a channel.
- **An AgentBackend has no status.** Its owner is not told when an edit is refused by the broker, only the Bindings that use it. An edit that narrows
  `allowedNamespaces` and is refused leaves the old, wider list in force. A schema refusal comes back at apply; a refusal that only the
  broker makes does not, so read the log or `agentkourier_config_invalid_objects` after changing an AgentBackend or a ChatConnection.
- **A Binding that is not served keeps its sessions.** Retention skips the sessions of a Binding that exists and was refused, until it is served
  again or deleted, so a Binding left broken keeps its sessions indefinitely.
- **Deleting a ChatConnection does not stop routing.** A broker holds its own ChatConnection at its last good generation after it is deleted, and with it the
  Bindings of it that are gone and the AgentBackends they use, until the connection returns or the broker restarts. That is what lets a move back to the
  ConfigMap keep the old pods routing while they end ([the how-to](../how-to/migrate-config-source.md#move-back-to-the-configmap) has the details, and
  `agentkourier_config_held_objects` says what is held). A Binding deleted alone, while its connection exists, stops in about 10 to 20 seconds, behind a stream of other deletions in about 80 at most; one deleted while
  its connection is held stays held, so bring the connection back first. A held Binding keeps its channel against a newer one, and a Binding that was just deleted keeps it for up to about 20 seconds, longer behind a stream of other deletions.
- **Deleting a CRD deletes every object of the kind.** Deleting the Bindings CRD alone stops the Bindings within about 20 seconds (longer behind a stream of other deletions), and a new mention gets no answer. Deleting all
  three takes the connection with them, and the broker holds the rest and keeps routing. In both the broker sets `agentkourier_config_stale` to 1, and a restart exits with code 2.

## What is checked, and where

A custom resource is checked twice: by the API server when it is applied, and by the broker when it reads it. The broker's check is the authority. The CRDs
carry a copy of most of its rules, as OpenAPI schema (types, enums, patterns, lengths and counts) and CEL rules, so that `kubectl apply` fails fast, naming the
field. A test holds the copy to the broker on a real API server: an object that the schema refuses is one that the broker refuses too, so
nothing is accepted on the file route that the schema refuses.

```text
$ kubectl apply -f bad.yaml
The Binding "bad" is invalid:
* spec.chat.channel: Invalid value: "general": spec.chat.channel in body should match '^[CG][A-Z0-9]+$'
* spec.identity: Invalid value: "object": set userId, tokenSecretRef, or both
```

**Refused at apply** by the schema or a CEL rule: the wrong type, a value outside an enum, a pattern or a length, a missing required field, a
count over a cap, and most of the rules between the fields of one object.

**Left to the broker**, which reports them as `Accepted=False` after the object is stored: a regex in a trigger's `match`, `exclude` or `extract` that
does not compile; a trigger's `promptTemplate` that does not parse, names an extract the trigger lacks, or fails when it runs; a duplicate trigger; the
parts of a URL that only Go's parser refuses (a bad percent escape, port or bracket); a `match` or `exclude` value of 512 characters or fewer that is
over 512 bytes; and every rule that spans objects: that the AgentBackend and ChatConnection exist and allow the namespace, that the dialect has an adapter, that
one Binding holds the channel, and that a Secret has its key.

The caps (50 triggers in a Binding, 32 `extract` entries in a trigger, 32 headers and 256 bytes of a header's name in an AgentBackend) apply on
both routes, because the schema and the broker's own validation both have them. [Upgrade](../how-to/upgrade.md#upgrade-to-the-release-that-bounds-triggers-extract-entries-and-headers) says how to find a config over one before you upgrade.

### Unknown fields

A field that the schema does not have is **refused by default** when you use `kubectl` or Helm 4, and the API server drops it when the client does not ask it to refuse.
For a Binding with `spec.chat.bogusField`, with kubectl 1.37.1 and Helm 4 (another kubectl may differ):

| Client | Result |
|---|---|
| `kubectl apply`, `kubectl create` | Refused: `strict decoding error: unknown field "spec.chat.bogusField"` |
| `kubectl apply --server-side` | Refused: `.spec.chat.bogusField: field not declared in schema` |
| `kubectl apply --validate=warn` | Stored without the field, with `Warning: unknown field "spec.chat.bogusField"` |
| `kubectl apply --validate=false` | Stored without the field, and no message |
| `helm upgrade`, with the field in a `config.*` value | Refused: `field not declared in schema`, and the release goes to `failed`, after Helm has applied the objects ahead of it: on the move to custom resources that includes the Deployment ([what to do](../how-to/migrate-config-source.md#when-it-does-not-work)) |

On the files route the loader refuses a file with a field it does not know, and so the whole config. On the custom resource route the API server's schema decides what an
object may hold, and a client that asks it to refuse unknown fields is refused (the table); one that does not, such as `--validate=false`, has the field dropped silently, and the
object runs without it, so a field that a release removed is ignored without an error. What Argo CD does with an unknown field is not covered here.

**A broker older than its CRDs ignores a field it does not know.** The broker does not refuse an object on the custom resource route because of a field that its own types lack,
in the `spec`, the `status` or the `metadata` of a Binding, an AgentBackend or a ChatConnection: the field is ignored and the object is read as the rest of it says. This is
what lets an older broker keep serving under newer CRDs. The CRDs are applied before an upgrade, and rolling back only the image leaves them new; a later release can add a
field with a default, and the API server then adds that field to every stored object it returns, without a change of the object's generation. If the broker refused such a
field it would serve no Binding at all, and a broker that cannot read its own ChatConnection would not start. A field that the broker's types do have, with a value that they cannot
read or that fails validation, is still refused.

The broker says what it ignored: one warning for each object, for each generation of it, `a custom resource has fields this broker does not know, and ignores them`, with the
`kind`, `namespace`, `name` and `generation` of the object and the path of each field (`spec.chat.futureField`, at most ten of them, and their count as `unknown_fields`), and never
a value. What tells an operator that the CRDs and the broker differ is `agentkourier_crd_schema_state`, which reads `mismatch` for a CRD whose annotation `agentkourier.dev/schema-sha256` is not the hash of the schema the broker was built
with, as when the CRDs come from a newer chart release than the image. It cannot see every difference: a CRD that was edited by hand keeps its annotation and reads `match`, and one that has
no annotation reads `unannotated`. To end the mismatch, bring the CRDs and the image to the same chart release.

## One ChatConnection

On files the directory holds exactly one `ChatConnection`: the broker refuses to start with none or with more, and `AGENTKOURIER_CHAT_CONNECTION` is refused. On custom
resources a cluster can hold the connections of several brokers, so each broker is told which one it serves, as `namespace/name`, in `AGENTKOURIER_CHAT_CONNECTION`, which is
required. It serves the Bindings whose `connectionRef` names that connection, and no others. That also lets two releases share a cluster. A Binding whose `connectionRef` names a connection that does not exist is
reported by the leader of every broker that sees it, as `ConnectionNotFound`, since none owns it.

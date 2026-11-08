# Security model

This page explains who Agent Kourier acts as, where the access boundary is, why kagent needs a verifying front door,
and how text from people, alerts and agents is kept inert.

## One identity per Binding

Slack access and agent access are separate. The Slack tokens let Agent Kourier read and post messages; the Binding
identity is the caller it presents to the agent backend. A backend that verifies bearer tokens checks that caller
through the agent itself or a front door.

Every turn in a Binding, chat or alert, runs as the Binding's single service identity: a bearer token from
`identity.tokenSecretRef`, or a user ID sent as `X-User-Id`. The person who typed or clicked is resolved and carried as
attribution, a prefix the agent can read and metadata, and recorded in the audit log with the Binding identity. The
prefix is informational; agents must not treat it as authorization.

This is a deliberate choice. An alert has no person, so an alert-driven run cannot use a person's token. In the
tested kagent 1.0.0-alpha5 OIDC setup, a session belongs to the identity that created it and turns from another owner
are refused. The backend sees the Binding identity; the agent's tools use the credentials configured for those tools.
So the Binding identity should hold only the permissions every member
of its channel is trusted with, and channels with different trust levels need separate Bindings.

For example, an alert starts an investigation, one channel member asks a follow-up, and another answers the agent's
question. All use the same backend identity, so they can continue the same kagent session without each obtaining a
backend token. You manage the credentials for the Binding, and automated alerts work without a person signing in.
The Slack people remain attributable in Agent Kourier's audit log.

The identity does not automatically grant tool permissions or configure Kubernetes RBAC. Configure those in the
backend and its tools. [Create the Binding identity](../how-to/create-binding-identity.md) explains the choices and
setup steps.

## The channel is the boundary

Access is channel-scoped. A Binding places one agent in one channel; anyone in the channel can talk to it and answer
its questions; a button press or typed answer counts only from a channel member and never from a bot. There are no
per-user or group permissions in v1, with one exception: a Binding with tool approvals can set
`interactions.approverGroups`, and then only members of those user groups may decide an approval.

Bindings are namespaced, and shared resources list the namespaces allowed to refer to them in `allowedNamespaces`. A
team's identity token Secret lives in its own namespace.

## Why kagent needs a front door

kagent's controller port (8083) trusts identity without verifying it. In insecure mode it takes `X-User-Id`; in its OIDC
(trusted-proxy) mode, which expects a validating proxy in front, it decodes the JWT without checking its signature, and a forged `alg: none` token was accepted. It also takes the
acting user from `X-User-Id` or `?user_id=` whenever `X-Agent-Name` is present, even alongside a valid token.

So Agent Kourier calls kagent only through a verifying front door, oauth2-proxy or agentgateway, which validates the
Binding's token and strips `X-Agent-Name`, `X-User-Id`, `X-Share-Token` and `X-Kagent-Insecure-Runtime-Identity`.
NetworkPolicies limit 8083 to that door and kagent's own components, and fence the kagent UI to its own verifying
proxy, because the UI relays `/mcp` and `/api` to the controller with `Authorization` intact. That UI proxy must not
accept the Binding's audience, or the token is valid through a second, wider door. The cluster's CNI must enforce
NetworkPolicy, or none of these fences exist. [Put a verifying front door before kagent](../how-to/front-door.md) has
the steps.

Isolation between Bindings holds because the door strips those headers and Agent Kourier never sends them. An
AgentBackend's static headers may not set any header that carries or selects identity, and a Binding with a token
never falls back to `userId` when the token cannot be read.

## Secrets

Secrets are read from Kubernetes Secrets by reference, through the API, and never written to logs, the audit table or
resource status. A Secret's value is held in a type whose every formatting path redacts. The chart grants list and
watch on Secrets in each namespace that holds a Secret its config values name (a ChatConnection's credentials, a Binding's token, an AgentBackend's header
value), in the release namespace with `store.type: postgres` (the DSN's Secret), and in each namespace of `rbac.secretNamespaces`; with `config.source: files` and none of these, in the
release namespace. With `config.source: files` such a Role covers every Secret of its namespace. With `config.source: crds` it names the Secrets
(`resourceNames`): the ones the config values and the DSN name, the one Secret that the served ChatConnection reads (the chart derives it from
`config.chatConnection`: the Secret of the connection's name, or `config.chatConnectionSecret`, in the connection's namespace), and the ones that `rbac.secretNames` lists for that namespace, and a
namespace that only `config.chatConnection` or `rbac.secretNames` names gets a Role for those names. A team's Secrets, which the chart cannot see, are granted by the team, with a Role in its own namespace. A namespace of `rbac.secretNamespaces` stays namespace-wide there, whatever `rbac.secretNames` says of it (read from
`templates/rbac.yaml`). With `config.source: crds` the chart also makes a
ClusterRole that lets the broker list and watch the three custom resource kinds, patch `bindings/status` and get the three CRDs by name. A rejected Binding token fails the turn, is re-read,
audited and counted; nothing falls back.

## What the people who write these objects can do

Agent Kourier reads three kinds of resource, `ChatConnection`, `AgentBackend` and `Binding`, from files or, with `config.source: crds`,
from custom resources. The checks are the same on both routes. What changes is who holds the pen: a ConfigMap has the people who may
edit it, and custom resources have everyone Kubernetes RBAC lets create or update them in a namespace. Decide who that is with the
rest of this section in mind.

**Write access to a Binding and an AgentBackend in a namespace is in effect read access to every Opaque Secret of that namespace that
the broker may read.** A Binding names its Secret (`tokenSecretRef`), and an AgentBackend names the Secrets behind its static headers
(`valueSecretRef`), by name and key and never by namespace, so each can only name a Secret of its own namespace. But the broker sends
what those Secrets hold to the AgentBackend's `url`, which the writer of the AgentBackend chooses:

- A Binding's identity goes with every request to the backend: the token as `Authorization: Bearer`, or, when the Binding has no token,
  `identity.userId` as `X-User-Id`. The AgentBackend's static headers go with it.
- The leader's probe sends them too. It fetches the agent's card from the same URL with the same credentials, and sends no message to
  chat, so a Binding that nobody writes in still sends its token at every probe.
- Whoever may write both a Binding and an AgentBackend in a namespace can therefore have any key of any Opaque Secret of that namespace that the broker
  may read sent to an address of their choosing, from the broker's pod.
- Narrower cases give less, not nothing. A writer of Bindings alone can name any key of any readable Opaque Secret of their namespace as a
  token and have it sent to the `url` of an existing AgentBackend that allows their namespace. A writer of an AgentBackend alone can name any readable
  Secret of its namespace in a header's `valueSecretRef`, and receives it once any served Binding uses the backend, and receives the token of every served
  Binding that uses it, from any namespace its `allowedNamespaces` lists. An AgentBackend that no served Binding uses is neither probed nor called.
- A writer of Bindings in the broker's own namespace can name the Slack Secret's `botToken` or the key of the database's DSN as a token. Keep tenants out
  of that namespace.

What bounds this is what the broker can read, and where it can connect:

- **The Secrets the broker may read.** With `config.source: crds` the chart's Roles name each Secret (`resourceNames`), and a team grants
  its own with a Role and RoleBinding ([the how-to](../how-to/migrate-config-source.md#let-a-team-add-its-own-binding)), so a Secret that nobody granted cannot be
  read through a Binding. The Role is the grant to the broker, not to a Binding: every writer of a Binding or an AgentBackend in that namespace can have the broker use the Secrets it
  names, so a team makes the Role for its token Secret only when that team's writers may do so. `rbac.secretNamespaces` grants a whole namespace and removes that bound for it, and so does every
  namespace-wide Role on the ConfigMap route, which is what the chart writes there.
  Removing a grant is not an immediate cut-off. The broker keeps a cache for each Secret it has read, and when a team deletes its Role, that cache goes on
  serving the value it holds: the broker logs the warning `secret informer cannot list or watch` for the Secret, and keeps calling the agent with the cached token, so the
  Binding stays `Ready` while its probes succeed. A Secret that is deleted or rotated after the Role went is not seen, and the broker uses the old value until it restarts
  (read from `internal/config/k8ssecrets/resolver.go`; not run). To cut a team off, delete the Binding or restart the broker as well.
- **Where the pod may connect.** The `url` is fetched from the broker's pod. The chart's NetworkPolicy is off by default. Turned on, it allows egress to DNS, the API
  server, the database, what you list in `networkPolicy.extraEgress`, and Slack's rule, which is port 443 to every address outside the private ranges. So a backend
  at a private address, such as an in-cluster Service, is unreachable until you list it in `extraEgress`, but any public HTTPS host stays reachable
  unless you set `networkPolicy.slack.cidrs` to `[]`. Slack's HTTP client honours the proxy environment variables (set them in `extraEnv`), but a backend's
  connection never does, so a backend's `url` must itself name a reverse proxy or an address that you list in `extraEgress`. Without the
  policy a `url` can name anything the pod reaches.
- **`allowedNamespaces`.** A Binding may use an AgentBackend or ChatConnection of another namespace only if that object lists the Binding's
  namespace. It lists none by default.
- **How much it reads.** The broker reads at most 1 MiB of an agent card, 4 MiB of the answer to a call (`GetTask`, `CancelTask`, `ListTasks`) and 2 MiB of one event of a
  stream, and a turn holds at most 341 KiB of the agent's reply in memory. No setting changes them. A response past a bound is an error, never a shortened read. A stream's length is not limited. A turn whose
  reply passes a bound ends with the outcome `response_too_large`: the thread is told, in one fixed sentence, that the agent sent more than Agent Kourier can read or hold, what it
  had shown stays, and Agent Kourier cancels the task at the agent, once and within 30 seconds. A cancel that fails is logged and leaves the task recorded, for a
  restart to reconcile; one whose own answer is too large to read counts as done, without being verified (a real a2a-go server answers a cancel with the whole task; not confirmed on kagent). A reply that is too large before the agent names a task ends the turn the same way, with no task to cancel, and
  the message is not sent again. On kagent 1.0 the final task holds every tool call and result of the turn again, so the tool output of a whole turn has to fit 2 MiB, though no
  result is that large. A lookup of a turn whose page of tasks is too large is asked again at smaller sizes, and if it still cannot be read the message is not sent, since the agent may have it: the turn
  ends with the outcome `lookup_incomplete` (also when the walk of the list passes five minutes or 200 pages) and the thread is told (see [the upgrade note](../how-to/upgrade.md#upgrade-to-the-release-that-lowers-the-bounds-on-what-it-reads)).
  **The bounds are of bytes, not of memory.** The memory it takes to read a response depends on the shape of its JSON, which the backend chooses: in a test process (not a pod), one
  event of 2 MiB took about 9 MiB of heap as one string and about 106 MiB as a data part of small objects, and one answer of 4 MiB took about 18 MiB as one string and about 210 MiB as
  small objects. Two or three such responses at once pass the default memory limit of 256Mi, and a stream that is open keeps buffers of about twice the largest event it has carried. Nothing limits how many streams or calls are
  open at once. So an AgentBackend has to be trusted as code that runs in the broker's pod, or given a broker of its own: a restart recovers the sessions, but it interrupts every turn of every
  tenant on that broker. A budget on what is decoded is not built.
- **How many objects, and how big.** Write access to these kinds can also cost the broker capacity, not only data: each Binding is probed (two probes every 5 minutes, four at a time, each up to 10 seconds)
  and has its status written, and every object is held in the broker's memory. Every string, list and map in the `spec` of the three kinds has a bound, in the schema and in `Validate`,
  and the keys of a map are bounded too ([Limits](../reference/limits.md) has each; a test fails for a field that has none). Those bounds hold an AgentBackend to about 156 KiB of JSON (32 headers of 4096 bytes, 256 allowed
  namespaces, a URL of 2048 bytes) and a ChatConnection to about 17 KiB. A Binding's bounds multiply: 50 triggers, each with 6 `match` and 6 `exclude` regular expressions and up to 32 `extract` entries of 512 bytes,
  32 `closeWhen` values of 256 bytes and a template of 8192 bytes, would allow about 2.1 MiB of JSON. What keeps a Binding well under that is the limit on what its regular expressions cost: 5000 instructions in all, so the
  largest Binding holds 1117 of the 2156 regular expressions that the other bounds allow (52%), and one that spends the budget on `match` and `exclude` entries before `extract` entries can hold about
  1235 (57%); `TestTheInstructionLimitHoldsABindingToAboutHalfOfItsRegexes` logs both. The largest Binding that `Validate` accepts is about 1.3 MiB of JSON, which is under what the API server takes in one write (etcd's default limit for a request is
  1.5 MiB, from its documentation and not checked here), so the bounds and not that limit are what size it. That figure depends on the instruction limit: a regex can be as dense as one character class of 512 bytes,
  which costs a few instructions, so the limit bounds how many regexes there are and not how much text. The bounds do not reach the metadata of an object: the API server checks each label on its own and limits neither how many an object has nor their total size
  (stated from Kubernetes' behaviour, not checked here). So the broker does not keep the labels, annotations, owner references, finalizers or managed fields of what it caches
  (`slim` in `internal/config/crsource/source.go`), and keeps the rest of the metadata and the `spec` and `status`.
  What an object holds in the broker's cache was measured by building the largest object that `Validate` accepts and decoding it as the cache does (an unstructured object, after the transform that drops what is above),
  taking the growth of the live heap after a collection over several copies, in a test process and not in a pod. `TestLargestObjectsAreWhatTheSecurityPageSays` in `internal/config/crsource` does it and logs
  the figures with `-v`. These objects carry no labels or annotations, so what is dropped does not change their figures. On one run, on Go 1.27.3 with GOMAXPROCS 8, on a macOS machine that other jobs were loading
  (load average about 28):

  | Object | JSON | Held by the cache | Copies that fill 205 MiB |
  |---|---|---|---|
  | The largest AgentBackend | 155.5 KiB | 0.17 MiB | about 1200 |
  | The largest ChatConnection | 17.1 KiB | 0.03 MiB | about 7900 |
  | The largest Binding | 1.28 MiB | 1.75 MiB | about 117 |

  The 205 MiB is what the chart's `GOMEMLIMIT` allows with the default memory limit of 256Mi. These figures are of the informer's cache alone; what the broker holds beyond it is below.
  What the broker does not hold of an object is what its metadata can add. `TestWhatTheCacheHoldsOfAnObjectWhoseSizeIsItsLabelsAndAnnotations` builds an AgentBackend of 1.27 MiB of JSON that is 80,458 labels and 200 KiB of
  annotations (16 held at once, same conditions): it took 5.7 MiB of heap each when labels and annotations were kept, and about 0.001 MiB each now. Of a Secret the broker keeps the type, the resourceVersion and the keys that the
  served objects name, not its other keys, which a namespace may fill up to Kubernetes's 1 MiB for one Secret (`trimTo` in `internal/config/k8ssecrets/keys.go`; `TestAMegabyteOfUnreferencedKeysCostsAlmostNothing`, 4 copies held at once):
  a Secret of 100,000 keys of 2 bytes took about 9 MiB of heap per copy when every key was kept, and about 1 KiB with one key kept. The informers' transform runs after client-go has decoded an object,
  so a large object still costs a burst of memory when it is read, which these figures do not show and which was not measured.
  The cache is not all that the broker holds of an object. A read of the config decodes each object the broker owns, keeps what it decoded for as long as the informer holds the same object, and resolves the Bindings into the
  Snapshot that the broker serves from. `TestWhatABrokerHoldsForEachLargestOwnedBinding` reads six of the largest Binding that way, with the same method and under the same conditions, and
  `TestWhatTheDecodedLargestAgentBackendHolds` and `TestWhatAFullBindingStatusAddsToTheCache` measure the other two figures below:

  | What the broker holds | For each of the largest Bindings |
  |---|---|
  | The informer's copy (the table above) | 1.75 MiB |
  | The decoded object, which the source keeps so that a read decodes only what changed, and which the Snapshot serves: the Snapshot copies the struct and shares the rest | 1.44 MiB |
  | The trigger table that the intake compiles for a Binding it serves (`chat.NewReplay`) | 0.63 MiB |
  | While a reload that found the Binding edited is built, the previous Snapshot, if something still holds it | 1.42 MiB more |

  So a Binding costs about 3.2 MiB (1.75 and 1.44) when it is not served and 3.8 MiB when it is, and the reload adds 1.4 MiB for each Binding it found edited until the previous Snapshot is released.
  By those figures about 54 of the largest Bindings, served, fill the 205 MiB, and about 39 do while a reload finds all of them edited (computed from the table, not measured). The Snapshot keeps only what it serves: a Binding that resolution leaves out (for
  instance one that names an AgentBackend that does not exist) and an AgentBackend that no served Binding names are not in it (`TestTheSnapshotRetainsOnlyWhatItServes` in `internal/config`). The source's decoded copy is another matter: it keeps one for
  every object it decoded, served or not. That is every AgentBackend of the cluster, whether or not a Binding names it (0.16 MiB for the largest, beside the informer's 0.17), and every Binding the broker owns. **Every broker
  owns the Bindings that name a ChatConnection that does not exist**, so that one of them reports each (it is the Binding's status that says so); such a Binding is served by none and costs every broker
  its informer copy and its decoded copy, about 3.2 MiB if it is the largest. A Binding that names another ChatConnection that exists is that connection's broker's: the informer of every broker holds it (1.75 MiB), and only its own broker decodes it.
  A Binding's decoded copy has no status, because the informer holds it. The figures assume an empty status. A grant on `bindings/status` can fill it, up to the CRD's bounds of 16 conditions with a message of up to 32768 bytes each: that adds 0.50 MiB
  (508 KiB in the test) to the informer's copy of each Binding the grant reaches, and nothing to the decoded one.
  So a namespace with a quota of N Bindings can have the broker hold up to about N times 3.2 MiB for them whether or not they are served, and 3.8 MiB when they are: 50 Bindings of the largest size are about 160 MiB and 190 MiB.

  **A read costs CPU in proportion to what changed.** The Reloader checks the source's fingerprint (the uid and generation of every object of the cluster, never of one broker's alone) every 10 seconds and reads again when it differs, and once a minute whatever it
  is, so a change to any object of the cluster makes a read on every broker. A read decodes, defaults and validates an object only if the informer has replaced the one the last read decoded. An edit does, and so does a status write (by the broker's leader, or by anyone who is granted `bindings/status`) or an update of metadata alone: the fingerprint does not move for those, and the next read, at the latest the once-a-minute one, decodes that one object again. A fresh list of the informer replaces every object (stated from client-go's behaviour, not tested here). In the same test the first
  read of the six Bindings took 146 ms (24 ms for each), a read after none had changed took 24 µs, and one after all six were edited took 135 ms (Go 1.27.3, GOMAXPROCS 8, a loaded macOS machine,
  not under `-race`). By the first figure the largest 50 Bindings cost about 1.2 seconds of CPU to decode, once, and again for each one that is edited. The resolution that follows each read runs on every read.

  **A Binding's regular expressions cost CPU for each message.** The limit on their instructions bounds that cost and does not make it small: `BenchmarkRegexCost` in `internal/chat` took 0.67 s for one message whose six fields are 16 KiB each, against a Binding at the budget of 5000 instructions with `.` regular expressions, and 0.81 s with `\pL`
  (the figures of [the upgrade page](../how-to/upgrade.md#upgrade-to-the-release-that-bounds-what-a-triggers-regular-expressions-cost), from its run, which says the machine and the load; they swing with the load). The events
  of one channel run one at a time on a lane of their own, and the lanes of different channels run at the same time, so a channel whose messages are such text can keep a core busy, and many of them slow the acknowledgments of the others. Whoever controls the text of that bot's messages chooses it. That page has the cases and the reasoning.

  Put a `ResourceQuota` in each tenant's namespace with `count/bindings.agentkourier.dev` and `count/agentbackends.agentkourier.dev`: the API server refuses a create past it
  with `exceeded quota` (measured on kind 1.37.0), and set `resources.limits.memory` for the quotas you give out. Add `count/secrets` too, to bound how many Secrets the namespace holds, and so how many a
  Binding's token or an AgentBackend's header can name that exist (Kubernetes's own object-count quota for Secrets, not run here). It does not bound what the broker follows: it follows each Secret that a served
  object names, by name and with an informer of its own, whether or not that Secret exists, and the bounds and the other two quotas bound those names. A quota limits the count and not the size of what is stored, so it sits beside the bounds and replaces none of them.
  Nothing limits how many objects a namespace holds without one. The limits you can run into, with what happens past each, are on [Limits](../reference/limits.md).
- **Names of Secrets that the broker may not read.** A team can name as many Secrets in its Bindings and AgentBackends as it likes, and the broker follows each name with an informer
  of its own, whether it exists or not and whether the ServiceAccount may read it or not. Nothing caps the names per namespace, and the `count/secrets` quota does not reach them, because the Secrets need not exist.
  What a denied name costs the other tenants comes out of one budget: all of the informers share one client, with a limiter of 50 requests a second. The first try of each name is not
  paced: it is two requests, and a thousand denied names take about 40 s of the limiter to be asked once, during which another tenant's request, a read of a Secret that no informer follows yet,
  queues behind them (inferred from how the limiter queues; not measured). After that the broker does not let client-go retry a denial for ever, as it would at a Secret every 30 to 60 s: it ends the informer
  at the first forbidden or unauthorized answer and asks for the Secret again only at a recheck of the config, once a minute, at 10 Secrets at once and then one every 100 ms, which is at most about 20 requests a second
  in all, however many names are denied. It logs a denial once for each Secret, not once for each try. The same rule applies to a Secret that was readable: a single forbidden or unauthorized answer for every Secret at once, from an expired or rotated token of the broker or an outage of the authorizer,
  ends every Secret informer, the broker's own included. Cached values are served and turns and readiness are unaffected, but a rotation of a Secret is not seen until a try reaches it, about 1 m 10 s later, plus 0.1 s for each denied Secret beyond the first 10 (read from `internal/config/k8ssecrets/resolver.go`; not run against a cluster). A review measured the behaviour before this, with 1650 denied names, in a Go test that runs the broker's Secret
  informers against a local API server with the broker's limiter (not a cluster): about 97 requests and 50 warnings a second for as long as it ran, client-side waits of 30 to 65 s, and, on a server that does not stream lists,
  another tenant's first read of a Secret failing with `cache not synced` after its 10 s. The same test after: the first two requests of each of the 1650 names took about 40 s and 1650 warnings in all, then 20.6 requests a second and no more
  warnings while a recheck went round, and another tenant's first read of a Secret took 41 ms. What remains is that bounded stream of requests, the open watch of each name that exists, and the first request of each name; the fix
  for a hostile tenant is to remove its Bindings or to quota them. The limits are on [Limits](../reference/limits.md).

What a writer of a Binding and an AgentBackend sees of your conversations. The agent of a Binding receives each mention of the bot in the channel, each reply in a thread the Binding owns
(with `chat.threadReplies: all`, the default, every reply in the thread, not only the ones that mention the bot), and with a `botMessage` trigger the messages of the bot that
the trigger names. Each comes with the sender's name, ID and email, where the chat platform gives them. That holds for any channel the bot is in, private channels too. Two
things add more. With `chat.threadReplies: mention`, a mentioned turn carries the thread's other replies as context. And on any Binding a mention of the bot by another bot or a
webhook makes the agent receive the thread from its root, every author's messages, whatever `threadReplies` says; that is on by default (`chat.botMentions: any`), and
`chat.botMentions: none` stops it ([Stored data](../reference/stored-data.md) describes what is kept). The agent also posts to the channel as the bot. Whoever chooses the
AgentBackend's `url` therefore also chooses who reads those conversations.

Other things a writer of a Binding can do:

- **Act as a user on a backend that trusts `X-User-Id`.** `identity.userId` is whatever the writer types, and an insecure-mode backend
  takes it as the caller. [Why kagent needs a front door](#why-kagent-needs-a-front-door) is the control.
- **Bind any channel the bot is in.** `chat.channel` is checked for its shape (`C…` or `G…` and capitals and digits), not for who owns the
  channel. Nothing ties a channel to a namespace, so keep write access to Bindings to people who may bind the channels they name.
- **Take a channel from a younger Binding.** Age alone decides who holds a channel, so an older Binding edited onto a younger one's channel displaces it
  ([Who holds a channel](config-routes.md#who-holds-a-channel)).
- **Set their own limits.** `interactions.toolApprovals`, its approver groups and the spend limits of a trigger are fields of the Binding, and so the writer's to set.

**Bindings that name one AgentBackend share it.** An AgentBackend is one door to everything behind its `url`, and each Binding that names it
goes through that door. What they share, and what they do not:

- **Shared.** The `url`. With `dialect: kagent-v1` the agent is named in the request path (`<url>/agents/<namespace>/<name>`), and `agent.name` and `agent.namespace` of a Binding
  are whatever its writer types, checked for their shape; `agent.name` is never checked against a list of agents, and `agent.namespace` is checked only when the backend
  sets `agents`, against the namespaces that it allows (below). So without `agents` every agent that the backend serves is in reach, and with it every agent in the namespaces it allows. With `dialect: a2a` the URL is the
  exact endpoint and the agent name is not sent, so the backend is that one agent. The dialect. And the static `headers`, whose Secret values are read in the AgentBackend's namespace
  and sent with every request of every Binding that names it, and with every probe. A Binding's writer cannot read those values, but each request they cause carries them.
- **Not shared.** The identity. Each Binding presents its own: the token of its own `identity.tokenSecretRef`, read in its own namespace, or its own `userId`. The connection
  to the agent is kept per Binding and agent, and a session belongs to its Binding.
- **So** a team that may write a Binding in a namespace that the AgentBackend allows can reach, with the Binding's identity and the shared headers, whatever the backend serves
  at that `url`, in the agent namespaces its `agents` allows if it sets them. Where the backend verifies that token and authorizes it for each agent, that check is the bound. Where it does not (a backend that takes `identity.userId`
  as the caller, or one whose static headers hold an API key that is the only credential), the bound is the AgentBackend itself.
- **A tenant can tell which agents exist, on `kagent-v1`, where the backend answers an unknown agent with a 404.** A Binding that names an agent makes the leader's probe ask the backend for
  its card, and a 404 is `AgentNotFound` in the Binding's status. A backend that authorizes first and answers 403 (`AgentForbidden`) reveals only the agents that the Binding's identity may use.
  This matters most when the tenant cannot read the token's Secret themselves.

**Limit which agents a Binding may name.** On `dialect: kagent-v1`, an AgentBackend can list the agent namespaces its Bindings may name in `agent.namespace`:

```yaml
spec:
  dialect: kagent-v1
  url: http://kagent-frontdoor.kagent.svc.cluster.local:4180
  allowedNamespaces: [payments, sre]
  agents:
    sameNamespace: true       # a Binding may name an agent in the Binding's own namespace
    namespaces: [shared]      # and any Binding that may use this backend may name one in shared
```

- **Left out, there is no limit.** That is how every AgentBackend behaves until you set `agents`: a Binding may name an agent in any namespace, and only the backend's front door decides.
- **Set, a Binding must match.** Its `agent.namespace` must be the Binding's own namespace, if `sameNamespace` is true, or one of `namespaces`. `sameNamespace` is the namespace of the Binding, not of the AgentBackend.
  What a Binding that names any other namespace costs depends on the route. **With custom resources** it is not served, and its `Accepted` condition is `False` with the reason `AgentNamespaceNotAllowed`;
  the check runs each time the broker reads the config, so setting or narrowing `agents` drops the Bindings that no longer match at the next read, and the others keep running. A task already sent to the agent is followed
  until it ends, across reconnects, with the backend it started with, and the broker's own cancel of a stuck task uses that backend too. The steps that look the Binding
  up find it gone: its next turn, one already queued included, is not sent; a pause for an approval or a question is not posted, and nothing ends that task on the agent,
  which stays waiting, while the timeout cancel of a prompt posted earlier fails at every sweep; a button press is not decided; and after a restart the broker does not follow the turn. **On the files route** the whole config
  fails to resolve, so nothing is dropped: at start the broker exits with code 2 and serves nothing (the chart's `config.rollOnChange` restarts the pod when the ConfigMap changes, and with SQLite the old pod is already stopped), and on a reload the broker keeps serving
  every Binding under the last good config, which has the old limit or none, and sets `agentkourier_config_stale` to 1. The message names the AgentBackend and the agent namespace
  and nothing else of the backend's spec. When the AgentBackend's newest generation is invalid, or it was deleted and is held, it adds that the limit is that of the generation being served; the problems of an
  invalid generation go only to a Binding in the AgentBackend's own namespace. A Binding that the backend does not let in at all (`allowedNamespaces`) is told only `NamespaceNotAllowed`.
- **Setting nothing is refused.** An `agents` value that allows no namespace (no `sameNamespace: true` and no `namespaces`) would reject every Binding, so the object is invalid. Leave `agents` out to impose no limit.
  `namespaces` holds at most 256 entries ([Limits](../reference/limits.md)).
- **An older broker ignores it.** On custom resources a broker from before the field does not know it and serves the Bindings with no limit ([a broker older than its CRDs](config-routes.md#unknown-fields)),
  so keep the broker and the CRDs on the same release before you rely on it.
- **Not on `a2a`.** There the `url` names the agent and `agent.namespace` is never sent, so `agents` would promise a control it does not give, and an `a2a` AgentBackend that sets it is invalid.
- **What it does not bound.** It checks the namespace of the agent, not its name: a Binding may name any agent of an allowed namespace. It does not give the Bindings different credentials, since the
  headers and the front door are still shared (above), and it does not replace a front door that authorizes the Binding's identity for each agent. It also bounds what a tenant can learn:
  on custom resources a Binding that names an agent outside them is not served, so it is not probed, and its status cannot be used to ask the backend whether an agent in another namespace exists.
  The limit itself is not secret from a team the backend lets in: by applying Bindings that name different namespaces and reading the reason, its author can learn which are allowed.
  `sameNamespace` compares the Binding's namespace, in the broker's cluster, with `agent.namespace`, a namespace on the backend. It keeps a team to its own agents only where the two mean the
  same, as for a kagent in the broker's cluster. Where kagent runs in another cluster whose namespaces belong to other teams, give each team its own AgentBackend, with only that team in `allowedNamespaces`
  and only its agents' namespaces in `namespaces`.

Where the teams must not share a door at all, use one AgentBackend per trust boundary: each has its own headers and Secrets, and lists in `allowedNamespaces` only the namespaces of the teams that may
use it. The list is empty by default, which allows the AgentBackend's own namespace and no other. On `a2a` the backend is one agent, so one AgentBackend for each agent is how you keep two
agents apart, and `allowedNamespaces` is the control over who may use it.

What a status tells a tenant, and what it does not. A Binding's `status` is read by whoever can read the Binding, and it is written for them:

- It never quotes the text of an object of another namespace, so it does not tell a tenant what another team's AgentBackend or ChatConnection says. An object
  that exists but does not list the tenant's namespace answers `NamespaceNotAllowed`, whatever else is wrong with it. A tenant can still learn, by naming it, whether
  an object of a namespace exists (`BackendNotFound` or `ConnectionNotFound`, against `NamespaceNotAllowed`).
- A `ChannelConflict` that was lost to a Binding of another namespace says only "a Binding in another namespace". The log has the name.
- A probe's message is a fixed sentence that names no host, address or port. It still tells whether the backend answered, and with which
  status code (`AgentNotFound` for a 404, `CredentialsRejected` for a 401, and so on), whether it did not answer in time, and whether the request failed some
  other way (`Unreachable`). It does not tell a name that does not resolve from a refused connection or an untrusted certificate. So a tenant who
  controls a `url` can tell which addresses answer an HTTP request and with what status, and the control is where the pod may connect. For the cause, a tenant's status says
  "the operator's log has the cause", and the person who runs Agent Kourier reads it.
- A `ChannelJoined` reason tells the tenant whether the bot can see the channel they named, is in it, and whether it is archived. A private channel that the bot is not in looks like
  one that does not exist.
- An `ApproversReadable` reason tells the tenant whether the bot can read each user group in the Binding's own `approverGroups`, and for `GroupNotFound` and `GroupsUnreadable` its message names the first group that fails by its place in
  that list and its ID, which the tenant wrote; the other reasons name no group. A group that does not exist and one that the bot cannot see are one reason (`GroupNotFound`), and a disabled group is expected to read the same (unverified). It says nothing of who is in a group.
- A secret value is never in a status. A condition's message is redacted and cut to 1024 bytes.

## Hostile text

Alert messages, other bots' messages, people's replies and agent output are all treated as untrusted.

- **Toward the agent.** Alert messages, other bots' messages and the thread replies that ride along as context reach
  the agent as data inside a tagged block that says it is never instructions. A person's own turn is sent as their text,
  with an attribution prefix. A trigger's prompt template may read only values that pass a strict shape check.
  Secrets are redacted from alert fields, thread context, tool arguments, audit rows and logs. A person's own message
  is stored and sent to the agent as written.
- **Toward the chat.** Agent output is escaped and stripped of control and format characters, and every post is sent
  with Slack's text parsing and unfurling turned off, so agent text cannot mention `@channel` or make Slack fetch a URL. Tool
  arguments are shown with secret-looking values redacted. A hostile-text conformance suite runs against every chat
  adapter, and an adapter ships only if it passes.

### What an agent can reach in chat

Hostile text can still steer a model, so two things are worth knowing about what a reply can do.

- **A declared person can be notified.** If the Binding sets `chat.mentions`, a reply that writes `@name` for a declared
  name makes Agent Kourier post a message that mentions that person and nobody else. Hostile alert text can steer the model
  into naming one, or the model can repeat a name quoted from an alert, and that notifies a real person. The set of people
  is fixed in config, and `chat.maxMentionsPerTurn` caps how many one turn notifies. That cap is per turn and nothing here limits mentions across
  turns, so an alert that keeps firing can notify the same person once for each turn its trigger's limits allow. A Binding with no
  `chat.mentions` has none of this.
- **A bare URL in a reply is a link.** Slack makes a bare URL into a link, and no escape in its text stops it, so a
  steered model can post a clickable URL on any host. The link shows its own address, it cannot be masked behind other
  text, and nothing unfurls it, so Slack does not fetch it. A person still has to choose to click it. A URL of a message in
  the connected workspace shows as Slack's own reference to that message.

- **Another agent can be woken.** A bot's mention of Agent Kourier starts a turn on any Binding with a `mention` trigger, unless it sets
  `chat.botMentions: none`, and Slack cannot tell an agent from any other bot. So hostile text that steers one agent into writing another's name
  wakes that other agent, and any bot or webhook in the channel can do the same directly. The turns are held to fixed limits, and the post and the
  thread reach the agent as untrusted data, but no hop limit exists, and the receiving agent decides what to trust. A bot's turn cannot answer a pending question, and a session that a bot's mention began takes a person's reply only if it mentions Agent Kourier, so a mention does not turn what people write in that thread into prompts. See
  [Chat triggers](../reference/chat-triggers.md#a-bots-mention).

### Not allowed, by design

These are refused on purpose. None of them can be configured, and a request to add one needs a decision of its own.

| An agent cannot | What happens instead | Why |
|---|---|---|
| Broadcast to a channel | `<!channel>`, `<!here>`, `@here` and `@everyone` in a reply stay plain text. `here`, `channel` and `everyone` are refused as declared names when a Binding loads. | One hostile alert could otherwise ping everyone in a channel. |
| Mention someone the Binding does not declare | A raw `<@U…>` or user ID stays plain text, and an undeclared `@name` notifies nobody and is counted. | The team opts people in. The model never chooses from the whole workspace. |
| Supply a user ID | The user ID comes only from `chat.mentions`, and the agent is told names, never IDs. | A hostile alert could otherwise name anyone in the workspace. |
| Mention a user group | `<!subteam^…>` stays plain text, and `chat.mentions[].user` refuses anything but a user ID. | Agent Kourier stays out of the paging path, and a group mention of an on-call rotation gets close to it. |
| Put its own words in a mention's notification | The message that mentions someone holds fixed words and the names from config. | The notification reaches people who may not be reading the thread. |
| Mask a link | `[text](url)` and `<url\|text>` are escaped, so the address is what shows. | A link that hides its target is how a phishing link works. |
| Make Slack fetch a URL, or show an image | Every post turns unfurling off, and `![alt](url)` is escaped. | A fetch happens with no one clicking, which can leak what the URL holds. |
| Write raw HTML, a date token or a platform's markup | They are escaped and shown as the characters they are. | The agent's text is data, never markup. |

Some limits come from this design and are not promises about the future:

- **A mention never goes inline.** The body stays literal text, and each mention follows it in a message of its own. In a
  test against Slack, a mention in a message's context line did not register as a mention when that message also had a body, so
  a separate message is the form that works on every output path.
- **A mention is not a page.** It is a convenience post, at most once for each person in a turn. It can be lost in a crash, Slack accepts a
  stale user ID with no error, and an outage of Agent Kourier sends nothing. A crash just after a mention is queued, or a timeout
  while it is recorded, can leave it with no audit row, and the row is not written later. Declare lasting owners, not a rota, and keep paging in your
  paging tool.

## The paging path

Agent Kourier is not an Alertmanager receiver. It reads the alert your alerting tool already posts to Slack, so an Agent Kourier outage does not stop an alert from reaching Slack.

## Not in scope

Agent Kourier is not an auth proxy for A2A (use agentgateway for that), does not build or host agents, and has no web
UI. At rest it encrypts nothing itself and relies on the volume or the database; what it keeps, and for how long, is in
[Stored data](../reference/stored-data.md).

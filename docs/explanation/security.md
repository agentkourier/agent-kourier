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
watch on Secrets only in the namespaces that hold a Secret the chart's config values name, plus `rbac.secretNamespaces`
and the release namespace. With `config.source: crds` those Roles name the Secrets (`resourceNames`)
instead of the namespace, except in `rbac.secretNamespaces`, the namespaces of `rbac.secretNames` get a Role for the names listed there, and a
ClusterRole lets the broker list and watch the three custom resource kinds, patch `bindings/status` and get the three CRDs by name. A rejected Binding token fails the turn, is re-read,
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
  read through a Binding. `rbac.secretNamespaces` grants a whole namespace and removes that bound for it, and so does every
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
  had shown stays, and Agent Kourier cancels the task at the agent, once and within 30 seconds. A cancel that fails is logged and leaves the task recorded, for the
  Stop control or a restart to reconcile; one whose own answer is the task, too large to read, counts as done. A reply that is too large before the agent names a task ends the turn the same way, with no task to cancel, and
  the message is not sent again. On kagent 1.0 the final task holds every tool call and result of the turn again, so the tool output of a whole turn has to fit 2 MiB, though no
  result is that large. A lookup of a turn whose page of tasks is too large is asked again at smaller sizes, and if it still cannot be read the message is not sent, since the agent may have it: the turn
  ends with the outcome `lookup_too_large` and the thread is told (see [the upgrade note](../how-to/upgrade.md#upgrade-to-the-release-that-lowers-the-bounds-on-what-it-reads)).
  **The bounds are of bytes, not of memory.** The memory it takes to read a response depends on the shape of its JSON, which the backend chooses: in a test process (not a pod), one
  event of 2 MiB took about 9 MiB of heap as one string and about 106 MiB as a data part of small objects, and one answer of 4 MiB took about 18 MiB as one string and about 210 MiB as
  small objects. Two or three such responses at once pass the default memory limit of 256Mi, and a stream that is open keeps buffers of about twice the largest event it has carried. Nothing limits how many streams or calls are
  open at once. So an AgentBackend has to be trusted as code that runs in the broker's pod, or given a broker of its own: a restart recovers the sessions, but it interrupts every turn of every
  tenant on that broker. A budget on what is decoded is not built.
- **How many objects.** Write access to these kinds can also cost the broker capacity, not only data: each Binding is probed (two probes every 5 minutes, four at a time, each up to 10 seconds)
  and has its status written, and every object is held in the broker's memory. The caps (50 triggers in a Binding, 32 `extract` entries in a trigger, 32 headers in an AgentBackend)
  limit how big one object is, and nothing limits how many a namespace holds. Put a `ResourceQuota` in each tenant's namespace with `count/bindings.agentkourier.dev` and
  `count/agentbackends.agentkourier.dev`: the API server refuses a create past it with `exceeded quota` (measured on kind 1.37.0). A quota limits the count and not the size
  of what is stored, so it sits beside the caps and replaces none of them. The limits you can run into, with what happens past each, are on [Limits](../reference/limits.md).

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

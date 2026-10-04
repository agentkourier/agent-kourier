# agent-courier: Specification

| | |
|---|---|
| Status | Draft v0.3, revised after judge review and grilling session (Appendix B) |
| Date | 2026-09-29 |
| Owner | Travis Bickford |
| Distribution | Internal first, designed to be open-sourced later |

## 1. Summary

agent-courier is a Kubernetes-native broker that sits between the outside world and
A2A-speaking agents. It does two things:

1. Receives alert webhooks (Alertmanager first) and turns them into agent
   investigations.
2. Lets people talk to agents from a messaging app (Slack first), including answering
   the agent's questions.

The two paths meet in one place: an alert opens a thread in an investigation channel,
the agent posts its findings there, and anyone in the channel can reply in that thread
to continue the same agent session.

Courier is an additional alert receiver, never a replacement. Existing alert
notifications are untouched, so a courier outage never loses a page.

Courier is always on. Agents may be suspended, scaled to zero, or restarting; courier
holds the connection to Slack and the webhook endpoint and wakes the agent by calling it.

The first deployment is a four-week pilot: one team, a read-only investigation agent,
and a separate kagent 1.0 alpha environment alongside the production kagent 0.10
install (section 12).

## 2. Problem and gap

Research on 2026-09-29 (sources in Appendix A) found:

- **kagent** has no webhook trigger (issue #2185, open, with a community `WebhookTrigger`
  CRD proposal and no maintainer progress since) and no Alertmanager, Grafana, or
  PagerDuty ingestion. Upstream has no maintained Slack integration: a full Slack bot PR
  (#1017) was closed unmerged, and the Slack template repo is a single-agent demo, last
  pushed 2025-08. Native `channels` support in 0.10.x applies only to OpenClaw/Hermes
  harness backends and is absent from the 1.0 types.
- A kagent maintainer stated that a third-party broker is needed for events that arrive
  while an agent is asleep, and that they are redesigning this area. The issue (#2261)
  was closed as not planned on 2026-09-27.
- **google/ax** is a sandbox orchestrator with a gRPC lifecycle API. It has no chat,
  webhook, or conversation concept. `TaskStatus.pending_approval` exists in its proto,
  but no code uses it; approval policies are a roadmap item.

### 2.1 Prior art

| Project | Covers | Why courier is still needed |
|---|---|---|
| giantswarm/klaus-gateway | Slack front door for kagent 1.0: A2A v1, HITL cards, streaming, per-user identity | Refuses any turn without a person's token, so alert-triggered runs are excluded by design. Coupled to Giant Swarm's platform. See 2.3. |
| apexlnc/kslack | kagent's closed Slack PR (#1017), moved to its own repo | Org transfer never happened; last push 2025-11 |
| eliasecchig/a2a-gateway | Generic A2A bridge to Slack, Discord, Telegram, Google Chat | No alert ingress, no kagent HITL, no Kubernetes resource model |
| gke-labs/kube-agents `a2a/gateway` | Backend-neutral chat adapter interface with a Slack adapter | Chat only. Worth reading for adapter interface shape. |
| zanglang/kagent-slack-bridge, a2a-slack-template | Minimal Socket Mode bridges | Targets kagent 0.10 URLs, single agent, no HITL |
| khook | Kubernetes events to kagent, with dedup | Kubernetes events only, targets the 0.x session API, last push 2026-01 |
| HolmesGPT, Robusta, argus, SherlockOps | Alert investigation with Slack output | Each is its own agent, not a gateway to arbitrary A2A agents |
| agentgateway | A2A and MCP proxy with auth, RBAC, rate limits | No chat or alert ingress. Complementary: recommended in front of the backend. |

### 2.2 The gap

The chat half of this problem is partly solved (klaus-gateway). The unsolved part, and
courier's reason to exist, is the combination:

1. An Alertmanager alert group becomes a Slack thread that is also a live agent session.
2. Humans continue that session and answer agent interactions in the thread.
3. It works with any A2A agent, not one platform's roster.
4. It is configured declaratively and runs independently of the agents.

### 2.3 Why build courier, not extend klaus-gateway or contribute upstream

Verified against source on 2026-09-29.

**Versus extending klaus-gateway**

| # | Benefit of courier | Evidence |
|---|---|---|
| K1 | **Alert-driven runs are possible.** klaus-gateway makes every call as a person and states that "a turn without a person's token is refused instead of running as a machine identity." An alert has no person. Adding alert ingress there would reverse one of its core design principles, not just add a feature. | klaus-gateway `docs/kagent-a2a.md`, Identity section |
| K2 | **No platform prerequisites.** klaus-gateway requires Dex account linking, muster, agentgateway as the data plane, Giant Swarm roster annotations (`ui.giantswarm.io/*`), and a Harness admission label before an agent is offered. Courier needs an A2A endpoint and a Slack app. | `docs/deployment.md`, `docs/kagent-a2a.md` |
| K3 | **Public protocol, not private APIs.** klaus-gateway compiles against stubs generated from kagent's controller protos (`AgentTemplateService`, `AgentInstanceService`) and never fetches an agent card. Courier uses public A2A plus one published extension. kagent's A2A gateway creates a session when no `contextId` is sent, so nothing kagent-internal is needed. This means courier works with non-kagent agents and is insulated from kagent's internal API churn during the 1.0 alphas. | klaus-gateway `pkg/kagent/gen`; kagent `docs/architecture/a2a-gateway.md` |
| K4 | **Team self-service.** klaus-gateway is configured through Helm values with one default agent. Courier's namespaced `Binding` lets application teams wire their own channel to their own agent without touching platform configuration. | `docs/deployment.md` |
| K5 | **Roadmap control.** klaus-gateway has four human contributors, all at Giant Swarm, and its design decisions trace to their internal platform plans. It has one star. Contributions would be judged against their platform's needs. | GitHub contributors; issue #350 |

**Versus contributing to kagent**

| # | Benefit of courier | Evidence |
|---|---|---|
| U1 | **Upstream has declined chat in core.** On the Slack bot PR, a maintainer said it "should probably be in a separate repo, instead of having it in the main repository." The issue asking for a Slack bot (#475) was closed as not planned. The separate repo (kslack) was never adopted into the org and went stale. There is no in-core path to contribute to. | kagent PR #1017, issue #475 |
| U2 | **Backend neutrality.** Code in kagent serves only kagent. Courier serves any A2A agent, which is the only route by which the ax side of the original goal can be served at all. | 6.3 |
| U3 | **Independent lifecycle.** A broker has to stay up while agents are suspended. A kagent maintainer stated this needs "a 3rd party to act as a broker." | kagent issue #2261 |
| U4 | **Delivery on our schedule.** kagent 1.0 shipped five alphas in nine days. Landing a large feature into that moving target means repeated rebases and maintainer review cycles. Internal-first delivery (D1) needs a shorter path. | kagent releases |

google/ax is not a contribution target: it is a sandbox orchestrator with no conversation
model, and the one proposal to add one (#430) has had no maintainer response.

**What we give up**

- **Per-user identity.** klaus-gateway already solves per-person OBO tokens and session
  shares. Courier deliberately does not (D14). If per-person tool authority becomes a
  requirement, klaus-gateway's approach is the reference.
- **Working code.** klaus-gateway has a tested Slack HITL surface, idempotent instance
  creation, sealed share storage, and restart resumption. Courier starts from zero.
  Read `docs/slack-hitl-surface.md` and `docs/kagent-a2a.md` before Phase 1 and borrow
  the designs; both projects are Apache-2.0.
- **Upstream alignment.** Upstream is redesigning the broker area (#2261). A native
  kagent solution could overlap courier's chat path (R9). Courier's hedge is backend
  neutrality and the alert-to-session model, which upstream has not proposed.
- **Shared maintenance.** Everything courier does, we maintain alone until it is
  open-sourced and adopted.

Where upstream contribution still makes sense: bug reports and fixes found while
building against kagent's A2A gateway and HITL extension, and feedback on #2185 so any
upstream webhook trigger stays compatible with courier.

## 3. Goals

- G1. Alertmanager webhook to agent investigation, with dedup, rate limits, and a daily
  cap. Courier is never in the paging path.
- G2. Slack conversations with agents through @mention, thread replies, and DMs.
- G3. One thread per alert, conversational, sharing a session with the investigation.
- G4. Agent interactions answered from Slack: questions with choices as selects in the
  MVP; tool approvals as buttons once a write-capable agent is bound.
- G5. Live-updating agent output in Slack.
- G6. Declarative configuration, suitable for GitOps: the same resource schema is
  loaded from files during the pilot and served as CRDs after hardening.
- G7. Backend-neutral: any A2A 1.0 agent works for chat and alerts. Backend-specific
  behavior is isolated in a dialect.
- G8. Every inbound event, agent call, and interaction decision is audit logged.
- G9. Every investigation collects a useful or not-useful rating, so value is measured,
  not assumed.

## 4. Non-goals (v1)

- A native google/ax adapter. ax is reached through an A2A agent (section 6.3).
- kagent 0.10.x support. The pilot uses a separate kagent 1.0 alpha environment (D24).
- Replacing existing alert notifications or sitting in the paging path.
- Tool approvals before a write-capable agent is bound (Phase 5).
- Chat platforms other than Slack, and alert sources other than Alertmanager. The
  interfaces allow them; v1 does not ship them.
- Per-user or group-based authorization. Access is channel-scoped.
- Per-user identity at the backend. Every turn runs as the Binding's service identity
  (D14).
- Being an auth proxy for A2A. Use agentgateway for that.
- Building or hosting agents, prompts, or tools.
- A web UI.

## 5. Decisions

| # | Topic | Decision |
|---|---|---|
| D1 | Audience | Internal first, clean seams for later OSS release |
| D2 | Backend coupling | Protocol-first, A2A only |
| D3 | ax | Supported only through an A2A-speaking agent |
| D4 | kagent version | 1.0 alpha, A2A 1.0 (see D24 for where it runs) |
| D5 | MVP scope | Thin slice of both ingress paths |
| D6 | Deployment | Kubernetes-native; CRDs and controller arrive in hardening (see D30) |
| D7 | Resource shape | Separate composable resources: `AgentBackend`, `ChatConnection`, `Binding` (`AlertSource` removed in v0.3, see D29) |
| D8 | Alert source | Alertmanager |
| D9 | Alert flow | Thread per alert, conversational, in a dedicated investigation channel (see D22) |
| D10 | Alert noise | Dedup by group key plus per-binding rate limits and daily cap |
| D11 | On resolve | Post a note; the session stays open for the thread's lifetime (see D28) |
| D12 | Authorization | Channel-scoped bindings |
| D13 | Approvals | Slack buttons, any channel member may approve. Deferred to Phase 5 (see D25). |
| D14 | Identity | One service identity per Binding for every turn; the Slack user is carried as attribution (revised in v0.2, was per-user email) |
| D15 | State | Pluggable store, SQLite default, Postgres for HA |
| D16 | Slack transport | Socket Mode and Events API over HTTP, both supported |
| D17 | Invocation | @mention, replies in courier-owned threads, DMs |
| D18 | Output | Live-updating message |
| D19 | Language | Go |
| D20 | Positioning | Build courier rather than extend klaus-gateway or contribute to kagent; rationale in 2.3 (added v0.2) |
| D21 | Reply while busy | Queue the reply and send it when the active task finishes or a pause ends (answered, timed out or cancelled); a reply never lands while an interaction is pending (added v0.2; pause rule from the WP-28 split) |
| D22 | Alert path | Courier is an additional Alertmanager receiver (`continue: true`). Existing notifications are unchanged. Investigation threads go to a dedicated channel and link back to the alert. (v0.3) |
| D23 | First agent | Read-only investigation agent: queries metrics, logs, and Kubernetes state; recommends, never changes. (v0.3) |
| D24 | Pilot environment | Production runs kagent 0.10.x. The pilot uses a separate kagent 1.0 alpha install with a read-only agent holding production read credentials, fed by production Alertmanager. Courier moves to production kagent at 1.0 GA. (v0.3) |
| D25 | Interactions scope | Ask-user (including nested ask-user) ships in the MVP, because an unanswered pause stalls a thread. Tool approvals move to Phase 5, with the first write-capable agent. (v0.3) |
| D26 | Rate-limited alerts | One digest line per window in the investigation channel plus a metric; no per-alert message. (v0.3) |
| D27 | Value signal | Useful and not-useful buttons on each investigation, stored per alert name and agent. (v0.3) |
| D28 | Session lifetime | Courier keeps the thread-to-session mapping for 90 days and resumes the same `contextId`; kagent handles suspend and resume. No courier idle timeout. (v0.3) |
| D29 | Alert routing | Teams route alerts to their Binding through their own prometheus-operator `AlertmanagerConfig`. The webhook token is a Secret in the Binding's namespace. Cluster-level alerts go to a platform-team Binding through the central Alertmanager config. (v0.3) |
| D30 | Config loading | Resources are Go types with kubebuilder markers from day one, loaded from YAML files in Phases 1 to 3. CRDs and the controller land in hardening. (v0.3) |
| D31 | Spend cap | `maxRunsPerDay` per Binding, in addition to the rate limit. (v0.3) |
| D32 | Pilot security | kagent 1.0 alpha runs in OIDC mode with per-Binding service tokens. Courier calls kagent only through a token-verifying front door (oauth2-proxy or agentgateway), never the controller port directly, because kagent's OIDC mode decodes but does not verify JWTs (WP-01 finding). NetworkPolicies restrict the controller port to that front door and kagent's own components, and fence each relay to the controller (the kagent UI) to its own verifying proxy (the chart's oauth2-proxy), which must not accept the Binding's audience. The front door strips `X-Agent-Name`, `X-User-Id`, `X-Share-Token` and `X-Kagent-Insecure-Runtime-Identity` from every request, because kagent lets them select the acting user or session (WP-03, WP-01d; implemented and proven in the full tier by WP-01d). The cluster's CNI must enforce NetworkPolicy. (v0.3, corrected after WP-01, WP-01b, WP-03, WP-01d and their reviews) |
| D33 | Pilot exit | Four weeks, one team. Continue if at least half of rated investigations are useful and engineers use threads unprompted. (v0.3) |
| D34 | Binding token source | For the pilot, a Binding's bearer token is read from `identity.tokenSecretRef` on every call (cached from a watch, never stored elsewhere), and rotated outside courier before it expires (Dex: 24 h). A rejected credential (`ErrCredentialsRejected`, HTTP 401/403 at the front door) fails the turn, re-reads the Secret, is audited and raises a metric; courier never falls back to `userId`. Minting with the client-credentials grant plugs in behind the same `TokenSource` interface once H-02 picks a provider that supports it (Q9). Secret values are held in a type whose every formatting path redacts. Secrets courier reads are type `Opaque`; the watch keeps values for Opaque Secrets only (key names for every other type), in the namespaces courier is granted. After the pilot, narrow to one watch per referenced Secret (a `metadata.name` field selector plus RBAC `resourceNames`). (Resolves Q10, after WP-03; narrowed by the WP-21e1 review.) |
| D35 | Chat-neutral model | Slack sits behind one platform-neutral seam, `chat.Platform` (6.2), so a second chat platform is one adapter and no change to session, interact, render, outbox or store. The model carries literal text with no markup field; each adapter neutralises markup for its platform and passes a shared hostile-text conformance suite before it ships. The outbox stores the versioned neutral message and the adapter renders it at delivery; a payload of the earlier, Slack-shaped form is abandoned as invalid with a log line, not migrated. Abandon metrics use neutral reasons (channel_gone, too_long, invalid_content, other), with the platform's own code in the log. The Slack message ID string is unchanged (`slack:<connection>:<channel>:<ts>`), and the store's columns keep their names. (Owner-approved 2026-10-02; WP-37.) |
| D36 | Bot messages | A bot's message counts only through an explicit `botMessage` trigger in the Binding's `chat.triggers` (8.2) that matches the bot by ID, never by display name. Courier's own messages, and those of every bot without a trigger, stay ignored, so bots cannot answer each other in a loop. The whole message is untrusted text (R2). (Proposed 2026-10-04 for the Slack-sourced alerts trial, CT-1; accepted by the owner 2026-10-04.) |

## 6. Architecture

### 6.1 Components

```
 Alertmanager ──webhook──┐
                         ▼
                  ┌──────────────┐      ┌───────────────┐
 Slack ◄─────────►│   Ingress    │─────►│  Event queue  │  (durable, in store)
  Socket Mode     │  adapters    │      └──────┬────────┘
  or Events API   └──────────────┘             ▼
                                        ┌───────────────┐
                  ┌──────────────┐      │    Router     │  resolves Binding,
                  │ Config loader│─────►│  + policy     │  dedup, rate limit,
                  │ (file → CRD) │      └──────┬────────┘  daily cap
                  └──────────────┘             ▼
                                        ┌───────────────┐     A2A 1.0
                  ┌──────────────┐      │ Session mgr   │────────────► Agent backend
                  │    Store     │◄────►│ + A2A client  │◄──stream──── (kagent 1.0, or any
                  │ SQLite / PG  │      │ + dialect     │              A2A agent)
                  └──────────────┘      └──────┬────────┘
                                               ▼
                                        ┌───────────────┐
                                        │   Renderer    │──► Slack (post, edit, buttons)
                                        └───────────────┘
```

| Component | Responsibility |
|---|---|
| Ingress adapters | Authenticate the inbound request, normalize it to an internal event, persist it, acknowledge fast. Slack requires a response within 3 seconds, so no agent call happens on the request path. For chat messages the persist step is the session manager's `Submit`: a local, idempotent (by messageId) write of the turn, after which the adapter acknowledges; a failed Submit is not acknowledged, so Slack redelivers (WP-29). |
| Event queue | Durable table in the store. Gives at-least-once processing across restarts. |
| Config loader | Validates references and builds the in-memory routing table. Phases 1 to 3: reads YAML files and reloads on change. Hardening: a controller watches CRDs and writes status conditions. Both produce the same Go types. |
| Router | Finds the Binding for an event, then applies dedup, rate limits, and the daily cap. |
| Session manager | Maps a conversation key to an A2A `contextId`. Owns task lifecycle, stream reconnects, and the reply queue. |
| Dialect | Per-backend differences: URL layout, agent card location, identity header, HITL payload. |
| Renderer | Turns A2A stream events into Slack messages, edits, and interactive blocks. Posts through the outbox (8.6). |
| Store | Sessions, event queue, outbox, dedup keys, pending interactions, feedback, audit log. |

### 6.2 Interfaces

Four Go interfaces carry the extension points. Only one implementation of each ships in v1,
except Store.

```go
// AlertEvent is source-neutral: Status, Fingerprint (dedup key), StartsAt, Summary,
// Labels, Annotations, SourceURL. Prompt templates see only this type, never the raw
// payload, so a second source type needs no template changes.
type AlertAdapter interface {  // Alertmanager; selected by Binding.spec.alerts.type
    Authenticate(*http.Request) error
    Parse(*http.Request) ([]AlertEvent, error)
}

// chat.Platform is the one seam between courier and a chat platform (Slack in v1; package
// internal/chat, the adapter in internal/chat/slack, selected by ChatConnection.spec.platform).
// Run delivers what people write (Inbound) and press (Interaction, already decoded into
// question-indexed answers) to the sink, and nothing else in courier names a platform. What
// courier says goes out as an Outgoing: a Message (header, body, context, tool line, note,
// mention, buttons), a Prompt (questions, each free text or choices) or an Ending (a
// prompt's replacement). The adapter lays it out and renders it at delivery. The outbox
// stores the same Outgoing, versioned, so a restart needs no knowledge of Block Kit.
type ChatPlatform interface {  // Slack; the Go name is chat.Platform
    Run(ctx context.Context, sink Sink) error // Sink: Message(Inbound), Interaction(Interaction)
    Post(ctx context.Context, t Target, o Outgoing, tag string) (Ref, error)
    Find(ctx context.Context, t Target, tag string, since time.Time) (ref Ref, found bool, err error)
    Update(ctx context.Context, r Ref, o Outgoing) error
    Stream(ctx context.Context, t Target, to, markdown string) (Stream, error) // ErrNoStreams if none
    StreamAt(r Ref) Stream                 // a stream another run started, found again by its Ref
    Ephemeral(ctx context.Context, t Target, user string, m Message) error
    React(ctx context.Context, r Ref, name string) error
    IsMember(ctx context.Context, channel, user string) (bool, error) // fails closed
    IsBot(ctx context.Context, user string) (bool, error)             // fails closed
    ResolveUser(ctx context.Context, id string) (User, error)
    RefAt(channel, thread, id string) Ref  // a Ref from the parts a caller recorded
    Fits(p Prompt) error                   // pure; ErrTooLarge if one message cannot hold it
    Classify(op Op, err error) Verdict     // Transient, Permanent (with a neutral Reason) or FatalAuth
    Capabilities() Caps                    // streams, edits, ephemerals, inline inputs, text limits
}

// Interaction is a pause the agent needs a human to answer. Its kind is ToolApproval
// (one or more tool calls with arguments) or AskUser (questions, each with optional
// choices and multi-select). Either kind may be nested: raised by a sub-agent, in which
// case it also carries the path to that sub-agent (name, task, and context).
type Dialect interface {       // a2a (generic), kagent-v1
    Endpoint(backend *AgentBackend, agent AgentRef) (url string, err error)
    // Sets the Binding identity (bearer token, else X-User-Id, never both) and the
    // backend's static headers; refuses reserved static headers and leaves req unchanged.
    Authenticate(req *http.Request, creds Credentials) error
    Attribute(msg *a2a.Message, actor User)            // who actually typed or clicked
    // Fetches the card through the front door and returns a JSON-RPC client whose
    // credentials are pinned to the front door host; follows no redirects, no proxies.
    Connect(ctx context.Context, backend *AgentBackend, agent AgentRef, creds Credentials) (*a2aclient.Client, error)
    // nil, nil: no structured pause (free text only where the dialect allows it).
    // ErrUnusablePause: paused but unanswerable, so report it and cancel the task.
    ParseInteraction(task a2a.Task) (*Interaction, error)
    // Classifies an artifact part as a tool call or tool result; ok false means plain content.
    ToolPart(part *a2a.Part) (tool ToolPart, ok bool)
    BuildResponse(in Interaction, answer Answer) (a2a.Message, error) // errors if incomplete
}

type Store interface { /* sessions, queue, dedup, interactions, audit */ }
```

`Credentials` holds the Binding identity (token or user ID) and the AgentBackend's static
headers with Secret values resolved. It never formats or marshals its values. The caller
applies `Authenticate` last, on a clone of each outgoing request.

**The chat-neutral contract (D35; spec 8.4, R2).** The model carries literal text only: a
header, context line, note, button label, hint, question, choice, answer, tool name or argument
holds characters, never markup, and the one rich field, `Body.Markdown`, is untrusted
CommonMark in which an adapter may allow emphasis, code and lists and must not allow a mention,
broadcast, link, image, HTML or date. There is no field for a platform's own markup, so no
caller can bypass an adapter's escaping. Each adapter neutralises markup for its platform,
removes hidden characters, cuts every field at the platform's limit with a marker, keeps
requests inert, and shows a user as a mention only for an ID of its platform's own shape. A
hostile-text corpus runs against every adapter (`chattest.Conformance`) in every place text can
go and by every method that sends; an adapter ships only if it passes. Courier's own code keeps
what no platform differs on: redaction, truncation policy, and the fixed words of notices. The
trigger table of 8.2 is the neutral `chat.Intake`, which a platform's `Run` feeds. The broker
builds the adapter named by `ChatConnection.spec.platform`; a platform with no adapter is a
wiring error, and a lint rule denies every package except the adapter, the broker and cmd (tests
aside) the Slack adapter and its client library, so a new package is covered from its first file.

### 6.3 Backends

Courier speaks A2A 1.0 over JSON-RPC: `SendStreamingMessage`, `GetTask`,
`SubscribeToTask`, `CancelTask`.

| Dialect | Chat and alerts | Approvals | Notes |
|---|---|---|---|
| `a2a` | Yes | Free-text only. An `input-required` pause with no recognized extension is shown as text, and the next thread reply is sent as a plain message. | Any A2A 1.0 agent that does not advertise a structured HITL extension |
| `kagent-v1` | Yes | Yes: tool approvals, ask-user questions, and nested sub-agent interactions, through extension `https://kagent.dev/extensions/hitl/v1` | `POST /agents/{ns}/{name}`, card at `.well-known/agent-card.json`. Auth by bearer token in OIDC mode, or `X-User-Id` in insecure mode. |

kagent 1.0.0-alpha5 validates HITL responses far less than assumed (WP-02, measured). Too
few answers, or an answer outside the offered choices, is accepted. An empty answer list
or a wrong request id returns JSON-RPC `-32603`, as an SSE event after a `WORKING` update
over HTTP 200, and leaves the task in `WORKING`: still so after 20 s, when a valid answer,
a plain-text message to the task, and a new turn in the context each get `-32004`;
`CancelTask` clears it. An unknown tool-approval id also returns `-32603`, and the task was
still `WORKING` 3 s later. A tool-approval reply with no decisions is treated as plain user
text, and the tool does not run. Not measured: waits beyond 20 s, recovery across a
restart, or any recovery other than `CancelTask`. The `kagent-v1` dialect therefore
validates every answer itself before sending (completeness, choice membership, known
request id), never uses the free-text fallback, and uses `CancelTask` to recover a wedged
task. For both ask-user and tool approval, the structured payload appears only when the
request carries `A2A-Extensions: https://kagent.dev/extensions/hitl/v1`; without it the
pause is still `input-required`, with the question (or ADK's approval hint) as text.
a2a-go v2.6.0's `a2aext.NewActivator` puts that header on the wire only when the agent card
lists the URI under `capabilities.extensions` (Q1, measured with controls). Whether a reply
must also carry the header is not measured. The payload's presence is fixed when the pause
is created, not when it is read: a later `ListTasks` without the header still shows each
pause as its creating request left it (WP-03 fixture oidc-identity/16, measured), and
a2a-go returns stored tasks unfiltered on `GetTask` too (source). So the `kagent-v1`
dialect sends the header on every send, resend and answer, and a pause re-read after a
restart (WP-33) has its payload only if the turn that made it carried the header.

**google/ax.** ax has no message model, so courier does not call it. Two supported
patterns, both of which look like a plain A2A backend to courier:

1. **Supported:** a kagent agent that delegates sandboxed work to ax. Users have
   demonstrated this pattern (ax issue #394).
2. **Unsupported upstream, possible in principle:** an A2A-speaking agent packaged in
   an ax task image, reached through `atenet-router` with a static `ate-target-actor`
   header from `AgentBackend.spec.headers`. ax removed its own A2A agent (issue #128),
   so this image would be ours to build and maintain. Not in v1.

In plain terms: v1 serves the "kagent or ax" intent for kagent, and for ax only
through kagent. A native adapter is revisited if ax ships durable interactions
(issue #430, an outside proposal with no maintainer response yet) or makes
`pending_approval` functional.

## 7. Resource model

API group `agentcourier.dev/v1alpha1` (placeholder, to be confirmed). All resources are
namespaced. Cross-namespace references are denied unless the target resource lists the
referring namespace in `spec.allowedNamespaces`. In Phases 1 to 3 these resources are
loaded from YAML files with the same schema (D30).

Ownership split: the platform team owns `AgentBackend` and `ChatConnection`. Application
teams own `Binding` and the webhook token Secret next to it.

v0.2 had a platform-owned `AlertSource` holding the webhook token. Teams now route
alerts through their own `AlertmanagerConfig`, which can only read Secrets in its own
namespace, so the token moved to the Binding and `AlertSource` had nothing left to hold.

```yaml
apiVersion: agentcourier.dev/v1alpha1
kind: AgentBackend
metadata: { name: kagent-pilot, namespace: courier-system }
spec:
  dialect: kagent-v1                 # a2a | kagent-v1
  url: https://kagent-frontdoor.kagent-pilot     # verifying proxy, never :8083 (D32)
  headers: []                        # static headers, values may come from Secrets;
                                     # identity headers are reserved; see section 7
  allowedNamespaces: ["sre", "payments"]
---
kind: ChatConnection
metadata: { name: slack-main, namespace: courier-system }
spec:
  platform: slack
  mode: socket                       # socket | http
  credentialsSecretRef: { name: slack-main }   # bot token, app token or signing secret
  directMessages:
    defaultBindingRef: { namespace: sre, name: sre-alerts }
  allowedNamespaces: ["sre", "payments"]
---
kind: Binding
metadata: { name: sre-alerts, namespace: sre }
spec:
  agent:
    backendRef: { namespace: courier-system, name: kagent-pilot }
    name: k8s-agent
    namespace: kagent
  identity:                          # every turn in this binding runs as this identity
    userId: courier-sre-alerts       # insecure mode: sent as X-User-Id
    tokenSecretRef: { name: courier-sre-alerts-oidc }  # OIDC mode (pilot default, D32)
  chat:
    connectionRef: { namespace: courier-system, name: slack-main }
    channel: C0123456789             # investigation channel ID, not name
    output: live                     # live | final
    triggers:                        # empty means [{type: mention}]; see 8.2
      - type: mention
      - type: botMessage             # alerts Alertmanager itself posts to this channel
        from: { botId: B0123456789 } # the poster's bot ID, never its display name (D36)
        preset: alertmanager         # fills in match, extract, thread and promptTemplate
        # $ anchors the receiver: without it, receiver=slack-critical also matches ...-kind
        match: { titleLink: 'receiver=slack-critical$' }
        exclude: { title: '^Prometheus heartbeat$' }
        thread: { cooldown: 2h }     # overrides the preset's 4h
        limits: { maxRunsPerDay: 20 } # overrides the preset's 40; its rateLimit (5 in 10m, 2 at once) stays
  alerts:
    type: alertmanager
    tokenSecretRef: { name: courier-webhook-token }  # same namespace as the Binding
    promptTemplate: |                # Go template over the normalized AlertEvent
      Investigate this alert and report likely cause and next steps.
    rateLimit: { maxRuns: 5, per: 10m, maxConcurrent: 2 }
    maxRunsPerDay: 40
  interactions:
    askUser: true
    toolApprovals: false             # Phase 5
    timeout: 30m
    approverGroups: []               # reserved, see R1; empty = any channel member
  feedback:
    enabled: true
  session:
    threadTTL: 2160h                 # 90 days
status:
  conditions: []                     # Ready, BackendReachable, ChannelJoined
  webhookPath: /hooks/alertmanager/sre/sre-alerts
```

The matching team-owned route, next to the Binding:

```yaml
apiVersion: monitoring.coreos.com/v1alpha1
kind: AlertmanagerConfig
metadata: { name: courier, namespace: sre }
spec:
  route:
    receiver: courier
    continue: true                   # existing notifications still fire
    matchers: [{ name: severity, value: critical }]
  receivers:
    - name: courier
      webhookConfigs:
        - url: https://courier.example.internal/hooks/alertmanager/sre/sre-alerts
          sendResolved: true
          httpConfig:
            authorization:
              credentials: { name: courier-webhook-token, key: token }
```

Rules:

- **Secret keys.** A single-value Secret reference (`alerts.tokenSecretRef`,
  `identity.tokenSecretRef`, and a header's `valueSecretRef`) takes an optional `key` that defaults to `token`. `ChatConnection.spec.credentialsSecretRef` names one Secret with
  fixed keys: `botToken`, `appToken` (Socket Mode), and `signingSecret` (HTTP mode).
- An empty namespace in a reference means the referring resource's namespace.
- `ChatConnection.spec.directMessages.defaultBindingRef` may name a Binding in any
  namespace, because the platform team owns connections and DM access is gated on
  membership in that Binding's channel (8.2). The Binding must use the same connection.
- **One bearer token per request.** The Binding identity (D14) owns the `Authorization`
  header. `AgentBackend` has no credentials of its own; a gateway in front of the
  backend, such as agentgateway, validates the Binding's OIDC token. Static headers may
  not set any header that carries or selects identity: `Authorization`,
  `Proxy-Authorization`, `Cookie`, `X-User-Id`, `X-Agent-Name`, `X-Share-Token`, or
  `X-Kagent-Insecure-Runtime-Identity` (D14, D32; kagent alpha5 honours the last four,
  WP-03 and the WP-20c review). If a Binding sets `identity.tokenSecretRef` and the token
  cannot be read, the turn fails; it never falls back to `userId` alone. A Binding with
  `tokenSecretRef` must target an OIDC-mode backend: kagent's insecure mode ignores the
  token and, with no `X-User-Id`, runs every turn as `admin@kagent.dev` (source).
- One channel maps to at most one Binding. A second Binding for the same channel is
  rejected with a status condition.
- Each Binding with `alerts` gets its own webhook path. Alertmanager routing decides
  which alerts reach which agent; courier does no label filtering in v1.
- prometheus-operator adds a namespace matcher to every `AlertmanagerConfig` route, so a
  team's route only sees alerts labelled with its namespace. Cluster-level alerts reach
  a platform-team Binding through a route in the central Alertmanager config.
- `continue: true` is required on every route to courier, so courier stays out of the
  paging path. The pilot runbook checks this before enabling a route.

## 8. Flows

### 8.1 Alert to thread

1. Alertmanager POSTs to the Binding's webhook path with a bearer token. The same alert
   also reaches its existing receivers, because the courier route uses `continue: true`.
2. The adapter checks the token in constant time against the Binding's Secret, parses
   the payload, writes the event to the queue, returns 200. Alertmanager retries only
   on 5xx, so a 200 is returned only after the event is durable.
3. Router computes the dedup key: `hash(binding, groupKey)`.
   - **No open session for the key:** check the rate limit and daily cap. If allowed,
     post the root message to the investigation channel with a link back to the alert
     (the Alertmanager `externalURL` and the alert's `generatorURL`), create a session,
     and send the first A2A message.
   - **Open session, status firing:** update the root message (alert count, last seen).
     No new agent run.
   - **Status resolved:** post a resolved note in the thread and mark the root message.
     The session stays open for the thread's lifetime (D28). The in-flight task is not
     cancelled.
4. When the rate limit or daily cap blocks an investigation, courier posts nothing for
   that alert. It increments a metric and, once per window, posts a single digest line:
   "7 alerts not investigated in the last 10m (rate limit)". The alert itself already
   reached people through its existing receiver.
5. The A2A `messageId` is derived from the dedup key and `startsAt`, so a retried
   delivery cannot start a second conversation.
6. When the investigation's task completes, its final message carries Useful and Not
   useful buttons (8.7).

Alert payload content is inserted into the prompt inside a delimited block labelled as
untrusted data. See risk R2.

### 8.2 Chat

These are the defaults, which a Binding with no `chat.triggers` gets. `chat.triggers` extends
them (below the table).

| Trigger | Behavior |
|---|---|
| @mention in a bound channel (not in a thread) | New thread, new session |
| Reply in a thread courier owns, with or without an @mention | Continues that session |
| @mention inside a thread courier doesn't own, in a bound channel | New session anchored to that thread; courier owns it from then on |
| Reply without an @mention in any other thread | Ignored |
| Any message posted by courier itself (including `message_changed` from its own edits) | Ignored, always: courier would otherwise answer itself without end |
| A message posted by any other bot or app | Ignored, unless a `botMessage` trigger names its bot ID (D36): two bots could otherwise reply to each other without end, and bot text is untrusted (R2) |
| An edit or deletion of any message (`message_changed`, `message_deleted`), or another non-message subtype | Ignored. Plain messages, `thread_broadcast` and `file_share` (its text only) count as messages |
| DM | Routed to the connection's `defaultBindingRef`. Allowed only if the user is a member of that Binding's channel. One session per DM thread. |

**`chat.triggers` (D36).** An empty list is `[{type: mention}]`, the table above. A list that
names triggers is the whole list: a Binding that lists only `botMessage` triggers starts no session
from an @mention, in or out of a thread (the intake says so once, at info). The row for a reply in
a thread courier owns is no trigger and holds either way, so people can follow up in the thread of
an alert without a mention. A Binding adds
triggers; v1 has two types, `mention` (the @mention rows) and `botMessage`. Triggers are tried in
order and the first that matches handles the message. A `botMessage` trigger accepts messages
from one bot, named by `from.botId` and never by display name, and then:

- `match` and `exclude` map a message field to a regex. The fields are `text`, `title`,
  `titleLink`, `fallback`, `footer` and `attachmentText`; the adapter flattens a legacy
  attachment into them. Every `match` entry must match and no `exclude` entry may.
- `extract` names values taken from a field by a regex, whose named group carries the entry's
  name. A message in which an extract does not match does not match the trigger.
- `thread.by: key` gives messages with the same extracted `key` one thread, anchored as for an
  @mention inside a thread courier doesn't own. `cooldown` (1m to 168h) allows no new turn for a
  key inside the window, `closeWhen` (`{state: RESOLVED}`) posts a note and starts no turn, and
  `maxIdle` (twice the cooldown to 720h; default the larger of 24h and twice the cooldown) abandons
  an open thread that has had no message of its key for that long.
- `promptTemplate` is a Go template over the trigger's view of the message. It is parsed and run
  once at load against a placeholder view, so a mistyped field name rejects the Binding.
- `limits` bounds how many investigations the trigger starts (below): `rateLimit` (`maxRuns`,
  `per`, `maxConcurrent`) and `maxRunsPerDay`, the shapes of `alerts.rateLimit` and
  `alerts.maxRunsPerDay` in section 7. A trigger with neither a preset nor `limits` is unlimited.
- `preset: alertmanager` fills in `match`, `extract`, `thread`, `limits` and `promptTemplate` for
  Alertmanager's Slack receiver (title `FIRING (n) - <alertname>` or `RESOLVED - <alertname>`,
  the default fallback with the group label values). Whatever the Binding sets overrides it per
  entry: a `match`, `exclude` or `extract` entry replaces the preset's entry of that name, and
  `thread.by`, `thread.cooldown`, `thread.closeWhen` and `promptTemplate` replace the preset's.
  `limits` merges by field: each of `rateLimit.maxRuns`, `rateLimit.per`, `rateLimit.maxConcurrent`
  and `maxRunsPerDay` that the Binding sets replaces the preset's, and the others stay (an
  explicit `maxRunsPerDay: 0` is no cap, not unset). An empty value means unset, so in v1 a Binding cannot switch a preset entry off. To drop a
  `match` entry, override it with a regex that always matches (`'.*'`); to change more than
  that, leave `preset` out and write the trigger in full.

Regexes are RE2 (linear time), at most 512 bytes, and compile at load, as do the named groups
and the template: a Binding that fails any of them is rejected. A bot message that no trigger
matches is ignored and counted (`courier_trigger_unmatched_total{binding}`, plus a debug log line
that names no message text).

How a bot message reaches the triggers:

- The Slack adapter hands one on only if the intake says a `botMessage` trigger of that channel's
  Binding names the bot, so the bots no trigger names never reach it, and an intake that cannot
  say gets none. It flattens the message's first legacy attachment into `title`, `titleLink`,
  `fallback`, `footer` and `attachmentText`, and `text` is the message's own text: all raw, as
  delivered, neither rendered nor unescaped (`mrkdwn_in` changes how Slack shows text, not the
  text). Later attachments are not read, so that a trigger matches the fields of one attachment.
  Both an incoming webhook's `bot_message` and an app's own post count as a bot's message.
- Courier's own messages, by user ID or by bot ID, are never handed on, even if a trigger names
  courier's own bot ID; that mistake is logged once, at warn, since load cannot know the ID.
- An edit or a deletion of a bot's message, and any other subtype, is ignored as it is for a person.
  A bot's reply inside a thread is ignored and not counted: a trigger is about a message that opens
  its own thread. A bot's message is never a mention, whatever its text says.

What a matched bot message does to its thread, for a trigger with `thread.by: key`. The key is
hashed (with the connection, channel and bot) and the store holds that hash, one row per key with
its thread, when it opened, was last seen and closed, and when its cooldown ends (unset until the
turn has started); the logs name a
key by the first eight characters of the hash. A *close* is a message whose extracted fields all
equal `closeWhen`; any other match is a *fire*. The time is when courier took the message.

| Message | The key has | Decision | What happens |
|---|---|---|---|
| fire | no thread, or a closed one whose cooldown has ended, or one whose turn never started and is over a minute old, or an open one idle past `maxIdle` | `started` | Reserve the thread, anchored to this message. Render the first prompt and queue it as a turn of a session anchored to this message (below); once that is taken, mark the thread started, which is when the cooldown begins. |
| fire | an open thread that is not idle past `maxIdle`, or a closed one inside its cooldown | `repeat` | Note "Fired again at `<time>`." in that thread, and reopen it. No turn. |
| fire | this very message as its thread (a redelivery) | `dedup` | Nothing if its turn started; otherwise ask for the start again, since the first ask failed. |
| fire, key empty | (no thread: the key came out empty) | `empty_key` | An investigation of its own, with no cooldown and no note. A warning, once for the trigger. |
| close | an open thread whose turn started | `closed` | Note "Resolved at `<time>`." in that thread, and close it. No turn. The cooldown goes on. |
| close | no thread, a closed one, or one whose turn never started | `close_unknown` | Ignored and counted. |
| fire | would be `started`, and a limit of the trigger is reached (`limits`, below) | `limited` | No turn, nothing posted in the alert's thread, and the reservation is given back, so the key holds nothing: its next fire starts once there is capacity. Counted, and in the digest of its window. |
| fire | the turn was refused: the session hook returned `chat.ErrNotStarted` (the first prompt failed its check, or its template could not run) | `not_started` | The event is acknowledged and not delivered again. The thread stays reserved and unmarked: no cooldown, and the key's next fire takes it over after a minute. The hook has already posted the note, audited and counted the refusal (below). |

The cooldown runs from the message that started the turn, not from the last message, so a key
starts at most one turn in each window however often it fires and resolves (a flapping alert is
one investigation). An open thread takes its key's messages, so an alert that keeps repeating
(Alertmanager's `repeat_interval`) stays one thread; but one with no message of its key for
`thread.maxIdle` is abandoned, whatever the cooldown, and the next fire starts a new
investigation. That is what mends a resolve that never registered: courier down past Slack's
retry window, or a resolve that arrived before its fire. `maxIdle` is at least twice the
cooldown, at most 720h, and unset it is the larger of 24h and twice the cooldown. It must also
be longer than the sender's repeat interval (Alertmanager's `repeat_interval`), or an alert
that is still firing is counted abandoned between two of its repeats and gets a second
investigation. Twice the cooldown covers that only because the preset's cooldown and
Alertmanager's default `repeat_interval` are both 4h; a longer `repeat_interval` needs a
`maxIdle` set above it. The cooldown begins when the turn has started and not
when the thread is reserved, so a turn that could not start holds no key. A session hook that
fails for a reason that will pass (a store error) returns an ordinary error, and the redelivery
asks again; one that will not start the turn (a refused prompt) returns `chat.ErrNotStarted`,
which acknowledges the event and counts `not_started`. Either way the next fire, a minute after
the reservation, takes the key over. The
notes are fixed text and a time, never any of the message; each is one outbox row named after
the message that caused it, so a redelivered event adds none. Each step is idempotent and a note
is queued before the change it goes with, so an error returns to Slack as an unacknowledged
event and the redelivery finishes the work; all of it is local writes, as it runs on the
acknowledgement path. A trigger with no `thread` has none of this: every match starts a turn.
A sweep deletes the rows that no longer do anything once an hour: a closed thread past its
cooldown, and an open one with no message of its key for 720h, which is the longest any `maxIdle`
can be, so it never deletes a thread that some trigger still counts as live. A key whose row is
gone starts afresh, as one with no row does.
Every decision is counted as `courier_trigger_decisions_total{binding,decision}`.

**Limits of a trigger (D26, D31).** `limits` holds a start to the trigger's `rateLimit.maxRuns` in
any `rateLimit.per`, `rateLimit.maxConcurrent` investigations running at once and `maxRunsPerDay`
in any 24 hours. The alertmanager preset's are the example of section 7: 5 in 10m, 2 at once, 40
a day (the cooldown folds an alert's repeats into its thread already, so they bound distinct
alerts, a storm). `per` is 10s to 24h. Only a start is limited: a repeat, a close and a note are
never. The check is the last step before the turn is asked for, and when it stops a start the
decision is `limited`: no turn, no note (D26), the key's reservation is given back so that it
holds nothing, and `courier_trigger_limited_total{binding,trigger,limit}` is counted, where
`limit` is `rate`, `concurrent` or `daily` (when several are reached, the daily is reported
first, then the rate, then the concurrency). A stopped alert reached people through its own
receiver. Giving the reservation back, rather than holding the key with no investigation behind
it, means a repeat of that alert, or a re-fire after the storm, starts an investigation as soon as
there is a place, and no thread of the key says "fired again" about an alert nobody looked at.

The counts are durable (`trigger_runs`, section 9), so they hold across a restart. Rate and daily
are sliding windows over the starts of the Binding's trigger (a start counts from the moment it
is decided until the window passes it); a fixed window would let twice the limit through at a
boundary. A start is one row for its message, so a redelivered event is counted once. A start whose turn
was refused (`not_started`) stays counted for the rate and the daily cap, since the refusal posts a
note of its own: a template that stops fitting during a storm is held to the limits like any other
start, and its alerts past them are `limited`, silent and in the digest. It is no running
investigation, so it uses no concurrency place. Concurrency counts the
trigger's started investigations whose session has an active task (running, or paused for a
person) or a turn that is queued and not yet delivered: a start that was taken has a place at
once, not when the agent gets to it. An investigation that never ends is counted for at most 24
hours, after which its row is purged. The trigger's place in the list names it, so a Binding that
reorders its triggers shares counts across the two for up to 24 hours. The check is a local read
and write, as the acknowledgement path allows. A trigger with no thread rules is limited the same
way.

**The digest line (D26).** Once for each window and each trigger, courier posts one line at the top
level of the Binding's channel through the outbox: "7 alert messages not investigated in the last 10m
(rate limit)." (more than one limit in a window: "(rate limit: 5, daily cap: 2)"). A window
opens with the first stopped start after the last digest and lasts the trigger's `rateLimit.per`
(an hour for a trigger with only a daily cap); the line is posted when it has ended, within
15 seconds, so that it can count. The row it asks the outbox for is named after the connection,
Binding, trigger and the start of the window, so a crash or a failed write between queueing it
and recording that it was queued posts it once. The line holds a count, a length and a limit and
never any text of an alert. It counts alert messages, not distinct alerts: a repeat of a stopped alert that is stopped
again is another. A window that ended while courier was down is posted when it is up.

**The first turn of a started thread.** The broker renders the trigger's `promptTemplate` over
the trigger's view of the message (`.Channel`, `.State`, `.Key`, `.Fields`, `.Link`, `.Source`)
and appends the whole message in the untrusted block (R2). `.Link` is the Slack
permalink that courier builds from the workspace address `auth.test` returned when it connected,
the channel and the message's ts (`https://<workspace>.slack.com/archives/<channel>/p<ts without
the dot>`); it is never a field of the message, so a sender's `titleLink` cannot reach the
instructions. The rendered prompt is submitted through the session manager's own intake, the path
a mention takes (the session row, the queued turn and a wake, all local writes), with the alert
message's ts as the thread and its ID as the turn's `messageId`, so a redelivery is the same
turn. Nobody wrote the turn, so it names no user: it is filed under the manager's system user
and carries no attribution prefix, which is for what a person wrote, and its audit rows name no
person. A person's reply in that thread then continues the session without a mention, as in
any thread courier owns. The text of a template reads `.Fields` entries, not `.Key`: the preset's
key is the group's label values, which hold spaces, and a value in the instruction part must be
name-shaped, so a template that reads `.Key` is refused.

A prompt that fails its check (a value outside the name shape, too long, a `.Link` that is no
link) or cannot be rendered (a template that reads an entry the trigger does not extract, or
fails to run) starts no turn. The thread gets a fixed note, "Not investigated: `.Fields.alertname`
failed the prompt check." (or "the prompt template could not be rendered."); the audit log gets an
event that names the message, the trigger, the value's name, the reason and the value's length and
never the value; and `courier_trigger_refused_total{binding,trigger,ref,reason}` is counted, where
`ref` and `reason` are from closed sets. The hook then returns `chat.ErrNotStarted`.

One window remains between queueing a turn and marking its thread started: a failed write or a
crash there, with the event not delivered again, leaves a reservation whose turn was taken. A later
fire that finds it stale would take the key over and start a second turn. So before taking over
a stale, unmarked reservation of another message, the intake asks the reply queue whether that
message's turn exists (queued or sent); if it does, it marks that thread started, with the cooldown
running from its own message, and the fire is a repeat in it.

Conversation key: `(connection, channel, thread_ts)` maps to `contextId`. The `messageId`
is derived from the Slack message itself, `(channel, message ts)`, not the event ID:
Slack delivers an @mention as both an `app_mention` and a `message` event with different
event IDs, and both must map to one turn.

The session row is written before the first A2A call, holding the key and the Binding;
`contextId` is filled in from the backend's first response. If courier crashes in
between, the retried send reuses the same `messageId`, with no `contextId`, as on the first
send. kagent deduplicates on it (WP-02, measured). While the task still runs, the replay
is itself a live stream of the same task and context: a `WORKING` snapshot with the text
so far, then the remaining deltas, the `lastChunk` chunk and the final task. After the
task has ended, the replay returns one snapshot of that task. Either way the task shows a
single run (one history message, one artifact); whether the model was called a second
time is not measured. A resend carrying a `contextId` (a crash during a thread reply) is not
deduplicated (WP-03): while the task runs it gets `-32004`, not a live join, and after the
task ends it starts a second full task. Courier never blindly resends into an existing
context. Before resending a turn whose task it has not recorded, it calls `ListTasks` with
the `contextId`, follows `nextPageToken` to the end, and looks for a task whose history
holds the turn's `messageId` (kagent records it there). Found: reconcile as in 8.3
(`GetTask`, then `SubscribeToTask` if not terminal). Absent: send. A `-32004` on that send
means some task is active, possibly an `input-required` pause, so the turn is queued as
below and the lookup repeats when the active task ends. Whether the model runs twice for a
duplicate task is not measured; the second task runs in full. An expired session is kept, not deleted, so courier can tell a reply in an
expired thread (notice) from a reply in a thread it never owned (ignored).

**Thread lifetime.** The mapping is kept for `threadTTL` (90 days) after the last
activity, and a reply at any point in that window resumes the same `contextId`. kagent
suspends and resumes idle sessions on its own. If the backend reports that the session
no longer exists, courier says so in the thread, clears the stored context, and the next
message in the thread starts a fresh context. Courier treats `TaskNotFound` for a recorded
task, or a first response whose `contextId` differs from the stored one, as the session no
longer existing (UNCONFIRMED: kagent's signal for a lost session is not measured). After
`threadTTL` the thread is expired for good: every later reply in it gets a fixed notice
saying the conversation has expired and that a new mention in the channel starts a new
one. The mention's two events (app_mention and message) give one notice.

**Reply while a task is running.** kagent allows one active task per session and
rejects a new message while one runs, with JSON-RPC `-32004` "resource conflict" (inside
the SSE stream for streaming sends; WP-02). A rejected message is not recorded: resending
it unchanged, with the same `messageId`, after the active task ends (cancelled or
completed) starts a new task in the same context (WP-02, measured). A queued reply
therefore keeps its `messageId`, and a resend that still gets `-32004` stays queued. Courier queues the reply per session, marks it
with a reaction, and sends queued replies in order, one per turn, when the active task
reaches a terminal state, or when a pause ends (answered, timed out or cancelled). A reply
sent while an interaction is pending is held the same way; it is neither sent nor treated as
the interaction's answer. A user can stop the active task from the thread; the stop control
calls `CancelTask`, and queued replies then proceed. A turn's `messageId` is
`slack:<connection>:<channel>:<ts>`, so the `app_mention` and `message` events for one
message are one turn. When `-32004` comes from a task courier did not record, courier
resends with backoff (at most 60 s apart), repeating the lookup before each resend, and
never drops the reply.

**Attribution is load-bearing (WP-36h, measured on kagent).** kagent keeps courier's
attribution at the start of the text a person wrote (`[Slack: <name> (<user id>)] ...`), and
it replays the agent's own earlier replies to the model as role `user` messages. The
attribution prefix is therefore the only thing that tells the model a person's message
from its own earlier output. Courier must prefix every message a person wrote; removing or
making the prefix optional breaks multi-turn context.

### 8.3 Streaming and long-running tasks

- Courier calls `SendStreamingMessage` and edits one Slack message as text arrives.
  kagent streams each artifact as chunks keyed by `artifactId` (WP-02, measured). A live
  `SendStreamingMessage` opens with two `SUBMITTED` task events and a `WORKING` update;
  the first text chunk has no `append` flag, later deltas carry `append: true`, and one
  final `lastChunk: true` chunk carries the artifact's full text with no `append` flag.
  `SubscribeToTask`, and a replayed in-flight send, open instead with a task snapshot
  holding the text so far, followed by `append: true` deltas, the `lastChunk` chunk and
  the final task. `GetTask` during a run returns `WORKING` with the text so far. Tool
  calls and results arrive as separate data-part artifacts, each a single
  `lastChunk: true` chunk. The client appends on `append: true` and otherwise replaces,
  per artifact.
- Edits are throttled per message (default one edit per 2 seconds, configurable) and
  coalesced. Tool calls appear as short status lines.
- Slack's `chat.startStream` requires a recipient user in channels. Chat threads use
  it with the requesting user as recipient. Alert threads have no requesting user, so
  they use throttled `chat.update`. Rate limits for both are verified in Phase 0.
- kagent 1.0 rejects A2A push notifications, and there is no replay cursor. On a dropped
  stream courier calls `GetTask` to reconcile state, then `SubscribeToTask`.
- On courier restart, non-terminal tasks found in the store are reconciled the same way.

### 8.4 Interactions

Ask-user and nested ask-user ship in the MVP. Tool approvals ship in Phase 5 (D25). Until
then, a Binding has `toolApprovals: false`; if its agent pauses for a tool approval
anyway, courier denies it, says so in the thread, and raises a metric, because the
Binding was declared to point at a read-only agent.

1. The task enters `input-required`. The dialect parses it into an `Interaction`. A pause
   the dialect can't use (for `kagent-v1`: a missing or malformed payload) is reported in
   the thread with its reason, audited, and the task is cancelled (6.3).
2. Courier stores it as pending and renders it in the thread:
   - **Ask user:** each question with its choices as a select (multi-select where
     allowed), or a free-text input when there are no choices, and a Submit button.
   - **Tool approval (Phase 5):** tool name and full arguments, Approve and Deny
     buttons. Several tool calls in one pause get one control each, plus approve-all
     and deny-all.
   - **Nested:** rendered as its inner kind, labelled with the sub-agent that raised it.
   - All agent-originated text is untrusted (R2): escaped (`&`, `<`, `>`), stripped of
     control and format characters, rendered as `plain_text` where Slack allows, and
     truncated to Slack's limits. The one exception is code in a markdown body: Slack shows
     code literally (probe of 2026-10-04, `tasks/plans/slack-code-rendering.md`), so the
     fences and code spans that courier itself writes carry their text raw, except `<`
     before `!`, `@` or `#`, which is still written `&lt;`; every other backtick, and every
     run of three or more `~`, is escaped, so Slack opens no code of its own. A stream that
     courier resumes after a restart escapes everything, code included. Tool arguments go in an escaped code block, with
     secret-looking values redacted (by key name and by token shape) before display. A choice
     list over Slack's select limit falls back visibly; no choice is ever dropped.
   - Every courier post, update and stream is sent with `parse` unset (or `none`), no
     `link_names`, and `unfurl_links=false` and `unfurl_media=false`. Escaping alone does not
     stop `@here` under `link_names`, and an unfurl makes Slack fetch an agent-written URL
     with no click, a zero-click way to leak data (R2). Text is rendered once, at the last
     step, never twice and never unescaped (WP-32b review).
3. On submit, courier verifies the request came from Slack, confirms the user is a
   channel member and not a bot, and checks the answer is complete. An incomplete
   answer is rejected in Slack; nothing is sent to the backend.
4. Courier records the decision and sends it through the dialect under the Binding's
   identity, with the Slack user as attribution.
5. First complete submission wins. The message is updated to show who decided and
   when, and the controls are removed.
6. On timeout, tool approvals are denied, questions are cancelled with `CancelTask`, and
   the thread is told.

**Measured on kagent (WP-36h, Phase 1 full tier).**
- kagent always calls the model with `stream=true` and always offers `ask_user` as a tool.
- An `ask_user` answer reaches the model as a tool message holding a JSON list, one entry
  per question in order: `[{"answer":["PostgreSQL","MySQL"],"question":"..."}, ...]`.
- A denied tool approval reaches the model as an empty assistant message followed by a
  tool message `{"error":"error tool \"<name>\" call is rejected"}`, so the model can
  continue knowing the call was refused.
- A paused task's final event posts no message of its own (D8, measured on kagent and the
  stub): the prompt is the only message carrying the question.
- Not measured: a non-streaming model call, and a nested sub-agent's ask.

### 8.5 Identity

kagent binds a session to the identity that created it and refuses turns from any
other identity. Courier therefore runs every turn in a Binding, chat or alert, as the
Binding's single service identity (`spec.identity`).

- **Backend identity:** the Binding identity, as a bearer token in OIDC mode or
  `X-User-Id` in insecure mode. The owner is the token's `userIdClaim` (`email` in the
  pilot), else its `sub`, so the token must carry the same owner claim on every mint.
- **Ownership is not a boundary on its own.** kagent lets any caller that sends
  `X-Agent-Name` choose the acting user with `X-User-Id` or `?user_id=`, even alongside a
  valid token (WP-03). Isolation between Bindings holds only because the front door strips
  both headers (D32) and courier never sends them.
- **Attribution:** the Slack user who typed or clicked is resolved (`users.info`,
  cached) and attached by the dialect as message metadata and a prefix the agent can
  see. The prefix is informational; agents must not treat it as authorization, which is
  always the Binding identity. Courier's audit log records the Slack user, email, and Binding identity together. For a chat
  turn the prefix goes at the start of the first text part, never as a part of its own.
  For HITL answers, attribution is metadata only: kagent rebuilds an answer into tool
  confirmation parts and drops the message text and metadata (alpha5 source,
  `go/adk/pkg/a2a/hitl.go` `BuildResumeHITLMessage`; not measured), so the agent never
  sees who answered. The audit log and the updated Slack message (8.4 step 5) record it.
- **Consequence:** kagent's logs and any tools the agent calls see the Binding identity,
  not the person. This is consistent with channel-scoped authorization (D12): the
  channel is the access boundary, and the Binding identity should hold only the
  permissions every channel member is trusted with.
- **Future:** per-user identity through kagent session shares (`READ_WRITE` share
  tokens) remains possible behind the same dialect method, if per-person tool authority
  is needed later.

### 8.6 Delivery guarantees

The event queue is at-least-once, and `chat.postMessage` has no idempotency key. A crash
between posting and recording the result would otherwise post twice. Courier prevents
this with an outbox:

1. Before posting, courier writes an outbox row with a deterministic ID derived from
   the event and the message's role (root, reply, digest, notice).
2. Every post carries Slack message metadata (`event_type: courier_message`, payload
   holding the outbox ID).
3. On recovery, an outbox row with no recorded message ref is reconciled first: courier
   reads recent channel or thread history with metadata included and adopts a matching
   message instead of posting again.
4. Edits (`chat.update`) are naturally idempotent and need no reconciliation. Reactions
   (role `reaction`, e.g. the queued marker of 8.2) also go through the outbox for
   durability but need no reconciliation: `reactions.add` on a message that already has
   the reaction answers `already_reacted`, which counts as delivered.
5. Interaction prompts (8.4) are posts too, so they go through the outbox with role
   `reply` and the interaction ID as the source ID. Outbox IDs derive from source and
   role, so they cannot collide with agent replies.
6. Streamed messages (8.3) are the exception to step 3. `chat.startStream` creates the
   message, but metadata can be attached only when the stream stops (to be confirmed
   in WP-04). Courier records the stream's ts in the outbox immediately after
   `startStream` returns. A crash inside that window cannot be reconciled from history
   and is accepted as an at-least-once duplicate, with the orphaned stream left to
   Slack's stream timeout. If WP-04 finds `startStream` accepts metadata, the window
   closes and this exception is removed.
7. **Audit writes are never dropped silently.** A record that authorizes an action is
   written first and fails the action if the write fails: an inbound event before it is
   queued (the webhook answers 5xx, so Alertmanager retries) and an interaction decision
   before the answer is sent (an unaudited approval never takes effect). A record of
   something already done (an agent call, an unusable pause and its cancel) is logged,
   metered and returned, but never blocks the follow-up, above all the cancel. A decision
   records who, when and the answer's shape (counts), never its choices or free text.

### 8.7 Feedback

- The final message of every alert investigation, and of a chat answer when the Binding
  has `feedback.enabled`, carries Useful and Not useful buttons.
- One vote per Slack user per investigation; a user can change their vote. Votes by bots
  are ignored.
- Stored with Binding, agent, alert name, and session, and exported as
  `courier_investigation_feedback_total{binding, agent, alertname, verdict}`.
- The pilot exit decision (D33) is computed from these rows.

## 9. State

| Table | Key | Purpose |
|---|---|---|
| `events` | event ID | Durable inbound queue with processing status |
| `outbox` | outbox ID | Intended Slack posts and their resulting message refs (8.6) |
| `sessions` | conversation key | `contextId`, current task ID, binding, last activity (expires at `threadTTL`) |
| `alert_groups` | dedup key | Session link, root message ref, firing or resolved |
| `run_counters` | binding, window | Rate-limit and daily-cap counters, and skipped counts for the digest |
| `trigger_runs` | connection, channel, alert ts | The starts of a Binding's chat triggers (8.2): one row for each alert message that started an investigation or that a limit stopped, with the window of the digest it belongs to. Kept 24 hours. The alert Binding's `run_counters` (WP-43) is a separate counter for the webhook |
| `interactions` | interaction ID | Kind, request, state, answer, decider, timestamps |
| `reply_queue` | session, sequence | Thread replies waiting for the active task to finish |
| `feedback` | session, Slack user | Verdict, alert name, agent, binding, timestamps |
| `audit` | sequence | Append-only record of events, agent calls, decisions |

- **SQLite** (default): single replica, PersistentVolume, WAL mode.
- **Postgres**: required for more than one replica.

## 10. Security

| Concern | Control |
|---|---|
| Webhook authenticity | Alertmanager does not sign payloads. Bearer token per Binding, stored in the Binding's namespace, constant-time compare. Optional mTLS at the ingress. |
| Pilot environment | The kagent 1.0 alpha install holds production read credentials. Courier reaches it only through a verifying front door; NetworkPolicies restrict the controller port to that door and kagent's own components, and fence the kagent UI to its own verifying proxy (the UI's nginx relays `/mcp` and `/api` to the controller). The pilot cluster's CNI must enforce NetworkPolicy, or none of these fences exist; whose nginx forwards `/mcp` and `/api` with `Authorization` intact (D32). The agent's credentials are read-only. |
| Paging path | Every route to courier uses `continue: true`; courier never replaces an existing receiver (D22). |
| Slack authenticity | Signing secret verification with timestamp check for HTTP mode. Socket Mode uses the app token. |
| Backend trust | kagent's controller port (8083) trusts identity without verifying it: in insecure mode via `X-User-Id`, and in OIDC mode by decoding the JWT without checking its signature (a forged `alg: none` token was accepted in WP-01). It also takes the acting user from `X-User-Id` or `?user_id=` whenever `X-Agent-Name` is present, even with a valid token (WP-03). Courier must therefore call a verifying front door (oauth2-proxy or agentgateway) that validates the Binding's token and strips `X-Agent-Name`, `X-User-Id`, `X-Share-Token` and `X-Kagent-Insecure-Runtime-Identity`, never 8083 directly. The chart's UI nginx already strips both in trusted-proxy mode (source). A NetworkPolicy limits 8083 to that front door and kagent's own components (UI, Substrate egress for TaskStore traffic), and a second policy fences the UI (8080) to its own oauth2-proxy, because its nginx relays `/mcp` and `/api` to 8083 with `Authorization` intact. That UI proxy must not accept the Binding's audience; otherwise the Binding token is valid through a second, wider door (WP-01b removed `oidc-extra-audience: courier-binding` for this reason). |
| Binding identity scope | Each Binding identity gets only the permissions every member of its channel is trusted with. Separate Bindings, not one shared identity, for channels with different trust levels. |
| Secrets | Read from Kubernetes Secrets by reference. Never written to CRD status, logs, or the audit table. |
| Tenancy | Namespaced Bindings with explicit `allowedNamespaces` on shared resources. |
| Spend | Per-binding rate limit, concurrency cap, and daily cap on alert runs. |
| Audit | Every event, agent call, and approval is recorded with identity. |

## 11. Operations

- **HA.** Socket Mode runs on the leader only (Kubernetes lease). HTTP ingress and
  workers run on all replicas. Queue rows are claimed with row locking on Postgres.
- **Observability.** Prometheus metrics: events received, deduped, rate limited; agent
  call latency and errors; stream reconnects; approval outcomes; Slack API errors.
  OpenTelemetry traces with the trace context passed to the backend. Structured logs.
- **Packaging.** Pilot: a single Deployment with a mounted config file and a
  PersistentVolume for SQLite. After hardening: a Helm chart containing CRDs,
  controller, RBAC, and an optional NetworkPolicy.
- **Network path.** Production Alertmanager must reach courier's webhook endpoint over
  TLS. During the pilot courier runs alongside the kagent 1.0 alpha install.
- **Failure behavior.** If the backend is unreachable, courier retries with backoff; the
  first failed send is retried silently (the first call of a new pod can fail where the next
  works), and if the retry fails too it posts a visible notice in the thread. After 15 minutes (`sendGiveUp`) the turn fails with
  a notice and the queue moves on. Events are never dropped silently.

## 12. Phases

| Phase | Scope | Exit criteria |
|---|---|---|
| 0. Pilot environment and spike | Stand up a pinned kagent 1.0 alpha in OIDC mode behind a NetworkPolicy, with a read-only investigation agent holding production read credentials. A2A client spike: send, stream, reconnect, ask-user and nested HITL, service-token auth, busy-session rejection. Slack streaming limits. | The agent answers a question about production over A2A with a Binding service token. Q1 to Q3 answered. |
| 1. Chat | Resource Go types with a file loader, Slack Socket Mode, @mention and thread replies, reply queue, live output, ask-user interactions, outbox, SQLite store | Two different users hold one multi-turn conversation that survives a courier restart. An ask-user question is answered from Slack. A crash-during-post test produces no duplicate messages. |
| 2. Alerts | Alertmanager receiver with per-Binding token, team `AlertmanagerConfig` and platform central route, normalized AlertEvent, dedup, rate limit, daily cap, digest line, thread per alert with link back, resolve handling, feedback buttons | A firing alert produces one thread; repeats update it; replies continue the session. A 50-alert burst yields capped investigations and one digest line, not 50 messages. Votes are recorded. |
| 3. Pilot | Four weeks, one team plus the platform Binding. Feedback dashboard. Weekly review of not-useful investigations. | Go or no-go recorded against D33 (section 13) |
| 4. Hardening (if go) | CRDs and controller, Postgres, HA, HTTP mode, DMs, Helm chart. Move the backend to production kagent at 1.0 GA and retire the pilot install. | Two replicas pass a failover test with no lost or duplicated events |
| 5. Tool approvals | Bind the first write-capable agent. Decide R1 (`approverGroups`) first. Approval buttons, timeout, audit. | A gated tool call is approved and denied from Slack, both audited, and R1 has a recorded decision |
| 6. OSS readiness | Docs, generic `a2a` dialect conformance tests, ax example | A non-kagent A2A agent works with no code changes |

### 12.1 Testing strategy

Every behavior in the exit criteria is proven end to end in a local kind cluster, with
simulated alert webhooks and a simulated Slack, before it reaches the pilot.

| Tier | Runs | Contents | Purpose |
|---|---|---|---|
| Unit | Every change | Go tests, no cluster | Logic, parsing, state transitions |
| E2E fast | Every change that touches a flow | kind; real Alertmanager (prometheus-operator); fake Slack; stub A2A agent; courier; a sink receiver | Courier's flows, quickly and deterministically |
| E2E full | Phase gates, and when the kagent pin changes | kind; Agent Substrate (gVisor); pinned kagent 1.0 alpha; `kagent-dev/mockllm` as the model; Dex for OIDC; fake Slack; Alertmanager; courier | Real kagent behavior: session ownership, busy rejection, HITL payloads, OIDC service identity |
| Live smoke | Pilot start, manually | Pilot environment, real Slack sandbox workspace | Things fakes cannot prove: Slack rate limits, real rendering |

Components:

- **Simulated alerts.** Two paths. Alerts posted to Alertmanager's API
  (`POST /api/v2/alerts`) go through real grouping, `AlertmanagerConfig` routing,
  `continue: true`, repeats, and resolves. Fixture payloads posted directly to courier
  cover edge cases: bad token, malformed body, oversized body, retries.
- **Sink receiver.** A second webhook receiver on every test route. Tests assert it still
  receives alerts while courier is down, which proves D22.
- **Fake Slack.** A Go server implementing the Web API methods courier uses and a Socket
  Mode WebSocket, plus a control API for tests to act as users (mention, reply, click,
  submit) and read what courier posted. Courier points at it through slack-go's API URL
  option. Built from response fixtures captured against real Slack in Phase 0.
- **Stub A2A agent.** A scripted A2A server that replays scenarios, including kagent's
  HITL extension payloads, from fixtures captured against real kagent in Phase 0. The
  full tier catches drift between the stub and real kagent.
- **Deterministic model.** `kagent-dev/mockllm` (Apache-2.0), the scripted
  OpenAI-compatible server kagent uses in its own E2E suite. It returns fixed text and
  tool calls, including ask-user calls, so full-tier runs cost nothing and are
  repeatable.
- **Fault injection.** A build tag compiles hooks into test images only, for example
  crash-after-post for the outbox test (8.6). Production images do not contain them.

## 13. Success criteria

**Pilot go/no-go (D33).** At the end of four weeks, continue only if both hold:

- At least 50% of rated investigations are rated useful, with at least 20 rated, so
  the decision does not rest on a handful of votes.
- Engineers other than the pilot owner reply in investigation threads without being
  asked, in at least a fifth of investigation threads. (Threshold proposed; confirm at
  pilot kickoff.)

Otherwise stop, or re-scope using the not-useful ratings by alert name.

**Engineering criteria.**

- Alert received to root message posted: under 5 seconds at p95.
- A courier outage causes no missed alert notification.
- No duplicate agent runs for one alert group during a firing period.
- No lost events across a courier restart.
- Adding a chat platform requires implementing `chat.Platform` in its own package,
  passing `chattest.Conformance`, and adding it to the broker's platform factory. It
  changes no package except the adapter and the broker, whose factory names it: a lint rule
  denies every other package the Slack adapter and its client library. Adding an alert
  source requires implementing one interface and touches no router, session, or store code.
- Moving to a new kagent API revision changes the dialect package only.

## 14. Risks

| # | Risk | Severity | Mitigation |
|---|---|---|---|
| R1 | **Any channel member can approve write actions** (D13), and the backend sees only the Binding identity (D14). No named human is accountable at the backend before a change is made. | High, deferred | Not live until Phase 5: the MVP agent is read-only (D23). Phase 5 cannot exit without a recorded R1 decision. `interactions.approverGroups` is reserved in the schema so it can be added without a breaking change. |
| R2 | **Prompt injection through alerts.** Labels and annotations are attacker-influenced text that reaches an agent with tools. | High | Untrusted-data delimiting, webhook auth, rate limits. During the pilot the agent is read-only, so the impact is limited to data disclosure in the investigation channel. The same holds for a bot message a `botMessage` trigger accepts (D36): the whole message always goes in the untrusted-data block after the instruction text, never where a template would put it, and a value extracted from it reaches the instruction part only after a character check; a value that fails it refuses the render. |
| R3 | **kagent 1.0 is alpha.** Five alphas shipped in nine days. Paths and HITL payloads may change. | High | All kagent specifics live in the dialect. Pin a tested alpha. Contract tests run against the pinned version. |
| R4 | Anything that reaches kagent's controller port can claim any identity: `X-User-Id` in insecure mode, an unverified JWT in OIDC mode, and in OIDC mode `X-Agent-Name` plus `X-User-Id` or `?user_id=`, which selects the acting user even with a valid token (WP-03: oauth2-proxy forwards them). | High | Courier talks only to a verifying front door (oauth2-proxy or agentgateway) that strips `X-Agent-Name`, `X-User-Id`, `X-Share-Token` and `X-Kagent-Insecure-Runtime-Identity`; NetworkPolicies restrict port 8083 and fence the UI relay. Proven in the full tier (WP-01b, WP-01c): `verify.sh` shows tokens forged from a real token (payload altered; `alg: none`) and a foreign-audience token are rejected at the front door; A2A, HITL headers, and SSE work through it; path traversal (`..`, `%2F`) is refused; a canary shows the controller still accepts a forged token on `/mcp`; and pods spoofing the trusted labels from other namespaces are refused. Proven for the header path (WP-01d): through the door, B's valid token with `X-Agent-Name` plus `X-User-Id` or `?user_id=`, in any case, gets `-31403` on send, stream, `GetTask`, `SubscribeToTask` and `CancelTask`, and a bogus `X-Share-Token` is dropped, while the same spoof works at 8083 (canary) and the Binding's own send, SSE and `GetTask` still work. The `X-User-Id` and runtime-identity strips are defence in depth (config and source; no observable while `X-Agent-Name` is stripped). The pilot door must use the same config (WP-05). The risk stays High: the controller is unchanged, only mitigated. |
| R5 | No stream replay in kagent. Text produced during a disconnect may be lost from the live view. | Medium | Reconcile from `GetTask` on reconnect and render the final task output. |
| R6 | Slack rate limits on message edits under many concurrent streams. | Medium | Per-message throttle, global limiter, fall back to `final` output under pressure. |
| R7 | SQLite limits the default install to one replica. | Low | Documented. Postgres path for HA. |
| R8 | DMs have no channel to scope access. | Low | Membership check against the default Binding's channel. |
| R9 | kagent ships a native broker or `WebhookTrigger` (#2185, and the redesign mentioned in #2261), overlapping courier. | Medium | No maintainer progress on #2185 yet. Watch both. If one lands, courier keeps the alert-to-thread mapping and backend neutrality, and consumes kagent-triggered sessions instead of starting them. |
| R10 | klaus-gateway generalizes and adds alert ingress, overlapping courier. | Low | Track it. Courier's differentiators are alert-to-session threads and backend neutrality. |
| R11 | **The pilot install holds production read credentials** on alpha software, and kagent's TaskStore trusts an unsigned identity header from agent actors (kagent #1660), so a compromised actor can act as another identity inside kagent. | High | Read-only credentials only; the verifying front door; NetworkPolicy; a pinned alpha; nothing in the pilot environment that isn't needed; torn down at the end of the pilot or at 1.0 GA. Revisit if kagent fixes #1660. |
| R12 | **Sparse or biased feedback.** Few votes, or votes only on bad answers, skew the go/no-go. | Medium | Minimum of 20 rated investigations. Weekly review. Engagement is measured alongside ratings. |
| R13 | **kagent 1.0 GA slips or changes shape**, stranding courier on an alpha. | Medium | The pilot is explicitly disposable. Changes stay in the dialect. The go/no-go is about value, independent of GA timing. |
| R14 | A team forgets `continue: true`, putting courier in its paging path. | Medium | Runbook check before enabling a route during the pilot. In hardening, the controller reads `AlertmanagerConfig` resources that target courier's webhook URL and sets a warning condition on the Binding when a route lacks `continue: true`. |
| R17 | **A malformed HITL reply wedges the kagent task** in `WORKING` (WP-02: an empty answer list or a wrong request id; still `WORKING` after 20 s, and every send to the session, including a new turn, gets `-32004`). `CancelTask` recovers it; other recovery is untested. After a courier crash, a wedged task is indistinguishable from a long-running one. | Medium | The dialect validates answers before sending (6.3); on `-32603` from an answer, courier cancels the task and tells the thread; a maximum WORKING age (`maxWorkingAge`, default 1 h, measured on courier's clock from the later of the task's status timestamp and the last new progress courier saw for it (new text, a new tool call or a state change; a replayed snapshot is not progress), so recent progress never looks wedged; with no timestamp, from that last progress or the stream loss). Courier's record of recent progress is in memory, so after a restart the age falls back to the status timestamp: a healthy task that has streamed for longer than maxWorkingAge since entering WORKING is cancelled at recovery (accepted limit; a restart-surviving progress signal such as kagent's per-artifact `timeline-position` is on the backlog) cancels a WORKING task as wedged at restart reconciliation, after a lost stream and when an answer gets `-32004`; it never cuts a live stream, which the inactivity watchdog covers. Consider an upstream issue. |
| R18 | **kagent has two agent runtimes that validate differently.** WP-02 measured the Go ADK runtime only. From alpha5 source (not measured), the harness runtime (`go/harness/runtime/a2a/executor.go`, `validateRequest`) refuses a plain user message unless it has exactly one text part. | Medium | Attribution goes into the first text part (8.5, WP-25c). The dialect's wire-shape tests pin the single-text-part shape. Measure against the harness runtime before any pilot agent uses it. |
| R16 | A crash between `chat.startStream` returning and courier recording the stream's ts leaves a duplicate message and an orphaned stream (8.6 step 6). | Low | ts recorded immediately; window is one local write. WP-04 checks whether startStream accepts metadata, which would close it. |
| R15 | ~~kagent 1.0 needs Agent Substrate with gVisor, which may not run in kind on Docker Desktop for Mac.~~ **Resolved by WP-01:** the full tier runs on macOS on Apple silicon (arm64; gVisor uses the systrap platform, no KVM), about 1.9 to 2.1 GiB idle, up in about three minutes (148 to 180 s measured). amd64 and Linux are expected from kagent's own CI but not measured. | Low | Pins matter (kagent 1.0.0-alpha5, Substrate 0.3.0-alpha1). |

## 15. Open questions

| # | Question | Resolve in |
|---|---|---|
| Q1 | ~~Does the A2A Go SDK support negotiating the kagent HITL extension?~~ **Resolved (WP-02):** through a2a-go v2.6.0 with `a2aext.NewActivator`, the structured payload arrives; a2a-go v2.6.0's `a2aext.NewActivator` sends `A2A-Extensions` when, and only when, the agent card lists the URI; without the activator, or with a card listing no extension, no header is sent and no payload arrives (measured with controls; matches `a2aext/activator.go`). Payloads are untyped, so courier defines its own structs. Pass the full `.well-known/agent-card.json` URL to `agentcard.Resolve`, and override the card's in-cluster URL with the front door. | Phase 0 |
| Q2 | What are the rate limits of `chat.startStream` and `chat.update` under many concurrent threads? (Method choice resolved in 8.3.) | Phase 0 |
| Q3 | How does kagent 1.0 OIDC mode authenticate a non-human service identity, and what claim becomes the session owner? **Answered (WP-02, WP-03):** kagent has no notion of a service identity. The session owner is a string from the token: the `userIdClaim` claim (`email` in the pilot), else `sub`. Through the front door with real Dex tokens, a second identity gets `-31403` on send, stream, `GetTask`, `SubscribeToTask`, `CancelTask` and `ListTasks` by `contextId`, including while the task runs. An unfiltered `ListTasks` lists only the caller's tasks. The same email with a different `sub` owns the same sessions. A token without `email` passes the door and is owned by its `sub`, so it and the same user's `email` token cannot touch each other's sessions: a Binding's token must carry the same owner claim on every mint. This isolation holds only while the front door strips `X-Agent-Name` and `X-User-Id` (R4). | Phase 0 (closed; door fix WP-01d) |
| Q4 | Final API group name and project license for the OSS release. | Phase 6 |
| Q6 | Is one Binding per channel too restrictive for teams wanting several agents in one channel? | Pilot review |
| Q7 | Which team runs the pilot, and which alerts (by name and severity) are routed first? | Before Phase 2 |
| Q8 | Where does the pilot kagent install run, and how does production Alertmanager reach courier over TLS? | Phase 0 |
| Q10 | How does courier obtain and refresh the Binding's OIDC bearer token? A token stored in a Secret expires (Dex: 24 h) and needs external rotation; alternatively courier holds client credentials and mints tokens itself (client-credentials grant; see Q9). Also: courier needs Secret VALUES (Slack tokens, webhook tokens, header values, identity credentials), but the WP-21d resolver returns key names only by design. | **Resolved by D34.** |
| Q9 | Which OIDC provider issues Binding service tokens? It must support the client-credentials grant (Dex does not); otherwise a static service user with the password grant, which is weaker. Client-credentials tokens often lack an `email` claim. The front door accepts one with no configuration change (`oidc-email-claim` is not needed), and kagent then owns sessions by `sub` (WP-03, measured). From source only, for H-02 to check against the real provider: the token must be a signed JWT (an opaque access token fails at the door and at kagent); `aud` must contain the door's client ID or an `--oidc-extra-audience` (`--oidc-audience-claim` selects `azp` if the provider puts the client there); `email_verified: false` is refused unless `--insecure-oidc-allow-unverified-email` is set; the issuer must match discovery. | Phase 0 (H-02) |

Q5 (queue rate-limited alerts) was resolved in v0.3 by D26: digest line, no queue.

## Appendix A: Research sources

Verified 2026-09-29.

- kagent: https://github.com/kagent-dev/kagent. Docs on main:
  `docs/architecture/a2a-gateway.md`, `a2a-transports.md`, `go/api/a2a/hitl.go`,
  `design/EP-476-OIDC-Authentication.md`, `design/EP-1270-scoped-authorization.md`.
  Issues #475, #2185, #2261, #2853. PRs #1017, #2741.
- kagent related: https://github.com/kagent-dev/khook,
  https://github.com/kagent-dev/a2a-slack-template
- google/ax: https://github.com/google/ax. `README.md`, `DESIGN.md`, `docs/concepts.md`,
  `docs/networking.md`, `docs/roadmap.md`, `pkg/apis/v1alpha1/ax.proto`.
  Issues #27, #128, #376, #394, #430.
- Adjacent: agentgateway, HolmesGPT, Robusta, Keep, Botkube, k8sgpt, Hermes Agent.
- Added in v0.2: https://github.com/giantswarm/klaus-gateway (README; issue #350
  documents kagent refusing a second user's turn in a session),
  eliasecchig/a2a-gateway, gke-labs/kube-agents `a2a/gateway`,
  zanglang/kagent-slack-bridge. kagent PR #2987 (session share expiry, open).
- Added for 2.3: klaus-gateway `docs/kagent-a2a.md` and `docs/deployment.md` and its
  contributor list; kagent PR #1017 review comments; kagent issue #475;
  https://github.com/apexlnc/kslack.

Not verified: the kagent roadmap project board, and whether push notification delivery
works in kagent 0.10.x. The kagent.dev docs site returned errors, so content was read
from the `kagent-dev/website` repository instead.

## Appendix B: Review history

### v0.1 judge review (2026-09-29)

An independent agent scored v0.1 against three criteria:

| Criterion | Score |
|---|---|
| Adaptability | 5 |
| Fit | 4 |
| Filling a needed gap | 5 |
| Overall | 5 |

Findings and their resolution in v0.2:

| Finding | Resolution |
|---|---|
| kagent refuses turns from a non-owner identity, breaking shared threads and approver-identity approvals | D14 revised to per-Binding service identity (8.5) |
| HITL model covered tool approval only; kagent also has ask-user and nested | `Interaction` type with three kinds (6.2, 8.4) |
| Free-text answer fallback is rejected by kagent's validation | Restricted to the generic `a2a` dialect (6.3) |
| No behavior for a reply while a task is running | D21 reply queue (8.2) |
| klaus-gateway missed as prior art; "no maintained Slack integration" overstated | 2.1 prior-art table, D20 positioning |
| `promptTemplate` coupled to the Alertmanager payload | Templates run over normalized `AlertEvent` (6.2, 7) |
| Identity assumed kagent insecure mode | `Dialect.Authenticate` covers OIDC bearer tokens (6.2, 7) |
| `chat.startStream` needs a recipient user in channels | Alert threads use `chat.update` (8.3) |
| Issue #2261 cited as open; ax approval concept missed | Corrected (2) |
| ChatPlatform had no inbound path for interactions | `Run` delivers interaction callbacks; `RenderInteraction` added (6.2) |

### v0.2 grilling session (2026-09-29)

Four rounds of adversarial questions against v0.2. Findings and resolutions:

| Finding | Resolution |
|---|---|
| Courier could sit in the paging path: an outage would lose a page | Additional receiver with `continue: true`, dedicated investigation channel (D22) |
| No concrete first agent; approvals were being built with nothing to gate | Read-only investigation agent (D23); tool approvals to Phase 5 (D25) |
| Spec targeted kagent 1.0 only, but production runs 0.10.x, so courier had no real users | Separate pilot kagent 1.0 alpha install, fed by production Alertmanager (D24) |
| Per-alert "not investigated" messages would flood the channel in an alert storm | Digest line per window (D26) |
| Nothing measured whether investigations help | Feedback buttons (D27) and a pilot go/no-go (D33) |
| 24h idle timeout silently dropped context for later replies | 90-day thread mapping, resume the same session (D28) |
| Per-Binding webhook paths needed platform-owned Alertmanager changes, defeating self-service | Team `AlertmanagerConfig`, token in the Binding's namespace, `AlertSource` removed (D29) |
| Four CRDs and a controller before the first Slack message | Same types, file loader until hardening (D30) |
| Rate limit alone allowed about 720 runs a day per Binding | `maxRunsPerDay` (D31) |
| Pilot install would hold production credentials with kagent's default no-auth mode | OIDC mode plus NetworkPolicy (D32; corrected after WP-01 to require a verifying front door) |
| At-least-once queue plus non-idempotent Slack posts could duplicate messages | Outbox with Slack message metadata reconciliation (8.6) |
| Team `AlertmanagerConfig` routes only see their own namespace's alerts | Platform Binding with a central route for cluster-level alerts (7) |

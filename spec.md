# Agent Kourier: Specification

| | |
|---|---|
| Status | Draft v0.3, revised after judge review and grilling session (Appendix B) |
| Date | 2026-09-29 |
| Owner | Travis Bickford |
| Distribution | Internal first, designed to be open-sourced later |

## 1. Summary

Agent Kourier is a Kubernetes-native broker that sits between the outside world and
A2A-speaking agents. It does two things:

1. Turns alerts (Alertmanager first) into agent investigations. An alert enters as the
   message Alertmanager already posts to Slack, read by a `botMessage` trigger (8.1, 8.2).
   Agent Kourier has no webhook endpoint of its own (D39).
2. Lets people talk to agents from a messaging app (Slack first), including answering
   the agent's questions.

The two paths meet in one place: an alert gets a thread under its message, the agent
posts its findings there, and anyone in the channel can reply in that thread to continue
the same agent session.

Agent Kourier is not an alert receiver. It reads the message Alertmanager already posts
to Slack, so existing alert notifications are untouched and an Agent Kourier outage never
loses a page.

Agent Kourier is always on. Agents may be suspended, scaled to zero, or restarting; Agent Kourier
holds the connection to Slack and wakes the agent by calling it.

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
  but no code uses it; approval policies are a roadmap item. Agent Kourier reaches ax through
  an A2A server running as an ax task (6.3).

### 2.1 Prior art

| Project | Covers | Why Agent Kourier is still needed |
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
Agent Kourier's reason to exist, is the combination:

1. An Alertmanager alert, as the message Alertmanager posts to Slack, gets a thread that is also a live agent session.
2. Humans continue that session and answer agent interactions in the thread.
3. It works with any A2A agent, not one platform's roster.
4. It is configured declaratively and runs independently of the agents.

### 2.3 Why build Agent Kourier, not extend klaus-gateway or contribute upstream

Verified against source on 2026-09-29.

**Versus extending klaus-gateway**

| # | Benefit of Agent Kourier | Evidence |
|---|---|---|
| K1 | **Alert-driven runs are possible.** klaus-gateway makes every call as a person and states that "a turn without a person's token is refused instead of running as a machine identity." An alert has no person. Adding alert ingress there would reverse one of its core design principles, not just add a feature. | klaus-gateway `docs/kagent-a2a.md`, Identity section |
| K2 | **No platform prerequisites.** klaus-gateway requires Dex account linking, muster, agentgateway as the data plane, Giant Swarm roster annotations (`ui.giantswarm.io/*`), and a Harness admission label before an agent is offered. Agent Kourier needs an A2A endpoint and a Slack app. | `docs/deployment.md`, `docs/kagent-a2a.md` |
| K3 | **Public protocol, not private APIs.** klaus-gateway compiles against stubs generated from kagent's controller protos (`AgentTemplateService`, `AgentInstanceService`) and never fetches an agent card. Agent Kourier uses public A2A plus one published extension. kagent's A2A gateway creates a session when no `contextId` is sent, so nothing kagent-internal is needed. This means Agent Kourier works with non-kagent agents and is insulated from kagent's internal API churn during the 1.0 alphas. | klaus-gateway `pkg/kagent/gen`; kagent `docs/architecture/a2a-gateway.md` |
| K4 | **Team self-service.** klaus-gateway is configured through Helm values with one default agent. Agent Kourier's namespaced `Binding` lets application teams wire their own channel to their own agent without touching platform configuration. | `docs/deployment.md` |
| K5 | **Roadmap control.** klaus-gateway has four human contributors, all at Giant Swarm, and its design decisions trace to their internal platform plans. It has one star. Contributions would be judged against their platform's needs. | GitHub contributors; issue #350 |

**Versus contributing to kagent**

| # | Benefit of Agent Kourier | Evidence |
|---|---|---|
| U1 | **Upstream has declined chat in core.** On the Slack bot PR, a maintainer said it "should probably be in a separate repo, instead of having it in the main repository." The issue asking for a Slack bot (#475) was closed as not planned. The separate repo (kslack) was never adopted into the org and went stale. There is no in-core path to contribute to. | kagent PR #1017, issue #475 |
| U2 | **Backend neutrality.** Code in kagent serves only kagent. Agent Kourier serves any A2A agent, including one running as an ax task, which is the only route by which the ax side of the original goal can be served at all. | 6.3 |
| U3 | **Independent lifecycle.** A broker has to stay up while agents are suspended. A kagent maintainer stated this needs "a 3rd party to act as a broker." | kagent issue #2261 |
| U4 | **Delivery on our schedule.** kagent 1.0 shipped five alphas in nine days. Landing a large feature into that moving target means repeated rebases and maintainer review cycles. Internal-first delivery (D1) needs a shorter path. | kagent releases |

google/ax is not a contribution target: it is a sandbox orchestrator with no conversation
model, and the one proposal to add one (#430) has had no maintainer response.

**What we give up**

- **Per-user identity.** klaus-gateway already solves per-person OBO tokens and session
  shares. Agent Kourier deliberately does not (D14). If per-person tool authority becomes a
  requirement, klaus-gateway's approach is the reference.
- **Working code.** klaus-gateway has a tested Slack HITL surface, idempotent instance
  creation, sealed share storage, and restart resumption. Agent Kourier starts from zero.
  Read `docs/slack-hitl-surface.md` and `docs/kagent-a2a.md` before Phase 1 and borrow
  the designs; both projects are Apache-2.0.
- **Upstream alignment.** Upstream is redesigning the broker area (#2261). A native
  kagent solution could overlap Agent Kourier's chat path (R9). Agent Kourier's hedge is backend
  neutrality and the alert-to-session model, which upstream has not proposed.
- **Shared maintenance.** Everything Agent Kourier does, we maintain alone until it is
  open-sourced and adopted.

Where upstream contribution still makes sense: bug reports and fixes found while
building against kagent's A2A gateway and HITL extension, and feedback on #2185 so any
upstream webhook trigger stays compatible with Agent Kourier.

## 3. Goals

- G1. An Alertmanager alert, read from the Slack message Alertmanager already posts, to an
  agent investigation, with a cooldown per alert key, rate limits, and a daily cap. Agent
  Kourier is never in the paging path.
- G2. Slack conversations with agents through @mention, thread replies, and DMs. DMs arrive with
  Phase 4 (D40); the Slack adapter drops them today.
- G3. One thread per alert, conversational, sharing a session with the investigation.
- G4. Agent interactions answered from Slack: questions with choices as selects in the
  MVP; tool approvals as buttons once a write-capable agent is bound (Phase 5, D25, D40).
- G5. Live-updating agent output in Slack.
- G6. Declarative configuration, suitable for GitOps: the same resource schema is
  loaded from files during the pilot and served as CRDs after hardening.
- G7. Backend-neutral: any A2A agent, 1.0 or 0.3, works for chat and alerts.
  Backend-specific behavior is isolated in a dialect.
- G8. Every inbound event, agent call, and interaction decision is audit logged.
- G9. Every investigation collects a useful or not-useful rating, so value is measured,
  not assumed.

## 4. Non-goals (v1)

- A separate google/ax dialect or adapter. ax is a supported backend through the `a2a` dialect (section 6.3).
- kagent 0.10.x support. The pilot uses a separate kagent 1.0 alpha environment (D24).
- Replacing existing alert notifications or sitting in the paging path.
- Tool approvals before a write-capable agent is bound (Phase 5).
- Chat platforms other than Slack, and alert sources other than Alertmanager. The
  chat interface and the `botMessage` trigger (8.2) allow them; v1 does not ship them.
- A direct Alertmanager webhook receiver. Dropped for good, not deferred (D39).
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
| D3 | ax | Supported through the `a2a` dialect: an A2A server running as an ax task, reached via `atenet-router` |
| D4 | kagent version | 1.0 alpha, A2A 1.0 (see D24 for where it runs) |
| D5 | MVP scope | Thin slice of both ingress paths |
| D6 | Deployment | Kubernetes-native; CRDs and controller arrive in hardening (see D30) |
| D7 | Resource shape | Separate composable resources: `AgentBackend`, `ChatConnection`, `Binding` (`AlertSource` removed in v0.3, see D29, which D39 supersedes) |
| D8 | Alert source | Alertmanager. Since D39 its source is the message it posts to Slack, read by a `botMessage` trigger; the direct webhook is dropped |
| D9 | Alert flow | Thread per alert, conversational, in a dedicated investigation channel (see D22). Since D39 the thread is under the alert's own message, in the channel Alertmanager posted it to |
| D10 | Alert noise | Dedup by group key plus per-binding rate limits and daily cap. Since D39 the dedup is a trigger's thread key and cooldown (8.2), and the limits are a trigger's `limits` |
| D11 | On resolve | Post a note; the session stays open for the thread's lifetime (see D28) |
| D12 | Authorization | Channel-scoped bindings |
| D13 | Approvals | Slack buttons, any non-bot channel member may approve when the Binding names no groups, narrowed to members of the Binding's `approverGroups` when it names any (R1, option 2 reconfirmed 2026-10-13). The Phase 5 implementation is covered by direct pinned-kagent evidence and the broker full-tier suite with fake Slack, including audited approve/deny, timeout, group authorization, mixed decisions and stale presses (2026-10-14). |
| D14 | Identity | One service identity per Binding for every turn; the Slack user is carried as attribution (revised in v0.2, was per-user email) |
| D15 | State | Pluggable store, SQLite default, Postgres for HA |
| D16 | Slack transport | Socket Mode and Events API over HTTP, both supported. Only Socket Mode is served today (D40); HTTP mode arrives in Phase 4 |
| D17 | Invocation | @mention, replies in Agent Kourier-owned threads, DMs. DMs arrive in Phase 4 (D40); the Slack adapter drops them today |
| D18 | Output | Live-updating message |
| D19 | Language | Go |
| D20 | Positioning | Build Agent Kourier rather than extend klaus-gateway or contribute to kagent; rationale in 2.3 (added v0.2) |
| D21 | Reply while busy | Queue the reply and send it when the active task finishes or a pause ends (answered, timed out or cancelled); a reply never lands while an interaction is pending (added v0.2; pause rule from the WP-28 split) |
| D22 | Alert path | Agent Kourier is an additional Alertmanager receiver (`continue: true`). Existing notifications are unchanged. Investigation threads go to a dedicated channel and link back to the alert. (v0.3) **Superseded by D39:** Agent Kourier is no longer an Alertmanager receiver, and the thread is under the alert's own message. What holds is that existing notifications are unchanged and Agent Kourier is never in the paging path. |
| D23 | First agent | Read-only investigation agent: queries metrics, logs, and Kubernetes state; recommends, never changes. (v0.3) |
| D24 | Pilot environment | Production runs kagent 0.10.x. The pilot uses a separate kagent 1.0 alpha install with a read-only agent holding production read credentials, fed by production Alertmanager. Agent Kourier moves to production kagent at 1.0 GA. (v0.3) |
| D25 | Interactions scope | Ask-user (including nested ask-user) ships in the MVP, because an unanswered pause stalls a thread. Tool approvals move to Phase 5, with the first write-capable agent. (v0.3) Phase 5 now carries `toolApprovals` and `approverGroups`; approvals remain denied by policy unless `toolApprovals` is true. |
| D26 | Rate-limited alerts | One digest line per window in the investigation channel plus a metric; no per-alert message. (v0.3) |
| D27 | Value signal | Useful and not-useful buttons on each investigation, stored per alert name and agent. (v0.3) |
| D28 | Session lifetime | Agent Kourier keeps the thread-to-session mapping for 90 days and resumes the same `contextId`; kagent handles suspend and resume. No Agent Kourier idle timeout. (v0.3) |
| D29 | Alert routing | Teams route alerts to their Binding through their own prometheus-operator `AlertmanagerConfig`. The webhook token is a Secret in the Binding's namespace. Cluster-level alerts go to a platform-team Binding through the central Alertmanager config. (v0.3) **Superseded by D39:** there is no webhook, no webhook token and no route to Agent Kourier. An alert reaches a Binding through the channel Alertmanager's Slack receiver posts it to. |
| D30 | Config loading | Resources are Go types with kubebuilder markers from day one, loaded from YAML files in Phases 1 to 3. CRDs and the controller land in hardening. (v0.3) Since D40 `Binding.status` and its conditions land with the controller; the file loader reports a problem as a load error (7). |
| D31 | Spend cap | `maxRunsPerDay` per Binding, in addition to the rate limit. (v0.3) Since D39 it is a trigger's `limits.maxRunsPerDay` (8.2); since D41 every `botMessage` trigger must set it. |
| D32 | Pilot security | kagent 1.0 alpha runs in OIDC mode with per-Binding service tokens. Agent Kourier calls kagent only through a token-verifying front door (oauth2-proxy or agentgateway), never the controller port directly, because kagent's OIDC mode decodes but does not verify JWTs (WP-01 finding). NetworkPolicies restrict the controller port to that front door and kagent's own components, and fence each relay to the controller (the kagent UI) to its own verifying proxy (the chart's oauth2-proxy), which must not accept the Binding's audience. The front door strips `X-Agent-Name`, `X-User-Id`, `X-Share-Token` and `X-Kagent-Insecure-Runtime-Identity` from every request, because kagent lets them select the acting user or session (WP-03, WP-01d; implemented and proven in the full tier by WP-01d). The cluster's CNI must enforce NetworkPolicy. (v0.3, corrected after WP-01, WP-01b, WP-03, WP-01d and their reviews) |
| D33 | Pilot exit | Four weeks, one team. Continue if at least half of rated investigations are useful and engineers use threads unprompted. (v0.3) |
| D34 | Binding token source | For the pilot, a Binding's bearer token is read from `identity.tokenSecretRef` on every call (cached from a watch, never stored elsewhere), and rotated outside Agent Kourier before it expires (Dex: 24 h). A rejected credential (`ErrCredentialsRejected`, HTTP 401/403 at the front door) fails the turn, re-reads the Secret, is audited and raises a metric; Agent Kourier never falls back to `userId`. Minting with the client-credentials grant plugs in behind the same `TokenSource` interface once H-02 picks a provider that supports it (Q9). Secret values are held in a type whose every formatting path redacts. Secrets Agent Kourier reads are type `Opaque`; the watch keeps values for Opaque Secrets only (key names for every other type), in the namespaces Agent Kourier is granted. After the pilot, narrow to one watch per referenced Secret (a `metadata.name` field selector plus RBAC `resourceNames`). (Resolves Q10, after WP-03; narrowed by the WP-21e1 review.) |
| D35 | Chat-neutral model | Slack sits behind one platform-neutral seam, `chat.Platform` (6.2), so a second chat platform is one adapter and no change to session, interact, render, outbox or store. The model carries literal text with no markup field; each adapter neutralises markup for its platform and passes a shared hostile-text conformance suite before it ships. The outbox stores the versioned neutral message and the adapter renders it at delivery; a payload of the earlier, Slack-shaped form is abandoned as invalid with a log line, not migrated. Abandon metrics use neutral reasons (channel_gone, too_long, invalid_content, other), with the platform's own code in the log. The Slack message ID string is unchanged (`slack:<connection>:<channel>:<ts>`), and the store's columns keep their names. (Owner-approved 2026-10-02; WP-37.) |
| D36 | Bot messages | A bot's message counts only through an explicit `botMessage` trigger in the Binding's `chat.triggers` (8.2) that matches the sender by ID and kind (`from.id`, `from.kind`; D42, which amends this row's "bot ID"), never by display name. Agent Kourier's own messages, and those of every bot without a trigger, stay ignored, so bots cannot answer each other in a loop. The whole message is untrusted text (R2). (Proposed 2026-10-04 for the Slack-sourced alerts trial, CT-1; accepted by the owner 2026-10-04.) |
| D37 | HA | Active/passive: a Kubernetes Lease picks the one replica that does work, and epoch fencing in Postgres refuses a deposed leader's writes; a leader that loses its lease or meets a stale epoch exits at once, and one whose lease is past its renew deadline refuses every write to the chat and exits. (Owner, 2026-10-04; H4-4. The refusal of chat writes: owner, 2026-10-05, H4-5b.) |
| D38 | Thread replies | A Binding's `chat.threadReplies` is `all` (default) or `mention`. In `mention`, only a reply that mentions Agent Kourier is a turn, and it carries the unmentioned replies since the last turn as untrusted data; a typed answer to a question that takes text replies needs no mention. (Owner, 2026-10-05; mention-only thread replies, TM-8.) |
| D39 | Direct webhook dropped | Agent Kourier has no Alertmanager webhook endpoint and is not an Alertmanager receiver. The Binding has no `alerts` field and no `status.webhookPath`. Alerts reach a Binding only through `chat.triggers`: a `botMessage` trigger reads the message Alertmanager already posts to Slack (8.2, D36), and alert-specific behaviour lives in the trigger and its prompt template, not in the API (a preset held it until D41). Dropped for good, not deferred. Supersedes D22 and D29, and the webhook parts of D8, D9, D10 and D31. A Binding with an `alerts:` block no longer loads (the decoder is strict), so a deployed config drops the block first. (Owner, 2026-10-10; DA-1, DA-2.) |
| D40 | Fields that work | The API carries only fields that work. Phase 5 added `Binding.spec.interactions.toolApprovals` and `approverGroups`; `toolApprovals` defaults to false, and `approverGroups` is validated and used when approvals are enabled. Three feature areas remain out of the types until their phase: `ChatConnection.spec.directMessages` with its `defaultBindingRef` (Phase 4; a direct message is not routed), `ChatConnection.spec.mode` and the `signingSecret` Secret key (Phase 4; only Socket Mode is served), and `Binding.status` with its conditions (Phase 4, with the controller of D30; nothing sets it). A config that sets one of those three still fails strict decoding. |
| D41 | Presets removed | Trigger presets are removed. A preset hid the regexes, thread rules, limits and prompt of a known sender behind one word, and it does not carry to a second chat platform, where each platform would need its own regex set. Alert-specific behaviour is a `botMessage` trigger written in full, from an example the docs carry (Alertmanager's Slack receiver is the canonical one). A `botMessage` trigger must set `limits.rateLimit` (all three fields) and `limits.maxRunsPerDay`; the preset was what gave an alert trigger a spend cap (D31), so without it the cap is explicit. An explicit `maxRunsPerDay: 0` still means no cap and is allowed, because the choice is written in the config. A config that sets `preset:` fails strict decoding, as D39 and D40 did for their fields, so a deployed config drops the key and pastes the example. Every trigger start reports source `botMessage` (the prompt view's `.Source` and the session's origin source); votes on sessions created before this change keep the `alertmanager` source they were stored with. Supersedes the preset parts of D31 and D39. (Owner, 2026-10-15.) |
| D42 | Second platform, additively | The API takes a second chat platform without breaking a Slack Binding. Renaming a field, changing its meaning, or changing a default that an apiserver would persist is breaking. Loosening validation, widening an enum, adding an optional field or a `spec.<platform>` block, and adding a metadata key are not. So four changes are made before release and the rest waits for the second adapter. (1) `from.botId` becomes `from.id` plus an optional `from.kind`. `kind` is an enum that holds only `bot` and defaults to `bot`; a later platform widens it (research: a Mattermost webhook post carries its owner's `user_id`, and Teams has application identity types, `tasks/plans/mattermost-adapter-research.md` and `teams-adapter-research.md`, upstream behaviour not measured by us). D36 still applies: an ID, never a display name. A trigger accepts a message only when the sender's ID and kind both equal `from.id` and `from.kind`. The Slack adapter gives every bot sender kind `bot`, so Slack behaviour is unchanged. The kind is not part of the thread key hash. A thread open at the upgrade is still found. The consequence of leaving the kind out of the hash is that two triggers in one channel that name the same ID with different kinds would share key threads, so a second adapter with more than one kind per ID (for example, a Mattermost bot that also owns a webhook) must revisit this. (2) The trigger message fields `attachmentText` and `titleLink` become `body` and `link`; `text`, `title`, `footer` and `fallback` keep their names. Each name is defined by what it holds, and each adapter documents how it flattens its native message into them (8.2). The names the untrusted block gives the fields follow. (3) The API has no Slack default for the acknowledgment reaction: `chat.acknowledgment.emoji` has no default marker and `Default()` does not fill it, and the platform supplies its own when it is unset (Slack: `hourglass_flowing_sand`), because once CRDs ship an apiserver default is written into every stored object. `enabled` still defaults to true. (4) The agent contract names the platform: the `agentkourier.dev/actor` metadata gains `platform`, the platform's ID (`slack`), and the text prefix is `[<Platform>: Name (id) <email>]` with the platform's display name, so it stays `[Slack: …]` on Slack (8.5). Two conventions go with them, with nothing built for them yet. A `ChatConnection` keeps credentials in `credentialsSecretRef`, with keys fixed per platform (Slack: `botToken` and `appToken`); non-secret settings go in a `spec.<platform>` block that is added along with the platform, and Slack has none. A doc comment that says "Slack channel ID", "Slack bot ID" or "Slack user group" says "the platform's ID (Slack: `C…`)" instead; the ID regexes in the types stay until a second adapter moves them to per-platform checks at resolution, and loosening them then breaks nothing. A config that sets `botId`, `attachmentText` or `titleLink` fails strict decoding, as D39, D40 and D41 did for their fields, so a deployed config renames them. Amends D36 (bot ID becomes sender ID and kind); the rows that name the old fields stay as history. (Owner, 2026-10-15.) |

## 6. Architecture

### 6.1 Components

```
                  ┌──────────────┐      ┌───────────────┐
 Slack ◄─────────►│   Ingress    │─────►│    Router     │  resolves Binding,
  Socket Mode     │  adapters    │      │  + policy     │  matches triggers,
  or Events API   └──────────────┘      └─┬──────────▲──┘  cooldown, rate limit,
                  ┌──────────────┐        │          │     daily cap
                  │ Config loader│────────┼──────────┘
                  │ (file → CRD) │        │
                  └──────────────┘        │
                                        ┌─▼─────────────┐     A2A 1.0
                  ┌──────────────┐      │ Session mgr   │────────────► Agent backend
                  │    Store     │◄────►│ + A2A client  │◄──stream──── (kagent 1.0, or any
                  │ SQLite / PG  │      │ + dialect     │              A2A agent)
                  └──────────────┘      └──────┬────────┘
                                               ▼
                                        ┌───────────────┐
                                        │   Renderer    │──► Slack (post, edit, buttons)
                                        └───────────────┘
```

Alertmanager is not a component: its alerts arrive as Slack messages and enter through the Slack ingress (8.1).
Only Socket Mode is served today; the Events API over HTTP, which the diagram also shows, arrives with HTTP mode in Phase 4 (D40).

| Component | Responsibility |
|---|---|
| Ingress adapters | Authenticate the inbound request, normalize it to an internal event, persist it, acknowledge fast. Slack requires a response within 3 seconds, so no agent call happens on the request path. For chat messages the persist step is the session manager's `Submit`: a local, idempotent (by messageId) write of the turn, after which the adapter acknowledges; a failed Submit is not acknowledged, so Slack redelivers (WP-29). |
| Event queue | Durable table in the store, with a worker, for at-least-once processing across restarts. It was built for the webhook; since D39 no ingress adapter writes to it, and a chat message persists through the session manager's `Submit` (above). It is not in the diagram for that reason. |
| Config loader | Validates references and builds the in-memory routing table. Phases 1 to 3: reads YAML files and reloads on change. Hardening: a controller watches CRDs and writes status conditions (`Binding.status` arrives with it, D40); until then a problem in the config is a load error (7). Both produce the same Go types. |
| Router | Finds the Binding for a message, then applies the trigger's cooldown, rate limits, and daily cap. In the code the chat intake does this for a trigger (8.2). |
| Session manager | Maps a conversation key to an A2A `contextId`. Owns task lifecycle, stream reconnects, and the reply queue. |
| Dialect | Per-backend differences: URL layout, agent card location, identity header, HITL payload. |
| Renderer | Turns A2A stream events into Slack messages, edits, and interactive blocks. Posts through the outbox (8.6). |
| Store | Sessions, event queue, outbox, dedup keys, pending interactions, feedback, audit log. |

### 6.2 Interfaces

Three Go interfaces carry the extension points. Only one implementation of each ships in v1,
except Store.

An alert source is not an interface. An alert is a bot's message in the Binding's channel, and
a `botMessage` trigger (8.2) turns it into the trigger's view (`.Channel`, `.State`, `.Key`,
`.Fields`, `.Link`, `.Source`), the only thing a prompt template sees. A second source is a
trigger written in full (D41), not code behind an interface.

```go
// chat.Platform is the one seam between Agent Kourier and a chat platform (Slack in v1; package
// internal/chat, the adapter in internal/chat/slack, selected by ChatConnection.spec.platform).
// Run delivers what people write (Inbound) and press (Interaction, already decoded into
// question-indexed answers) to the sink, and nothing else in Agent Kourier names a platform. What
// Agent Kourier says goes out as an Outgoing: a Message (header, body, context, tool line, note,
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
    IsInAnyGroup(ctx context.Context, groups []string, user string) (bool, error) // fails closed; reads in order, no cache
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
go and by every method that sends; an adapter ships only if it passes. Agent Kourier's own code keeps
what no platform differs on: redaction, truncation policy, and the fixed words of notices. The
trigger table of 8.2 is the neutral `chat.Intake`, which a platform's `Run` feeds. The broker
builds the adapter named by `ChatConnection.spec.platform`; a platform with no adapter is a
wiring error, and a lint rule denies every package except the adapter, the broker and cmd (tests
aside) the Slack adapter and its client library, so a new package is covered from its first file.

### 6.3 Backends

Agent Kourier speaks A2A 1.0 over JSON-RPC: `SendStreamingMessage`, `GetTask`,
`SubscribeToTask`, `CancelTask`. It also speaks A2A 0.3 JSON-RPC to an agent whose card is a
0.3 card, such as kagent 0.9.x, through the SDK's compatibility transport. A 0.3 transport has
no `tasks/list`, so a lookup of a turn on such an agent cannot be answered. Recovery then
takes the queued turn as not the recorded task's own, so the task is followed first and that
turn is sent after it, and a resend treats the turn as absent: it is sent, behind a recorded
task if one runs. A turn may therefore run twice.

| Dialect | Chat and alerts | Approvals | Notes |
|---|---|---|---|
| `a2a` | Yes | Free-text only. An `input-required` pause with no recognized extension is shown as text. Replies enqueued after the question pass the same authorization, audit, and deadline checks as Submit before being sent as plain messages. | A2A 1.0 and 0.3 agents without a structured HITL extension, with a card at the exact RPC URL plus `/.well-known/agent-card.json`, as AX serves it |
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

**google/ax.** ax has no message model of its own, so Agent Kourier does not call its
lifecycle API. It is a supported backend through the `a2a` dialect:

1. **Supported:** an A2A-speaking agent packaged in an ax task image, reached through
   `atenet-router` with a static `ate-target-actor` header from
   `AgentBackend.spec.headers`. The card is served at the URL plus
   `/.well-known/agent-card.json`. ax removed its own A2A agent (issue #128), so the
   agent is ours to build and maintain; `examples/ax` is a reference (ADK on Bedrock
   Haiku). Agent Kourier sends the Binding's bearer token and the agent verifies it; the
   reference checks issuer, audience and service subject. The AX tier (12.1) covers S1
   to S5, S12 and the question deadline: streaming, context across restarts and actor
   suspend/resume, queued replies, plain-text questions answered in the thread,
   crash-after-post adoption and safe failure notices.
2. **Also supported:** a kagent agent that delegates sandboxed work to ax. Users have
   demonstrated this pattern (ax issue #394).

Structured tool approvals (S11) and tool step cards (S13) are kagent-only. A generic
A2A agent, on ax or elsewhere, asks questions as plain text and leaves other pause
data opaque. A separate ax dialect is revisited if ax ships durable interactions
(issue #430, an outside proposal with no maintainer response yet) or makes
`pending_approval` functional.

## 7. Resource model

API group `agentkourier.dev/v1alpha1` (placeholder, to be confirmed). All resources are
namespaced. Cross-namespace references are denied unless the target resource lists the
referring namespace in `spec.allowedNamespaces`. In Phases 1 to 3 these resources are
loaded from YAML files with the same schema (D30).

Ownership split: the platform team owns `AgentBackend` and `ChatConnection`. Application
teams own `Binding`.

There is no alert resource and no alert field on a Binding. An alert is a bot's message in
the Binding's channel, and a trigger of the Binding matches it (D39, 8.2). v0.2's
platform-owned `AlertSource` held a webhook token; it was removed in v0.3 (D29), and the
webhook itself was dropped in D39.

```yaml
apiVersion: agentkourier.dev/v1alpha1
kind: AgentBackend
metadata: { name: kagent-pilot, namespace: agent-kourier-system }
spec:
  dialect: kagent-v1                 # a2a | kagent-v1
  url: https://kagent-frontdoor.kagent-pilot     # verifying proxy, never :8083 (D32)
  headers: []                        # static headers, values may come from Secrets;
                                     # identity headers are reserved; see section 7
  allowedNamespaces: ["sre", "payments"]
---
apiVersion: agentkourier.dev/v1alpha1
kind: ChatConnection
metadata: { name: slack-main, namespace: agent-kourier-system }
spec:
  platform: slack
  credentialsSecretRef: { name: slack-main }   # keys botToken and appToken
  allowedNamespaces: ["sre", "payments"]
---
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: { name: sre-alerts, namespace: sre }
spec:
  agent:
    backendRef: { namespace: agent-kourier-system, name: kagent-pilot }
    name: k8s-agent
    namespace: kagent
  identity:                          # every turn in this binding runs as this identity
    userId: agent-kourier-sre-alerts       # insecure mode: sent as X-User-Id
    tokenSecretRef: { name: agent-kourier-sre-alerts-oidc }  # OIDC mode (pilot default, D32)
  chat:
    connectionRef: { namespace: agent-kourier-system, name: slack-main }
    channel: C0123456789             # investigation channel ID, not name (Slack: C…)
    output: live                     # live | final
    threadReplies: all               # all | mention; see 8.2
    triggers:                        # empty means [{type: mention}]; see 8.2
      - type: mention
      - type: botMessage             # alerts Alertmanager itself posts to this channel
        from: { id: B0123456789 }    # the sender's ID, never its display name (D36); kind defaults to bot (D42)
        # The title keeps out Grafana's "FIRING: <rule>" and anything else not shaped "FIRING (n) - <alertname>"
        # or "RESOLVED - <alertname>", optionally behind a bracketed tag. Alertmanager's default fallback text
        # carries the group's label values. Every match entry must match and no exclude entry may.
        match:
          title: '^(?:\[[^\]]{1,32}\] )?(?:FIRING \(\d+\)|RESOLVED) - '
          fallback: '^\[(?:FIRING(?::\d+)?|RESOLVED)\] '
          # $ anchors the receiver: without it, receiver=slack-critical also matches ...-kind
          link: 'receiver=slack-critical$'
        exclude: { title: '^Prometheus heartbeat$' }
        extract:                     # the named group carries the entry's name
          state: { field: title, regex: '^(?:\[[^\]]{1,32}\] )?(?P<state>FIRING|RESOLVED)' }
          # the group's label values without the count, so repeats of an alert share a thread
          key: { field: fallback, regex: '^\[[A-Z]+(?::\d+)?\] (?P<key>[^(|]*[^(|\s])' }
          alertname: { field: title, regex: '^(?:\[[^\]]{1,32}\] )?(?:FIRING \(\d+\)|RESOLVED) - (?P<alertname>.+)$' }
        thread: { by: key, cooldown: 2h, closeWhen: { state: RESOLVED } }
        limits:                      # required on a botMessage trigger (D41)
          rateLimit: { maxRuns: 5, per: 10m, maxConcurrent: 2 }
          maxRunsPerDay: 20          # 0 would mean no cap
        promptTemplate: |
          An alert is {{ .State }} in channel {{ .Channel }}: {{ .Fields.alertname }}.
          Investigate it with your read-only tools. Report the likely cause, the evidence you
          checked, and the next steps a person should take.
  interactions:
    askUser: true
    toolApprovals: false             # true enables kagent-v1 approval prompts
    # approverGroups: [S0123456789]  # optional Slack user groups for approvals
    timeout: 30m
  feedback:
    enabled: true
  session:
    threadTTL: 2160h                 # 90 days
```

The three documents above (`AgentBackend` `kagent-pilot`, `ChatConnection` `slack-main`, `Binding`
`sre-alerts`) load as one config with the file loader. The interaction approval fields are part of the
API and are resolved into the intake policy. The remaining fields in this table are designed for later phases and
are refused as unknown fields by the strict decoder:

| Field | Arrives with | What it will do | Today |
|---|---|---|---|
| `Binding.spec.interactions.toolApprovals` | Phase 5 | Allows a kagent-v1 tool-approval pause to be rendered and decided from Slack (8.4) | Defaults to false: every tool-approval pause is denied through the dialect, with a notice in the thread and an `auto_denied` outcome |
| `Binding.spec.interactions.approverGroups` | Phase 5 | Names the Slack user groups whose members may approve a tool call; omitted or empty means any non-bot member of the Binding's channel (R1). It gates tool approvals only, not ask-user answers, and an approver must also be a channel member and not a bot. A Binding with `toolApprovals: true` and no group loads with a warning | A nonempty list requires `toolApprovals: true`; a configured list is checked at press time and a member outside every group receives `denied_not_approver` |
| `ChatConnection.spec.directMessages.defaultBindingRef` | Phase 4 | Routes a DM to the named Binding, which may be in any namespace and must use the same connection, because the platform team owns connections; DM access is gated on membership in that Binding's channel (8.2, R8) | A DM is not routed: the Slack adapter drops it, and the intake finds a Binding by channel only |
| `ChatConnection.spec.mode` (`socket` or `http`) and the Secret key `signingSecret` | Phase 4 | HTTP mode, with signing-secret verification (10) | Only Socket Mode is served, so `credentialsSecretRef` needs the keys `botToken` and `appToken` |
| `Binding.status.conditions` (`Ready`, `BackendReachable`, `ChannelJoined`) | Phase 4, with the controller (D30) | The controller reports each Binding's state | Nothing sets a status; a problem with the config is a load error (below) |

Alertmanager needs no route to Agent Kourier: its Slack receiver posts to the Binding's
channel as it does for people today. The trigger above follows the documented example
(`docs/how-to/investigate-alertmanager-alerts.md`; 4h cooldown, 5 starts in 10m, 2 at once, 40 a day,
and an optional receiver match, `match.link`). It keeps the receiver match, overrides the cooldown (2h)
and the daily cap (20), and adds an `exclude` for the heartbeat. It needs the receiver's title in the shape `FIRING (n) - <alertname>` or `RESOLVED - <alertname>`
(optionally behind a bracketed tag), with Alertmanager's default fallback text (8.2).
Alertmanager's default Slack title begins `[FIRING:n]` and does not match, so a receiver that
keeps it needs its title changed, or the trigger's `match` and `extract` entries written for
the title it has. `agent-kourier dry-run` checks either against the channel's history.

Rules:

- **Secret keys.** A single-value Secret reference (`identity.tokenSecretRef` and a header's
  `valueSecretRef`) takes an optional `key` that defaults to `token`. `ChatConnection.spec.credentialsSecretRef` names one Secret with
  keys fixed per platform (D42); Slack's are `botToken` and `appToken` (Socket Mode). Non-secret settings of a platform go in a
  `spec.<platform>` block that is added along with the platform (D42); Slack has none.
- An empty namespace in a reference means the referring resource's namespace.
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
- One channel maps to at most one Binding. A second Binding for the same connection and
  channel is a config error that names both ("`<channel>` on `ChatConnection <ns>/<name>` is
  already bound by `Binding <ns>/<name>`"). The load fails as a whole: at start-up Agent
  Kourier does not start, and while it runs it keeps the last good config, which the reload
  status marks stale. A status condition on the Binding comes with the controller (D40).
- Which alerts reach a channel is Alertmanager's routing, through its Slack receiver, and
  not Agent Kourier's. Which of a bot's messages in that channel start an investigation is
  the trigger's `match` and `exclude` (8.2).
- Agent Kourier is not an Alertmanager receiver, so it is on no route and cannot be in the
  paging path. It only reads what Alertmanager already posted to Slack (D39).

## 8. Flows

### 8.1 Alert to thread

An alert reaches a Binding only as a message in the Binding's channel (D39). Alertmanager's
Slack receiver posts it, as it does for people today. Agent Kourier is on no Alertmanager
route and reads the message afterwards, so an Agent Kourier outage loses no notification.

1. Alertmanager posts the alert to the Binding's channel through its existing Slack
   receiver. That post is unchanged by Agent Kourier and reaches people whether or not
   Agent Kourier is running.
2. Slack delivers the message to Agent Kourier like any other message in the channel. The
   Slack adapter hands on a bot's message only if a `botMessage` trigger of the channel's
   Binding names the sender by ID and kind (D36, D42, 8.2). The intake acknowledges Slack once its local
   writes are done; an event it fails to take is not acknowledged, and Slack redelivers it.
3. The trigger matches the message and extracts its `state` and `key`. For a trigger with
   `thread.by: key` (the documented example's key is the alert group's label values) the
   decision table of 8.2 says what follows:
   - **No thread for the key (`started`):** the investigation runs in a thread under the
     alert's own message, which is the link back to the alert. The Slack permalink of that
     message is available to the prompt template as `.Link`; the documented example's template does
     not use it.
   - **The alert fires again while its thread is open or inside the cooldown (`repeat`):**
     a note "Fired again at `<time>`." in that thread. No turn.
   - **The alert resolves (`closed`):** a note "Resolved at `<time>`." in that thread, and
     the thread is closed. No turn, and the in-flight task is not cancelled. The session
     stays open for the thread's lifetime (D28).
4. When a limit of the trigger stops a start, Agent Kourier posts nothing in the alert's
   thread. It counts the start and, once per window, posts a single digest line at the top
   level of the channel: "7 alert messages not investigated in the last 10m (rate limit)."
   (8.2). The alert itself already reached people through Alertmanager's own post.
5. The A2A `messageId` is the alert message's own ID, `slack:<connection>:<channel>:<ts>`,
   so a redelivered event is the same turn and cannot start a second conversation.
6. When the investigation's task completes, its final message carries Useful and Not
   useful buttons (8.7).

There is no catch-up. An alert Alertmanager posts while Agent Kourier is down past Slack's
retry window (8.2; Slack's exact Socket Mode retry behaviour is unverified here) gets no
investigation until it fires again, and that repeat finds no thread and is `started`.

Agent Kourier posts no root message of its own for an alert and updates none: the alert's
message is Alertmanager's, and a repeat is a note in its thread. A person's reply in the
thread continues the session as in any thread Agent Kourier owns (8.2).

Alert message content is inserted into the prompt inside a delimited block labelled as
untrusted data. See risk R2.

### 8.2 Chat

These are the defaults, which a Binding with no `chat.triggers` gets. `chat.triggers` extends
them (below the table).

| Trigger | Behavior |
|---|---|
| @mention in a bound channel (not in a thread) | New thread, new session |
| Reply in a thread Agent Kourier owns | Continues that session. With `chat.threadReplies: mention` (D38), only a reply that @mentions Agent Kourier does; the rest are ignored, except a typed answer to a pending question that takes text replies (8.4) |
| @mention inside a thread Agent Kourier doesn't own, in a bound channel | New session anchored to that thread; Agent Kourier owns it from then on |
| Reply without an @mention in any other thread | Ignored |
| Any message posted by Agent Kourier itself (including `message_changed` from its own edits) | Ignored, always: Agent Kourier would otherwise answer itself without end |
| A message posted by any other bot or app | Ignored, unless a `botMessage` trigger names its sender (`from.id` and `from.kind`; D36, D42): two bots could otherwise reply to each other without end, and bot text is untrusted (R2) |
| An edit or deletion of any message (`message_changed`, `message_deleted`), or another non-message subtype | Ignored. Plain messages, `thread_broadcast` and `file_share` (its text only) count as messages |
| DM (Phase 4, D40) | Routed to the connection's `defaultBindingRef`. Allowed only if the user is a member of that Binding's channel. One session per DM thread. Today a DM is not routed: the Slack adapter drops it, and the intake finds a Binding by channel only |

**`chat.triggers` (D36).** An empty list is `[{type: mention}]`, the table above. A list that
names triggers is the whole list: a Binding that lists only `botMessage` triggers starts no session
from an @mention, in or out of a thread (the intake says so once, at info). The row for a reply in
a thread Agent Kourier owns is no trigger and holds whatever the triggers say; whether it needs a mention is
`chat.threadReplies` (below). A Binding adds
triggers; v1 has two types, `mention` (the @mention rows) and `botMessage`. Triggers are tried in
order and the first that matches handles the message. A `botMessage` trigger accepts messages
from one sender, named by `from.id` and never by display name, and then:

- `from.kind` says what kind of sender `from.id` is (D42). It is an enum that holds only `bot`
  and defaults to `bot`, so a Binding that leaves it out names a bot. A later platform widens the enum
  with the kinds it has; that loosens validation and breaks no Binding. A trigger accepts a message only when the sender's ID and kind both equal `from.id` and `from.kind`. The Slack adapter gives every bot sender kind `bot`, so Slack behaviour is unchanged. The kind is not part of the thread key hash.
- `match` and `exclude` map a message field to a regex. The fields are `text`, `title`, `link`,
  `fallback`, `footer` and `body`. Each name is defined by what it holds (D42): `text` is the
  message's top-level text, outside any attachment or card. `title`, `link`, `body`, `footer` and
  `fallback` are the heading, the heading's URL, the main text, the small print and the plain-text
  summary of the message's structured part (Slack: its first legacy attachment). Each adapter documents how it flattens its native message
  into them (the Slack adapter does, below). Every `match` entry must match and no `exclude` entry may.
- `extract` names values taken from a field by a regex, whose named group carries the entry's
  name. A message in which an extract does not match does not match the trigger.
- `thread.by: key` gives messages with the same extracted `key` one thread, anchored as for an
  @mention inside a thread Agent Kourier doesn't own. `cooldown` (1m to 168h) allows no new turn for a
  key inside the window, `closeWhen` (`{state: RESOLVED}`) posts a note and starts no turn, and
  `maxIdle` (twice the cooldown to 720h; default the larger of 24h and twice the cooldown) abandons
  an open thread that has had no message of its key for that long.
- `promptTemplate` is a Go template over the trigger's view of the message. It is parsed and run
  once at load against a placeholder view, so a mistyped field name rejects the Binding.
- `limits` bounds how many investigations the trigger starts (below): `rateLimit` (`maxRuns`
  starts in any `per`, and `maxConcurrent` investigations running at once; `maxRuns` and
  `maxConcurrent` are at least 1, and `per` is 10s to 24h) and `maxRunsPerDay` (at least 0;
  0 is no cap). A `botMessage` trigger must set `limits.rateLimit`, all three of its fields, and
  `limits.maxRunsPerDay` (D41): a trigger that leaves either out does not load, and the error names the field.
  An explicit `maxRunsPerDay: 0` loads and means no cap, because the choice is written in the config.
  `limits` on a `mention` trigger is refused.
- A known sender, such as Alertmanager's Slack receiver, is a trigger written in full, from the
  example the docs carry for it; nothing is filled in from a name (D41). The
  example matches Alertmanager's title `FIRING (n) - <alertname>` or `RESOLVED - <alertname>` and its
  default fallback with the group label values (Alertmanager's default title, `[FIRING:n] ...`, does
  not match, so the receiver sets its title). A fix to the example reaches a Binding only when
  its author copies it, and `agent-kourier dry-run` is the check.

Regexes are RE2 (linear time), at most 512 bytes, and compile at load, as do the named groups
and the template: a Binding that fails any of them is rejected. A bot message that no trigger
matches is ignored and counted (`agentkourier_trigger_unmatched_total{binding}`, plus a debug log line
that names no message text).

How a bot message reaches the triggers:

- The Slack adapter hands one on only if the intake says a `botMessage` trigger of that channel's
  Binding names the bot, so the bots no trigger names never reach it, and an intake that cannot
  say gets none. It flattens the message's first legacy attachment into `title`, `link` (its
  `title_link`), `fallback`, `footer` and `body` (its `text`), and `text` is the message's own text: all raw, as
  delivered, neither rendered nor unescaped (`mrkdwn_in` changes how Slack shows text, not the
  text). Later attachments are not read, so that a trigger matches the fields of one attachment.
  Both an incoming webhook's `bot_message` and an app's own post count as a bot's message.
- Agent Kourier's own messages, by user ID or by bot ID, are never handed on, even if a trigger names
  Agent Kourier's own bot ID; that mistake is logged once, at warn, since load cannot know the ID.
- An edit or a deletion of a bot's message, and any other subtype, is ignored as it is for a person.
  A bot's reply inside a thread is ignored and not counted: a trigger is about a message that opens
  its own thread. A bot's message is never a mention, whatever its text says.

What a matched bot message does to its thread, for a trigger with `thread.by: key`. The key is
hashed (with the connection, channel and the sender's ID, not its kind; D42) and the store holds that hash, one row per key with
its thread, when it opened, was last seen and closed, and when its cooldown ends (unset until the
turn has started); the logs name a
key by the first eight characters of the hash. A *close* is a message whose extracted fields all
equal `closeWhen`; any other match is a *fire*. The time is when Agent Kourier took the message.

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
investigation. That is what mends a resolve that never registered: Agent Kourier down past Slack's
retry window, or a resolve that arrived before its fire. `maxIdle` is at least twice the
cooldown, at most 720h, and unset it is the larger of 24h and twice the cooldown. It must also
be longer than the sender's repeat interval (Alertmanager's `repeat_interval`), or an alert
that is still firing is counted abandoned between two of its repeats and gets a second
investigation. With the documented example's 4h cooldown the default `maxIdle` is 24h,
longer than Alertmanager's default 4h `repeat_interval`; a `repeat_interval` of 24h or more
needs `maxIdle` set above it. The cooldown begins when the turn has started and not
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
Every decision is counted as `agentkourier_trigger_decisions_total{binding,decision}`.

**Limits of a trigger (D26, D31).** `limits` holds a start to the trigger's `rateLimit.maxRuns` in
any `rateLimit.per`, `rateLimit.maxConcurrent` investigations running at once and `maxRunsPerDay`
in any 24 hours. The documented Alertmanager example's are 5 in 10m, 2 at once and 40
a day (the cooldown folds an alert's repeats into its thread already, so they bound distinct
alerts, a storm). `per` is 10s to 24h. Only a start is limited: a repeat, a close and a note are
never. The check is the last step before the turn is asked for, and when it stops a start the
decision is `limited`: no turn, no note (D26), the key's reservation is given back so that it
holds nothing, and `agentkourier_trigger_limited_total{binding,trigger,limit}` is counted, where
`limit` is `rate`, `concurrent` or `daily` (when several are reached, the daily is reported
first, then the rate, then the concurrency). A stopped alert reached people through Alertmanager's
own post. Giving the reservation back, rather than holding the key with no investigation behind
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

**The digest line (D26).** Once for each window and each trigger, Agent Kourier posts one line at the top
level of the Binding's channel through the outbox: "7 alert messages not investigated in the last 10m
(rate limit)." (more than one limit in a window: "(rate limit: 5, daily cap: 2)"). A window
opens with the first stopped start after the last digest and lasts the trigger's `rateLimit.per`
(a loaded trigger always has a rate limit); the line is posted when it has ended, within
15 seconds, so that it can count. The row it asks the outbox for is named after the connection,
Binding, trigger and the start of the window, so a crash or a failed write between queueing it
and recording that it was queued posts it once. The line holds a count, a length and a limit and
never any text of an alert. It counts alert messages, not distinct alerts: a repeat of a stopped alert that is stopped
again is another. A window that ended while Agent Kourier was down is posted when it is up.

**The first turn of a started thread.** The broker renders the trigger's `promptTemplate` over
the trigger's view of the message (`.Channel`, `.State`, `.Key`, `.Fields`, `.Link`, `.Source`)
and appends the whole message, normalized and with its secrets taken out (redaction), in the untrusted block (R2). `.Link` is the Slack
permalink that Agent Kourier builds from the workspace address `auth.test` returned when it connected,
the channel and the message's ts (`https://<workspace>.slack.com/archives/<channel>/p<ts without
the dot>`); it is never a field of the message, so a sender's `link` cannot reach the
instructions. The rendered prompt is submitted through the session manager's own intake, the path
a mention takes (the session row, the queued turn and a wake, all local writes), with the alert
message's ts as the thread and its ID as the turn's `messageId`, so a redelivery is the same
turn. Nobody wrote the turn, so it names no user: it is filed under the manager's system user
and carries no attribution prefix, which is for what a person wrote, and its audit rows name no
person. A person's reply in that thread then continues the session as in any thread
Agent Kourier owns (with `threadReplies: mention`, only one that mentions Agent Kourier). The text of a
template reads `.Fields` entries, not `.Key`: the documented example's key is the group's label values, which hold spaces, and a value in the instruction part must be
name-shaped, so a template that reads `.Key` is refused.

A prompt that fails its check (a value outside the name shape, too long, a `.Link` that is no
link) or cannot be rendered (a template that reads an entry the trigger does not extract, or
fails to run) starts no turn. The thread gets a fixed note, "Not investigated: `.Fields.alertname`
failed the prompt check." (or "the prompt template could not be rendered."); the audit log gets an
event that names the message, the trigger, the value's name, the reason and the value's length and
never the value; and `agentkourier_trigger_refused_total{binding,trigger,ref,reason}` is counted, where
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
`contextId` is filled in from the backend's first response. If Agent Kourier crashes in
between, the retried send reuses the same `messageId`, with no `contextId`, as on the first
send. kagent deduplicates on it (WP-02, measured). While the task still runs, the replay
is itself a live stream of the same task and context: a `WORKING` snapshot with the text
so far, then the remaining deltas, the `lastChunk` chunk and the final task. After the
task has ended, the replay returns one snapshot of that task. Either way the task shows a
single run (one history message, one artifact); whether the model was called a second
time is not measured. A resend carrying a `contextId` (a crash during a thread reply) is not
deduplicated (WP-03): while the task runs it gets `-32004`, not a live join, and after the
task ends it starts a second full task. Agent Kourier never blindly resends into an existing
context, except to an agent that cannot list tasks (6.3). Before resending a turn whose task it has not recorded, it calls `ListTasks` with
the `contextId`, follows `nextPageToken` to the end, and looks for a task whose history
holds the turn's `messageId` (kagent records it there). Found: reconcile as in 8.3
(`GetTask`, then `SubscribeToTask` if not terminal). Absent: send. A `-32004` on that send
means some task is active, possibly an `input-required` pause, so the turn is queued as
below and the lookup repeats when the active task ends. Whether the model runs twice for a
duplicate task is not measured; the second task runs in full. An expired session is kept for a grace period (90 days) after its threadTTL has run out, so that
Agent Kourier can tell a reply in an expired thread (notice) from a reply in a thread it never owned (ignored). After the grace the
session is deleted with every row that belongs to it, and the thread is one Agent Kourier never owned: a reply without an @mention is
ignored, and an @mention starts a new session anchored to the thread (as in "@mention inside a thread Agent Kourier doesn't own"). A
session is never deleted while it has an active or paused task, a pending interaction, a reply that was not delivered, or an
outbox row that was neither posted nor abandoned. The one exception is a task that a crash left recorded on a session already
past the deletion horizon (its Binding's threadTTL plus the grace): retention first asks the agent to cancel it, bounded and
best effort, then clears it and cancels its pending interactions, so that the session is then deleted as any whose work has
ended; a task a runner of the leader still holds is left to that runner. A session whose Binding has left the config is held to the longest threadTTL of
any Binding (and at least the default) plus the grace.

**Thread lifetime.** The mapping is kept for `threadTTL` (90 days) after the last
activity, and a reply at any point in that window resumes the same `contextId`. kagent
suspends and resumes idle sessions on its own. If the backend reports that the session
no longer exists, Agent Kourier says so in the thread, clears the stored context, and the next
message in the thread starts a fresh context. Agent Kourier treats `TaskNotFound` for a recorded
task, or a first response whose `contextId` differs from the stored one, as the session no
longer existing (UNCONFIRMED: kagent's signal for a lost session is not measured). After
`threadTTL` the thread is expired for good: every later reply in it gets a fixed notice
saying the conversation has expired and that a new mention in the channel starts a new
one. The mention's two events (app_mention and message) give one notice. In `mention`, a reply in an expired thread that
does not mention Agent Kourier gets no notice. A reply in a thread whose Binding is gone gets the notice if it mentions
Agent Kourier, or if the Binding that now owns the channel takes all replies, or when no Binding owns the channel (the
default mode is `all`).

**Reply while a task is running.** kagent allows one active task per session and
rejects a new message while one runs, with JSON-RPC `-32004` "resource conflict" (inside
the SSE stream for streaming sends; WP-02). A rejected message is not recorded: resending
it unchanged, with the same `messageId`, after the active task ends (cancelled or
completed) starts a new task in the same context (WP-02, measured). A queued reply
therefore keeps its `messageId`, and a resend that still gets `-32004` stays queued. Agent Kourier queues the reply per session, marks it
with the configured acknowledgment reaction when enabled, and sends queued replies in order, one per turn, when the active task
reaches a terminal state, or when a pause ends (answered, timed out or cancelled). A reply
sent while an interaction is pending is held the same way, and is not its answer, unless the question takes
text replies (the `a2a` dialect's single free-text question): then the first such reply after the question is
its answer, decided with a button's checks and audit (8.4), and never sent as a turn. A user can stop
the active task from the thread; the stop control calls `CancelTask`, and queued replies then proceed. A turn's `messageId` is
`slack:<connection>:<channel>:<ts>`, so the `app_mention` and `message` events for one
message are one turn. When `-32004` comes from a task Agent Kourier did not record, Agent Kourier
resends with backoff (at most 60 s apart), repeating the lookup before each resend, and
never drops the reply.

**Failed task notices.** A failed task always gets a notice, even if it streamed a partial answer. Agent Kourier reads at
most 4096 bytes of the status message's text and recognizes only known context-limit, rate-limit, authentication,
timeout and permission signals, including exact supported metadata codes. A generic backend error code does not
identify a cause. The notice uses a fixed explanation and next step for a recognized category; an unknown failure gets
a generic explanation and operator next step. Both include a bounded `AK-` reference derived from the thread and task
IDs. The same reference is recorded as `failure_ref` with `failure_category` on the turn-end log, where the full task ID
is also available. Raw status text and arbitrary metadata are never included in chat or logs.

**Message acknowledgment.** `chat.acknowledgment` defaults to `{enabled: true}`. The API has no default emoji (D42): an unset `emoji`
is the platform's own default reaction, which on Slack is `hourglass_flowing_sand`. The emoji is a reaction name in the form the platform takes; on Slack it is bare, without colons. Every accepted message gets the reaction while waiting or
processing, including the first message in a session. The reaction is removed when its turn ends or the agent pauses
to ask a question; an accepted typed answer gets its own reaction while the continuation runs. `enabled: false` suppresses
the reaction for newly accepted messages, including queued replies. Ignored messages are not marked. The original emoji
and message reference are persisted with the reply and active turn so cleanup uses those values after reload or recovery.
Adds and removals share the original source identity in the outbox, so a delayed add cannot undo a removal.

**Mention-only thread replies (D38).** `chat.threadReplies` is `all` (the default: any person's reply in a thread
Agent Kourier owns is a turn) or `mention`. In `mention`, a reply is a turn only if it mentions Agent Kourier, whether the thread
was started by a mention or by a trigger. The adapter decides what a mention is: on Slack, what Slack reports as one
(an `app_mention`), which includes Agent Kourier's user token anywhere in the text, in a quote or in code; plain text such as
`@agent_kourier` is not one, and a mention added by an edit does not count. Direct messages (Phase 4) will need none. An unmentioned reply is
ignored: nothing is written, no reaction is added, and the thread's `threadTTL` is not extended. It is counted
(`agentkourier_thread_unmentioned_total{binding}`). The one exception is a reply while the agent waits on a question that
takes text replies (8.4): it is that question's answer, and never a turn. It
counts only while the session's current task waits on a question whose prompt Agent Kourier has posted; a typed reply at any
other time is ignored like any other unmentioned reply. A mentioned turn carries the unmentioned replies since the
session's previous turn: Agent Kourier reads the thread between the previous turn's message and this one (both exclusive) when it
sends the turn, and appends them after the person's text, oldest first, in a hashed `thread-replies` block of untrusted
data (R2), each with its author's display name (`<from>`) and, apart from it, the platform's user ID (`<id>`), which
alone says who wrote it; each reply's text is cleaned, read through a 4000-byte window, redacted, escaped and cut (in that
order), and a reply with no white space keeps its start. Agent Kourier's own posts, other bots' messages, messages that were
themselves turns or answers, and replies that are blank once their invisible characters are deleted are left out. At most the newest 50 replies, 1000 UTF-16 units each and
6000 for the block with its lead sentence, are kept; the block says how many were left out. The window comes from the
reply queue, so it is the same after a restart or a failover, and a reply posted after a mention falls in the next
mention's window. A platform that cannot read a thread sends the mentioned message alone; a read that fails, or a
thread whose window is longer than the adapter reads (on Slack, more than 5 pages, 1000 messages), sends it with a fixed
line saying the earlier replies could not be read (`agentkourier_thread_context_total{binding,outcome}`).
In `mention`, the final message of each turn (not one that ends in a question, not a turn with no output) carries, under
its text and above the feedback question when there is one (8.7), the context line "Mention me in this thread to ask a
follow-up." (owner's decision 6b; added with TM-9).

**Attribution is load-bearing (WP-36h, measured on kagent).** kagent keeps Agent Kourier's
attribution at the start of the text a person wrote (`[Slack: <name> (<user id>)] ...`; `[<Platform>: ...]` with D42), and
it replays the agent's own earlier replies to the model as role `user` messages. The
attribution prefix is therefore the only thing that tells the model a person's message
from its own earlier output. Agent Kourier must prefix every message a person wrote; removing or
making the prefix optional breaks multi-turn context.

### 8.3 Streaming and long-running tasks

- Agent Kourier calls `SendStreamingMessage` and edits one Slack message as text arrives.
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
  stream Agent Kourier calls `GetTask` to reconcile state, then `SubscribeToTask`.
- On Agent Kourier restart, non-terminal tasks found in the store are reconciled the same way. The
  session records the turn whose output the task belongs to, in the same write as the task (the
  turn that began it, and after a button answer the answer's), and recovery renders the task's output
  under it, so a stream that the stopped process left open is ended and replaced with the whole output
  and no half-written message stays in the thread; with no recorded turn it is posted anew.

### 8.4 Interactions

Ask-user and nested ask-user ship in the MVP. The Phase 5 implementation provides tool approvals for the
`kagent-v1` dialect. `Binding.spec.interactions.toolApprovals` defaults to false, so
an approval on a Binding that does not enable it is denied through the dialect, the
thread receives a fixed notice, and the approval outcome metric is raised. The
generic `a2a` dialect accepts only text-only questions; a structured pause on it is
unusable and cancels the task.

1. The task enters `input-required`. The dialect parses it into an `Interaction`. A pause
   the dialect can't use (for `kagent-v1`: a missing or malformed payload) is reported in
   the thread with its reason, audited, and the task is cancelled (6.3). A task in
   `auth-required` is refused the same way on every dialect, before any parsing: no dialect
   can answer it from the thread, and without an interaction it would have no timeout.
   For a stored interaction whose prompt is too large or permanently abandoned, the
   `unusable_pause` audit is written before the fixed notice is queued and the interaction
   is closed. Recovery retries the cancellation of that closed row and records only a
   result it knows: `ErrNotPaused` does not create a successful cancellation row. A crash
   after a successful cancellation but before its audit can therefore leave that success
   row absent; recovery does not invent it.
2. Agent Kourier stores it as pending and renders it in the thread:
   - **Ask user:** each question with its choices as a select (multi-select where
     allowed), or a free-text input when there are no choices, and a Submit button.
   - **Text replies (`a2a` dialect):** a single question with no choices also takes a typed reply in the thread, and its
     prompt says so. While the session's task waits on it and its prompt is posted, the first reply after the question is
     its answer, checked and audited as a Submit is (steps 3 to 5), and never sent as a turn; in a Binding with
     `chat.threadReplies: mention` it needs no mention (D38).
   - **Tool approval (`kagent-v1` only):** each tool name and its redacted,
     normalized JSON arguments are shown as literal rich text. One tool gets
     **Approve** and **Deny** buttons. Several tool calls get one radio choice per
     tool, plus **Submit**, **Approve all** and **Deny all**. The adapter refuses a
     name or argument that cleaning would change, that contains an unsupported
     value or control, or that does not fit in the message; it never truncates the
     approval evidence. `[redacted]` means the approver has not seen that original
     value.
   - **Nested:** rendered as its inner kind, labelled with the sub-agent that raised it.
   - All agent-originated text is untrusted (R2): escaped (`&`, `<`, `>`), stripped of
     control and format characters, rendered as `plain_text` where Slack allows, and
     truncated to Slack's limits. The one exception is code in a markdown body: Slack shows
     code literally (probe of 2026-10-04, `tasks/plans/slack-code-rendering.md`), so the
     fences and code spans that Agent Kourier itself writes carry their text raw, except `<`
     before `!`, `@` or `#`, which is still written `&lt;`; every other backtick, and every
     run of three or more `~`, is escaped, so Slack opens no code of its own. A stream that
     Agent Kourier resumes after a restart escapes everything, code included. Tool approval
     arguments use a rich-text preformatted element rather than markdown, so agent text stays
     literal; secrets are redacted by key name and token shape before the request is stored.
     A choice list over Slack's select limit falls back visibly; no choice is ever dropped.
   - Every Agent Kourier post, update and stream is sent with `parse` unset (or `none`), no
     `link_names`, and `unfurl_links=false` and `unfurl_media=false`. Escaping alone does not
     stop `@here` under `link_names`, and an unfurl makes Slack fetch an agent-written URL
     with no click, a zero-click way to leak data (R2). Text is rendered once, at the last
     step, never twice and never unescaped (WP-32b review).
3. On submit, Agent Kourier verifies the request came from Slack, confirms the user is a
   channel member and not a bot, and checks the answer is complete. An incomplete
   answer is rejected in Slack; nothing is sent to the backend. For a tool approval
   on a Binding that names `approverGroups`, the user must also be in one of
   them (R1); the groups do not gate an ask-user answer. Membership is read in the
   configured order without caching, and the first group-read error ends the check.
   A channel member outside all groups is audited as `denied_not_approver`. A
   permanent group-read failure ends that press with a fixed message and an operator
   error naming the Binding, makes no decision or audit row, and leaves the interaction
   pending; a transient read failure keeps the press for retry.
4. Agent Kourier records the decision and sends it through the dialect under the Binding's
   identity, with the Slack user as attribution. A denial carries the dialect's fixed
   rejection reason, not free text from Slack.
5. First complete submission wins. The message is updated to show who decided and
   when, and the controls are removed. A failed update leaves the controls for the
   repair loop; it does not resend the already recorded decision. A person whose
   press loses is told who decided first; a late press is told it was not taken.
6. On timeout, an enabled tool approval is audited as `timed_out` before Agent
   Kourier denies every tool through the dialect. The prompt is replaced with a
   timed-out ending and a fixed notice is queued. If the agent does not take that
   denial, a later sweep cancels the task. Questions are cancelled with `CancelTask`
   and use their question notice. The first-complete store decision wins a
   person/sweep race; the thread notice or ending can reflect the sweep's work even
   when the person's row wins, while the task receives one accepted answer. When a
   person's answer wins after the timeout audit, the sweep appends a `superseded`
   audit for its timeout, logging a failed compensation write. Recovery reuses a
   durable timeout audit, repairing a missing audit before it retries denial.

The audit row for an answered approval names the Binding, person and time, counts
approved and denied tools, and lists cleaned tool names in request order, capped by
the audit row limit. It never stores arguments or rejection reasons. The metric
`agentkourier_approval_outcomes_total{binding,outcome}` records `approved`,
`denied`, `partially_approved` and `timed_out`; policy refusals use `auto_denied`
and `ask_user_cancelled`.

**Measured in a direct run against pinned kagent 1.0.0-alpha5.**
- kagent always calls the model with `stream=true` and always offers `ask_user` as a tool.
- An `ask_user` answer reaches the model as a tool message holding a JSON list, one entry
  per question in order: `[{"answer":["PostgreSQL","MySQL"],"question":"..."}, ...]`.
- A denied tool approval reaches the model as an empty assistant message followed by a
  tool message `{"error":"error tool \"<name>\" call is rejected"}`, so the model can
  continue knowing the call was refused.
- A multi-tool approval pause emitted one input-required request with two distinct
  tool call IDs and approval IDs. Sending one response that approved `add_numbers`
  and denied `check_status` completed the task; only `add_numbers` executed.
- A paused task's final event posts no message of its own (D8, measured on kagent and the
  stub): the prompt is the only message carrying the question.
- Not measured: a non-streaming model call, and a nested sub-agent's ask. The
  multi-tool evidence covers the pinned kagent 1.0.0-alpha5 fixture. The broker full-tier
  suite with fake Slack also passed on 2026-10-14, including approval/denial audit
  and outcome metrics, timeout, group authorization, mixed decisions and a stale press.

### 8.5 Identity

kagent binds a session to the identity that created it and refuses turns from any
other identity. Agent Kourier therefore runs every turn in a Binding, chat or alert, as the
Binding's single service identity (`spec.identity`).

- **Backend identity:** the Binding identity, as a bearer token in OIDC mode or
  `X-User-Id` in insecure mode. The owner is the token's `userIdClaim` (`email` in the
  pilot), else its `sub`, so the token must carry the same owner claim on every mint.
- **Ownership is not a boundary on its own.** kagent lets any caller that sends
  `X-Agent-Name` choose the acting user with `X-User-Id` or `?user_id=`, even alongside a
  valid token (WP-03). Isolation between Bindings holds only because the front door strips
  both headers (D32) and Agent Kourier never sends them.
- **Attribution:** the user who typed or clicked is resolved (on Slack, `users.info`,
  cached) and attached by the dialect as message metadata and a prefix the agent can
  see. The metadata is under the key `agentkourier.dev/actor` and always carries the user's `id`, carries
  `name` and `email` only when known and, with D42, carries the `platform`: the platform's ID (`slack`). The prefix is
  `[<Platform>: Name (id) <email>]` with the platform's display name, so on Slack it is
  `[Slack: Name (U123) <email>]` as before. Agents should read the actor from the metadata and not
  parse the prefix. The prefix is informational; agents must not treat it as authorization, which is
  always the Binding identity. Agent Kourier's audit log records the user, email, and Binding identity together. For a chat
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

Inbound delivery is at-least-once (Slack redelivers an event that is not acknowledged), and `chat.postMessage` has no idempotency key. A crash
between posting and recording the result would otherwise post twice. Agent Kourier prevents
this with an outbox:

1. Before posting, Agent Kourier writes an outbox row with a deterministic ID derived from
   the event and the message's role (reply, digest, notice; reactions in step 4). The store still accepts a `root` role; nothing in production has ever posted one, because it was for the webhook's root message, which was never built (D39).
2. Every post carries Slack message metadata (`event_type: agentkourier_message`, payload
   holding the outbox ID).
3. On recovery, an outbox row with no recorded message ref is reconciled first: Agent Kourier
   reads recent channel or thread history with metadata included and adopts a matching
   message instead of posting again.
4. Edits (`chat.update`) are naturally idempotent and need no reconciliation. Reactions
   (role `reaction`, e.g. the acknowledgment marker of 8.2) also go through the outbox for
   durability but need no reconciliation: `reactions.add` on a message that already has
   the reaction answers `already_reacted`, which counts as delivered.
5. Interaction prompts (8.4) are posts too, so they go through the outbox with role
   `reply` and the interaction ID as the source ID. Outbox IDs derive from source and
   role, so they cannot collide with agent replies.
6. Streamed messages (8.3) are the exception to step 3. `chat.startStream` creates the
   message, but metadata can be attached only when the stream stops (to be confirmed
   in WP-04). Agent Kourier records the stream's ts in the outbox immediately after
   `startStream` returns. A crash inside that window cannot be reconciled from history
   and is accepted as an at-least-once duplicate, with the orphaned stream left to
   Slack's stream timeout. If WP-04 finds `startStream` accepts metadata, the window
   closes and this exception is removed.
7. **Audit writes are never dropped silently.** A record that authorizes an action is
   written first and fails the action if the write fails: an interaction decision before
   the answer is sent (an unaudited approval never takes effect). The inbound-event record
   (`audit.Writer.Event`) was the webhook's, written before the event was queued so that a
   failed write answered 5xx; nothing calls it now, and a chat message or a trigger match
   writes no audit row before it is taken. A record of something already done (an agent call,
   an unusable pause and its cancel, a feedback vote, a refused trigger start, a stale task
   ended at retention) is logged, metered and returned, but never blocks the follow-up, above
   all the cancel. A decision
   records who, when and the answer's shape (counts), never its choices or free text.

### 8.7 Feedback

- The final message of every investigation that a trigger started carries Useful and Not useful buttons;
  the final message of an answer to a person carries them when the Binding has `feedback.enabled`. A message that ends
  on a question to a person (input-required or auth-required) carries none; the answer that follows does.
- A vote counts only from a member of the bound channel and never from a bot, and only for the message that was
  pressed (its turn, channel and thread must match Agent Kourier's record of that post). A person has one vote on each
  rated message; the latest press wins, and the vote is acknowledged to them alone, in an ephemeral.
- A vote is stored in `feedback` with the Binding, agent, alert name, session and turn. The Binding and agent are
  those of the session when it was created, so a changed or removed Binding neither relabels nor blocks votes on
  earlier investigations. Votes are audited by IDs and verdict only, and counted in
  `agentkourier_investigation_feedback_total{binding, agent, alertname, verdict}`: a counter of votes cast (a change is one
  more vote under its new verdict). The alert name is the trigger's `alertname`, kept only in the name shape; the
  session origin records the bounded source and subject that caused the thread (a trigger start reports `botMessage`, D41), while the feedback adapter maps the
  `botMessage` source, the `alertmanager` source of sessions created before D41, and `legacy` values migrated from the older schema to alert attribution.
  A subject from another source is generic metadata, so the label is `none` for an answer or `unknown` for an investigation. A Binding has at most
  100 names in a process, then `other`.
- The pilot exit decision (D33) is computed from the rows, not the metric (owner, 2026-10-05): each investigation
  counts once, useful when most of its investigation votes say Useful (a tie is not useful); "rated" means at least
  one vote. The query is in `docs/reference/stored-data.md`.

## 9. State

| Table | Key | Purpose |
|---|---|---|
| `events` | event ID | Durable inbound queue with processing status. Built for the webhook; since D39 nothing writes to it |
| `outbox` | outbox ID | Intended Slack posts and their resulting message refs (8.6) |
| `sessions` | conversation key | `contextId`, current task ID, binding, generic origin source and subject, agent at creation, last activity (expires at `threadTTL`; deleted 90 days later, with its replies, interactions, presses and outbox rows) |
| `trigger_threads` | binding, key hash | The thread of each key of a Binding's chat triggers (8.2): its thread, when it opened, was last seen and closed, and when its cooldown ends. The key is stored only as a hash. Swept hourly |
| `trigger_runs` | connection, channel, alert ts | The starts of a Binding's chat triggers (8.2): one row for each alert message that started an investigation or that a limit stopped, with the window of the digest it belongs to. Kept 24 hours |
| `interactions` | interaction ID | Kind, request, state, answer, decider, timestamps. The request is stored normalized (invisible characters deleted) and with the agent's secrets redacted (hint, question text, tool arguments), except a question's choices; request and answer are cleared 25 hours after the decision (7 days for an answer not yet delivered to the session). The row stays until its session is deleted (below). |
| `reply_queue` | session, sequence | Thread replies waiting for the active task to finish. Text is cleared on delivery; a reply undelivered for 30 days whose session has no active task is dropped, text cleared. |
| `feedback` | session, turn, Slack user | Verdict, kind (investigation or answer), alert name, agent, binding, timestamps. Kept (not deleted with its session) |
| `audit` | sequence | Append-only record of events, agent calls, decisions. Append-only except for a purge of entries older than `AGENTKOURIER_AUDIT_RETENTION` (default `keep`: never purged; otherwise at least 24h). |

Retention (hourly, leader only): a session, and the rows of `reply_queue`, `interactions`, `interaction_presses` and `outbox`
that belong to it, are deleted when its last activity is older than its Binding's `threadTTL` plus 90 days, unless it holds work
as listed in 8.2 (a task older than that horizon is ended first, counted as `stale_tasks`); only rows already settled (delivered, decided, posted or abandoned) are deleted. `feedback` is kept until the
pilot's review (D33). The counts are in `agentkourier_retention_removed_total`. Kept with no rule yet: the outbox rows of a digest or
a root post (a role the store accepts and nothing in production has ever posted), and a notice posted into a thread after its session was deleted.

- **SQLite** (default): single replica, PersistentVolume, WAL mode.
- **Postgres**: required for more than one replica.
- **Choosing.** The store is Agent Kourier's own setting, not a resource: it is process-wide, and a reload cannot swap a database
  under a running broker. `AGENTKOURIER_STORE` is `sqlite` (default; `AGENTKOURIER_DB_PATH`) or `postgres`, and with Postgres
  `AGENTKOURIER_POSTGRES_DSN_SECRET` is `namespace/name/key` of the Secret that holds the DSN, read through the same Secret caches as
  every other Secret (D15, section 10). The DSN is never logged, shown in an error, `/readyz` or a metric; an unreadable or empty
  Secret fails the start. Agent Kourier cannot see its replica count, so R7 is enforced by the Helm chart, which refuses more than one
  replica with SQLite. With Postgres the replicas elect a leader (section 11, D37): `AGENTKOURIER_LEASE_NAMESPACE`,
  `AGENTKOURIER_LEASE_NAME` and `AGENTKOURIER_POD_NAME` say where the Lease is and who the replica is, and
  `AGENTKOURIER_LEASE_DURATION`, `AGENTKOURIER_LEASE_RENEW_DEADLINE` and `AGENTKOURIER_LEASE_RETRY_PERIOD` set its timings (15 s, 10 s, 2 s).
  There is no election with SQLite.

## 10. Security

| Concern | Control |
|---|---|
| Bot message authenticity | A bot's message counts only through a `botMessage` trigger that names the sender by the ID and kind the platform reports (on Slack, the bot ID and kind `bot`), never by display name (D36, D42). Several senders can share one bot ID (an incoming webhook that carries Alertmanager, Grafana and a heartbeat), so `match` and `exclude` narrow what counts, and anyone who can post as that bot can start investigations, held to the trigger's limits, when it has any. The message is untrusted text (R2). |
| Pilot environment | The kagent 1.0 alpha install holds production read credentials. Agent Kourier reaches it only through a verifying front door; NetworkPolicies restrict the controller port to that door and kagent's own components, and fence the kagent UI to its own verifying proxy (the UI's nginx relays `/mcp` and `/api` to the controller). The pilot cluster's CNI must enforce NetworkPolicy, or none of these fences exist; whose nginx forwards `/mcp` and `/api` with `Authorization` intact (D32). The agent's credentials are read-only. |
| Paging path | Agent Kourier is not an Alertmanager receiver and is on no alert route. It only reads what Alertmanager already posted to Slack, so it cannot sit in the paging path and its outage loses no page (D39, which replaces D22's `continue: true`). |
| Slack authenticity | Socket Mode, the only mode served today, uses the app token. HTTP mode (Phase 4, D40) will verify the signing secret with a timestamp check. |
| Backend trust | kagent's controller port (8083) trusts identity without verifying it: in insecure mode via `X-User-Id`, and in OIDC mode by decoding the JWT without checking its signature (a forged `alg: none` token was accepted in WP-01). It also takes the acting user from `X-User-Id` or `?user_id=` whenever `X-Agent-Name` is present, even with a valid token (WP-03). Agent Kourier must therefore call a verifying front door (oauth2-proxy or agentgateway) that validates the Binding's token and strips `X-Agent-Name`, `X-User-Id`, `X-Share-Token` and `X-Kagent-Insecure-Runtime-Identity`, never 8083 directly. The chart's UI nginx already strips both in trusted-proxy mode (source). A NetworkPolicy limits 8083 to that front door and kagent's own components (UI, Substrate egress for TaskStore traffic), and a second policy fences the UI (8080) to its own oauth2-proxy, because its nginx relays `/mcp` and `/api` to 8083 with `Authorization` intact. That UI proxy must not accept the Binding's audience; otherwise the Binding token is valid through a second, wider door (WP-01b removed `oidc-extra-audience: agent-kourier-binding` for this reason). |
| Binding identity scope | Each Binding identity gets only the permissions every member of its channel is trusted with. Separate Bindings, not one shared identity, for channels with different trust levels. |
| Secrets | Read from Kubernetes Secrets by reference. Never written to CRD status, logs, or the audit table. An agent's tool arguments, hint and question text are normalized and redacted before a request is stored; a question's choices and a person's answer are stored as given until the interaction is cleared. Text written to logs and to the audit table is normalized and redacted the same way. |
| Tenancy | Namespaced Bindings with explicit `allowedNamespaces` on shared resources. |
| Spend | Per-trigger rate limit, concurrency cap, and daily cap on the investigations a trigger starts, for every `botMessage` trigger, which must set limits (8.2, D41); an explicit `maxRunsPerDay: 0` lifts the daily cap only. |
| Audit | Every event, agent call, and approval is recorded with identity. Subject to retention (`AGENTKOURIER_AUDIT_RETENTION`; kept for ever by default). |

## 11. Operations

- **HA.** Two replicas on Postgres, active/passive. A Kubernetes Lease picks the leader, which runs
  all of Agent Kourier's work: Socket Mode, session runners, the outbox, sweeps and digests. The standby
  serves health and metrics and waits. A takeover is a restart to the new leader: the same recovery
  runs. Every write transaction checks the leader's epoch in Postgres and fails if it is stale, and
  a leader that loses its lease or a write to a stale epoch exits at once (D37). A leader whose
  lease was not renewed within its renew deadline (it was paused) refuses every write to the chat
  from then on and exits, so a paused, deposed leader cannot finish a stream it had open; each
  Slack call, or agent send, already in flight when the pause began (one for each concurrent writer)
  can still happen once more, and S16 measures it. The new leader renders a recovered task under the
  turn the session recorded for it, which ends and replaces a stream the old leader left open.
  Active/active (workers on all replicas) is deferred until one replica's throughput is not enough.
  The design is in `tasks/plans/h4-4-ha-design.md`.
- **Observability.** Prometheus metrics: intake (`agentkourier_intake_total`), trigger
  decisions (`agentkourier_trigger_decisions_total`) and the starts a limit stopped
  (`agentkourier_trigger_limited_total`); agent
  call latency and errors; stream reconnects; approval outcomes; Slack API errors; and, for
  `chat.threadReplies: mention` (D38), `agentkourier_thread_unmentioned_total{binding}` (replies ignored for want
  of a mention) and `agentkourier_thread_context_total{binding,outcome}` (what a mentioned turn's thread read
  came to: `read`, `empty`, `cut`, `failed` or `unsupported`).
  OpenTelemetry traces with the trace context passed to the backend. Structured logs.
- **Packaging.** Pilot: a single Deployment with a mounted config file and a
  PersistentVolume for SQLite. After hardening: a Helm chart containing CRDs,
  controller, RBAC, and an optional NetworkPolicy.
- **Network path.** Alertmanager does not reach Agent Kourier (D39); it posts to Slack.
  Agent Kourier serves nothing that Alertmanager or Slack calls in Socket Mode: its only
  listener is the health and metrics port (`/healthz`, `/readyz`, `/metrics`). It calls Slack,
  the backend, the Kubernetes API, with Postgres the database and, when an OTLP endpoint is
  configured for tracing, the collector (OTLP over HTTP). During the pilot Agent
  Kourier runs alongside the kagent 1.0 alpha install.
- **Failure behavior.** If the backend is unreachable, Agent Kourier retries with backoff; the
  first failed send is retried silently (the first call of a new pod can fail where the next
  works), and if the retry fails too it posts a visible notice in the thread. After 15 minutes (`sendGiveUp`) the turn fails with
  a notice and the queue moves on. Events are never dropped silently.

## 12. Phases

| Phase | Scope | Exit criteria |
|---|---|---|
| 0. Pilot environment and spike | Stand up a pinned kagent 1.0 alpha in OIDC mode behind a NetworkPolicy, with a read-only investigation agent holding production read credentials. A2A client spike: send, stream, reconnect, ask-user and nested HITL, service-token auth, busy-session rejection. Slack streaming limits. | The agent answers a question about production over A2A with a Binding service token. Q1 to Q3 answered. |
| 1. Chat | Resource Go types with a file loader, Slack Socket Mode, @mention and thread replies, reply queue, live output, ask-user interactions, outbox, SQLite store | Two different users hold one multi-turn conversation that survives an Agent Kourier restart. An ask-user question is answered from Slack. A crash-during-post test produces no duplicate messages. |
| 2. Alerts | `botMessage` triggers (8.2) that read the message Alertmanager already posts to Slack: a trigger written in full for Alertmanager's receiver (D41), a thread under each alert with a cooldown per key and notes for repeats and resolves, rate limit, daily cap, digest line, the prompt template with the alert in the untrusted block, feedback buttons. Built as CT-1 to CT-7b; the webhook receiver was dropped (D39) | A firing alert produces one thread; repeats add a note to it; replies continue the session. A 50-alert burst yields capped investigations and one digest line, not 50 messages. Votes are recorded. |
| 3. Pilot | Four weeks, one team plus the platform Binding. Feedback dashboard. Weekly review of not-useful investigations. | Go or no-go recorded against D33 (section 13) |
| 4. Hardening (if go) | CRDs and controller, Postgres, HA, HTTP mode, DMs, Helm chart. These add back the fields that wait for them (D40): `ChatConnection.spec.mode` and `signingSecret`, `ChatConnection.spec.directMessages`, and `Binding.status`. Move the backend to production kagent at 1.0 GA and retire the pilot install. | Two replicas pass a failover test with no lost or duplicated events |
| 5. Tool approvals | Bind the first write-capable agent. R1 is decided (2026-10-11, option 2 reconfirmed 2026-10-13): `approverGroups` is implemented. Approval buttons, timeout, audit, and the `toolApprovals` and `approverGroups` API fields are live. | A gated tool call is approved and denied from Slack, both audited, and R1 has a recorded decision |
| 6. OSS readiness | Docs, generic `a2a` dialect conformance tests, ax example (reference agent and AX kind tier shipped) | A non-kagent A2A agent works with no code changes |

### 12.1 Testing strategy

Every behavior in the exit criteria is proven end to end in a local kind cluster, with
simulated alert messages and a simulated Slack, before it reaches the pilot.

| Tier | Runs | Contents | Purpose |
|---|---|---|---|
| Unit | Every change | Go tests, no cluster | Logic, parsing, state transitions |
| E2E fast | Every change that touches a flow | kind; real Alertmanager (prometheus-operator); fake Slack; stub A2A agent; Agent Kourier; a sink receiver | Agent Kourier's flows, quickly and deterministically |
| E2E full | Phase gates, and when the kagent pin changes | kind; Agent Substrate (gVisor); pinned kagent 1.0 alpha; `kagent-dev/mockllm` as the model; Dex for OIDC; fake Slack; Alertmanager; Agent Kourier | Real kagent behavior: session ownership, busy rejection, HITL payloads, OIDC service identity |
| E2E AX | When AX/ADK or generic A2A changes | Separate kind cluster; pinned unmodified AX, Redis, Substrate (gVisor), persistent ADK/A2A server, deterministic model, Dex, fake Slack, Alertmanager, Agent Kourier | Shared streaming, queue, context and outbox scenarios; authorized plain-text question replies, restart recovery, actor suspend/resume and deadlines. Structured kagent tool approvals are explicitly excluded. |
| Live smoke | Pilot start, manually | Pilot environment, real Slack sandbox workspace | Things fakes cannot prove: Slack rate limits, real rendering |

Components:

- **Simulated alerts.** An alert is posted to the fake Slack's incoming-webhook endpoint as a
  bot, the way Alertmanager's Slack receiver posts it, and goes through the whole trigger path
  of 8.2 (S14, S15, S17). What a real Alertmanager renders from the receiver's template is
  exercised in the sandbox (CT-7a, CT-7b), where a kind cluster's Alertmanager posts through a
  real incoming webhook into a real channel.
- **Sink receiver.** A second webhook receiver on every test route, built to prove D22: an
  alert still reaches it while Agent Kourier is down. D39 removed Agent Kourier from
  Alertmanager's routes, so there is nothing left for it to prove. The kind tiers still deploy
  it, and their Alertmanager, as a routing check, until the owner decides to remove them.
- **Fake Slack.** A Go server implementing the Web API methods Agent Kourier uses and a Socket
  Mode WebSocket, plus a control API for tests to act as users (mention, reply, click,
  submit) and read what Agent Kourier posted. Agent Kourier points at it through slack-go's API URL
  option. Built from response fixtures captured against real Slack in Phase 0.
- **Stub A2A agent.** A scripted A2A server that replays scenarios, including kagent's
  HITL extension payloads, from fixtures captured against real kagent in Phase 0. The
  full tier catches drift between the stub and real kagent.
- **Deterministic model.** `kagent-dev/mockllm` (Apache-2.0), the scripted
  OpenAI-compatible server kagent uses in its own E2E suite. It returns fixed text and
  tool calls, including ask-user calls, so full-tier runs cost nothing and are
  repeatable.
- **AX fixture model.** The separate test agent runs deterministic ADK responses through the same
  persistent server as the Bedrock reference. Its cluster-local controls release gates and call
  AX task lifecycle RPCs. Tests check Substrate state as well as AX phase. `make ax-smoke`
  exercises actual Bedrock/Haiku calls separately. See `hack/kind/ax/README.md`.
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

- ~~Alert received to root message posted: under 5 seconds at p95.~~ **Void (D39):** Agent Kourier posts no root message for an alert; the alert's message is Alertmanager's. No replacement is set yet; whether to set one (for example alert message to the first post in its thread) is the owner's call at H-31.
- An Agent Kourier outage causes no missed alert notification.
- No duplicate agent runs for one alert key inside its cooldown (8.2).
- No lost events across an Agent Kourier restart.
- Adding a chat platform requires implementing `chat.Platform` in its own package,
  passing `chattest.Conformance`, and adding it to the broker's platform factory. It
  changes no package except the adapter and the broker, whose factory names it: a lint rule
  denies every other package the Slack adapter and its client library. Adding an alert
  source that posts to a chat channel is a `botMessage` trigger in the Binding, written in full (D41), and touches no router, session, or store code.
- Moving to a new kagent API revision changes the dialect package only.

## 14. Risks

| # | Risk | Severity | Mitigation |
|---|---|---|---|
| R1 | **Any channel member can approve write actions** (D13), and the backend sees only the Binding identity (D14). No named human is accountable at the backend before a change is made. | High | The Phase 5 implementation supports optional approver groups. Empty means any member of the Binding's channel may approve. A Binding with `toolApprovals: true` and no group loads, with a logged warning. The groups gate tool approvals only, not ask-user answers, and an approver must be in a group, a channel member and not a bot. The backend still sees only the Binding identity (D14); the audit row is the accountable record. **Residual risk:** a Binding with no group still lets any channel member approve a write action, and the backend cannot tell who. Direct pinned-kagent evidence and the broker full-tier suite with fake Slack cover a two-tool approval with one approved and one denied call; only the approved tool executes (2026-10-14). |
| R2 | **Prompt injection through alerts.** Labels and annotations are attacker-influenced text that reaches an agent with tools. | High | Untrusted-data delimiting, the sender-ID check (D36), rate limits. During the pilot the agent is read-only, so the impact is limited to data disclosure in the investigation channel. The same holds for a bot message a `botMessage` trigger accepts (D36): the whole message always goes in the untrusted-data block after the instruction text, never where a template would put it, and a value extracted from it reaches the instruction part only after a character check; a value that fails it refuses the render. The message's fields have their secrets taken out before they reach the agent. |
| R3 | **kagent 1.0 is alpha.** Five alphas shipped in nine days. Paths and HITL payloads may change. | High | All kagent specifics live in the dialect. Pin a tested alpha. Contract tests run against the pinned version. |
| R4 | Anything that reaches kagent's controller port can claim any identity: `X-User-Id` in insecure mode, an unverified JWT in OIDC mode, and in OIDC mode `X-Agent-Name` plus `X-User-Id` or `?user_id=`, which selects the acting user even with a valid token (WP-03: oauth2-proxy forwards them). | High | Agent Kourier talks only to a verifying front door (oauth2-proxy or agentgateway) that strips `X-Agent-Name`, `X-User-Id`, `X-Share-Token` and `X-Kagent-Insecure-Runtime-Identity`; NetworkPolicies restrict port 8083 and fence the UI relay. Proven in the full tier (WP-01b, WP-01c): `verify.sh` shows tokens forged from a real token (payload altered; `alg: none`) and a foreign-audience token are rejected at the front door; A2A, HITL headers, and SSE work through it; path traversal (`..`, `%2F`) is refused; a canary shows the controller still accepts a forged token on `/mcp`; and pods spoofing the trusted labels from other namespaces are refused. Proven for the header path (WP-01d): through the door, B's valid token with `X-Agent-Name` plus `X-User-Id` or `?user_id=`, in any case, gets `-31403` on send, stream, `GetTask`, `SubscribeToTask` and `CancelTask`, and a bogus `X-Share-Token` is dropped, while the same spoof works at 8083 (canary) and the Binding's own send, SSE and `GetTask` still work. The `X-User-Id` and runtime-identity strips are defence in depth (config and source; no observable while `X-Agent-Name` is stripped). The pilot door must use the same config (WP-05). The risk stays High: the controller is unchanged, only mitigated. |
| R5 | No stream replay in kagent. Text produced during a disconnect may be lost from the live view. | Medium | Reconcile from `GetTask` on reconnect and render the final task output. |
| R6 | Slack rate limits on message edits under many concurrent streams. | Medium | Per-message throttle, global limiter, fall back to `final` output under pressure. |
| R7 | SQLite limits the default install to one replica. | Low | Documented. Postgres path for HA. |
| R8 | DMs (Phase 4) have no channel to scope access. | Low | Membership check against the default Binding's channel, when DMs arrive. Today the Slack adapter drops DMs, so none reaches a Binding. |
| R9 | kagent ships a native broker or `WebhookTrigger` (#2185, and the redesign mentioned in #2261), overlapping Agent Kourier. | Medium | No maintainer progress on #2185 yet. Watch both. If one lands, Agent Kourier keeps the alert-to-thread mapping and backend neutrality, and consumes kagent-triggered sessions instead of starting them. |
| R10 | klaus-gateway generalizes and adds alert ingress, overlapping Agent Kourier. | Low | Track it. Agent Kourier's differentiators are alert-to-session threads and backend neutrality. |
| R11 | **The pilot install holds production read credentials** on alpha software, and kagent's TaskStore trusts an unsigned identity header from agent actors (kagent #1660), so a compromised actor can act as another identity inside kagent. | High | Read-only credentials only; the verifying front door; NetworkPolicy; a pinned alpha; nothing in the pilot environment that isn't needed; torn down at the end of the pilot or at 1.0 GA. Revisit if kagent fixes #1660. |
| R12 | **Sparse or biased feedback.** Few votes, or votes only on bad answers, skew the go/no-go. | Medium | Minimum of 20 rated investigations. Weekly review. Engagement is measured alongside ratings. |
| R13 | **kagent 1.0 GA slips or changes shape**, stranding Agent Kourier on an alpha. | Medium | The pilot is explicitly disposable. Changes stay in the dialect. The go/no-go is about value, independent of GA timing. |
| R14 | ~~A team forgets `continue: true`, putting Agent Kourier in its paging path.~~ **Void (D39):** Agent Kourier is no longer an Alertmanager receiver, so no route to it exists. | Dropped | Nothing to check. Agent Kourier only reads what Alertmanager already posted to Slack, so it cannot sit in the paging path. |
| R17 | **A malformed HITL reply wedges the kagent task** in `WORKING` (WP-02: an empty answer list or a wrong request id; still `WORKING` after 20 s, and every send to the session, including a new turn, gets `-32004`). `CancelTask` recovers it; other recovery is untested. After an Agent Kourier crash, a wedged task is indistinguishable from a long-running one. | Medium | The dialect validates answers before sending (6.3); on `-32603` from an answer, Agent Kourier cancels the task and tells the thread; a maximum WORKING age (`maxWorkingAge`, default 1 h, measured on Agent Kourier's clock from the later of the task's status timestamp and the last new progress Agent Kourier saw for it (new text, a new tool call or a state change; a replayed snapshot is not progress), so recent progress never looks wedged; with no timestamp, from that last progress or the stream loss). Agent Kourier's record of recent progress is in memory, so after a restart the age falls back to the status timestamp: a healthy task that has streamed for longer than maxWorkingAge since entering WORKING is cancelled at recovery (accepted limit; a restart-surviving progress signal such as kagent's per-artifact `timeline-position` is on the backlog) cancels a WORKING task as wedged at restart reconciliation, after a lost stream and when an answer gets `-32004`; it never cuts a live stream, which the inactivity watchdog covers. Consider an upstream issue. |
| R18 | **kagent has two agent runtimes that validate differently.** WP-02 measured the Go ADK runtime only. From alpha5 source (not measured), the harness runtime (`go/harness/runtime/a2a/executor.go`, `validateRequest`) refuses a plain user message unless it has exactly one text part. | Medium | Attribution goes into the first text part (8.5, WP-25c). The dialect's wire-shape tests pin the single-text-part shape. Measure against the harness runtime before any pilot agent uses it. |
| R19 | **kagent 1.0 cannot run on a cluster that does not serve the pod-certificate APIs.** kagent 1.0.0-alpha5 runs every session as an Agent Substrate actor, and Substrate 0.3.0-alpha1's chart requires the `ClusterTrustBundle`, `ClusterTrustBundleProjection` and `PodCertificateRequest` feature gates and the `certificates.k8s.io/v1beta1` API. Through Kubernetes 1.36 these are beta and off by default. Kubernetes 1.37 makes them GA in `certificates.k8s.io/v1` with the gates locked on (KEP-4317, KEP-3257), but Substrate 0.3.0-alpha1 calls ClusterTrustBundles through `v1beta1` only, so a 1.37 cluster still needs `--runtime-config=certificates.k8s.io/v1beta1=true`. Managed EKS sets neither feature gates nor runtime config; there the chart installs cleanly and every Substrate workload fails to start (kagent #3071). Found 2026-10-07 on a 1.35 cluster with the gates off. The kind tiers turn them on (`hack/kind/full/kind-config.yaml`), so no tier showed it. | High | Check the target cluster before any kagent 1.0 install (WP-50b): `kubectl api-resources --api-group=certificates.k8s.io` must list `clustertrustbundles` and `podcertificaterequests`, and `kubectl api-versions` must list `certificates.k8s.io/v1beta1`. Where they cannot be enabled (EKS through 1.36 at least), the pilot needs another cluster or a backend without Substrate through the generic `a2a` dialect (Q8). Unverified: whether Substrate 0.4.0-alpha1, kagent main's pin, moved to the `v1` APIs. |
| R16 | A crash between `chat.startStream` returning and Agent Kourier recording the stream's ts leaves a duplicate message and an orphaned stream (8.6 step 6). | Low | ts recorded immediately; window is one local write. WP-04 checks whether startStream accepts metadata, which would close it. |
| R15 | ~~kagent 1.0 needs Agent Substrate with gVisor, which may not run in kind on Docker Desktop for Mac.~~ **Resolved by WP-01:** the full tier runs on macOS on Apple silicon (arm64; gVisor uses the systrap platform, no KVM), about 1.9 to 2.1 GiB idle, up in about three minutes (148 to 180 s measured). amd64 and Linux are expected from kagent's own CI but not measured. Off kind, the cluster must serve the pod-certificate APIs (R19). | Low | Pins matter (kagent 1.0.0-alpha5, Substrate 0.3.0-alpha1). |

## 15. Open questions

| # | Question | Resolve in |
|---|---|---|
| Q1 | ~~Does the A2A Go SDK support negotiating the kagent HITL extension?~~ **Resolved (WP-02):** through a2a-go v2.6.0 with `a2aext.NewActivator`, the structured payload arrives; a2a-go v2.6.0's `a2aext.NewActivator` sends `A2A-Extensions` when, and only when, the agent card lists the URI; without the activator, or with a card listing no extension, no header is sent and no payload arrives (measured with controls; matches `a2aext/activator.go`). Payloads are untyped, so Agent Kourier defines its own structs. Pass the full `.well-known/agent-card.json` URL to `agentcard.Resolve`, and override the card's in-cluster URL with the front door. | Phase 0 |
| Q2 | What are the rate limits of `chat.startStream` and `chat.update` under many concurrent threads? (Method choice resolved in 8.3.) | Phase 0 |
| Q3 | How does kagent 1.0 OIDC mode authenticate a non-human service identity, and what claim becomes the session owner? **Answered (WP-02, WP-03):** kagent has no notion of a service identity. The session owner is a string from the token: the `userIdClaim` claim (`email` in the pilot), else `sub`. Through the front door with real Dex tokens, a second identity gets `-31403` on send, stream, `GetTask`, `SubscribeToTask`, `CancelTask` and `ListTasks` by `contextId`, including while the task runs. An unfiltered `ListTasks` lists only the caller's tasks. The same email with a different `sub` owns the same sessions. A token without `email` passes the door and is owned by its `sub`, so it and the same user's `email` token cannot touch each other's sessions: a Binding's token must carry the same owner claim on every mint. This isolation holds only while the front door strips `X-Agent-Name` and `X-User-Id` (R4). | Phase 0 (closed; door fix WP-01d) |
| Q4 | Final API group name ~~and project license~~ for the OSS release. **License resolved (owner, 2026-10-05):** Apache-2.0 (`LICENSE`). | Phase 6 |
| Q6 | Is one Binding per channel too restrictive for teams wanting several agents in one channel? | Pilot review |
| Q7 | ~~Which team runs the pilot, and which alerts (by name and severity) are routed first?~~ **Resolved (owner, 2026-10-05):** the production `#alerts-critical` channel; anyone in it rates; the owner reviews weekly for four weeks; first alerts `KubePodCrashLooping`, `KubePodNotReady`, `KubeContainerWaiting`, `TargetDown`, `KubeContainerOOMKilled` (`tasks/plans/pilot-production-alerts.md`, decisions 3 and 4). | Before Phase 2 |
| Q8 | Where does the pilot kagent install run, ~~and how does production Alertmanager reach Agent Kourier over TLS~~ (void, D39: Alertmanager posts to Slack, not to Agent Kourier)? The cluster must serve the pod-certificate APIs that Substrate needs (R19). | Phase 0 |
| Q10 | How does Agent Kourier obtain and refresh the Binding's OIDC bearer token? A token stored in a Secret expires (Dex: 24 h) and needs external rotation; alternatively Agent Kourier holds client credentials and mints tokens itself (client-credentials grant; see Q9). Also: Agent Kourier needs Secret VALUES (Slack tokens, header values, identity credentials, and, before D39 dropped the webhook, webhook tokens), but the WP-21d resolver returns key names only by design. | **Resolved by D34.** |
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
| Agent Kourier could sit in the paging path: an outage would lose a page | Additional receiver with `continue: true`, dedicated investigation channel (D22) |
| No concrete first agent; approvals were being built with nothing to gate | Read-only investigation agent (D23); tool approvals to Phase 5 (D25) |
| Spec targeted kagent 1.0 only, but production runs 0.10.x, so Agent Kourier had no real users | Separate pilot kagent 1.0 alpha install, fed by production Alertmanager (D24) |
| Per-alert "not investigated" messages would flood the channel in an alert storm | Digest line per window (D26) |
| Nothing measured whether investigations help | Feedback buttons (D27) and a pilot go/no-go (D33) |
| 24h idle timeout silently dropped context for later replies | 90-day thread mapping, resume the same session (D28) |
| Per-Binding webhook paths needed platform-owned Alertmanager changes, defeating self-service | Team `AlertmanagerConfig`, token in the Binding's namespace, `AlertSource` removed (D29) |
| Four CRDs and a controller before the first Slack message | Same types, file loader until hardening (D30) |
| Rate limit alone allowed about 720 runs a day per Binding | `maxRunsPerDay` (D31) |
| Pilot install would hold production credentials with kagent's default no-auth mode | OIDC mode plus NetworkPolicy (D32; corrected after WP-01 to require a verifying front door) |
| At-least-once queue plus non-idempotent Slack posts could duplicate messages | Outbox with Slack message metadata reconciliation (8.6) |
| Team `AlertmanagerConfig` routes only see their own namespace's alerts | Platform Binding with a central route for cluster-level alerts (7) |

### Direct webhook dropped (2026-10-10)

The owner decided that the Binding's `alerts` field, a per-Binding Alertmanager webhook, is too tied to
Alertmanager. The field is removed, and the direct path is dropped for good, not deferred (D39). Alerts reach a
Binding only through `chat.triggers`, where a `botMessage` trigger reads the message Alertmanager already posts
to Slack (D36, 8.2); alert-specific behaviour lives in a preset and a prompt template. Nothing was built of the
receiver itself: the Alertmanager adapter and the webhook server (WP-40, WP-41) were never dispatched. What changed in
this document:

| Earlier text | Now |
|---|---|
| D22 (additional receiver, `continue: true`), D29 (team `AlertmanagerConfig`, webhook token), and the webhook parts of D8, D9, D10 and D31 | Marked superseded by D39; the rows stay as history |
| Section 7: the `alerts` block, `status.webhookPath`, the `AlertmanagerConfig` example, the webhook-path, namespace-matcher and `continue: true` rules, the webhook token Secret | Removed. A rule says that Alertmanager's routing decides what reaches a channel and the trigger's `match` and `exclude` decide what starts an investigation |
| 6.1 diagram: the Alertmanager webhook arrow and the Event queue box; the Router, its policy and the Config loader | Redrawn. The Alertmanager arrow and the Event queue box are gone, Ingress feeds the Router directly, and the Router matches triggers. The Event queue row of the components table says why it is not drawn |
| 6.2 `AlertAdapter`, `AlertEvent`, "Four Go interfaces" | Removed; three interfaces. The trigger's view of a message (8.2) is what a prompt template sees |
| 8.1 (webhook flow: bearer token, `groupKey` dedup, root message update, `messageId` from `startsAt`) | Rewritten as the Slack-sourced flow: a thread under the alert's own message, a note for a repeat and for a resolve, the limits and digest of 8.2, `messageId` from the alert message's ID |
| 8.2: limits defined as "the shapes of `alerts.rateLimit` and `alerts.maxRunsPerDay`" | Defined where they are used |
| Section 9: `alert_groups`, `run_counters`, and the webhook counter in `trigger_runs` | Neither table was built; `trigger_threads` (migration 0013) is listed. `events` is marked as unwritten |
| Section 10: webhook authenticity, and the paging path as `continue: true` on every route | A bot-message authenticity row; the paging path holds because Agent Kourier reads Slack and is on no Alertmanager route |
| R14 (a team forgets `continue: true`), the 5-second root-message criterion, Q8's TLS path for Alertmanager | Void |
| Phase 2 (section 12), simulated alert webhooks, and the sink receiver of 12.1 | The Phase 2 row is what CT-1 to CT-7b built; the simulated alert is a bot message posted to the fake Slack; the sink has nothing left to prove |

The rows of the v0.1 and v0.2 tables above are not edited; where they name the webhook, D22 or D29, this entry
supersedes them.

### Fields that work (2026-10-10)

The owner decided that the API carries only fields that work, and that a planned feature's fields land with the
feature (D40). Five fields were in the types and did nothing, or did the wrong thing, so they are removed until
their phase: `Binding.spec.interactions.approverGroups` (validated, never read), `Binding.spec.interactions.toolApprovals`
(`false` denied a tool-approval pause; `true` left it unanswered, with an error in the log, because nothing renders a tool approval until Phase 5), `ChatConnection.spec.directMessages` with `defaultBindingRef`
(a direct message is dropped by the Slack adapter and the intake routes by channel only), `ChatConnection.spec.mode`
with the `signingSecret` Secret key (only Socket Mode is served), and `Binding.status` (never set). Phases 4 and 5
and their designs are unchanged. What changed in this document:

| Earlier text | Now |
|---|---|
| Section 7: the `mode`, `directMessages`, `toolApprovals`, `approverGroups` and `status` lines of the examples, and the rule for `directMessages.defaultBindingRef` | Removed from the examples and the rules. One table after the examples says which field arrives with which phase and what happens today |
| Section 7: `signingSecret` as a key of `credentialsSecretRef`; "a second Binding for the same channel is rejected with a status condition" | The keys are `botToken` and `appToken`. A second Binding is a load error that names both Bindings, because the file loader has no status (D30) |
| G2, D16, D17, D25, D30 | A note on each that the DM, HTTP mode, tool-approval and status parts arrive in their phase (D40). The decisions are not rewritten |
| 6.1 and the config loader row; 8.2 DM row and the mention rule of D38; 8.4 opening; section 10 Slack authenticity; section 12 Phase 4 and 5 rows; R1 and R8 | Each keeps its design and says what happens today: DMs dropped, only Socket Mode served, every tool-approval pause denied, no status. The Phase 4 and 5 rows name the fields they add back |

The rows of the v0.1 and v0.2 tables above are not edited; where they name these fields, this entry and D40 qualify
them.

### R1 decided (2026-10-11)

The owner took the R1 decision, optional approver groups: `Binding.spec.interactions.approverGroups` is added in Phase 5, with
`toolApprovals` (D40). Empty means any member of the Binding's channel may approve. A Binding with `toolApprovals: true`
and no approver group loads, with a logged warning. The groups gate tool approvals only, not ask-user answers, and an
approver must be in a group, a channel member and not a bot. The backend still sees only the Binding identity (D14);
the audit row is the accountable record. The residual risk stays in R1: a Binding with no group still lets any channel
member approve. Neither field is in the types yet and every tool-approval pause is still denied; this entry records the
decision, not a change in behaviour. What changed in this document:

| Earlier text | Now |
|---|---|
| D13, D25, D40, and the `approverGroups` row of the section 7 table | `approverGroups` lands in Phase 5 unconditionally, no longer conditional on the R1 decision; D13 says approval is narrowed to the groups when a Binding names any |
| 8.4 opening and step 3, and the Phase 5 row of section 12 | The opening names both fields as Phase 5 additions that are not in the types yet; step 3 adds the group check for a tool approval in Phase 5; the row says R1 is decided and drops the condition |
| R1 | The decision, its date and the residual risk |

The rows of the tables above are not edited.

### Tool approval implementation recorded (2026-10-13)

The Phase 5 implementation is present for the `kagent-v1` dialect. `Binding.spec.interactions.toolApprovals` and
`approverGroups` are API fields with validation and loader wiring: approvals default to a dialect denial, while an
enabled approval is shown in Slack and checked against channel membership, bot status and any configured groups.
The prompt uses redacted literal arguments, the decision is audited before it is sent under the Binding identity, and
timeouts deny through kagent. A pinned kagent 1.0.0-alpha5 run emitted one two-tool pause; approving `add_numbers` and
denying `check_status` completed the task and only `add_numbers` executed. The broker full-tier suite with fake Slack
passed on 2026-10-14, covering audited approve/deny, timeout, authorization, mixed decisions and stale presses.
The complete fast suite also passed; R1 option 2 remains recorded.

What changed in this document:

| Earlier text | Now |
|---|---|
| D13, D25, D40, the section 7 approval rows and R1 | They describe the fields and policy as implemented, including the no-group warning, group gate, `denied_not_approver` result and the residual risk when no group is configured |
| Section 6.2 and section 8.4 | The platform contract includes `IsInAnyGroup`; the approval prompt, fixed denial, audit, timeout and first-complete behavior are specified against the current code |
| Phase 5 exit row | The pinned kagent two-tool observation and passing broker full-tier suite with fake Slack are recorded; the original audited Slack approve/deny exit criterion is preserved |

### Trigger presets removed (2026-10-15)

The owner decided that a trigger preset hides too much behind one word and does not carry to a second chat platform
(D41). The `alertmanager` preset is gone: Alertmanager's Slack receiver is a `botMessage` trigger written in full, from
the example the docs carry. Without the preset's defaults a `botMessage` trigger must set `limits.rateLimit` and
`limits.maxRunsPerDay`, so a trigger is no longer unlimited by omission; an explicit `maxRunsPerDay: 0` is still no cap.
A config that sets `preset:` fails strict decoding. Every trigger start now reports source `botMessage`; the feedback
adapter keeps accepting `alertmanager` for sessions stored earlier. What changed in this document:

| Earlier text | Now |
|---|---|
| D31 and D39 | Each gets a pointer to D41; the preset sentence of D39 says the behaviour lives in the trigger and its prompt template |
| 6.2, last sentence of the alert-source paragraph | A second source is a trigger written in full |
| Section 7: the sample Binding's `preset: alertmanager` with per-field overrides, and the paragraph on the preset's title shape | The sample trigger is written in full with explicit limits; the paragraph states the title-shape requirement for the documented example |
| 8.1 step 3, the `.Link` remark, the `maxIdle` and limits paragraphs of 8.2 | They refer to the documented example's key, template, cooldown and limits |
| 8.2 `limits` bullet and the preset bullet | Limits are required on a `botMessage` trigger; the bullet on presets and their field-by-field overrides is replaced by one on a trigger written in full |
| Section 10, Spend | Every `botMessage` trigger has limits; "one with neither a preset nor `limits` is unlimited" is removed |
| Section 12, Phase 2 row; section 13 extensibility; the origin-source sentence of 8.7 | No preset; a new alert source is a trigger written in full; the origin source is `botMessage` |

The earlier entries of this appendix are not edited; where they name a preset (the 2026-10-10 entry does), this entry supersedes them.

### Platform-neutral API before release (2026-10-15)

The owner decided that the API should take a second chat platform without breaking a Slack Binding (D42), and that
what would be breaking later is changed now, while nothing is released. Four changes: `from.botId` becomes `from.id`
plus an optional `from.kind` (default `bot`); the trigger fields `attachmentText` and `titleLink` become `body` and
`link`; the API no longer defaults the acknowledgment reaction (the platform does); and the agent contract names the
platform (`platform` in `agentkourier.dev/actor`, and a `[<Platform>: …]` prefix that is still `[Slack: …]` on Slack).
Two conventions are recorded with nothing built for them: a `ChatConnection` keeps credentials in
`credentialsSecretRef` with keys fixed per platform and non-secret settings in a `spec.<platform>` block, and doc
comments say "the platform's ID (Slack: `C…`)". A config that sets `botId`, `attachmentText` or `titleLink` fails strict
decoding. What changed in this document:

| Earlier text | Now |
|---|---|
| D36 | "Matches the bot by ID" is "matches the sender by ID and kind", with a pointer to D42; the rest of the row stands |
| Section 7: the sample Binding's `from.botId` and `match.titleLink`, the receiver-match sentence, and the Secret keys rule | `from.id` and `match.link`; the keys of `credentialsSecretRef` are fixed per platform and a platform's own settings go in `spec.<platform>` |
| 8.1 step 2, the trigger table row for other bots, and 8.2 `chat.triggers` | The trigger names a sender by `from.id` and `from.kind`, and both must match; the message fields are `text`, `title`, `link`, `fallback`, `footer` and `body`, each defined by what it holds, with the Slack flattening stated; the thread key hashes the sender's ID and not its kind |
| The 8.2 prompt-view paragraph | The `.Link` remark names the field `link` |
| The 8.2 acknowledgment paragraph | The default is `{enabled: true}`; an unset emoji is the platform's own default (Slack: `hourglass_flowing_sand`) |
| 8.2 attribution paragraph and 8.5, Attribution | The actor metadata key `agentkourier.dev/actor` is named, with `platform`; the prefix is `[<Platform>: Name (id) <email>]`; agents read the metadata, not the prefix |
| Section 10, bot message authenticity; R2 | The sender is named by the ID and kind the platform reports (on Slack, the bot ID and kind `bot`); both must match `from.id` and `from.kind` |

The earlier entries of this appendix are not edited; where they name `botId`, `attachmentText` or `titleLink` they are the record of what was then specified, and this entry supersedes them.

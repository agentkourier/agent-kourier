# Security model

This page explains who Agent Kourier acts as, where the access boundary is, why kagent needs a verifying front door,
and how text from people, alerts and agents is kept inert.

## One identity per Binding

Every turn in a Binding, chat or alert, runs as the Binding's single service identity: a bearer token from
`identity.tokenSecretRef`, or a user ID sent as `X-User-Id`. The person who typed or clicked is resolved and carried as
attribution, a prefix the agent can read and metadata, and recorded in the audit log with the Binding identity. The
prefix is informational; agents must not treat it as authorization.

This is a deliberate choice. An alert has no person, so an alert-driven run cannot use a person's token, and kagent
binds a session to the identity that created it and refuses turns from any other. The consequence is that the agent's
tools see the Binding identity, not the person. So the Binding identity should hold only the permissions every member
of its channel is trusted with, and channels with different trust levels need separate Bindings.

## The channel is the boundary

Access is channel-scoped. A Binding places one agent in one channel; anyone in the channel can talk to it and answer
its questions; a button press or typed answer counts only from a channel member and never from a bot. There are no
per-user or group permissions in v1.

Bindings are namespaced, and shared resources list the namespaces allowed to refer to them in `allowedNamespaces`. A
team's identity token Secret lives in its own namespace.

## Why kagent needs a front door

kagent's controller port (8083) trusts identity without verifying it. In insecure mode it takes `X-User-Id`; in OIDC
mode it decodes the JWT without checking its signature, and a forged `alg: none` token was accepted. It also takes the
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
watch on Secrets only in the namespaces the config names. A rejected Binding token fails the turn, is re-read,
audited and counted; nothing falls back.

## Hostile text

Alert messages, other bots' messages, people's replies and agent output are all treated as untrusted.

- **Toward the agent.** Text from people and other bots that reaches an agent is framed as data inside a tagged block
  that says it is never instructions. A trigger's prompt template may read only values that pass a strict shape check.
  Secrets are redacted before text is stored, logged, or sent.
- **Toward the chat.** Agent output is escaped and stripped of control and format characters, and every post is sent
  with no link parsing and no unfurling, so agent text cannot mention `@channel` or make Slack fetch a URL. Tool
  arguments are shown with secret-looking values redacted. A hostile-text conformance suite runs against every chat
  adapter, and an adapter ships only if it passes.

## The paging path

Agent Kourier is not an Alertmanager receiver. It reads the alert your alerting tool already posts to Slack, so an Agent Kourier outage never loses a page.

## Not in scope

Agent Kourier is not an auth proxy for A2A (use agentgateway for that), does not build or host agents, and has no web
UI. At rest it encrypts nothing itself and relies on the volume or the database; what it keeps, and for how long, is in
[Stored data](../reference/stored-data.md).

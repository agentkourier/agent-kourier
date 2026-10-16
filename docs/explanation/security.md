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
per-user or group permissions in v1.

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
and the release namespace. A rejected Binding token fails the turn, is re-read,
audited and counted; nothing falls back.

## Hostile text

Alert messages, other bots' messages, people's replies and agent output are all treated as untrusted.

- **Toward the agent.** Alert messages, other bots' messages and the thread replies that ride along as context reach
  the agent as data inside a tagged block that says it is never instructions. A person's own turn is sent as their text,
  with an attribution prefix. A trigger's prompt template may read only values that pass a strict shape check.
  Secrets are redacted from alert fields, thread context, tool arguments, audit rows and logs. A person's own message
  is stored and sent to the agent as written.
- **Toward the chat.** Agent output is escaped and stripped of control and format characters, and every post is sent
  with no link parsing and no unfurling, so agent text cannot mention `@channel` or make Slack fetch a URL. Tool
  arguments are shown with secret-looking values redacted. A hostile-text conformance suite runs against every chat
  adapter, and an adapter ships only if it passes.

## The paging path

Agent Kourier is not an Alertmanager receiver. It reads the alert your alerting tool already posts to Slack, so an Agent Kourier outage does not stop an alert from reaching Slack.

## Not in scope

Agent Kourier is not an auth proxy for A2A (use agentgateway for that), does not build or host agents, and has no web
UI. At rest it encrypts nothing itself and relies on the volume or the database; what it keeps, and for how long, is in
[Stored data](../reference/stored-data.md).

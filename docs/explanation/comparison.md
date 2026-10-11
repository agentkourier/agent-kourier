# Comparison with other projects

This page sets Agent Kourier beside the projects that cover parts of the same ground, as they stood when the project
was researched on 2026-09-29. Each of them may have moved on since.

## Prior art

| Project | Covers | Why Agent Kourier is still needed |
|---|---|---|
| giantswarm/klaus-gateway | A Slack front door for kagent 1.0: A2A v1, human-in-the-loop cards, streaming, per-user identity | It refuses any turn without a person's token, so alert-triggered runs are excluded by design. It is coupled to Giant Swarm's platform. |
| apexlnc/kslack | kagent's closed Slack pull request, moved to its own repository | Not adopted into the kagent organisation; last push 2025-11. |
| eliasecchig/a2a-gateway | A generic A2A bridge to Slack, Discord, Telegram and Google Chat | No alert ingress, no kagent human-in-the-loop, no Kubernetes resource model. |
| gke-labs/kube-agents `a2a/gateway` | A backend-neutral chat adapter interface with a Slack adapter | Chat only. |
| zanglang/kagent-slack-bridge, a2a-slack-template | Minimal Socket Mode bridges | Target kagent 0.10 URLs, one agent, no human-in-the-loop. |
| khook | Kubernetes events to kagent, with dedup | Kubernetes events only; targets the 0.x session API. |
| HolmesGPT, Robusta, argus, SherlockOps | Alert investigation with Slack output | Each is its own agent, not a gateway to arbitrary A2A agents. |
| agentgateway | An A2A and MCP proxy with auth, RBAC and rate limits | No chat or alert ingress. Complementary: recommended in front of the backend. |

## Against extending klaus-gateway

- **Alert-driven runs.** klaus-gateway makes every call as a person. An alert has no person, so adding alert ingress
  there would reverse one of its core principles, not add a feature.
- **No platform prerequisites.** klaus-gateway needs Dex account linking, its own data plane and roster annotations
  before an agent is offered. Agent Kourier needs an A2A endpoint and a Slack app.
- **Public protocol.** klaus-gateway compiles against kagent's internal controller APIs. Agent Kourier uses public A2A
  and one published extension, so it works with non-kagent agents and is insulated from kagent's internal changes.
- **Team self-service.** klaus-gateway is configured through Helm values with one default agent. Agent Kourier's
  namespaced Binding lets each team wire its own channel to its own agent.

## Against contributing to kagent

- **Upstream declined chat in core.** The Slack bot pull request was asked to live in a separate repository, and the
  issue asking for a Slack bot was closed as not planned.
- **Backend neutrality.** Code in kagent serves only kagent. Agent Kourier serves any A2A agent, including one running
  as a Google AX task.
- **An independent lifecycle.** A broker has to stay up while agents are suspended.

## What Agent Kourier gives up

- **Per-person identity.** klaus-gateway solves per-person tokens and session shares. Agent Kourier runs every turn
  as the Binding's identity. If per-person tool authority becomes a requirement, klaus-gateway's approach is the
  reference.
- **Upstream alignment.** kagent is redesigning how events reach sleeping agents, and a native solution could overlap
  Agent Kourier's chat path. Agent Kourier's hedge is backend neutrality and the alert-to-session model.

[Why a broker](why-a-broker.md) explains the design these choices lead to.

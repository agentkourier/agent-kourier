# Connect an agent on Google AX

For a platform engineer running Google AX: at the end, Agent Kourier talks to an A2A agent running in an AX Task,
through AX's router, with the generic `a2a` dialect. Support is experimental.

Agent Kourier does not call AX's own API, and it creates, pauses and resumes no Task: that lifecycle stays with you.

## Before you start

- **Your own A2A agent, running as an AX Task.** AX ships no A2A agent.
  [`examples/ax`](https://github.com/agentkourier/agent-kourier/tree/main/examples/ax) is the reference: an ADK agent on
  Bedrock Haiku.
- **The agent verifies Agent Kourier's token.** Agent Kourier sends the Binding's service token as a bearer token. In
  the AX path no front door checks it, so the agent must (the reference checks issuer, audience and subject), or an
  OIDC-verifying proxy must sit in front of the router.
- **A TLS gateway in front of the router** that forwards only the A2A endpoint and the card, fixes the actor header,
  and is reachable from Agent Kourier only.

## Declare the backend

The URL is the gateway's, and a static header names the actor as `<atespace>/<actor>`. Agent Kourier fetches the card
at the URL plus `/.well-known/agent-card.json`.

```yaml
apiVersion: agentkourier.dev/v1alpha1
kind: AgentBackend
metadata:
  name: ax
  namespace: agent-kourier-system
spec:
  dialect: a2a
  url: https://ax-gateway.example.internal/a2a # (1)!
  headers:
    - name: ate-target-actor
      value: agent-kourier-ax/assistant
```

1. The exact JSON-RPC endpoint, used as written.

## What to expect

- Questions arrive as plain text, and a person answers by replying in the thread.
- Structured tool approvals and tool step cards are kagent-only.
- The AX kind tier (`make e2e-ax-up`, `make e2e-ax`) runs the shared Slack scenarios against a real AX and Substrate
  install with a deterministic model: streaming, context across restarts, actor suspend and resume, queued replies and
  text questions answered in the thread.

## Check it before a real channel

The [AX example guide](https://github.com/agentkourier/agent-kourier/blob/main/examples/ax/README.md) has the
deployment steps (the gateway, egress and credential injection, the template overlay) and an acceptance procedure.
Run it before you bind a real channel: a real AX deployment's pause and resume is covered by that procedure, not yet
by a pilot.

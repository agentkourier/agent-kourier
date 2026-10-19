# Connect a generic A2A agent

For a team with an agent that speaks A2A but does not run on kagent: at the end, Agent Kourier sends a Binding's turns
to that agent through the generic `a2a` dialect.

## Before you start

The agent must:

- serve an A2A 1.0 or 0.3 agent card whose capabilities advertise streaming. Agent Kourier rejects an agent whose card
  does not;
- offer a JSON-RPC interface. A card that lists only gRPC or REST is refused;
- serve its card at the endpoint URL plus `/.well-known/agent-card.json`, with a trailing slash on the URL trimmed;
- answer with task events and put its answer in artifact text parts, or answer a new turn with one agent message that
  names no task. A message is the whole answer, and with no task there is nothing to follow after a dropped stream or to
  look up after a restart. An answer to a question must continue its task with task events.

The [agent contract](../reference/agent-contract.md) lists the rest: the methods it must serve, what each message
carries, and how replies, task states and errors are read.

## Declare the backend

```yaml
config:
  agentBackends:
    my-agent:
      spec:
        dialect: a2a
        url: https://my-agent.example.internal/a2a/ # (1)!
        headers: # (2)!
          - name: X-Tenant
            value: payments
        allowedNamespaces: [payments]
```

1. The exact JSON-RPC endpoint, used as written. Keep a trailing slash if the agent's route has one: Agent Kourier
   follows no redirects, because a redirect could carry the Binding's token to another host.
2. Optional static headers, sent on every request. A value can come from a Secret with `valueSecretRef`. Headers
   that carry or select identity are refused: `Authorization`, `Proxy-Authorization`, `Cookie`, `X-User-Id`,
   `X-Agent-Name`, `X-Share-Token` and `X-Kagent-Insecure-Runtime-Identity`.

## Choose how the Binding identifies itself

See [Create the Binding identity](create-binding-identity.md) for choosing the account or name and obtaining a token.

In the Binding's `identity`:

- `tokenSecretRef` sends the Secret's value as `Authorization: Bearer`. Something must verify it: the agent itself, or
  a proxy in front of it. If the Secret cannot be read, the turn is retried and then fails after 15 minutes; it never falls back to `userId`.
- `userId` alone is sent as `X-User-Id`, for an agent that trusts its network.

## What works on this dialect

- Chat and alert investigations, with streamed answers.
- An agent's question, when it pauses with `input-required` and text, if the Binding sets `interactions.askUser: true`.
  A single question with no choices also takes a typed reply in the thread.
- A `Working…` card (a line, in an alert thread) while the agent is silent for 3 seconds, with the default
  `chat.progress`.
- Tool calls as step cards, when the agent marks its tool parts as Google ADK or kagent does (`adk_type`, `kagent_type`
  or `kagent.dev/a2a/part-type`, set to `function_call` or `function_response`): see
  [Tool calls and results](../reference/agent-contract.md#tool-calls-and-results). Without that marker the calls are
  not shown.
- Not: structured choices or tool approvals. The generic `a2a` dialect accepts only a text-only input-required
  question; a structured pause is refused and the task is cancelled. These capabilities come from kagent's extension,
  which only the `kagent-v1` dialect speaks, so no Binding approval setting enables them here.
- An A2A 0.3 agent has no task list, so after a restart a turn may be sent, and run, twice.

## Check it

Mention the bot in a bound channel and watch the pod log for `session turn started` and `session turn ended`. A card
problem shows in `session turn failed`; see [Troubleshoot an install](troubleshoot.md#agent-card-lists-no-json-rpc-interface).

# Connect a generic agent

For a team with an agent that speaks A2A: at the end, Acme Relay sends a binding's turns to that agent.

## Before you start

The agent must serve an agent card whose capabilities advertise streaming. A card that lists only gRPC is refused.

## Declare the backend

Add an agent backend with the `a2a` dialect and the exact URL of the JSON-RPC endpoint.

```yaml
# This comment looks like a heading but sits in a code fence.
dialect: a2a
url: https://agent.example.internal/a2a/
```

### Static headers

A backend can send static headers on every request. Headers that carry identity are refused.

## Check it

Mention the bot in the channel and read the pod log for `turn started`.

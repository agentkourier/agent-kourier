# Write a Binding

For an application team that wants its agent in its channel: at the end, a Binding in your namespace connects your
agent to your channel, through the backend and Slack connection the platform team runs.

## Before you start

- The platform team runs Agent Kourier, an [AgentBackend](../reference/agentbackend.md) for your agent's runtime and a
  [ChatConnection](../reference/chatconnection.md) for Slack, and lists your namespace in both resources'
  `allowedNamespaces`.
- The Agent Kourier bot is a member of your channel, and you have the channel's ID (it starts with `C`, or `G` for a private channel).
- If the backend verifies tokens, a Secret in your namespace holds the Binding's token under the key `token`.

## Write it

```yaml
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata:
  name: payments-assistant
  namespace: payments
spec:
  agent:
    backendRef: # (1)!
      namespace: agent-kourier-system
      name: kagent
    namespace: payments # (2)!
    name: payments-agent
  identity:
    tokenSecretRef: # (3)!
      name: payments-agent-token
      key: token
  chat:
    connectionRef:
      namespace: agent-kourier-system
      name: slack
    channel: C0123456789
    output: live # (4)!
    threadReplies: all # (5)!
```

1. The platform team's AgentBackend. A reference to another namespace works only if that resource lists your
   namespace in `allowedNamespaces`.
2. The agent's namespace and name on the backend, not in your cluster.
3. The token every turn of this Binding is sent with. Without a front door that verifies it, use `userId` instead,
   which is sent as `X-User-Id`.
4. `live` streams the answer as the agent writes it; `final` posts it once, when the turn ends.
5. `all` makes every person's reply in a thread Agent Kourier owns a turn. `mention` takes only the replies that mention the bot.

## Load it

The broker reads its resources from files, usually a ConfigMap. With the Helm chart, add the Binding under
`config.bindings`, keyed by its name, with its namespace:

```yaml
config:
  bindings:
    payments-assistant:
      namespace: payments
      spec:
        # the spec above
```

Then `helm upgrade`. By default (`config.rollOnChange`) the chart rolls the pod when the config changes.

If the platform team manages its own ConfigMap (`config.existingConfigMap`), add the Binding's YAML there as a file
ending in `.yaml`, and ask them to list your namespace in `rbac.secretNamespaces` so the broker may read your Secret.

## Check it

Mention the bot in the channel. A thread opens and the agent answers in it. If nothing happens, see
[Troubleshoot an install](troubleshoot.md#the-mention-gets-no-reply-and-the-log-shows-no-session-turn-started).

One channel takes one Binding. A second Binding for the same channel on the same connection makes the whole
config fail to load: a reload keeps the last good config, and a fresh start fails. The error names the Binding that
already holds the channel.

## Variations

- **Let the agent ask questions in the thread:** set `interactions.askUser: true`. See
  [Let the agent ask questions](agent-questions.md).
- **Investigate the alerts a bot posts to the channel:** add a `botMessage` trigger. See
  [Investigate Alertmanager alerts](investigate-alertmanager-alerts.md) for the preset, or
  [Start investigations from any bot's messages](any-bot-trigger.md) for another tool's bot.
- **Keep threads around for less than 90 days:** set `session.threadTTL`, for example `720h`.
- **Rating buttons on answers:** set `feedback.enabled: true`. Investigations that a trigger started always carry them.

Every field, with its default, is in the [Binding reference](../reference/binding.md).

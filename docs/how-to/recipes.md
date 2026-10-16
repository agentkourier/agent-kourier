# Start from a recipe

For a team that wants an agent in a Slack channel and would rather start from a working example than from a blank file:
at the end, you have applied a recipe's Agent Kourier resources, and its agent if the recipe has one, and know which
checks come with it. There are two recipes: one for Alertmanager alerts, with an agent, and one for another bot's
posts, which holds the Agent Kourier resources only.

A recipe is a directory under
[`examples/recipes`](https://github.com/agentkourier/agent-kourier/tree/main/examples/recipes) with the resources that
connect a channel to an agent, the agent they talk to, and checks. The files on this page are included from the
repository, so what you read here is what the repository holds.

## Triage Alertmanager alerts with a read-only agent

The `sre-triage` recipe starts an investigation in the thread of each alert that Alertmanager posts to a channel. The
agent reads the cluster with read-only tools, says what it found and what it checked, and can ask the thread a
question. Its trigger is the `alertmanager` preset, so each alert group gets one thread, a repeat inside the cooldown
or the thread's 24-hour idle limit adds a note, a resolve closes the thread, and the preset's limits apply: see
[Investigate Alertmanager alerts](investigate-alertmanager-alerts.md).

!!! warning "The agent half runs on the kind full tier"

    The agent is a kagent 1.0 `Agent` on Agent Substrate, and it names the kind full tier's worker pool. It does not
    carry over to a managed cluster such as EKS: see [Connect a kagent agent](connect-kagent.md) for why, and for what
    to use there. The Agent Kourier resources do not depend on it; they work with any agent behind a backend you
    point them at.

### Before you start

- A Slack app with the bot invited to the alert channel: [Configure Slack](configure-slack.md). You need the channel's
  ID and the bot ID of the integration that posts the alerts.
- An Alertmanager Slack receiver whose title reads `FIRING (<n>) - <alertname>` or `RESOLVED - <alertname>`, and whose
  fallback is Alertmanager's default.
- The kind full tier with kagent 1.0.0-alpha5, a verifying front door ([Put a verifying front door before
  kagent](front-door.md)), a kagent `ModelConfig` named `sandbox-model`, and a token for the Binding's identity
  ([Create the Binding identity](create-binding-identity.md)).

### Agent Kourier's resources

Three files, in the namespace `agent-kourier`. The Binding is the recipe: a `botMessage` trigger on the alert bot,
narrowed to one receiver, and `askUser` on. Replace `CREPLACEME` and `B0REPLACEME`.

<!-- name-check: off -->
<!-- The recipe's directory is called kourier, which the product-name check would flag; the titles and includes below name it. -->

```yaml title="kourier/binding.yaml"
--8<-- "sre-triage/kourier/binding.yaml"
```

```yaml title="kourier/agentbackend.yaml"
--8<-- "sre-triage/kourier/agentbackend.yaml"
```

```yaml title="kourier/chatconnection.yaml"
--8<-- "sre-triage/kourier/chatconnection.yaml"
```

<!-- name-check: on -->

Put the directory in a ConfigMap and point the chart at it with `config.existingConfigMap`; the chart needs
`rbac.secretNamespaces` to find the Secrets `slack` and `sre-triage-token`. The recipe's README has the commands.

### The agent

The agent and its prompt. The tools are listed by name and only read; the tool server also runs `--read-only`, and
its ServiceAccount is bound to the built-in `view` ClusterRole.

```yaml title="agent/agent.yaml"
--8<-- "sre-triage/agent/agent.yaml"
```

```yaml title="agent/agent-prompt.yaml"
--8<-- "sre-triage/agent/agent-prompt.yaml"
```

The recipe also holds the tool server's values and RBAC, and `faults.yaml`, a namespace `shop` with five broken apps
and one healthy one to try the agent on. The
[recipe's README](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/sre-triage/README.md) has the
steps to apply the agent, in order, and what to change for your own cluster.

### Check it

The recipe ships its checks. `go test ./test/recipes` loads the three resources with Agent Kourier's own loader and
replays messages through the trigger. Each case is one message and what the Binding must make of it; the first two
are:

```yaml title="evals/triggers.yaml (the first two cases)"
--8<-- "sre-triage/evals/triggers.yaml:first-cases"
```

These cases judge one message at a time. They do not evaluate the thread per alert, the cooldown, the close or the
limits. Against your own channel, replay its real history through the trigger with a
[dry run](tune-triggers.md) before you rely on it.

The recipe also holds quality scenarios, one for each seeded fault and one for the healthy app, as data for a person
to read an agent's answer against. Nothing runs them yet, and their shape may change. One of them:

```yaml title="evals/scenarios/checkout-crash-loop.yaml"
--8<-- "sre-triage/evals/scenarios/checkout-crash-loop.yaml"
```

## Start investigations from a CI bot's failure posts

The `custom-bot-trigger` recipe is for a bot that has no preset: Alertmanager is the only one with a preset. A CI
system posts a message to a channel when a pipeline fails, and the recipe starts an investigation in a thread of its
own for each pipeline and branch. It holds Agent Kourier's resources only and ships no agent, so the backend's `url`
has to point at an A2A agent you run. The trigger is written in full, as in
[Start investigations from any bot's messages](any-bot-trigger.md): `match` and `exclude` pick out the bot's failure
posts, `extract` pulls out a state, a pipeline and a key, `thread` gives one thread to each key and closes it when the
state is `PASSED`, `limits` bounds how many investigations start, and `promptTemplate` writes the first prompt.

!!! warning "The message shape is made up"

    The regexes read a post that the recipe invented, and no real product posts it. Replace every regex with one
    written against your own bot's messages, fetched as JSON (`conversations.history` returns them).

### Before you start

- A Slack app with the bot invited to the channel: [Configure Slack](configure-slack.md). You need the channel's ID and
  the `bot_id` of the CI bot's posts.
- An A2A agent, and its URL for the AgentBackend. For kagent, set `dialect: kagent-v1` as in
  [Connect a kagent agent](connect-kagent.md).

### Agent Kourier's resources

The recipe has three files, in the namespace `agent-kourier`: the Binding below, an AgentBackend, and a
ChatConnection that names the Secret `slack-ci`. Replace `CREPLACEME` and `B0REPLACEME`.

<!-- name-check: off -->

```yaml title="kourier/binding.yaml"
--8<-- "custom-bot-trigger/kourier/binding.yaml"
```

<!-- name-check: on -->

With no `limits`, a trigger that has no preset is unlimited, so a storm of failing pipelines would start as many
investigations. The recipe's README has the commands to create the Secret, load the directory into the chart and
replay the channel's history through the trigger first.

### Check it

`go test ./test/recipes` loads the three resources with Agent Kourier's own loader and replays made-up posts through
the trigger, each judged alone. The first is a failure that matches, with the values it extracts:

```yaml title="evals/triggers.yaml (the first case)"
--8<-- "custom-bot-trigger/evals/triggers.yaml:matched"
```

The pipeline and key regexes take only letters, digits and `_ . -`. A pipeline named `team/web-build` therefore passes
`match`, fails an `extract`, and so matches no trigger. Nothing starts and nothing is posted in Slack: the miss shows
in the metric `agentkourier_trigger_unmatched_total`, in a debug log line and in a [dry run](tune-triggers.md). A case
pins it:

```yaml title="evals/triggers.yaml (a name outside the regex)"
--8<-- "custom-bot-trigger/evals/triggers.yaml:quiet-miss"
```

The cases do not evaluate the cooldown, thread keys across messages, `limits` or `closeWhen`.

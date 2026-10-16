# Start from a recipe

For a team that wants an agent in a Slack channel and would rather start from a working example than from a blank file:
at the end, you have applied a recipe's Agent Kourier resources, and its agent if the recipe has one, and know which
checks come with it. There are three recipes: one for Alertmanager alerts, with an agent, one for another bot's
posts, which holds the Agent Kourier resources only, and one that answers questions from documentation, with an agent that
runs on any cluster. A team that is new to Agent Kourier should start with the last one.

A recipe is a directory under
[`examples/recipes`](https://github.com/agentkourier/agent-kourier/tree/main/examples/recipes) with the resources that
connect a channel to an agent, the agent they talk to, and checks. The files on this page are included from the
repository, so what you read here is what the repository holds.

!!! warning "The recipes' Slack Secret is called `slack`"

    The recipes' ChatConnection names a Secret `slack`. [Configure Slack](configure-slack.md) and
    [Install with Helm](install-with-helm.md) create `agent-kourier-slack`. Create the Secret under the name the
    recipe's ChatConnection uses, or change the ChatConnection to match.

## Triage Alertmanager alerts with a read-only agent

The `sre-triage` recipe starts an investigation in the thread of each alert that Alertmanager posts to a channel. The
agent reads the cluster with read-only tools, says what it found and what it checked, and can ask the thread a
question. Its trigger is a `botMessage` trigger written in full, so each alert group gets one thread, a repeat inside the
cooldown or the thread's 24-hour idle limit adds a note, a resolve closes the thread, and the trigger's limits apply: see
[Investigate Alertmanager alerts](investigate-alertmanager-alerts.md), which shows the same trigger.

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
  kagent](front-door.md)), a kagent `ModelConfig` named `sandbox-model`, and a token for the Binding's identity from an OIDC issuer
  that the front door accepts ([Create the Binding identity](create-binding-identity.md)). Without an OIDC issuer, the
  agent half is not usable as written.

### Agent Kourier's resources

Three files, in the namespace `agent-kourier`. The Binding is the recipe: a `botMessage` trigger on the alert bot,
narrowed to one receiver, and `askUser` on. Replace `CREPLACESRE` and `B0REPLACEME`.

```yaml title="kourier/binding.yaml"
--8<-- "sre-triage/kourier/binding.yaml"
```

```yaml title="kourier/agentbackend.yaml"
--8<-- "sre-triage/kourier/agentbackend.yaml"
```

```yaml title="kourier/chatconnection.yaml"
--8<-- "sre-triage/kourier/chatconnection.yaml"
```

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

The `custom-bot-trigger` recipe is for any bot whose posts you want to act on: its trigger is written by hand, as the
Alertmanager trigger is. A CI system posts a message to a channel when a pipeline fails, and the recipe starts an
investigation in a thread of its own for each pipeline and branch. It holds Agent Kourier's resources only and ships no agent, so the backend's `url`
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
- An agent that trusts the `X-User-Id` header: the Binding's `identity.userId` is sent as that header and proves nothing.
  If your agent or its front door verifies tokens, use `identity.tokenSecretRef` instead ([Create the Binding
  identity](create-binding-identity.md)).

### Agent Kourier's resources

The recipe has three files, in the namespace `agent-kourier`: the Binding below, an AgentBackend, and a
ChatConnection that names the Secret `slack`, the same ChatConnection as the alert recipe's, so the two recipes share one
Slack app. Replace three placeholders: `CREPLACECI` (the channel's ID) and `B0REPLACEME` (the CI bot's `bot_id`) in the
Binding, and the host `REPLACE-ME` in the AgentBackend's `url`.

```yaml title="kourier/binding.yaml"
--8<-- "custom-bot-trigger/kourier/binding.yaml"
```

```yaml title="kourier/agentbackend.yaml"
--8<-- "custom-bot-trigger/kourier/agentbackend.yaml"
```

A `botMessage` trigger does not load without `limits`, and the limits are what bound a storm of failing pipelines,
which would otherwise start as many investigations. The recipe's README has the commands to create the Secret, load the
directory into the chart and replay the channel's history through the trigger first.

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

## Answer questions from your documentation

The `docs-agent` recipe turns a mention in a help channel into an answer drawn from a body of Markdown documentation.
The agent searches the pages, reads the best ones, answers in the thread and lists the pages it used; when the pages do
not cover the question, it is told to say so. Here the documentation is Agent Kourier's own, and the recipe's README
says what to change for yours. The agent is an Agent Development Kit (ADK) program in a Go module of its own, served
over A2A and reached through the generic `a2a` dialect, so it needs neither kagent nor Agent Substrate. It runs as a
plain Deployment on any cluster that can reach a model provider. Its search is by keywords, served as an MCP tool, and
the tool is written so that a search by embeddings can replace it.

### Before you start

- A Slack app with the bot invited to the help channel: [Configure Slack](configure-slack.md). You need the channel's ID.
- A model provider's key, for OpenRouter or any OpenAI-compatible HTTPS endpoint, and a model that supports tool calls.
- Docker, to build the agent's image: the documentation goes into the image at build time.

### Agent Kourier's resources

Three files, in the namespace `agent-kourier`. The Binding is the recipe: a `mention` trigger, nothing else, and
`feedback.enabled` for the useful and not-useful buttons. Replace `CREPLACEDOCS`.

```yaml title="kourier/binding.yaml"
--8<-- "docs-agent/kourier/binding.yaml"
```

```yaml title="kourier/agentbackend.yaml"
--8<-- "docs-agent/kourier/agentbackend.yaml"
```

```yaml title="kourier/chatconnection.yaml"
--8<-- "docs-agent/kourier/chatconnection.yaml"
```

A mention trigger takes no limits, so anyone who can mention the bot in the channel can spend model quota. Bound the key
at the provider.

### The agent

The agent runs as one pod. The key comes from a Secret that you create; no credential is in the file.

```yaml title="agent/manifests/deployment.yaml"
--8<-- "docs-agent/agent/manifests/deployment.yaml"
```

The recipe's [README](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/docs-agent/README.md) has
the steps in order: build the image, make the Secrets, apply the manifests (which include a NetworkPolicy that lets only
pods in the namespace `agent-kourier` reach the agent, where the network plugin enforces NetworkPolicy) and load the Agent Kourier resources into the chart.

### Check it

The module has its own tests and a smoke test that needs no credentials: `make docs-agent-test` and
`make docs-agent-smoke`. The smoke test starts the real agent on loopback with a scripted stand-in for the language model
and asks it fixed questions over A2A. The model's part is the stand-in's, so the run checks the wiring and the retrieval,
not the quality of any model's answer. A run against a real model, `make docs-agent-smoke-live`, needs your credentials.

`go test ./test/recipes` loads the three resources with Agent Kourier's loader. The trigger evals judge a `botMessage`
trigger one message at a time and do not evaluate a mention, so the recipe's file holds one case, that another bot's post
is not taken:

```yaml title="evals/triggers.yaml"
--8<-- "docs-agent/evals/triggers.yaml"
```

The recipe also holds quality scenarios: questions the documentation answers, each with the page that answers it, and
questions it does not answer, for which the answer must say so. The module's tests check that the page exists and that
the search finds it, and that the missing topic is on no page. Whether a model then answers well is for a person to read
against the scenario. One of them:

```yaml title="evals/scenarios/connect-non-kagent-agent.yaml"
--8<-- "docs-agent/evals/scenarios/connect-non-kagent-agent.yaml"
```

The search ranks pages by shared words and knows no synonyms. The agent keeps its conversations in memory and runs one
replica, and it checks no token, so it relies on the NetworkPolicy. The README lists the rest.

# Start from a recipe

For a team that would rather start from a working example than from a blank file, whether its agent answers in a Slack or
Microsoft Teams channel or acts on another service's webhooks: at the end, you have applied a recipe's Agent Kourier
resources, and its agent if the recipe has one, and know which checks come with it.

There are nine recipes. Five put an agent in a chat channel, and are on this page:

- one for Alertmanager alerts, with an agent
- one for another bot's posts, which holds the Agent Kourier resources only
- one that answers questions from documentation, with an agent that runs on any cluster
- one whose agent you define in a YAML file: a prompt, a model and the MCP tools it may call
- one that puts that agent in a Microsoft Teams channel instead, with Alertmanager's alerts, which is experimental and
  needs Agent Kourier 0.6.0 or later

A team that is new to Agent Kourier should start with the documentation agent. A team that wants its own agent without
writing one should start with the fourth.

The other four, `webhook-github`, `webhook-jira`, `webhook-bitbucket` and `webhook-linear`, start an agent from another
service's signed webhooks. They are experimental, and need Agent Kourier 0.6.0 or later.

`webhook-github` ships an agent that posts on GitHub as a machine user. The other three hold the Agent Kourier resources
only, for an A2A agent of yours that acts on the service with its own tools.
[Start agents from webhooks](receive-webhooks.md) and its page for each service walk through them.

`examples/recipes/echo-agent` is not one of the nine: it is a plain A2A agent with no model, with Agent Kourier files for
it, which other guides use to try a connection, and it has no recipe README of its own.

A recipe is a directory under
[`examples/recipes`](https://github.com/agentkourier/agent-kourier/tree/main/examples/recipes) with the resources that
connect a channel, or a service's webhooks, to an agent, the agent they talk to if the recipe ships one, and checks. The files on this page are included from the
repository, so what you read here is what the repository holds.

!!! warning "The recipes' Slack Secret is called `slack`"

    The recipes' ChatConnection names a Secret `slack`, the name [Install with Helm](install-with-helm.md) and
    [Configure Slack](configure-slack.md) create. If yours has another name, create the Secret under the name the
    recipe's ChatConnection uses, or change the ChatConnection to match and set `config.chatConnectionSecret` to the Secret's name.

## Triage Alertmanager alerts with a read-only agent

The `sre-triage` recipe starts an investigation in the thread of each alert that Alertmanager posts to a channel. The
agent reads the cluster with read-only tools, says what it found and what it checked, and can ask the thread a
question. Its trigger is a `botMessage` trigger written in full, so:

- each alert group gets one thread
- a repeat inside the cooldown or the thread's 24-hour idle limit adds a note
- a resolve closes the thread
- the trigger's limits apply

See [Investigate Alertmanager alerts](investigate-alertmanager-alerts.md), which shows the same trigger.

!!! warning "The agent half runs on the kind full tier"

    The agent is a kagent 1.0 `Agent` on Agent Substrate, and it names the kind full tier's worker pool. It does not
    carry over to a managed cluster such as EKS: see [Connect a kagent agent](connect-kagent.md) for why, and for what
    to use there.

    The Agent Kourier resources do not depend on it. They work with any agent behind a backend you point them at.

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

Apply the directory as custom resources (`kubectl apply -f examples/recipes/sre-triage/kourier/`, into the namespace
`agent-kourier`, which must exist). Each resource sits in that namespace and names the others without one.

Agent Kourier reads them from the cluster on the chart's default route, `config.source: crds`, with
`config.chatConnection: agent-kourier/slack`. The chart grants the Secret `slack` from that name, and
`rbac.secretNames` grants `sre-triage-token`, since the chart cannot see the other Secrets of resources you apply
yourself. With `config.*` left empty, the release installs the CRDs itself, so they are served before you apply the
resources.

The recipe's README has the values and the commands, in order. The other recipes' resources work the same way.

This route needs a chart release that has custom resources, and Kubernetes 1.33 or later. Release 0.1.6 does not have
it: `crds` and `config.source` are not among its values.

On 0.1.6, or to stay on a ConfigMap, the same files load as one, with `config.existingConfigMap` and
`rbac.secretNamespaces: [agent-kourier]`. On a release that has custom resources, add `config.source: files` too: it
refuses `config.existingConfigMap` without it, and 0.1.6 refuses the key.
[Move the config between a ConfigMap and custom resources](migrate-config-source.md) moves an install from one to the
other.

### The agent

The agent and its prompt. The tools are listed by name and only read. The tool server also runs `--read-only`, and
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
replays messages through the trigger.

Each case is one message and what the Binding must make of it. The first two are:

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
investigation in a thread of its own for each pipeline and branch.

The recipe holds Agent Kourier's resources only and ships no agent, so the backend's `url` has to point at an A2A agent you
run.

The trigger is written in full, as in [Start investigations from any bot's messages](any-bot-trigger.md):

- `match` and `exclude` pick out the bot's failure posts
- `extract` pulls out a state, a pipeline and a key
- `thread` gives one thread to each key and closes it when the state is `PASSED`
- `limits` bounds how many investigations start
- `promptTemplate` writes the first prompt

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
ChatConnection that names the Secret `slack`. It is the same ChatConnection as the alert recipe's, so the two recipes
share one Slack app.

Replace three placeholders: `CREPLACECI` (the channel's ID) and `B0REPLACEME` (the CI bot's `bot_id`) in the
Binding, and the host `REPLACE-ME` in the AgentBackend's `url`.

```yaml title="kourier/binding.yaml"
--8<-- "custom-bot-trigger/kourier/binding.yaml"
```

```yaml title="kourier/agentbackend.yaml"
--8<-- "custom-bot-trigger/kourier/agentbackend.yaml"
```

A `botMessage` trigger does not load without `limits`, and the limits are what bound a storm of failing pipelines,
which would otherwise start as many investigations. The recipe's README has the commands to create the Secret, replay
the channel's history through the trigger first, and apply the directory as custom resources.

### Check it

`go test ./test/recipes` loads the three resources with Agent Kourier's own loader and replays made-up posts through
the trigger, each judged alone. The first is a failure that matches, with the values it extracts:

```yaml title="evals/triggers.yaml (the first case)"
--8<-- "custom-bot-trigger/evals/triggers.yaml:matched"
```

The pipeline and key regexes take only letters, digits and `_ . -`. A pipeline named `team/web-build` therefore passes
`match`, fails an `extract`, and so matches no trigger.

Nothing starts and nothing is posted in Slack: the miss shows
in the metric `agentkourier_trigger_unmatched_total`, in a debug log line and in a [dry run](tune-triggers.md). A case
pins it:

```yaml title="evals/triggers.yaml (a name outside the regex)"
--8<-- "custom-bot-trigger/evals/triggers.yaml:quiet-miss"
```

The cases do not evaluate the cooldown, thread keys across messages, `limits` or `closeWhen`.

## Answer questions from your documentation

The `docs-agent` recipe turns a mention in a help channel into an answer drawn from a body of Markdown documentation.
The agent searches the pages, reads the best ones, answers in the thread and lists the pages it used. When the pages do
not cover the question, it is told to say so.

Here the documentation is Agent Kourier's own, and the recipe's README says what to change for yours.

The agent is an Agent Development Kit (ADK) program in a Go module of its own, served over A2A and reached through the
generic `a2a` dialect, so it needs neither kagent nor Agent Substrate. It runs as a plain Deployment on any cluster that
can reach a model provider. Its search is by keywords, served as an MCP tool, and the tool is written so that a search
by embeddings can replace it.

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

The agent runs as one pod. The key comes from a Secret that you create, and no credential is in the file.

```yaml title="agent/manifests/deployment.yaml"
--8<-- "docs-agent/agent/manifests/deployment.yaml"
```

The recipe's [README](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/docs-agent/README.md) has
the steps in order: build the image, make the Secrets, apply the manifests and apply the Agent Kourier resources as
custom resources. The manifests include a NetworkPolicy that lets only pods in the namespace `agent-kourier` reach the
agent, where the network plugin enforces NetworkPolicy.

### Check it

The module has its own tests and a smoke test that needs no credentials: `make docs-agent-test` and
`make docs-agent-smoke`. The smoke test starts the real agent on loopback with a scripted stand-in for the language model
and asks it fixed questions over A2A.

The model's part is the stand-in's, so the run checks the wiring and the retrieval,
not the quality of any model's answer. A run against a real model, `make docs-agent-smoke-live`, needs your credentials.

`go test ./test/recipes` loads the three resources with Agent Kourier's loader. The trigger evals judge a `botMessage`
trigger one message at a time and do not evaluate a mention, so the recipe's file holds one case, that another bot's post
is not taken:

```yaml title="evals/triggers.yaml"
--8<-- "docs-agent/evals/triggers.yaml"
```

The recipe also holds quality scenarios: questions the documentation answers, each with the page that answers it, and
questions it does not answer, for which the answer must say so. The module's tests check that the page exists and that
the search finds it, and that the missing topic is on no page.

Whether a model then answers well is for a person to read
against the scenario. One of them:

```yaml title="evals/scenarios/connect-non-kagent-agent.yaml"
--8<-- "docs-agent/evals/scenarios/connect-non-kagent-agent.yaml"
```

The search ranks pages by shared words and knows no synonyms. The agent keeps its conversations in memory and runs one
replica, and it checks no token, so it relies on the NetworkPolicy. The README lists the rest.

## Define an agent from a prompt

The `prompt-agent` recipe is an agent that a team writes as one `agent.yaml` in Pydantic AI's agent spec format: a
prompt, a model, and the MCP tools it may call. A small Python server serves it over A2A, reached through the generic
`a2a` dialect, so it runs as a plain Deployment on any cluster that can reach a model provider and the MCP server.

Switching between providers (OpenRouter, Anthropic, OpenAI, an OpenAI-compatible endpoint, Ollama) is a change to the
`model:` line and the key's variable. The README lists the ones that have been run, and how: Anthropic's and OpenAI's own
APIs, and Bedrock, have not.

```yaml title="agent/agent.yaml"
--8<-- "prompt-agent/agent/agent.yaml"
```

Chat and alert text is untrusted, so the agent refuses to start on anything in the file that could give it a tool the
file does not list: any capability but MCP, an MCP entry without `allowed_tools`, a provider-specific model setting, a
model that searches the web, and more.

The agent can then call the listed MCP tools and one tool of its own, `ask_user`, which
asks the thread a question. The tool list is enforced; what the model says it did is not.

### Before you start

- A Slack app with the bot invited to the channel: [Configure Slack](configure-slack.md). You need the channel's ID.
- A model provider's key, a model that supports tool calls, and an MCP server with the tools the agent may call.
- Docker, only to build an image of your own: the manifests name the agent's image that each release publishes.

### Agent Kourier's resources

Three files, in the namespace `agent-kourier`. The Binding is the recipe: a `mention` trigger, `askUser` on, and the
token that the agent checks, from the Secret `prompt-agent-bearer`, which `rbac.secretNames` grants. Replace
`CREPLACEPROMPT`.

```yaml title="kourier/binding.yaml"
--8<-- "prompt-agent/kourier/binding.yaml"
```

```yaml title="kourier/agentbackend.yaml"
--8<-- "prompt-agent/kourier/agentbackend.yaml"
```

```yaml title="kourier/chatconnection.yaml"
--8<-- "prompt-agent/kourier/chatconnection.yaml"
```

A mention trigger takes no limits, so anyone who can mention the bot in the channel can spend model quota, and a bot's
mention is held only to the broker's fixed limits. Bound the key at the provider.

The agent refuses to start without the token's file, unless it is told to accept every caller, so the same value goes
in a Secret in the agent's namespace too.

The recipe's [README](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/prompt-agent/README.md)
has the steps in order (choose the image, write `agent.yaml`, make the Secrets, apply the agent and Agent Kourier's
resources). It also has each field of `agent.yaml`, the providers that have been run, everything the agent refuses and
why, and its limits.

### Check it

`make prompt-agent-test` drives the agent over A2A with a scripted model and a real MCP server: streaming, follow-ups,
the tool policy, cancel, questions, and the token check. `go test ./test/recipes` loads the three resources with Agent
Kourier's loader. The agent keeps its conversations in memory and runs one replica.

## Put the prompt agent in a Microsoft Teams channel

!!! warning "Experimental"

    Microsoft Teams support needs Agent Kourier 0.6.0 or later: 0.5.x refuses `platform: teams`. It has not yet been run as a
    full install against Microsoft Teams: [Configure Microsoft Teams](configure-teams.md) says what was checked.

The `prompt-agent-teams` recipe reuses the `prompt-agent` recipe's agent as it is, and binds it to a Microsoft Teams
channel: a mention of the bot starts a session, and an alert card that Alertmanager posts to the channel through a
Workflows webhook starts an investigation in that alert's thread. It also holds the Teams app's `manifest.json`.

### Before you start

- The prompt agent running, as in [Define an agent from a prompt](#define-an-agent-from-a-prompt), with its Secrets.
- A bot registered in your Microsoft Entra tenant and its app installed in a team: steps 1 to 5 of
  [Configure Microsoft Teams](configure-teams.md). You need the tenant ID, the bot's application ID and the channel's ID.
- For the alerts, a Workflows flow on the channel and Alertmanager 0.28.0 or later, whose `msteamsv2` receiver posts to
  it ([Send Alertmanager alerts to a channel](configure-teams.md#send-alertmanager-alerts-to-a-channel)).

### Agent Kourier's resources

Three files, in the namespace `agent-kourier`. Replace the placeholder GUIDs with your tenant ID and the bot's application
ID, and `19:REPLACE-prompt-teams@thread.tacv2` with the channel's ID. The Binding has a `mention` trigger and a `botMessage`
trigger for Workflows' posts, whose regexes read the card of Alertmanager's default templates.

```yaml title="kourier/binding.yaml"
--8<-- "prompt-agent-teams/kourier/binding.yaml"
```

```yaml title="kourier/agentbackend.yaml"
--8<-- "prompt-agent-teams/kourier/agentbackend.yaml"
```

```yaml title="kourier/chatconnection.yaml"
--8<-- "prompt-agent-teams/kourier/chatconnection.yaml"
```

The ChatConnection is in `http` mode and polls Microsoft Graph for the alerts. The recipe's
[README](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/prompt-agent-teams/README.md) has the
steps in order: the app package, the Secret `teams`, the chart values with the chat listener, and the alert route.

### Check it

`go test ./test/recipes` loads the three resources with Agent Kourier's loader and runs the recipe's trigger evals,
`evals/triggers.yaml`, against card text written from Alertmanager's templates:

```yaml title="evals/triggers.yaml"
--8<-- "prompt-agent-teams/evals/triggers.yaml:first-case"
```

`agent-kourier dry-run` reads only Slack, so it cannot replay a Teams channel's history.

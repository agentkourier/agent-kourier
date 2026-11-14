# prompt-agent

Someone mentions the Agent Kourier bot in a channel. An agent that a team defined in one YAML file, a prompt, a model
and the MCP tools it may call, answers in the thread. To switch between providers (OpenRouter, Anthropic, OpenAI, an
OpenAI-compatible endpoint, a local Ollama), you change the `model:` line and the variable that holds the key;
[Providers](#providers) says which of them have been run, and how.

`agent/agent.yaml` is written in Pydantic AI's own agent spec format, with no field of Agent Kourier's. `agent/agent.py`,
one Python file to read top to bottom, loads it, refuses at start anything that could give the agent a tool the file
does not list, and serves the agent over A2A (the Agent2Agent protocol) with the a2a-sdk. Agent Kourier reaches it
through the generic `a2a` dialect, its adapter for any plain A2A server
([glossary](../../../docs/reference/glossary.md)). It needs no kagent, so it runs on any Kubernetes cluster that can
reach a model provider. **Start here if your team wants an agent from a prompt and would rather not write one.** To see
how an agent meets the contract with no model at all, read the
[echo agent](../echo-agent/agent/README.md); for a full agent in Go, see [`docs-agent`](../docs-agent/README.md).

## What it does

- A mention of the bot in the channel, outside a thread, opens a thread and a session with the agent. A mention inside
  a thread Agent Kourier does not own does the same, anchored to that thread
  ([Chat triggers](../../../docs/reference/chat-triggers.md)).
- A mention by another bot or an incoming webhook that can post in the channel starts a turn too, with a fixed prompt,
  within fixed limits across every bot on the Binding: 3 turns in any 30 minutes, 1 at a time, 20 in any 24 hours
  ([A bot's mention](../../../docs/reference/chat-triggers.md#a-bots-mention)). Such a turn can call the agent's tools
  like any other. `chat.botMentions: none` in the Binding turns it off.
- The agent runs the model with the prompt (`instructions`) of `agent.yaml` and the MCP tools (the Model Context
  Protocol, the standard way to give a model tools) that its `allowed_tools` lists, and one tool of the wrapper's own,
  `ask_user`, which `agent.yaml` does not list.
- The answer streams into the thread as the model writes it: each piece of text is an append to one A2A artifact, and a
  text that follows a tool call starts a new paragraph (`text_of` in `agent/agent.py`). Agent Kourier throttles its
  writes to Slack (every 2 seconds by default), so a reply of dozens of appends arrives in a few updates.
- A person's reply in the thread continues the session (the Binding leaves `chat.threadReplies` at `all`). The agent
  keeps each thread's history in memory under the thread's `contextId`, so a follow-up sees the earlier turns.
- `ask_user` pauses the run: the question is posted in the thread, and the person's reply resumes the same run with the
  answer as the tool's result. A question left unanswered past the Binding's `interactions.timeout` (30 minutes by
  default) is cancelled, and the next turn in the thread runs normally (`repair_messages` in `execute`).
- A cancel stops the run, during a tool call too, and keeps the messages the run had so far, so the next turn builds on
  them.
- A turn that fails ends the task `failed` with `<error type>: <message>`, which Agent Kourier reads to pick a failure
  category for the thread's notice. The text has secrets removed and is cut short (see [Tokens](#tokens-name-and-the-redaction)
  and [Limits](#limits)).
- The tool calls are not shown in the thread: the agent sends only text, and marks no tool parts, so Agent Kourier draws
  no step cards for them ([Tool calls and results](../../../docs/reference/agent-contract.md#tool-calls-and-results)).

## What the agent can and cannot do

The wrapper enforces which tools the agent can call. It does not, and cannot, enforce what the model says.

- **The tool list is enforced; the model's words are not.** The agent can call the MCP tools that `allowed_tools`
  names, and `ask_user`, and nothing else. Pydantic AI offers the model only those tools, and a call to any other is
  answered as an unknown tool and never reaches the MCP server. Chat and alert text is untrusted, and a model can be
  talked into anything in words. In a hand run on 2026-10-17, a model (gemma4, on Ollama) told to restart a cluster
  called a `restart_cluster` tool it did not have, which never reached the server, and then replied "DONE". **Never trust the agent's claim that it did something.**
  Check what the tool or the system it acts on records.
- **A cancelled action may be redone on the next turn.** A cancel stops the run, but it cannot undo what an MCP tool
  already did, and the history the agent keeps holds the call. On the next turn the model may make the same call again,
  unasked: in the same hand run, a model (Claude Haiku) did exactly that with a slow tool cancelled in flight. Prefer read-only tools. For a tool with side
  effects, tell the agent in `instructions` to confirm with `ask_user` before it calls it, and have the MCP server
  refuse a repeat it should not run.
- **`allowed_tools` bounds the tools' names, not their arguments.** An allowed tool can be called with any arguments
  the model writes, including ones a message in the thread put there. The MCP server must authorize what each call
  does: which namespace, which object, which action. Give its token no more rights than the tools need.
- **The model provider sees everything in the thread.** Every turn, the history and the tool results go to the model
  provider that `model:` names.

## What you'll see in Slack

```text
#ops
  You            @Agent Kourier Why is checkout-db's disk filling up?
    thread       a reaction on your message while the agent works
    thread       a Working… card while the agent is silent
    thread       the answer streams in
  You            (in the thread) And what changed yesterday?
    thread       the answer to the follow-up, which sees the first turn
```

When the agent asks a question, the thread shows it, and your reply in the thread is the answer. The reaction is Agent
Kourier's default acknowledgment, and the card its default `chat.progress`.

## Prerequisites

- **Slack.** A Slack app with a bot token and an app-level token, and the bot invited to the channel:
  [Configure Slack](../../../docs/how-to/configure-slack.md). You need the channel's ID.
- **Agent Kourier, from a chart release that has custom resources,** in a namespace that can reach the agent's
  Service, on Kubernetes 1.33 or later. The [`docs-agent`](../docs-agent/README.md) recipe's step 3 says how to check the
  chart you install, and what to do on a release without them.
- **A model provider and a key.** See [Providers](#providers) for the ones that have been run. The model must support
  tool calls.
- **An MCP server** that serves the tools your agent may call over Streamable HTTP or SSE at an `http(s)` URL, and a
  token for it if it needs one. The example `agent.yaml` names `http://runbooks-mcp.ops.svc.cluster.local:8080/mcp`
  with two tools, `runbook` and `recent_deploys`: that server is not part of this recipe.
- **Docker** to build the agent's image: Agent Kourier's releases publish the broker's image and chart, not this
  agent's. And a way to get the image to your nodes (a registry, or
  `kind load docker-image` on a kind cluster).

## Files

```
kourier/          AgentBackend, ChatConnection and Binding: Agent Kourier's side
agent/
  agent.yaml      the example agent, in Pydantic AI's agent spec format
  agent.py        the wrapper: loads and polices agent.yaml, serves A2A, checks the bearer token
  Dockerfile      the image: agent.py, the example agent.yaml, the pinned packages
  requirements.in, requirements.txt   what the agent runs on, pinned with hashes
  manifests/      ConfigMap (your agent.yaml), Deployment, Service, NetworkPolicy
  test_agent.py, harness.py, mcp_server.py, conftest.py   the module's tests (see Checks)
```

The image holds Pydantic AI (pinned in `agent/requirements.in`) with the model clients for OpenAI, Anthropic and
Bedrock and the MCP client, on CPython 3.14, and runs as the non-root user 65532.

## Apply it

### 1. Build the image

From the repository root:

```sh
make prompt-agent-image
```

This builds `agent-kourier-prompt-agent:dev`. Agent Kourier's releases do not publish an image of the prompt agent, so you build it.
On a kind cluster, load it (`kind load docker-image agent-kourier-prompt-agent:dev`). On another cluster, push it to
your registry and set `image:` in `agent/manifests/deployment.yaml` to it, by digest
(`<registry>/...@sha256:...`), so that every node runs the image you built.

### 2. Write your agent

Edit `agent.yaml` in `agent/manifests/configmap.yaml`: the ConfigMap is what the pod reads, mounted at
`/etc/prompt-agent/agent.yaml`. [Write your agent.yaml](#write-your-agentyaml) has each field. Then, in
`agent/manifests/deployment.yaml`:

- Name the model key's variable after your provider: the Deployment sets `ANTHROPIC_API_KEY`, for the example's
  `anthropic:` model. That route, to Anthropic's own API, is not one that has been run; `openrouter:` is
  ([Providers](#providers)). For another provider, rename it ([Providers](#providers)) and add any base URL it needs as a
  plain `value`.
- For each `${NAME}` in your `agent.yaml`, set a variable `NAME` from a Secret. The Deployment sets
  `RUNBOOKS_MCP_TOKEN` from the Secret `prompt-agent-mcp`, for the example's MCP entry. Remove it if your MCP server
  takes no token.

Leave `PROMPT_AGENT_TOKEN_FILE` as it is (step 3).

### 3. Make the Secrets

All the agent's objects go in the namespace `prompt-agent`, which the AgentBackend's URL names. Three Secrets: two for
the agent, and the bearer token, which has a copy in each of two namespaces.

```sh
kubectl create namespace prompt-agent
kubectl create namespace agent-kourier   # where step 5 installs Agent Kourier; skip it if it exists
read -rs MODEL_API_KEY   # paste the model provider's key and press Enter; nothing is echoed
printf %s "$MODEL_API_KEY" | kubectl -n prompt-agent create secret generic prompt-agent-model --from-file=api-key=/dev/stdin
unset MODEL_API_KEY
read -rs MCP_TOKEN       # the MCP server's token
printf %s "$MCP_TOKEN" | kubectl -n prompt-agent create secret generic prompt-agent-mcp --from-file=token=/dev/stdin
unset MCP_TOKEN
```

The keys go in on standard input, so they are on no command line. The example's `agent.yaml` writes the header as
`Bearer ${RUNBOOKS_MCP_TOKEN}`, so the Secret holds the token alone.

**The agent fails closed.** It refuses to start unless `PROMPT_AGENT_TOKEN_FILE` names a file that holds a token, and
then answers 401 to any request without `Authorization: Bearer <that token>`. Agent Kourier sends that header from the
Binding's `identity.tokenSecretRef`. So one value goes in two Secrets, both named `prompt-agent-bearer` with the key
`token`: one in the namespace `agent-kourier`, which Agent Kourier reads, and one in `prompt-agent`, which the pod
mounts. Make the value once, and both Secrets from it, without printing it:

```sh
token="$(openssl rand -hex 32)"
kubectl create secret generic prompt-agent-bearer -n agent-kourier --from-literal=token="$token"
kubectl create secret generic prompt-agent-bearer -n prompt-agent --from-literal=token="$token"
unset token
```

`--from-literal` adds no trailing newline; Agent Kourier refuses a token with a control character, such as the newline
that `--from-file` keeps from a file. Agent Kourier reads a Secret only if the chart grants it: step 5 grants this
one with `rbac.secretNames`.

The agent reads the token file on every request, so a rotated token works without a restart; a mounted Secret reaches
the pod a little after Agent Kourier sees the change, so a few requests can get 401 meanwhile. The model key and the MCP
token are environment variables, so a changed value needs a restart of the pod. When the two copies differ, the agent
answers 401, and `kourier/binding.yaml` says what Agent Kourier then does.

To run the agent with no token at all, remove `PROMPT_AGENT_TOKEN_FILE` and set `PROMPT_AGENT_ALLOW_ANONYMOUS` to `1`
(exactly `1`; any other value does not count), and remove the `bearer` volume and its mount, which otherwise keep the
pod in `ContainerCreating` without the Secret. Any caller that reaches the pod can then read and add to every
conversation the agent holds, and spend your model key.

### 4. Apply the agent

```sh
kubectl apply -n prompt-agent -f examples/recipes/prompt-agent/agent/manifests/
kubectl -n prompt-agent rollout status deployment/prompt-agent
```

A pod that does not become ready has refused your `agent.yaml` or a variable: `kubectl -n prompt-agent logs
deploy/prompt-agent` names the file and what it refused ([What the agent refuses, and why](#what-the-agent-refuses-and-why)).
A missing Secret leaves the pod in `CreateContainerConfigError`, or `ContainerCreating` for the mounted bearer.

The Deployment has one replica, on purpose: the history lives in the pod's memory, and the Service does not pin a
thread to a pod (see [Limits](#limits)). The NetworkPolicy lets only Agent Kourier's pods (labelled
`app.kubernetes.io/name=agent-kourier`, in the namespace `agent-kourier`) reach the agent on 8080, where the network
plugin enforces NetworkPolicy. It leaves egress open, since the model provider's and the MCP servers' addresses are
yours; `agent/manifests/networkpolicy.yaml` has a commented egress list to close it.

### 5. Agent Kourier's resources

1. In `kourier/binding.yaml`, replace `CREPLACEPROMPT` with the channel's ID. The three resources are in the namespace
   `agent-kourier`; if yours is another, change `metadata.namespace` in all three, the namespace of the bearer Secret,
   the namespace in `agent/manifests/networkpolicy.yaml`, and the values below.
2. Make the Secret the ChatConnection names, `slack`, in the namespace `agent-kourier`, with the keys `botToken` and
   `appToken`. **These recipes call it `slack`, as the [Configure Slack](../../../docs/how-to/configure-slack.md) and
   [Install with Helm](../../../docs/how-to/install-with-helm.md) guides do.**

   ```sh
   kubectl -n agent-kourier create secret generic slack \
     --from-literal=botToken="$SLACK_BOT_TOKEN" \
     --from-literal=appToken="$SLACK_APP_TOKEN"
   ```

   Agent Kourier serves one ChatConnection; this recipe's `slack` is the same one the other recipes ship, so if you run
   several, they share it, and each Binding needs its own channel.
3. Apply the directory as custom resources, and install the chart to read them. These values replace the inline
   `config` of the values file in Install with Helm; the rest of that guide applies.

   ```yaml title="prompt-agent-values.yaml"
   config:
     source: crds
     chatConnection: agent-kourier/slack
   rbac:
     secretNames: {agent-kourier: [prompt-agent-bearer]}
   ```

   The chart grants the Secret `slack` from `config.chatConnection`. It cannot see the Secrets of resources you apply
   yourself, so `rbac.secretNames` grants the Binding's token, `prompt-agent-bearer`. Then, in this order:

   ```sh
   helm upgrade --install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
     -n agent-kourier -f prompt-agent-values.yaml --set replicaCount=0
   kubectl apply --dry-run=server -f examples/recipes/prompt-agent/kourier/
   kubectl apply -f examples/recipes/prompt-agent/kourier/
   helm upgrade --install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
     -n agent-kourier -f prompt-agent-values.yaml
   kubectl -n agent-kourier wait --for=condition=Ready akbinding/prompt-chat --timeout=120s
   ```

   The first `helm` command installs the CRDs with no broker running; the resources go in before the broker starts,
   because a broker that finds no ChatConnection does not start. If the cluster already has the CRDs from another
   release, add `crds.install: false` to the values. The `docs-agent` recipe's step 3 explains each command, and how to
   load the same files as a ConfigMap instead. A Binding that is not `Ready` says why in
   `kubectl -n agent-kourier get akbinding prompt-chat -o yaml`; [Binding status](../../../docs/reference/binding-status.md)
   explains each reason. With the two bearer copies different, its `BackendReachable` condition says
   `CredentialsRejected`.

Then mention the bot in the channel and ask a question your agent's tools can answer.

## What to change

- **The agent.** Everything about it is `agent.yaml`: see [Write your agent.yaml](#write-your-agentyaml). The agent
  reads it at start, so a change needs a restart of the pod
  (`kubectl -n prompt-agent rollout restart deployment/prompt-agent`). `agent/agent.yaml` and the copy in
  `agent/manifests/configmap.yaml` start equal, and this repository's tests keep them so; in your own copy, the
  ConfigMap is the one that counts.
- **The provider.** The `model:` line, the key's variable in the Deployment, and the egress if you close it.
- **The Binding.** The channel. `interactions.askUser: true` lets the agent's `ask_user` reach the thread: without it,
  Agent Kourier cancels the task when the agent asks and tells the thread so, so leave it on unless your
  `instructions` never ask. `chat.threadReplies: mention` makes only a reply that mentions the bot a turn, for threads
  where people talk to each other. `chat.botMentions: none` keeps other bots' mentions from starting turns. The names `prompt-agents`, `prompt-chat` and the namespace `prompt-agent` if they
  collide with yours. `agent.name` and `agent.namespace` are labels on the `a2a` dialect: the backend's URL alone
  selects the agent.
- **The AgentBackend.** Its `url` is the Service, `http://prompt-agent.prompt-agent.svc.cluster.local:8080`, with no
  path: the agent serves JSON-RPC at the root and its card at `/.well-known/agent-card.json`. Change it if the agent
  lives elsewhere.

## Checks

- `make prompt-agent-test` runs the module's tests (pytest, CPython 3.14; `PROMPT_PYTHON` names another interpreter).
  They drive the agent over A2A, in process, with a scripted model and a real MCP server over Streamable HTTP: streamed
  appends and `completed`, a second turn that sees the first, an MCP call that carries the expanded token with only the
  allowed tools offered, a disallowed call that never reaches the server, each refusal in
  [What the agent refuses](#what-the-agent-refuses-and-why), cancel during a slow MCP call, `ask_user` and its answer,
  a question cancelled while paused, the heartbeat, a repeated `messageId`, the bearer check and the body cap. They
  also check that the ConfigMap holds the example `agent.yaml`.
- `make prompt-agent-lint` runs ruff and audits the pinned runtime packages with pip-audit (it needs the network).
- `go test ./test/recipes` loads `kourier/` with Agent Kourier's loader and resolves its references.
- The recipe has no `evals/`: its Binding starts only from a mention, which the trigger evals do not judge.
- Nothing in this repository runs the agent against a live model, in a cluster or in Slack: those are yours to run.

## Limits

Each value below is a constant of `agent/agent.py` or of Pydantic AI, unless it says otherwise: only an edit of
`agent.py` changes it, never `agent.yaml` or a variable.

- **A `working` update every 120 seconds while a run goes on.** It is an interval, not a bound: nothing happens past
  it. It keeps a turn that waits on a long tool call inside the 5 minutes of silence the
  [agent contract](../../../docs/reference/agent-contract.md) allows.
- **1000 entries of each kind in memory:** thread histories, the `messageId`s of turns run, and paused questions. Past
  it, the entry written longest ago is forgotten: a thread whose history goes starts over, with no notice in the thread;
  a resent turn whose `messageId` went runs again; and an answer to a question that went does not resume the paused run,
  but runs as a new prompt in its thread.
- **A failed task's text is cut to 1000 characters.** Agent Kourier reads its first 4096 bytes, only to pick a
  failure category, and 1000 characters are at most 4000 bytes, so the cut only makes it shorter.
- **One question at a time.** When the model asks several questions in one response, only the first is posted. The
  answer to it resumes the run, and the others are answered "Ask one question at a time." A blank question is never
  posted: the model is told to ask one with words in it.
- **A thread's history is never trimmed.** A long thread grows until the model refuses it as longer than its context
  window. That turn fails, and a failed turn saves nothing, so every later turn of that thread fails the same way. A new
  thread starts empty.
- **Tasks are kept in memory until a restart, with no bound.** One task per turn, so a pod that runs long enough grows
  until it reaches its memory limit, 512Mi in `agent/manifests/deployment.yaml` (a setting you can raise). Past it the
  pod is killed: every thread starts over, and Agent Kourier reads a task it was following as the session gone.
- **`/tmp` holds at most 64Mi** (`sizeLimit` of its `emptyDir` in the Deployment, a setting). The libraries' temporary
  files go there; `agent.py` writes none. Past it the pod is evicted.
- **Everything is in memory.** A restart forgets every thread's history and paused question; the next reply in a thread
  starts over. One replica only: with two, a follow-up can reach the pod that has not seen the thread.
- **50 model responses per run** (Pydantic AI's `UsageLimits.request_limit`), counted from 0 on each turn and on the run
  that an answer resumes. Past it the task fails with `UsageLimitExceeded`. `agent.yaml` cannot change it.
- **MCP: 5 seconds to connect, 300 seconds for an answer** (Pydantic AI's defaults for its MCP client). A server that
  does not connect in time fails the task. A tool call with no answer in time is an error the model is told of, and it
  may call again, up to `retries`; then the task fails.
- **`tool_timeout` does not reach MCP calls.** Pydantic AI applies it to the agent's own function tools, and the only
  one is `ask_user`, which never waits. Setting it changes nothing.
- **A request body of at most 1 MiB.** Past it the agent answers 413 and reads no more. Agent Kourier's largest message,
  a person's turn, is far below it. A caller without the bearer token gets 401 before its body is read.
- **No limit of its own on runs.** A mention trigger takes no limits, so any person who can mention the bot in the
  channel can spend model quota; a bot's mention is held to the broker's fixed limits (see [What it does](#what-it-does)).
  Bound the key at the provider.
- **The redaction covers the failed text only.** The pod's logs are not redacted (see
  [Tokens](#tokens-name-and-the-redaction)).
- **A rotated MCP token or model key needs a restart**: they are environment variables, read once.

## Write your agent.yaml

`agent.yaml` is Pydantic AI's agent spec, read with `AgentSpec`. Only these keys are allowed; any other stops the
agent at start, since Pydantic AI would drop a misspelt or newer key without a word.

| Key | What it does here |
|---|---|
| `model` | Required. `provider:name`, of a provider in the table below. See [Providers](#providers). |
| `name`, `description` | The agent card's name and description; the name defaults to `prompt-agent`. |
| `instructions` | The prompt: a string, or a list of strings. `${NAME}` is not expanded here, so a token never reaches the model through it. But a string with `{{` in it is a Handlebars template to Pydantic AI, rendered with no data: `{{alertname}}` reaches the model as nothing, and one it cannot parse, such as `{{ .Values.image }}`, stops the agent at start with `HandlebarsParseError`. Write `\{{` for a literal `{{`. |
| `capabilities` | A list of `MCP` entries, and nothing else. See [MCP entries](#mcp-entries). |
| `model_settings` | Only the provider-neutral settings: `max_tokens`, `temperature`, `top_p`, `top_k`, `timeout`, `parallel_tool_calls`, `tool_choice`, `seed`, `presence_penalty`, `frequency_penalty`, `logit_bias`, `stop_sequences`, `thinking`, `cache`, `service_tier`. |
| `retries` | How many times Pydantic AI lets the model retry a tool call that failed, or an output it could not use (1 by default): an integer, or `{tools: n, output: n}`. |
| `end_strategy`, `metadata` | As Pydantic AI documents them. |
| `tool_timeout` | Allowed, and has no effect: see [Limits](#limits). |
| `output_schema`, `deps_schema` | Allowed, and have no effect: the wrapper sets the output itself (text, or a question) and passes no dependencies. Leave them out. |
| `$schema` | Allowed, for an editor's schema. |

### MCP entries

```yaml
capabilities:
  - MCP:
      url: http://runbooks-mcp.ops.svc.cluster.local:8080/mcp
      authorization_token: Bearer ${RUNBOOKS_MCP_TOKEN}
      allowed_tools: [runbook, recent_deploys]
```

| Keyword | |
|---|---|
| `url` | Required: `http://` or `https://`, with a host. The MCP client uses SSE for a URL whose path has an `/sse` segment, and Streamable HTTP otherwise. |
| `allowed_tools` | Required: a non-empty list of tool names. The agent can call these and no other tool of the server. |
| `authorization_token` | Sent as the `Authorization` header, verbatim: write the scheme too (`Bearer ${NAME}`). |
| `headers` | A mapping of header names to values, sent on every request to the server. |
| `id` | The entry's name. Pydantic AI names it after the URL's host and last path segment otherwise. Required for an IPv6 host. |
| `native` | Only `false`, the default. |
| `description` | Allowed; used only by native MCP, which is refused, so it has no effect. |

### Providers

The provider reads its key from the environment, not from `agent.yaml`. These are the providers that have been run with
this wrapper's approach: an `agent.yaml` that changed only the `model:` line, a pod-like environment with only that
provider's variables, and an MCP tool called through an authenticated server. They were run by hand on 2026-10-17,
with Pydantic AI 2.57.0; nothing in this repository runs them.

| `model:` | Variables | What was run |
|---|---|---|
| `openrouter:<vendor>/<model>` | `OPENROUTER_API_KEY` | OpenRouter, with an Anthropic and an OpenAI model. |
| `anthropic:<model>` | `ANTHROPIC_API_KEY` | Only against OpenRouter's Anthropic-compatible API, with `ANTHROPIC_BASE_URL`. Anthropic's own API has not been run. |
| `openai:<model>` | `OPENAI_API_KEY` | Only against OpenRouter's OpenAI-compatible API, with `OPENAI_BASE_URL`. OpenAI's own API has not been run. This is OpenAI's Responses API; `openai-responses:` is the same provider by another name. |
| `openai-chat:<model>` | `OPENAI_BASE_URL`, and `OPENAI_API_KEY` if the endpoint needs one | The Chat Completions API, for any OpenAI-compatible endpoint: run against OpenRouter and against Ollama's OpenAI API. |
| `ollama:<model>` | `OLLAMA_BASE_URL` (`http://<host>:11434/v1`) | A local Ollama. |
| `bedrock:<model id>` | `AWS_BEARER_TOKEN_BEDROCK` and `AWS_DEFAULT_REGION`, or AWS credentials | **Untested.** The provider reached Bedrock and got 403 for the key used: no Bedrock completion has run. |

The Anthropic and OpenAI clients follow `ANTHROPIC_BASE_URL` and `OPENAI_BASE_URL` when the environment has them, so
the Deployment sets only the variables your provider needs.

### Tokens: `${NAME}` and the redaction

- `${NAME}` is replaced with the environment variable `NAME`, so that no token is written in `agent.yaml`. It is
  expanded **only** in an MCP entry's `authorization_token` and `headers` values: anywhere else it is sent as written,
  and an expanded prompt would send the token to the model provider. `NAME` is letters, digits and `_`, not starting
  with a digit.
- A variable that is unset, empty, or holds a control character stops the agent at start, naming the file and the
  variable, never its value. The value is stripped of surrounding white space first, since a Secret made from a file
  often ends in a newline. A header that holds a control character or a character outside ASCII, written or expanded,
  stops the agent too.
- The values are read once, at start: a rotated token needs a restart of the pod.
- **The redaction.** A failed task's text can quote an error that quotes a header or a key, and any caller with the
  bearer token can read it with `GetTask`. So before it leaves the agent, the wrapper replaces with `[redacted]` every
  value that `${NAME}` expanded to, whatever its length. It also replaces the value of every environment variable whose
  name has `KEY`, `TOKEN`, `SECRET` or `PASSWORD` as a part between underscores (`ANTHROPIC_API_KEY`,
  `AWS_BEARER_TOKEN_BEDROCK`), when that value is 8 characters or more, so that a short word is not cut out of every
  text. Only a value quoted whole is found: a key quoted in part, such as with stars between its ends, stays as it
  is. Only that text is redacted: what uvicorn, the a2a-sdk and Pydantic AI write to the pod's log goes out as it is.

### What the agent refuses, and why

Chat and alert text is untrusted, so the agent may use only the MCP tools `agent.yaml` lists. Pydantic AI's spec format
offers much more, so the wrapper reads `agent.yaml` before Pydantic AI does, and stops at start, naming the file and
what it refused, on any of these (`load_agent` and `check_model` in `agent/agent.py`):

- **A key that is not a key of the spec.** Pydantic AI ignores a key it does not know, so `tools:` or a misspelling
  would be dropped without a word.
- **A capability other than `MCP`.** The spec offers a host shell and files (`LocalWorkspace`), web fetch and search,
  provider-side tools and more. A capability Pydantic AI adds in a later release is refused too.
- **A `model_settings` key outside the list above.** `extra_body` and `extra_headers` go to the provider as written, and
  `extra_body` can replace the request's tool list; a provider's own settings (`openai_native_tools`,
  `bedrock_additional_model_requests_fields`, ...) can add its web search or fetch.
- **A provider the image has no client for**, or a `model:` that is not `provider:name`.
- **A model name with a character other than ASCII letters, digits and `. _ / : ~ -`**, so that an escaped, a
  full-width or a look-alike character cannot hide a name from the next two checks.
- **A model that searches the web on the provider's side:** a name with `:online` (OpenRouter's web search),
  `search` (OpenAI's search and deep research models) or `perplexity` in it, in any case.
- **An OpenRouter preset**, an `@` anywhere in the name: a preset keeps settings in OpenRouter's dashboard, server
  tools among them, where `agent.yaml` cannot see them.
- **An MCP keyword outside the table above.** `local:` would connect to a URL of its own, with the token, instead of
  `url`; `defer_loading` is refused too.
- **An MCP `url` that is not `http(s)://host/...`**, or that ends in `.py` or `.js`. Given a path, the MCP client starts
  that script as a program over stdio.
- **An MCP entry without `allowed_tools`** as a non-empty list of names. Pydantic AI filters the tools only for a list:
  with none, or `null`, the agent would be offered every tool the server has.
- **`native:` anything but `false`.** Native MCP makes the model provider call the server itself, with the token.
- **An MCP `id` that is not a name without a colon, and an IPv6 URL without an `id`.** Pydantic AI refuses a
  capability id with a colon, and derives the id from the URL's host when there is none, so `http://[::1]:8080/mcp`
  needs an `id:`.
- **A malformed `authorization_token` or `headers`**: not a string, a header name that is not an HTTP token, or a value
  with a control character or a character outside ASCII.

**The model check is a heuristic.** It reads only the name. What a gateway does behind it, such as an endpoint that
`openai-chat:` or `openai:` reaches through `OPENAI_BASE_URL`, or the model that `openrouter:openrouter/auto` picks,
it cannot see. What the wrapper allows, it takes as written: list in `allowed_tools` only the tools the agent may call,
on a server you trust, and point a base URL only at an endpoint you trust not to add tools of its own.

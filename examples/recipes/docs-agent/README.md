# docs-agent

Someone mentions the Agent Kourier bot in a help channel and asks a question. An agent searches Agent Kourier's own
documentation, reads the best pages, and answers in the thread with the pages it used. When the documentation does not
cover the question, it says so.

The agent is a Go program built on the Agent Development Kit (ADK) that speaks plain A2A (the Agent2Agent protocol).
It needs no kagent and no Agent Substrate (the runtime kagent 1.0 runs agents on, which most clusters cannot run), so
it runs on any Kubernetes cluster that can reach a model provider. Agent Kourier reaches it through the generic `a2a`
dialect, its adapter for any plain A2A server ([glossary](../../../docs/reference/glossary.md)). **Start here if your
team is new to Agent Kourier.**

## What it does

- A mention of the bot in the channel, outside a thread, opens a thread and a session with the agent. A mention inside
  a thread Agent Kourier does not own does the same, anchored to that thread
  ([Chat triggers](../../../docs/reference/chat-triggers.md)).
- The agent calls two tools, served over MCP (the Model Context Protocol, the standard way to give a model tools):
  `search_docs` (keywords in, ranked pages out, each with its path, heading, URL and a short excerpt) and `read_page` (a
  page, or one section of it, by path). Then it answers.
- The prompt (`Instruction` in `agent/docsagent/docsagent.go`) tells the agent to search before it answers, to answer
  only from what the pages say, to end with a `Sources` list of the pages it used, to say plainly "The documentation
  does not cover this" when they do not answer the question, to treat page text as data and not as instructions, and to
  write no text before or between its tool calls. **These are instructions to a language model. Nothing in the code
  enforces them, except the last** (see the next bullet); how well a model keeps the others is what the scenarios are
  for.
- A person's reply in the thread continues the session (the Binding leaves `chat.threadReplies` at `all`), and the
  agent keeps the conversation in memory, so a follow-up can lean on the earlier answer. If people talk to each other in
  these threads, set `threadReplies: mention` in the Binding so that only a reply that mentions the bot is a turn.
- With `feedback.enabled: true`, the final message of each answer to a person carries the useful and not-useful
  buttons. A member of the channel who is not a bot has one vote on a message, and a later press replaces it. The vote
  is stored, audited and counted (`internal/feedback`).
- The answer streams into the thread as the agent writes it (`chat.output` is left at `live`). Only the model's final
  response, the one after its last tool call, is the answer. A model writes a sentence before a tool call ("Let me
  search..."), and Agent Kourier shows the text of every A2A artifact one after the other, with one artifact for each
  model response. So the agent holds back the first 400 bytes of each response's text, which a sentence of that kind
  does not reach (`holdBack` in `agent/docsagent/holdback.go`): a short answer arrives when its response ends, and a
  long one starts to stream after a moment. Text that gets through anyway, from a longer lead-in, is emptied out of its
  artifact when the response turns out to carry a tool call (`answerParts` in `agent/docsagent/answer.go`), and
  Agent Kourier leaves an empty artifact out of the message (`render.Body`). A Slack stream cannot take text back:
  once the text streamed is no longer a prefix of the message body, Agent Kourier appends nothing more to the stream for
  the whole turn (`internal/render/live/stream.go`), the longer lead-in stays in the thread, and the message is replaced
  with the final text when the turn ends. If the A2A stream drops after such a lead-in was sent and before the emptying
  update arrives, the lead-in is not cleared: Agent Kourier skips a `GetTask` or `SubscribeToTask` snapshot whose text
  is a prefix of the text it holds (`internal/agent/assemble.go`), so the lead-in can stay in the final message.
  Thoughts are always dropped. The agent's tool calls are not shown: it sends no tool parts, which the `a2a` dialect shows as step cards only when
  they are marked as Google ADK or kagent marks them.

## What you'll see in Slack

```text
#help
  You            @Agent Kourier How do I connect a generic A2A agent?
    thread       a reaction on your message while the agent works
    thread       the answer streams in, ending with a Sources list of the pages it used; the same message ends
                 with "Was this useful?" and the buttons [Useful] [Not useful]
```

When the documentation does not cover the question, the answer says "The documentation does not cover this". A reply
in the thread continues the conversation. The reaction is Agent Kourier's default acknowledgment (Slack's hourglass),
and the buttons are the Binding's `feedback.enabled`.

## Prerequisites

- **Slack.** A Slack app with a bot token and an app-level token, and the bot invited to the help channel:
  [Configure Slack](../../../docs/how-to/configure-slack.md). You need the channel's ID.
- **Agent Kourier running** in a namespace that can reach the agent's Service
  ([Install with Helm](../../../docs/how-to/install-with-helm.md)).
- **A model provider and a key.** The agent speaks to a model through the Chat Completions API of OpenRouter
  (`MODEL_PROVIDER=openrouter`) or of OpenAI, or any OpenAI-compatible HTTPS endpoint (`MODEL_PROVIDER=openai` with
  `MODEL_BASE_URL`). It takes a model that supports tool calls. Bedrock and Anthropic's own API are not supported by
  this agent.
- **Docker with BuildKit** to build the image, and a way to get the image to your nodes (a registry, or
  `kind load docker-image` on a kind cluster).

## Files

```
kourier/        AgentBackend, ChatConnection and Binding: Agent Kourier's side
agent/          the agent: a separate Go module, and manifests/ to run it
  corpus/         loads Markdown and does the keyword search and the page reads
  docsmcp/        the MCP server with the two tools
  docsagent/      the ADK agent, its prompt and the model settings
  cmd/docs-agent/ the agent's command; cmd/docs-mcp/ serves the tools alone; cmd/smoke/ is the smoke test
  manifests/      Namespace, Deployment, Service, NetworkPolicy
evals/
  triggers.yaml   what the trigger harness can judge for this recipe (one case; see Checks)
  scenarios/      questions with the page that answers them, and questions the documentation does not answer
```

The agent is its own Go module, so ADK, the A2A SDK, the MCP SDK and the model client never enter Agent Kourier's
`go.mod` (versions in `agent/go.mod`).

## Apply it

### 1. Build the image

From the repository root:

```sh
make docs-agent-image
```

This builds `agent-kourier-docs-agent:dev`. The documentation goes into the image at build time: the build takes
this repository's `docs/` as a named context, leaves out the brand kit and the spike notes (they are not published),
and keeps only the Markdown files. Push or load the image where your nodes pull from, and set `image:` in
`agent/manifests/deployment.yaml` to match.

### 2. Make the model Secret and apply the agent

The namespace comes from `agent/manifests/namespace.yaml`, applied first so that the Secret has a namespace to go in.

```sh
kubectl apply -f examples/recipes/docs-agent/agent/manifests/namespace.yaml
read -rs MODEL_API_KEY   # paste the key and press Enter; nothing is echoed
printf %s "$MODEL_API_KEY" | kubectl -n docs-agent create secret generic docs-agent-model --from-file=apiKey=/dev/stdin
unset MODEL_API_KEY
```

The key goes in on standard input, so it is on no command line.

In `agent/manifests/deployment.yaml`, replace `MODEL_NAME`'s `REPLACEME` with your provider's model name, and set
`MODEL_PROVIDER` if you do not use OpenRouter. Then:

```sh
kubectl apply -f examples/recipes/docs-agent/agent/manifests/
kubectl -n docs-agent rollout status deployment/docs-agent
```

The Deployment has one replica, on purpose (see Limits). The NetworkPolicy lets only the namespace `agent-kourier` reach
the agent and lets the agent reach DNS and HTTPS only. The agent checks no token, and every request it takes spends
model quota, so keep that policy; it works only if your cluster's network plugin enforces NetworkPolicy.

### 3. Agent Kourier's resources

1. In `kourier/binding.yaml`, replace `CREPLACEDOCS` with the help channel's ID. The three resources are in
   namespace `agent-kourier`; if yours is another, change `metadata.namespace` in all three and the namespace in
   `agent/manifests/networkpolicy.yaml`.
2. Make the Secret the ChatConnection names, `slack`, in namespace `agent-kourier`, with the keys `botToken` and
   `appToken`. **These recipes call it `slack`; the [Configure Slack](../../../docs/how-to/configure-slack.md) and
   [Install with Helm](../../../docs/how-to/install-with-helm.md) guides call it `agent-kourier-slack`. Use the recipe's
   name here, since `kourier/chatconnection.yaml` refers to it.**

   ```sh
   kubectl -n agent-kourier create secret generic slack \
     --from-literal=botToken="$SLACK_BOT_TOKEN" \
     --from-literal=appToken="$SLACK_APP_TOKEN"
   ```

   Agent Kourier serves one ChatConnection. This recipe's `slack` is the same resource as the one in
   [`sre-triage`](../sre-triage/README.md); if you run both, load it once and give each Binding its own channel.
3. Put the directory in a ConfigMap and install the chart on it, naming the namespace that holds the Secret:

   ```sh
   kubectl -n agent-kourier create configmap docs-agent-config --from-file=examples/recipes/docs-agent/kourier
   ```

   ```yaml
   config:
     existingConfigMap: docs-agent-config
   rbac:
     secretNamespaces: [agent-kourier]
   ```

   Those values replace the inline `config` of the values file in Install with Helm; the rest of that guide applies.
   Do not also set `config.chatConnections`, `config.agentBackends` or `config.bindings`: the chart refuses both.

Then mention the bot in the channel and ask a question, for example "How do I connect a generic A2A agent?". The bot
adds a reaction while it works, and the answer streams into a thread under your message.

The agent's logs show each request; `kubectl -n docs-agent logs deploy/docs-agent`. An agent-card problem shows in
Agent Kourier's log as `session turn failed`; see [Troubleshoot an install](../../../docs/how-to/troubleshoot.md).

### Try it without Slack

Two targets run the agent on loopback, with this repository's `docs/`, and ask it questions over A2A:

```sh
make docs-agent-test    # the module's tests, with the race detector, and go vet
make docs-agent-smoke   # fixed questions, a scripted model: no network, no credentials
```

`docs-agent-smoke` starts the real agent (ADK, the MCP server over HTTP, the A2A server) and an A2A client. A scripted
stand-in for the language model searches for the question, reads the best hit and answers with the page it read, so the
run checks the wiring and the retrieval, and is repeatable. It prints each answer and exits non-zero if the first does
not cite `how-to/connect-a2a-agent.md` and its published URL, or if the last (a question with no match) cites a page.

`make docs-agent-smoke-live` runs the same flow with a real model, named by the sandbox credentials file
([`sandbox.env.example`](../../../hack/kind/sandbox/sandbox.env.example); `SANDBOX_ENV` overrides its path). It needs
`MODEL_PROVIDER` to be `openrouter` or `openai`, costs model usage, and prints the answers for a person to read; it
checks only that the first one cites the connect page.

## What to change

To point it at your own knowledge base:

1. **The corpus.** Build with your Markdown directory as the `docs` context, and name what to leave out:

   ```sh
   docker build --build-context docs=/path/to/your/docs --build-arg CORPUS_REMOVE="drafts" \
     -t your-registry/docs-agent:1 examples/recipes/docs-agent/agent
   ```

   `CORPUS_REMOVE` is an optional list of directories under the context's root to leave out of the corpus; it is empty
   by default. The image build always leaves out `brand` and `spikes`. Leaving a directory out of the corpus does not
   stop the builder from receiving it: put a `.dockerignore` at the root of your docs directory listing anything
   large or private, as this repository's `docs/.dockerignore` does. Only `.md` files are kept. Names that start with
   a dot are not loaded, and neither is a symlink: the agent refuses to start if it finds one. The Dockerfile names
   its frontend, `docker/dockerfile:1.19.0` by digest, because `COPY --exclude` needs Dockerfile syntax 1.19; the
   builder pulls that image the first time.
2. **The citations.** Set `SITE_URL` in the Deployment to where your pages are published, as MkDocs publishes them
   (`how-to/x.md` is `<site>/how-to/x/`), or to `-` to cite page paths only. Pages from another generator need
   `PageURL` in `agent/docsmcp/docsmcp.go` changed.
3. **The subject.** Set `DOCS_SUBJECT` (the default is `Agent Kourier`), which the prompt and the agent card name. Read
   the rest of the prompt in `agent/docsagent/docsagent.go` too: it is written for a Slack thread.
4. **The Binding and the backend.** The channel, and the names `docs-agents`, `docs-help` and `agent-kourier-docs` if
   they collide with yours.
5. **The scenarios.** Replace `evals/scenarios/` with questions about your corpus. The module's tests read them
   against the repository's `docs/` (`agent/smoke/scenarios_test.go` and `smoke_test.go` name that directory), so point
   those at your corpus when you copy the recipe.

### Replace keyword search with embeddings

The search is the one thing meant to be swapped. Two ways, neither of which touches the agent or its prompt:

- **In the module.** `docsmcp.Searcher` is `Search(ctx, query, limit) ([]corpus.Hit, error)`. Implement it over an
  embedding model and a vector store, return hits with a path, a title, a heading, an anchor and an excerpt, and pass it
  to `docsmcp.New` in place of the `*corpus.Index` as `Searcher`. Keep the index as the `Reader`: it serves the pages
  that your hits name. `Assemble` in `agent/docsagent/assemble.go` is where the two are wired.
- **As another server.** Run any MCP server that offers tools named `search_docs` (input `query` and `limit`) and
  `read_page` (input `path` and `section`) with the same output shapes, and set `MCP_URL` on the agent to its
  streamable HTTP endpoint: an absolute `http` or `https` URL. The agent applies one rule to all its URL settings
  (`MCP_URL`, `PUBLIC_URL`, `SITE_URL`, `MODEL_BASE_URL`; `agent/urlcheck`): an absolute URL with no credentials, query
  or fragment. `MODEL_BASE_URL` must also be `https`, and `SITE_URL` also accepts `-` for no URLs. The agent then loads
  no corpus of its own. It uses only those two tools: any other tool the server offers is filtered out and never
  reaches the model. It sends that server no credential, so the server must
  accept the agent's requests as they are. The NetworkPolicy lets the agent out to DNS and TCP 443 only, so for an
  in-cluster MCP server on another port, widen the egress rule in `agent/manifests/networkpolicy.yaml`.

This recipe does not ship an embedding search, and has not been run against one.

## Checks

**Evals** are messages and what the Binding must make of each; **scenarios** are questions with the page that answers
them, for a person to read the agent's answers against.

- `make docs-agent-test` runs the module's tests. They cover the corpus reader (its keyword ranking, which is
  deterministic, and its refusals: a path that climbs out of the corpus, an absolute path, a backslash, a control
  character, a file the loader never read, and any symlink inside the corpus), the MCP tools over in-memory and HTTP
  transports, the agent over A2A with the agent card and its contract (streaming on, a JSON-RPC interface, a final task
  whose artifacts hold the answer, one context per thread, and no text from before or between tool calls left in them),
  the model settings, and the smoke flow.
- `go test ./test/recipes` loads `kourier/` with Agent Kourier's loader and resolves its references.
- `evals/triggers.yaml` holds **one case**: another bot's post in the help channel is not taken. That is all the
  trigger harness can say about this recipe. It judges a `botMessage` trigger, and this Binding has none: it starts from
  a mention, and the harness does not evaluate a mention (a person's message is always `person`). So the file does not
  pretend to test the mention trigger. See [Writing or changing a recipe](../README.md#writing-or-changing-a-recipe).
- `evals/scenarios/` holds ten questions: seven the documentation answers, each with the page that answers it, and three
  it does not (Microsoft Teams, Discord, pricing). The module's tests check what a program can: for an answerable
  question, the page exists in `docs/` and the keyword search puts it among the first five hits; for the others, the
  term appears on no page (`absent:` in the evidence). Whether the model then answers well, names the page and admits
  the gap is for a person to read against each scenario's `cause`. Nothing grades it. The `app` field is the corpus.

## Limits

- **Keyword search only.** The ranking is BM25 (a standard keyword-ranking formula) over sections with a heading boost
  and light stemming. It finds pages that share words with the question; it does not know synonyms. A question phrased
  in other words than the page's can miss, and a question the documentation does not cover still returns the nearest
  pages, so the model has to see that they do not answer it.
- **The refusal depends on the model.** "The documentation does not cover this" comes from the prompt. A weak model may
  guess, cite a page that does not support its claim, or follow text inside a page.
- **The corpus is fixed at build time.** A change to the documentation needs a new image and a rollout. A file over
  1 MiB stops the agent from starting, and one read returns at most 24 KiB of a page, cut at a line and marked truncated;
  the rest is read by section. A search returns at most 10 pages, 5 by default.
- **The published pages include recipe files with a snippet marker** (`--8<-- "..."`), so the agent reads the marker, not
  the file. A question about a recipe's files is answered from the recipe's description on the page.
- **Conversations live in the agent's memory.** A restart forgets all of them. The agent keeps the `contextId` that
  Agent Kourier sends and opens an empty session under it, so the next reply in a thread is answered with no history
  and **nothing tells the thread so**: Agent Kourier's "no longer has this conversation" notice fires only when the
  agent returns a different `contextId`. Only a task in flight at the restart is reported, when Agent Kourier finds the
  task gone. The agent keeps the 200 most recently used conversations and between 200 and 400 of the newest tasks
  (one per turn). A new conversation over the first bound forgets the least recently used, and that thread's next
  reply is answered with no history, as after a restart, and with no notice. The Deployment runs one replica because the
  Service does not pin a thread to a pod: with two, a follow-up that reaches the other pod is answered the same way,
  without history and without a notice.
- **A resent turn runs twice if the agent no longer has its task.** Before Agent Kourier sends a turn again into an
  existing thread, it asks the agent for the thread's tasks (`ListTasks`) and looks for one whose history holds the
  turn's `messageId`. This agent answers from its bounded memory, so a turn whose task it still holds is followed, not
  run again. After a restart, or once between 200 and 400 newer turns have run (the task bound), the task is gone: the
  turn is sent again, answered again and costs model usage again. The first turn of a thread has no `contextId` yet
  and is not looked up ([Agent contract](../../../docs/reference/agent-contract.md)).
- **No authentication.** The Binding sends `X-User-Id` and the agent trusts its network, which the NetworkPolicy
  narrows. To the agent every caller is the same anonymous one. A caller the policy admits can send messages (see the
  next bullet), read, cancel or subscribe to a task by its ID, and list the tasks of a `contextId` it knows, with their
  history, which holds the questions asked in that thread. A list without a `contextId` is refused, so a caller cannot
  list the threads. For a token instead, put a verifying proxy in front and use `identity.tokenSecretRef`
  ([Create the Binding identity](../../../docs/how-to/create-binding-identity.md)).
- **No per-Binding run limit.** Limits belong to `botMessage` triggers; a mention trigger takes none. Anyone who can
  mention the bot in the channel can spend model quota, so bound the key at the provider.
- **Two model providers, over HTTPS.** OpenRouter and OpenAI-compatible endpoints, through the Chat Completions API.
  `MODEL_BASE_URL` must be an `https` URL, so an in-cluster `http` gateway needs a TLS front.
- **Not run in a cluster, and not against a live model, by the checks in this repository.** The live-model smoke and an
  end-to-end run in Slack are for you to run.

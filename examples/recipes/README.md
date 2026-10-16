# Recipes

A recipe is a worked example of one use case: the Agent Kourier resources that connect a Slack channel to an agent, the
agent they talk to (for two of the three), and checks that show the mapping does what the recipe says.

**A new team should start with [`docs-agent`](docs-agent/README.md).** It needs a model key and any cluster, and it
shows the whole path: a mention in a channel, an agent, an answer in the thread.

## Pick a recipe

| Recipe | Starts from | You need | Runs on |
|---|---|---|---|
| [`docs-agent`](docs-agent/README.md) | An `@mention` in a help channel. A mention trigger, thread replies and feedback buttons. | A model provider's key, Docker to build the image, and Slack. The agent is an ADK (Agent Development Kit) program that speaks plain A2A (the Agent2Agent protocol) and searches a Markdown corpus through an MCP tool, citing the pages it used. | Any Kubernetes cluster that can reach the model provider. |
| [`custom-bot-trigger`](custom-bot-trigger/README.md) | Another bot's post, such as a failed CI run. A hand-written `botMessage` trigger: `match`, `exclude`, `extract`, `thread` with a cooldown and `closeWhen`, `limits` and `promptTemplate`. | Your own A2A agent, and Slack. The recipe ships Agent Kourier resources only. Its `identity.userId` works only if your agent trusts the `X-User-Id` header. | Wherever your agent is. |
| [`sre-triage`](sre-triage/README.md) | An Alertmanager alert posted to Slack. A `botMessage` trigger written in full for Alertmanager's posts, one thread per alert group, and a read-only kagent agent that reads the cluster and can ask a question. | For the agent half: kagent 1.0.0-alpha5, Agent Substrate, a verifying front door and an OIDC token. Plus an Alertmanager Slack receiver, a model, and Slack. | The agent half runs only on this repository's kind full tier (a local cluster; `make e2e-full-up`). The Agent Kourier resources work with any agent. |

The [glossary](../../docs/reference/glossary.md) defines kind full tier, Agent Substrate, front door, dialect and A2A.
Each recipe's README defines them again where it first uses them.

All three also need a Slack app with a bot token and an app-level token, and the bot invited to the channel:
[Configure Slack](../../docs/how-to/configure-slack.md). The recipes call the Slack Secret `slack`, not
`agent-kourier-slack` as that guide and [Install with Helm](../../docs/how-to/install-with-helm.md) do; each recipe's
Secret step says so.

## Placeholders

A recipe's files load as they are. Where a value has to be yours, the file holds a placeholder that still passes the
resource's validation. Replace each one before you apply the recipe:

| Placeholder | Where | Replace it with |
|---|---|---|
| `CREPLACESRE`, `CREPLACECI`, `CREPLACEDOCS` | the channel in each recipe's `kourier/binding.yaml` | The channel's ID (a channel ID must match `^[CG][A-Z0-9]+$`, so the placeholder starts with `CREPLACE` and ends with the recipe). Each recipe has its own, so that recipes applied together never put two Bindings on one channel. |
| `B0REPLACEME` | `sre-triage` and `custom-bot-trigger`: `from.id` in the Binding, and the `bot` of the cases in `evals/triggers.yaml` | The `bot_id` of the bot whose posts the trigger reads. The dry run in the recipe's README prints it. |
| `REPLACE-ME` | `custom-bot-trigger`: the host in the AgentBackend's `url` (`http://REPLACE-ME.agents.svc.cluster.local:8080`) | Your A2A agent's URL. |

Other values are meant to be changed too (the receiver in `sre-triage`, the regexes in `custom-bot-trigger`, the model
name in `docs-agent`); each recipe's "What to change" lists them.

## Applying several recipes

Agent Kourier reads one config directory and serves exactly one ChatConnection (it refuses to start with more). To run
more than one recipe, copy each recipe's `kourier/` files into that one directory under names that do not collide
(`sre-triage-binding.yaml`, not `binding.yaml` twice). Then:

- **Keep one ChatConnection.** Every recipe ships the same one: `slack` in the namespace `agent-kourier`, whose Secret
  `slack` holds the keys `botToken` and `appToken`. Two copies make two documents with one identity, and the load
  refuses the duplicate (`duplicate of <file>#<n>`), so delete all but one. You create the Secret once.
- **One channel each.** A channel of a ChatConnection belongs to at most one Binding. A second Binding for the same
  connection and channel fails the resolve with `<channel> on ChatConnection <namespace>/<name> is already bound by
  Binding <namespace>/<name>`. Each recipe ships its own channel placeholder, so replace each with a channel of its own,
  and invite the bot to each channel.
- **The other objects are named apart.** No two recipes use the same kind, namespace and name.

At start-up any of these problems stops Agent Kourier. While it runs, a reload with a load or resolve problem keeps the
last good config and marks it stale. If you want a ChatConnection of another name or another Secret, change it in every
recipe you apply.

## Writing or changing a recipe

This part is for people who edit a recipe or add one. You do not need it to apply one.

### Layout

```
examples/recipes/
  README.md          this page
  <recipe>/
    README.md        what the recipe does, how to apply it, what to change; sections in this order:
                     What it does / What you'll see in Slack / Prerequisites / Files / Apply it / What to change /
                     Checks / Limits, then any reference appendix
    kourier/         the Agent Kourier resources: AgentBackend, ChatConnection, Binding
    agent/           the agent the Binding talks to
    evals/
      triggers.yaml  messages and what the Binding must make of each
      scenarios/     quality cases for the agent, as data for a person to read an answer against; nothing runs them yet
```

`kourier/` and `agent/` are separate because Agent Kourier loads every `*.yaml` and `*.yml` file directly inside its
config directory (subdirectories are not read) and refuses any document whose `apiVersion` is not
`agentkourier.dev/v1alpha1` or whose `kind` is not `AgentBackend`, `ChatConnection` or `Binding`. A kagent `Agent`, a
`RemoteMCPServer`, a `Role` or a Deployment in that directory makes the whole load fail. So the files you give to
Agent Kourier live in `kourier/`, and everything the agent needs lives in `agent/` and is applied by its own tooling.

A Secret is not one of the three kinds either, so a recipe cannot ship one. Its README names the Secrets the resources
refer to, and you create them yourself.

### What the tests check

`go test ./test/recipes` loads the `kourier/` directory of every recipe with the loader Agent Kourier itself uses, and
then resolves it with the same reference check. Each Binding's `agent.backendRef` and `chat.connectionRef` must name
an AgentBackend and a ChatConnection in that directory. With no namespace in the reference, the object must be in the
Binding's namespace; with another namespace, the object must list the Binding's namespace in `allowedNamespaces`. The
test fails, naming the file or directory, for a field the resources do not have, a document of another `apiVersion` or
`kind`, a reference that resolves to nothing, a recipe without a `kourier/` directory, or a `kourier/` directory with no
Binding. It does not check the Secrets, since you create them, and it does not check anything in `agent/` (a recipe's
own tests may: `go test ./test/kindmanifests` pins the manifests in `sre-triage/agent/`).

A recipe you add follows the rules above: the `slack` ChatConnection as it is, names no other recipe uses, and a channel
placeholder of its own. The test also copies every recipe's files into one directory, keeping one copy of a
ChatConnection that is identical in several (the decoded objects are compared, so comments and layout do not matter),
then loads and resolves them and has the broker choose its one ChatConnection. It fails on a different ChatConnection
under the same name, a second ChatConnection, any other duplicate, and two Bindings on one channel.

### Trigger evals

An eval is a message and what the Binding must make of it. A recipe whose Binding has a `botMessage` trigger can pin
what that trigger takes in `evals/triggers.yaml`. The test builds the Binding from the recipe's `kourier/` directory,
with the loader's defaults applied, and hands each case's message to the same matcher the intake uses for a live message
(the one `agent-kourier dry-run` replays history through). A case whose result differs from `expect` fails the test; the
failure names the recipe, the case, what was expected and what came out, and, for a message of a named bot that no
trigger took, each trigger and the field whose regex turned it away (or the extract that found nothing). A recipe
without the file is not tested for this, and the test notes it in its verbose output.

```yaml
cases:
  - name: a firing alert
    binding: recipe/alerts          # namespace/name of a Binding in kourier/
    message:
      bot: B0REPLACEME              # the poster's bot ID; leave out for a person's message
      title: 'FIRING (1) - KubePodCrashLooping'
      link: 'https://alertmanager.example.com/#/alerts?receiver=slack-critical'
      fallback: '[FIRING:1] KubePodCrashLooping shop (critical) | https://alertmanager.example.com/#/alerts?receiver=slack-critical'
    expect:
      outcome: matched
      trigger: 0                    # zero-based index in the Binding's chat.triggers: 0 is the first trigger
      fields: { state: FIRING, alertname: KubePodCrashLooping, key: KubePodCrashLooping shop }
```

- A bot's message can set `text`, `title`, `link`, `fallback`, `footer` and `body`, the fields a trigger's regexes read,
  and `inThread: true` for a reply inside a thread. It needs at least one of the six: the Slack adapter drops a bot's
  message with none before any trigger sees it. A person's message sets `user` and `text` and needs both, for the
  adapter drops a person's message that lacks either.
- Each value is the raw string from the message's JSON as Slack delivers it, not what the Slack client displays: `text`
  is the message's top-level `text`, and the other five are the `title`, `title_link`, `fallback`, `footer` and `text`
  (the `body`) of the message's first legacy attachment (the others are not read). The adapter neither renders nor
  unescapes them. Copy the values from a real message's JSON, for example a `conversations.history` response, rather
  than from the screen.
- `outcome` is one of `matched` (a trigger takes the message), `unmatched` (a bot the Binding names, but no trigger takes
  the message), `excluded` (all the match regexes of a trigger matched and its exclude regex turned the message away),
  `other_bot` (no trigger names the bot), `person` (a person's message, which no `botMessage` trigger sees) and `ignored`
  (a reply inside a thread by a bot the Binding names).
- `trigger` and `fields` apply to `matched` only. `trigger` is the zero-based index in the Binding's `chat.triggers`, so
  the recipes that list their `botMessage` trigger first use `trigger: 0`. `fields`, when given, must equal the
  extracted values exactly.
- A key the format does not have, a repeated key or a case name used twice fails the test, as does a Binding that is not
  in `kourier/`.

These evals judge one message at a time. They do not evaluate the thread cooldown, thread keys across messages (a case
sees the `key` it extracts, not whether a second message joins the first one's thread), the rate limit, the daily cap or
`closeWhen`, and they do not evaluate a `mention` trigger: a person's message is always `person`. Those are covered by
the tests of the intake itself.

### Scenarios

`evals/scenarios/*.yaml` holds quality cases for the agent. Each file is one scenario, as data for a person to read an
agent's answer against; nothing runs them yet, so their shape may change. `go test ./test/recipes` checks every
scenario file of every recipe for that shape:

```yaml
name: checkout crashes at start   # what the case is about
app: checkout                     # the workload it is about
alert: KubePodCrashLooping        # what starts the case: an alert name, or `question:` with a person's question; exactly one
cause: '...'                      # what a good answer must name
evidence:                         # what a good answer cites
  - '...'
```

The test fails, naming the file, for a field the shape does not have, a missing or blank `name`, `app`, `cause` or
`evidence`, a blank evidence item, or neither or both of `alert` and `question`. A recipe without scenarios is not tested
for this. What a field refers to is for the recipe's own tests: `go test ./test/kindmanifests` requires `sre-triage` to
have exactly one scenario for each Deployment of its `faults.yaml`, and each of its alerts to be one the sandbox fires
for that app.

## Planned recipes

None of these exists yet.

| Recipe | Starts from | Shows |
|---|---|---|
| `sre-remediation` | A follow-up in a triage thread | Per-tool-call approval with approver groups |

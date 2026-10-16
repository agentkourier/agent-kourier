# Recipes

A recipe is a worked example of one use case: the Agent Kourier resources that connect a Slack channel to an
agent, the agent they talk to, and checks that show the mapping does what the recipe says.

This page lists the recipes and fixes the layout every recipe follows.

## Recipes

| Recipe | Starts from | Shows |
|---|---|---|
| [`sre-triage`](sre-triage/README.md) | An Alertmanager alert posted to Slack | A `botMessage` trigger written in full for Alertmanager's posts, alert threads keyed by alert, and a read-only kagent agent that reads the cluster and can ask a question |
| [`custom-bot-trigger`](custom-bot-trigger/README.md) | Another bot's post, such as a failed CI run | A hand-written `botMessage` trigger: `match`, `exclude`, `extract`, `thread` with a cooldown and `closeWhen`, `limits` and `promptTemplate`. Agent Kourier resources only; bring your own agent |

## Layout

```
examples/recipes/
  README.md          this page
  <recipe>/
    README.md        what the recipe does, how to apply it, what to change
    kourier/         the Agent Kourier resources: AgentBackend, ChatConnection, Binding
    agent/           the agent the Binding talks to
    evals/
      triggers.yaml  messages and what the Binding must make of each
      scenarios/     quality cases for the agent, as data for a person to read an answer against; nothing runs them yet
```

### Why `kourier/` and `agent/` are separate

Agent Kourier reads its resources from one directory. It loads every `*.yaml` and `*.yml` file directly inside that
directory (subdirectories are not read) and refuses any document whose `apiVersion` is not
`agentkourier.dev/v1alpha1` or whose `kind` is not `AgentBackend`, `ChatConnection` or `Binding`. A kagent `Agent`, a
`RemoteMCPServer`, a `Role` or a Deployment in that directory makes the whole load fail. So the files you give to
Agent Kourier live in `kourier/`, and everything the agent needs lives in `agent/` and is applied by its own tooling.

A Secret is not one of the three kinds either, so a recipe cannot ship one. Its README names the Secrets the
resources refer to, and you create them yourself.

### Placeholders

A recipe's files load as they are. Where a value has to be yours, such as a Slack channel ID, the file holds a
placeholder that still passes the resource's validation (a channel ID must match `^[CG][A-Z0-9]+$`, so the
placeholder is `CREPLACEME`). Replace it before you apply the recipe.

## Checked on every test run

`go test ./test/recipes` loads the `kourier/` directory of every recipe with the loader Agent Kourier itself uses, and
then resolves it with the same reference check. Each Binding's `agent.backendRef` and `chat.connectionRef` must name
an AgentBackend and a ChatConnection in that directory. With no namespace in the reference, the object must be in the
Binding's namespace; with another namespace, the object must list the Binding's namespace in `allowedNamespaces`. The
test fails, naming the file or directory, for a field the resources do not have, a document of another `apiVersion`
or `kind`, a reference that resolves to nothing, a recipe without a `kourier/` directory, or a `kourier/` directory
with no Binding. It does not check the Secrets, since you create them, and it does not check anything in `agent/` (a recipe's own
tests may: `go test ./test/kindmanifests` pins the manifests in `sre-triage/agent/`).

### Trigger evals

A recipe whose Binding has a `botMessage` trigger can pin what that trigger takes in `evals/triggers.yaml`. The test
builds the Binding from the recipe's `kourier/` directory, with the loader's defaults applied, and hands each case's
message to the same matcher the intake uses for a live message (the one `agent-kourier dry-run` replays history
through). A case whose result differs from `expect` fails the test; the failure names the recipe, the case, what was
expected and what came out, and, for a message of a named bot that no trigger took, each trigger and the field whose
regex turned it away (or the extract that found nothing). A recipe without the file is not tested for this, and the
test notes it in its verbose output.

```yaml
cases:
  - name: a firing alert
    binding: recipe/alerts          # namespace/name of a Binding in kourier/
    message:
      bot: B0REPLACEME              # the poster's bot ID; leave out for a person's message
      title: 'FIRING (1) - KubePodCrashLooping'
      titleLink: 'https://alertmanager.example.com/#/alerts?receiver=slack-critical'
      fallback: '[FIRING:1] KubePodCrashLooping shop (critical) | https://alertmanager.example.com/#/alerts?receiver=slack-critical'
    expect:
      outcome: matched
      trigger: 1                    # index in the Binding's chat.triggers
      fields: { state: FIRING, alertname: KubePodCrashLooping, key: KubePodCrashLooping shop }
```

- A bot's message can set `text`, `title`, `titleLink`, `fallback`, `footer` and `attachmentText`, the fields a trigger's
  regexes read, and `inThread: true` for a reply inside a thread. It needs at least one of the six: the Slack adapter
  drops a bot's message with none before any trigger sees it. A person's message sets `user` and `text` and needs
  both, for the adapter drops a person's message that lacks either.
- Each value is the raw string from the message's JSON as Slack delivers it, not what the Slack client displays: `text`
  is the message's own `text`, and the other five are the `title`, `title_link`, `fallback`, `footer` and `text` of the
  message's first legacy attachment (the others are not read). The adapter neither renders nor unescapes them. Copy the
  values from a real message's JSON, for example a `conversations.history` response, rather than from the screen.
- `outcome` is one of `matched` (a trigger takes the message), `unmatched` (a bot the Binding names, but no trigger
  takes the message), `excluded` (all the match regexes of a trigger matched and its exclude regex turned the message
  away), `other_bot` (no trigger names the bot), `person` (a person's message, which no `botMessage` trigger sees) and
  `ignored` (a reply inside a thread by a bot the Binding names).
- `trigger` and `fields` apply to `matched` only. `fields`, when given, must equal the extracted values exactly.
- A key the format does not have, a repeated key or a case name used twice fails the test, as does a Binding that is
  not in `kourier/`.

These evals judge one message at a time. They do not evaluate the thread cooldown, thread keys across messages (a case
sees the `key` it extracts, not whether a second message joins the first one's thread), the rate limit, the daily cap
or `closeWhen`, and they do not evaluate a `mention` trigger: a person's message is always `person`. Those are
covered by the tests of the intake itself.

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

The test fails, naming the file, for a field the shape does not have, a missing or blank `name`, `app`, `cause` or `evidence`, a
blank evidence item, or neither or both of `alert` and `question`. A recipe without scenarios is not tested for this.
What a field refers to is for the recipe's own tests: `go test ./test/kindmanifests` requires `sre-triage` to have
exactly one scenario for each Deployment of its `faults.yaml`, and each of its alerts to be one the sandbox fires for that app.

## Planned recipes

None of these exists yet.

| Recipe | Starts from | Shows |
|---|---|---|
| `docs-agent` | An `@mention` in a help channel | A mention trigger, thread replies, feedback buttons, and an agent that does not need kagent |
| `sre-remediation` | A follow-up in a triage thread | Per-tool-call approval with approver groups |

Two Bindings can share one ChatConnection.

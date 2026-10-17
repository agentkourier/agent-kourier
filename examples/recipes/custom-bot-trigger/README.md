# custom-bot-trigger

A CI system posts a message to a Slack channel when a pipeline fails. This recipe turns each failure into an agent
investigation in its own thread, using a `botMessage` trigger (a rule that reads another bot's posts) written by hand.

It holds the Agent Kourier side only: a Binding, the AgentBackend and ChatConnection it refers to, and trigger evals. It
ships no agent. Point the Binding at any A2A agent you run (A2A is the Agent2Agent protocol; see
[Connect a generic A2A agent](../../../docs/how-to/connect-a2a-agent.md)).

## What it does

The recipe reads a post that it made up, which no real product posts:

```text
title:     Pipeline FAILED - web-build
fallback:  [FAILED] web-build on main (#1423) | https://ci.example.com/pipelines/web-build/runs/1423
body:      Stage test failed after 4m12s. 3 tests failed.
```

A success reads `Pipeline PASSED - web-build` and `[PASSED] web-build on main (#1430) | ...`. **Replace every regex in
`kourier/binding.yaml` with ones written against your own bot's messages** (Apply it, step 2).

The Binding, `kourier/binding.yaml`, names the CI bot, picks these posts out with regexes on the title and fallback,
extracts a state, a pipeline and a key from them, gives each pipeline-and-branch key a thread, and bounds how many
investigations start. A failure whose body says a retry is scheduled, or `known flaky`, starts nothing. At most 3
investigations start in any 15 minutes, 1 runs at once and 20 start in a day.

## What you'll see in Slack

What happens to a post from the CI bot in the channel:

| The post | What happens in Slack |
|---|---|
| A `FAILED` post for a pipeline and branch with no thread | A new thread under the post, and the agent's investigation streams into it. |
| The same pipeline and branch fails again, while its thread is open or soon after it closed | A `Fired again at <time>` note in that thread. No new investigation. |
| A `PASSED` post for that pipeline and branch | A `Resolved at <time>` note, and the thread closes. |
| A failure a limit stops | Nothing in its thread. Once per window, one line in the channel: "2 alert messages not investigated in the last 15m (rate limit)." |

The count line says "alert messages" for any `botMessage` trigger, whatever the bot posts.

To see it work without waiting for a real failure, post the made-up message through a Slack incoming webhook. Create the
webhook in the Binding's channel (a webhook posts into the one channel you give it). The trigger accepts only the bot in
`from.id`, so for the test `from.id` must be the webhook's bot ID: post once, read the `SENDER` of that post in the dry
run (Apply it, step 3), set `from.id` to it, and update the ConfigMap (below). **Set `from.id` back to your CI bot's ID
afterwards, and update the ConfigMap again.** Slack's behaviour with attachments is not checked by this repository.

```sh
kubectl -n agent-kourier create configmap ci-failures-config \
  --from-file=examples/recipes/custom-bot-trigger/kourier --dry-run=client -o yaml | kubectl apply -f -
```

The pod sees a change to a mounted ConfigMap only after the kubelet syncs it (Kubernetes behaviour, not checked here).
Agent Kourier then reloads the changed files by itself: it checks the config directory every 10 seconds
(`internal/config/reload.go`) and keeps the last good config if the new one does not load. The chart's
`config.rollOnChange` does not apply to a ConfigMap you manage (`config.existingConfigMap`), so the pod is not
restarted. A reload cannot add a Secret namespace; that needs a restart.

```sh
curl -X POST -H 'Content-Type: application/json' "$SLACK_WEBHOOK_URL" --data '{"attachments":[{
  "title":"Pipeline FAILED - web-build",
  "title_link":"https://ci.example.com/pipelines/web-build/runs/1423",
  "fallback":"[FAILED] web-build on main (#1423) | https://ci.example.com/pipelines/web-build/runs/1423",
  "text":"Stage test failed after 4m12s. 3 tests failed."}]}'
```

Post it twice for the "Fired again" note. Post `Pipeline PASSED - web-build` with the fallback
`[PASSED] web-build on main (#1430) | https://ci.example.com/pipelines/web-build/runs/1430` for the close.

## Prerequisites

- **Slack.** A Slack app with a bot token and an app-level token, and the bot invited to the channel:
  [Configure Slack](../../../docs/how-to/configure-slack.md). You need the channel's ID and the `bot_id` of the CI bot's
  posts (it starts with `B`; it is not the Slack app ID, which starts with `A`).
- **An A2A agent you run, and its URL.** The AgentBackend's `url` is a placeholder (`REPLACE-ME`). The agent has to
  stream. For kagent, set `dialect: kagent-v1` and its base URL as in
  [Connect a kagent agent](../../../docs/how-to/connect-kagent.md).
- **An agent that trusts `X-User-Id`, or a token.** The Binding's `identity.userId` is sent to the agent as the
  `X-User-Id` header, with no proof behind it. It works only if the agent, or a proxy in front of it, trusts that
  header. If your agent or its front door (a proxy in front of the agent that checks the bearer token) verifies tokens,
  use `identity.tokenSecretRef` instead: [Create the Binding identity](../../../docs/how-to/create-binding-identity.md).
- **To run the dry run (step 3): Go and a checkout, or Docker instead of Go.** The dry run is a subcommand of the
  `agent-kourier` binary. Run it with Go at the version in `go.mod`, from a checkout of this repository, as below; or
  with Docker, using the released image `ghcr.io/agentkourier/agent-kourier:v0.1.0`, whose entrypoint is that binary.
  Either way you run it from the checkout, since the command reads this recipe's `kourier/` directory.

## Files

```
kourier/        AgentBackend, ChatConnection and Binding
evals/
  triggers.yaml the made-up messages the Binding must take or turn away (run by go test ./test/recipes)
```

## Apply it

1. Replace the placeholders:
    - `channel: CREPLACECI` in `kourier/binding.yaml`, with your channel's ID (it starts with `C`; `G` for an older
      private channel), not its name.
    - `id: B0REPLACEME`, with the CI bot's `bot_id`. The dry run in step 3 prints it if you do not have it.
    - The AgentBackend `url` in `kourier/agentbackend.yaml` (`http://REPLACE-ME.agents.svc.cluster.local:8080`), with
      your agent's. A dialect is Agent Kourier's adapter for one kind of A2A server. On `a2a` (a generic A2A agent) the
      URL alone selects the agent, and the Binding's `agent.name` and `agent.namespace` are labels that the Binding
      still has to set. On `kagent-v1`, `agent.name` and `agent.namespace` select the agent.
2. Replace the regexes, the `exclude` markers and the prompt wording with ones that fit your bot's messages and your
   agent's tools. Fetch a few real posts as JSON (`conversations.history` returns them) and read which field holds what.
   A trigger reads six fields, all raw strings as Slack delivers them: `text` is the message's top-level `text`, and
   `title`, `link`, `body`, `footer` and `fallback` come from its first legacy attachment. It never reads Block Kit
   blocks. [Start investigations from any bot's messages](../../../docs/how-to/any-bot-trigger.md) and
   [Chat triggers](../../../docs/reference/chat-triggers.md) have the rest.
3. Replay the channel's history through the trigger before going live. The dry run only reads Slack. It reads the bot
   token from a file (mode 600) or else from `SLACK_BOT_TOKEN`. To make the file from the `$SLACK_BOT_TOKEN` you use
   in the next step, without printing the token:

    ```sh
    mkdir -p ~/.config/agent-kourier
    ( umask 077; printf '%s\n' "$SLACK_BOT_TOKEN" > ~/.config/agent-kourier/slack-bot-token )
    chmod 600 ~/.config/agent-kourier/slack-bot-token
    ```

    ```sh
    go run ./cmd/agent-kourier dry-run --config examples/recipes/custom-bot-trigger/kourier \
      --binding agent-kourier/ci-failures --token-file ~/.config/agent-kourier/slack-bot-token
    ```

    With Docker instead of Go, mount the directory and pass the token through the environment. `SLACK_BOT_TOKEN` must be
    exported (`export SLACK_BOT_TOKEN`) for `-e SLACK_BOT_TOKEN` to pass it in; the token is then not on the command
    line. The image is the released one:

    ```sh
    docker run --rm -e SLACK_BOT_TOKEN -v "$PWD/examples/recipes/custom-bot-trigger/kourier:/config:ro" \
      ghcr.io/agentkourier/agent-kourier:v0.1.0 dry-run --config /config --binding agent-kourier/ci-failures
    ```

    Each of the bot's posts should come out `matched`, with the state, pipeline and key you expect, `excluded` for a
    retry or flaky marker, or `unmatched` with the step that turned it away (a `match` regex, an `extract`, or the
    sender's kind). A post of a bot the Binding does not name comes out `other_bot`, with its bot ID in the `SENDER`
    column. See [Tune triggers with a dry run](../../../docs/how-to/tune-triggers.md).
4. Make the Secret the ChatConnection names, in namespace `agent-kourier` (`kubectl create namespace agent-kourier`
   first if it does not exist). **These recipes call it `slack`; the
   [Configure Slack](../../../docs/how-to/configure-slack.md) and
   [Install with Helm](../../../docs/how-to/install-with-helm.md) guides call it `agent-kourier-slack`. Use the recipe's
   name here, since `kourier/chatconnection.yaml` refers to it.** It holds the fixed keys `botToken` and `appToken`. If
   you applied `sre-triage` or `docs-agent`, which ship the same ChatConnection, it exists already.

    ```sh
    kubectl -n agent-kourier create secret generic slack \
      --from-literal=botToken="$SLACK_BOT_TOKEN" \
      --from-literal=appToken="$SLACK_APP_TOKEN"
    ```

    The Secret is not in the recipe's directory, since a recipe cannot ship one. If you use `identity.tokenSecretRef`,
    make that Secret too.
5. Put the directory in a ConfigMap, in the namespace you install the chart into (`agent-kourier` in Install with
   Helm), and install the chart on it. The chart mounts a `config.existingConfigMap` at the directory Agent Kourier
   reads, one key per file:

    ```sh
    kubectl -n agent-kourier create configmap ci-failures-config \
      --from-file=examples/recipes/custom-bot-trigger/kourier
    ```

    ```yaml
    config:
      existingConfigMap: ci-failures-config
    rbac:
      secretNamespaces: [agent-kourier]
    ```

    The chart cannot read the ConfigMap to find the namespace of the Secret the ChatConnection names, so
    `rbac.secretNamespaces` says it. Do not also set `config.chatConnections`, `config.agentBackends` or
    `config.bindings`; the chart refuses both. Those values replace the inline `config` of the values file in
    [Install with Helm](../../../docs/how-to/install-with-helm.md); the rest of that guide applies as written.

## What to change

- The regexes and the trigger's fields: they are written for the made-up message above.
- `exclude`: your bot's retry and flaky markers, or remove it. Without it, a failure that will be retried also starts an
  investigation.
- `thread.cooldown` and `thread.maxIdle`: how long a branch that goes green and red again should reuse a thread.
- `limits` and `thread.maxIdle`: size both on purpose, because they interact with a bot that posts each failure once.
    - A start a limit stops gives the key's reservation back and posts nothing under the failure post. If nothing fires
      that key again, that failure is never investigated. With `maxConcurrent: 1`, a failure of another pipeline or
      branch that arrives while an investigation runs is such a stopped start: it shows in the channel's one-line
      count, in `agentkourier_trigger_limited_total` and in an INFO log line.
    - A bot that posts failures but never successes never closes a thread. Each repeat of the key moves its
      last-seen time forward, so a key that fails at least every `maxIdle` never gets a second investigation.
- To let people also mention the bot in this channel, add `- type: mention` to `triggers`. The list is the whole
  list, so without it a mention starts nothing. Put it after the `botMessage` trigger to keep the trigger's index at 0
  in `evals/triggers.yaml`, or update the indexes there.

## Checks

**Evals** are messages and what the Binding must make of each. `evals/triggers.yaml` pins what the trigger makes of one
message at a time: the failure that matches with its extracted fields, a post that fails `match.fallback` alone, a title
that is no pipeline result, pipeline names with a slash or a space, the `exclude` hit (a scheduled retry, and a flaky
marker), a failure from which `key` cannot be extracted, a success, another bot, a person and a reply inside a thread.
No message can isolate `match.title`, because `extract.state` rejects the same titles; the entry stays so that the dry
run names `match title` for such a post. They run with `go test ./test/recipes`; the format is in
[Writing or changing a recipe](../README.md#writing-or-changing-a-recipe). When you change a regex, change the messages
to ones copied from your bot and run it again.

## Limits

- **The message shape is made up.** No real product posts it. The evals pass on the made-up posts and say nothing about
  your bot's.
- **Each eval case is one message.** Thread keys across messages, the cooldown, `maxIdle`, `limits` and `closeWhen` are
  not evaluated. The success case shows why: the matcher classifies a success post as `matched`, with `state: PASSED`;
  it is the intake that sees `closeWhen` equal the extracted fields and closes the thread. Those rules are covered by
  the intake's own tests.
- **`exclude` also turns away a success post that carries a marker**, and a success that arrives while no thread is open
  for its key (for instance after the failure was excluded) closes nothing.
- **The evals do not exercise the prompt template's rendering.** Load the Binding and read a real first prompt.
- **A pipeline name outside the regex is a quiet miss.** See the reference below.

## Reference: this recipe's trigger

All of this is in the Binding's `triggers[0]`; every field is in [Chat
triggers](../../../docs/reference/chat-triggers.md).

| Field | In this recipe | Behaviour |
|---|---|---|
| `from.id` | `B0REPLACEME` | The one bot whose messages this trigger accepts, by bot ID, never by display name. |
| `match` | `title` starts `Pipeline FAILED - ` or `Pipeline PASSED - `; `fallback` starts `[FAILED] ` or `[PASSED] ` | Every entry must match. A regex is a search on the raw text of its field, so anchor it with `^` and `$`. A field the message lacks is empty. |
| `exclude` | `body` says a retry is scheduled, or `known flaky` | No entry may match. A failure that will be retried, or that is marked flaky, starts nothing. |
| `extract` | `state`, `pipeline` and `key` | Each regex has a named group with the entry's name. A message in which one finds nothing does not match the trigger. |
| `thread.by: key` | the key is `<pipeline> on <branch>` | Messages with the same `key` share one thread, anchored to the first. The key is kept per bot and channel. |
| `thread.cooldown` | `2h` | A failure inside this window after an investigation starts adds a "Fired again" note, and reopens a closed thread. 1 minute to 168 hours. |
| `thread.maxIdle` | `12h` | An open thread with no message of its key for this long counts as abandoned, and the next failure starts a new investigation. At least twice the cooldown, at most 720 hours. |
| `thread.closeWhen` | `state: PASSED` | A message whose extracted fields all equal the entry closes its thread with a note and starts no turn. |
| `limits` | 3 starts in any 15 minutes, 1 at once, 20 a day | A `botMessage` trigger does not load without `limits`. Only a start is limited. |
| `promptTemplate` | reads `.Fields.state`, `.Channel` and `.Fields.pipeline` | A Go template over the trigger's view of the message, parsed and run once at load, so a field the trigger does not extract rejects the Binding. |

### Names the regexes take, and values the prompt takes

The `pipeline` regex, and the pipeline part of the `key` regex, take only letters, digits and `_ . -`. A pipeline whose
name holds anything else (`team/web-build`, `web build`) passes `match` and `state` and fails those two extracts, so the
message matches no trigger: it starts nothing and posts nothing in Slack. It shows only in the
`agentkourier_trigger_unmatched_total` metric, a debug log line and the dry run, which names the extract that missed.
The evals pin this. Widening the regex instead sends the value on to the template, where a value that fails the prompt
check gets a visible "Not investigated" note under the failure post, and no turn. Choose between the quiet miss and the
visible refusal on purpose.

Each value the template reads is checked before it reaches the instruction part of the prompt: letters, digits and
`_ . - :`, at most 128 bytes. A value that fails refuses the turn: the thread gets a "Not investigated" note, and no
cooldown starts. That is why the template reads `pipeline` and `state` but not the branch: a branch such as
`feature/login` has a `/`. The branch still reaches the agent, because the whole message follows the rendered template,
normalized and with its secrets redacted, inside a block labelled as untrusted data. Do not read `{{ .Key }}` in the
template with this key shape: the key is `<pipeline> on <branch>`, so it always holds a space, which the rule for `.Key`
(letters, digits, `_ . - : /`) does not allow, and every start would be refused. Read the entries from `.Fields`
instead.

# custom-bot-trigger

A CI system posts a message to a Slack channel when a pipeline fails. This recipe turns each failure into an agent
investigation in its own thread, using a `botMessage` trigger written by hand, because no preset exists for this bot
(the only preset is for Alertmanager).

It holds the Agent Kourier side only: a Binding, the AgentBackend and ChatConnection it refers to, and trigger evals.
It ships no agent. Point the Binding at any agent you run; the [`sre-triage`](../sre-triage/README.md) recipe shows one wired to read-only
tools, and a CI-failure agent needs tools that can reach your CI system.

## The message shape is made up

The regexes in `kourier/binding.yaml` read a generic CI bot's post that this recipe invented. No real product posts
this shape. **Replace every regex with ones written against your own bot's messages.** Fetch a few real posts as
JSON (`conversations.history` returns them) and read which field holds what:

- `text` is the message's own `text`.
- `title`, `titleLink`, `fallback`, `footer` and `attachmentText` are the `title`, `title_link`, `fallback`, `footer`
  and `text` of the message's first legacy attachment. Later attachments are not read.

All six are the raw strings Slack delivers, not what the Slack client displays. The trigger's regexes read only
these six fields. Block Kit blocks are never read. For a bot that posts only blocks, `text` (the fallback Slack uses
in notifications) is the one field a trigger can read, and a bot message with no attachment content and empty `text`
is dropped before any trigger sees it.

The made-up posts this recipe is written for:

```text
title:          Pipeline FAILED - web-build
fallback:       [FAILED] web-build on main (#1423) | https://ci.example.com/pipelines/web-build/runs/1423
attachmentText: Stage test failed after 4m12s. 3 tests failed.
```

A success reads `Pipeline PASSED - web-build` and `[PASSED] web-build on main (#1430) | ...`.

## What each field does

All of this is in the Binding's `triggers[0]`. The fields are described in
[Chat triggers](../../../docs/reference/chat-triggers.md) and the guide
[Start investigations from any bot's messages](../../../docs/how-to/any-bot-trigger.md).

| Field | In this recipe | Behaviour |
|---|---|---|
| `from.botId` | `B0REPLACEME` | The one bot whose messages this trigger accepts, by bot ID, never by display name. Another bot's post is not handed on. |
| `match` | `title` starts `Pipeline FAILED - ` or `Pipeline PASSED - `; `fallback` starts `[FAILED] ` or `[PASSED] ` | Every entry must match. A regex is a search on the raw text of its field, so anchor it with `^` and `$` where you mean to. A field the message lacks is empty. |
| `exclude` | `attachmentText` says a retry is scheduled, or `known flaky` | No entry may match. A failure that will be retried, or that is marked flaky, is turned away and starts nothing. |
| `extract` | `state`, `pipeline` and `key` | Each regex has a named group with the entry's name, which supplies the value. A message in which one finds nothing does not match the trigger. |
| `thread.by: key` | the key is `<pipeline> on <branch>` | Messages with the same `key` share one thread, anchored to the first. The key is kept per bot and channel. |
| `thread.cooldown` | `2h` | After a thread closes, the key starts no new investigation until 2 hours after the message that started the thread; a failure inside that window adds a "Fired again" note to the closed thread and reopens it. The window is 1 minute to 168 hours. |
| `thread.maxIdle` | `12h` | An open thread with no message of its key for this long counts as abandoned, and the key's next failure starts a new investigation. This covers a success post that never arrived. It must be at least twice the cooldown and at most 720 hours; unset, it is the larger of 24 hours and twice the cooldown. |
| `thread.closeWhen` | `state: PASSED` | A message whose extracted fields all equal the entry closes its thread with a note and starts no turn. The field must be one the trigger extracts. |
| `limits` | 3 starts in any 15 minutes, 1 at once, 20 a day | A start a limit stops makes no turn and posts nothing under the failure post; once per window one line in the channel counts the stopped starts. Only a start is limited. **A trigger with no `limits` and no preset is unlimited**, so a storm of failing pipelines would start as many investigations. |
| `promptTemplate` | reads `.Fields.state`, `.Channel` and `.Fields.pipeline` | A Go template over the trigger's view of the message. It is parsed and run once at load, so a field the trigger does not extract rejects the Binding. |

While a key's thread is open, every further failure of that key that comes within 12 hours of the key's last
message adds a "Fired again" note rather than a new investigation, whatever the cooldown. After a longer silence the
thread counts as abandoned (`thread.maxIdle`) and the next failure starts an investigation. The cooldown matters once
the thread has closed. A failure for a key with no thread, or whose thread is closed and past its cooldown, starts an
investigation.

### Names the regexes take, and values the prompt takes

The `pipeline` regex, and the pipeline part of the `key` regex, take only letters, digits and `_ . -`. A pipeline whose name holds anything else
(`team/web-build`, `web build`) passes `match` and `state` and fails those two extracts, so the message matches no
trigger: it starts nothing and posts nothing in Slack. It shows only in the `agentkourier_trigger_unmatched_total`
metric, a debug log line and the dry run, which names the extract that missed. The evals pin this. Widening the
regex instead sends the value on to the template, where a value that fails the prompt check (below) gets a visible
"Not investigated" note under the failure post, and no turn. Choose between the quiet miss and the visible refusal on
purpose.

Each value the template reads is checked before it reaches the instruction part of the prompt: letters, digits and
`_ . - :`, at most 128 bytes. A value that fails refuses the turn: the thread gets a "Not investigated" note, and no
cooldown starts. That is why the template reads `pipeline` and `state` but not the branch: a branch such as
`feature/login` has a `/`. The branch still reaches the agent, because the whole message follows the rendered
template, normalized and with its secrets redacted, inside a block labelled as untrusted data. Do not read
`{{ .Key }}` in the template with this key shape: the key is `<pipeline> on <branch>`, so it always holds a space,
and the rule for `.Key` allows letters, digits, `_ . - :` and `/` only, so every start would be refused. Read the
entries from `.Fields` instead.

## Apply it

1. Replace the placeholders:
    - `channel: CREPLACEME` in `kourier/binding.yaml`, with your channel's ID (it starts with `C`; `G` for an older
      private channel), not its name.
    - `botId: B0REPLACEME`, with the `bot_id` of the CI bot's messages. It is not the Slack app ID (which starts with
      `A`). A [dry run](../../../docs/how-to/tune-triggers.md) prints each message's sender.
    - The AgentBackend `url`, with your agent's. On the `a2a` dialect the URL alone selects the agent, and the
      Binding's `agent.name` and `agent.namespace` are labels there that the Binding still has to set. For kagent,
      set `dialect: kagent-v1` and its base URL as in [Connect kagent](../../../docs/how-to/connect-kagent.md), and
      then `agent.name` and `agent.namespace` select the agent.
2. Replace the regexes, `exclude` markers and the prompt wording with ones that fit your bot's messages and your
   agent's tools.
3. Create the Secret the ChatConnection names, in the namespace `agent-kourier`, with the fixed keys `botToken` and
   `appToken` ([Configure Slack](../../../docs/how-to/configure-slack.md)):

    ```sh
    kubectl -n agent-kourier create secret generic slack-ci \
      --from-literal=botToken="$SLACK_BOT_TOKEN" \
      --from-literal=appToken="$SLACK_APP_TOKEN"
    ```

    The Binding authenticates with `identity.userId`, which a backend that trusts `X-User-Id` accepts. If your agent or
    its front door verifies tokens, use `tokenSecretRef` instead
    ([Create the Binding identity](../../../docs/how-to/create-binding-identity.md)). The Secret is not in this
    directory: a recipe cannot ship one.
4. Invite the Agent Kourier bot to the channel.
5. Replay the channel's history through the trigger before going live, with the loader that production uses:

    ```sh
    go run ./cmd/agent-kourier dry-run --config examples/recipes/custom-bot-trigger/kourier \
      --binding agent-kourier/ci-failures --token-file ~/.config/agent-kourier/slack-bot-token
    ```

    Each of the bot's posts should come out `matched`, with the state, pipeline and key you expect, `excluded` for a
    retry or flaky marker, or `unmatched` with the regex that turned it away
    ([Tune triggers with a dry run](../../../docs/how-to/tune-triggers.md)).
6. Load the three resources into Agent Kourier. The chart mounts a `config.existingConfigMap` at the directory Agent
   Kourier reads, one key per file, so the directory makes one:

    ```sh
    kubectl -n <release-namespace> create configmap <name> \
      --from-file=examples/recipes/custom-bot-trigger/kourier
    ```

    Set `config.existingConfigMap: <name>` and `rbac.secretNamespaces: [agent-kourier]` in your values: the chart
    cannot read the ConfigMap to find the namespace of the Secret the ChatConnection names. Do not also set
    `config.chatConnections`, `config.agentBackends` or `config.bindings`; the chart refuses both
    ([Install with Helm](../../../docs/how-to/install-with-helm.md)).

## What to change

- The regexes and the trigger's fields: they are written for the made-up message above.
- `exclude`: your bot's retry and flaky markers, or remove it. Without it, a failure that will be retried also starts an investigation.
- `thread.cooldown` and `thread.maxIdle`: how long a branch that goes green and red again should reuse a thread.
- `limits` and `thread.maxIdle`: size both on purpose, because they interact with a bot that posts each failure
  once.
    - A start a limit stops gives the key's reservation back and posts nothing under the failure post. If nothing fires
      that key again, that failure is never investigated. With `maxConcurrent: 1`, a failure of another pipeline or
      branch that arrives while an investigation runs is such a stopped start: it shows in the channel's one-line
      count, in `agentkourier_trigger_limited_total` and in an INFO log line.
    - A bot that posts failures but never successes never closes a thread. Each repeat of the key moves its
      last-seen time forward, so a key that fails at least every `maxIdle` never gets a second investigation.
- To let people also mention the bot in this channel, add `- type: mention` to `triggers`. The list is the whole
  list, so without it a mention starts nothing. Put it after the `botMessage` trigger to keep the trigger's index at 0
  in `evals/triggers.yaml`, or update the indexes there.

## Evals

`evals/triggers.yaml` pins what the trigger makes of one message at a time: the failure that matches with its
extracted fields, a post that fails `match.fallback` alone, a title that is no pipeline result, pipeline names with a
slash or a space, the `exclude` hit (a scheduled retry, and a flaky marker), a failure from which `key` cannot be
extracted, a success, another bot, a person and a reply inside a thread. No message can isolate `match.title`,
because `extract.state` rejects the same titles; the entry stays so that the dry run names `match title` for such a
post. They run
with `go test ./test/recipes` ([the recipes index](../README.md) describes the format and what it cannot check).
When you change a regex, change the messages to ones copied from your bot and run it again.

Limits to know about:

- Each case is one message. Thread keys across messages, the cooldown, `maxIdle`, `limits` and `closeWhen` are not
  evaluated. The success case shows why: the matcher classifies a success post as `matched`, with `state: PASSED`;
  it is the intake that sees `closeWhen` equal the extracted fields and closes the thread. Those rules are covered by
  the intake's own tests.
- `exclude` also turns away a success post that carries a marker, and a success that arrives while no thread is open
  for its key (for instance after the failure was excluded) closes nothing.
- The evals do not exercise the prompt template's rendering; load the Binding and read a real first prompt.

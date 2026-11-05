# Start investigations from any bot's messages

For a team whose alerts reach a Slack channel through a bot that is not Alertmanager: at the end, a `botMessage`
trigger written in full reads that bot's posts, and each alert starts an agent investigation in its own thread.

Agent Kourier accepts the messages of one bot per trigger, named by its Slack bot ID, and reads them with the regexes
you give it. Only Alertmanager is exercised in the sandbox and the end-to-end tests. For any bot you write `match`,
`extract`, `thread`, `limits` and `promptTemplate`. For Alertmanager, start from the worked trigger in
[Investigate Alertmanager alerts](investigate-alertmanager-alerts.md) instead.

## Before you start

- A [Binding](write-a-binding.md) for the channel, and the Agent Kourier bot invited to it.
- The bot ID of the integration that posts the alerts (`B...`). A [dry run](tune-triggers.md) of the channel prints
  each message's sender.
- A few of its real messages, to write the regexes against. Note which field holds what: `text` is the message's
  top-level text; `title`, `link`, `body`, `footer` and `fallback` come from its first legacy attachment (its
  `title`, `title_link`, `text`, `footer` and `fallback`).

## Write the trigger

The example below is for a made-up message format, one line in `text`:

```text
[FIRING] DiskAlmostFull on node-7: /var is 93% full
[RESOLVED] DiskAlmostFull on node-7: /var is 61% full
```

Replace the regexes with ones that fit your bot's messages. The
[`custom-bot-trigger` recipe](recipes.md#start-investigations-from-a-ci-bots-failure-posts) is a complete Binding of
this kind, with its AgentBackend, ChatConnection and checks.

```yaml
  chat:
    connectionRef: {namespace: agent-kourier, name: slack}
    channel: C0123456789
    triggers:
      - type: mention # (1)!
      - type: botMessage
        from:
          id: B0123456789 # (2)!
        match:
          text: '^\[(FIRING|RESOLVED)\] ' # (3)!
        exclude:
          text: '^\[[A-Z]+\] Heartbeat '
        extract: # (4)!
          state:
            field: text
            regex: '^\[(?P<state>FIRING|RESOLVED)\]'
          alertname:
            field: text
            regex: '^\[[A-Z]+\] (?P<alertname>[A-Za-z0-9_.:-]+)'
          key:
            field: text
            regex: '^\[[A-Z]+\] (?P<key>[A-Za-z0-9_.:-]+ on [A-Za-z0-9_.-]+)'
        thread: # (5)!
          by: key
          cooldown: 4h
          closeWhen:
            state: RESOLVED
        limits: # (6)!
          rateLimit: {maxRuns: 5, per: 10m, maxConcurrent: 2}
          maxRunsPerDay: 40
        promptTemplate: | # (7)!
          An alert is {{ .State }} in channel {{ .Channel }}: {{ .Fields.alertname }}.
          Investigate it with your read-only tools. Report the likely cause, the evidence you
          checked, and the next steps a person should take.
```

1. Keep this if people should also be able to mention the bot. A list that names triggers is the whole list.
2. The sender's ID: on Slack, the alert messages' `bot_id`, never a display name. `from.kind` is `bot`, its default and only value; leave it out.
3. Every `match` entry must match, and no `exclude` entry may. Regexes are RE2, at most 512 bytes and
   a program of at most 2000 instructions (see [Limits](../reference/limits.md#write-a-binding-an-agentbackend-or-a-chatconnection)); they read the first 16 KiB of a field.
4. Each entry's regex has a named group with the entry's name, which supplies the value. A message in which an
   extract does not match does not match the trigger.
5. Messages with the same `key` share one thread. A fire while the thread is open, or within the cooldown, adds a note and reopens a closed thread; a message whose `state`
   is `RESOLVED` closes the thread with a note. Neither starts a turn.
6. Required: a `botMessage` trigger without `limits.rateLimit` (all of `maxRuns`, `per` and `maxConcurrent`) or without
   `limits.maxRunsPerDay` does not load. These are the values of the Alertmanager example; `maxRunsPerDay: 0` is no
   daily cap, and has to be written to be chosen.
7. The first turn's prompt. Read extracted values through `.Fields`: a value the template reads must be name-shaped
   (letters, digits, `_.-:`), and the key here holds spaces, so a template that reads `.Key` loads, but every alert's prompt is refused when it renders: a "Not investigated" note, and no turn.

The Binding is validated when it loads: a regex that does not compile, an extract without its named group, a
`thread.by: key` without a `key` extract, or a template that reads a field the trigger does not extract rejects it.

## Check it before going live

Replay the channel's history through the trigger with a [dry run](tune-triggers.md). Each of the bot's messages should
come out `matched`, with the state, alert name and key you expect, `excluded` when an `exclude` regex turned it away, or
`unmatched` with the step that turned it away (a `match` regex, an `extract`, or the sender's kind).
Tune the regexes until the lines read right, then load the Binding.

After it is live, `agentkourier_trigger_matched_total` and `agentkourier_trigger_unmatched_total` count the bot's
messages the trigger took and turned away, and `agentkourier_trigger_decisions_total` what each did to its thread.
Every field and decision is in [Chat triggers](../reference/chat-triggers.md).

## Let another agent start yours

No trigger is needed for this. A bot's mention of Agent Kourier starts a turn in the thread that holds it, on any Binding with a `mention`
trigger, unless the Binding sets `chat.botMentions: none`. [Chat triggers](../reference/chat-triggers.md#a-bots-mention) has the rules and the limits.

To have one agent call another:

1. Run them as separate Slack apps. Agent Kourier never reads its own posts, and a channel belongs to at most one Binding per connection, so
   two Bindings on one app cannot do this.
2. Give the target a Binding for the channel the mention will be in, and invite its bot there.
3. On the source's Binding, declare the target's bot user under `chat.mentions`, so that its agent can write `@name`.
4. Set `chat.threadReplies: mention` on a Binding whose thread the other agent may be mentioned in. A session that a bot's mention began takes only a
   person's mention whatever the Binding says, but a thread its Binding already owned keeps the Binding's rule, so with `all` a person's reply is a
   turn there and also for any other agent that owns the thread that way.

Know what this does not bound:

- **It accepts any bot.** Any bot or incoming webhook that can post in the channel can start your agent, and a post with a mention token in its text
  or in a section or context block is enough. In a channel full of integrations, set `chat.botMentions: none`. A bot a `botMessage` trigger names
  is left to that trigger.
- **There is no hop limit.** If two agents can each mention the other, only the limits stop them, and people see the bots trade messages until
  then.
- **The source hears nothing back.** A mention counts as made once it is posted, even if the target stopped it at a limit, or does not exist.
- **The receiving agent decides what to trust.** The model alone decides to write a name, so a crafted alert can get one agent to mention another. The first
  prompt tells the agent to check before it acts, and the thread reaches it as untrusted data. Give an agent that can take a consequential step
  a tool with its own check, and do not let it act on the mention alone.
- **The agent is told only what the thread holds.** There is no field for a job: put what the agent should do in its own instructions.
- **Same thread only.** This starts no thread in another channel.

A [dry run](tune-triggers.md) replays history, which has no mention events, so it does not show these. A Binding that took no bots' mentions before takes them after an upgrade; set `chat.botMentions: none` first if it should not.


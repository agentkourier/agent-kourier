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
    connectionRef: {namespace: agent-kourier-system, name: slack}
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
3. Every `match` entry must match, and no `exclude` entry may. Regexes are RE2, at most 512 bytes.
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
come out `matched`, with the state, alert name and key you expect, or `unmatched` with the regex that turned it away.
Tune the regexes until the lines read right, then load the Binding.

After it is live, `agentkourier_trigger_matched_total` and `agentkourier_trigger_unmatched_total` count the bot's
messages the trigger took and turned away, and `agentkourier_trigger_decisions_total` what each did to its thread.
Every field and decision is in [Chat triggers](../reference/chat-triggers.md).

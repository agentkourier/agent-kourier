# Investigate Alertmanager alerts

For a team whose Alertmanager already posts alerts to a Slack channel: at the end, each alert posted there starts an
agent investigation in that alert's thread, with one thread per alert, a cooldown, a rate limit and a daily cap.

Agent Kourier reads the message Alertmanager posts. Alertmanager's own receivers and routes are untouched, so the
alert still reaches people if Agent Kourier is down. The trigger below is written in full: the regexes, the thread
rules, the limits and the prompt are all in the Binding, so you can read and change each one. For alerts another tool's
bot posts, see [Start investigations from any bot's messages](any-bot-trigger.md).

## Before you start

- A [Binding](write-a-binding.md) for the alert channel, or the details to write one, and the bot invited to that
  channel.
- The bot ID of the Slack integration that posts the alerts. It starts with `B`. It is the `bot_id` on the alert
  messages, not the Slack app ID (which starts with `A`), and never a display name. A
  [dry run](tune-triggers.md) prints each message's sender, which is one way to read it.

## Shape the receiver's title

The trigger below matches one message shape. It needs:

- a title that reads `FIRING (<n>) - <alertname>` or `RESOLVED - <alertname>`, optionally behind a bracketed tag of up
  to 32 characters such as `[kind] `;
- Alertmanager's default fallback text, which begins `[FIRING:<n>]` (`[FIRING]` and `[RESOLVED]` also match).

Alertmanager's default Slack title also begins `[FIRING:<n>]`, so a receiver that keeps the default title does not
match. Set the receiver's title to the shape above, or change the `match` and `extract` regexes for your own template.

## Add the trigger

In the Binding's `chat` block:

```yaml
  chat:
    connectionRef:
      namespace: agent-kourier-system
      name: slack
    channel: C0123456789
    output: live
    triggers:
      - type: mention # (1)!
      - type: botMessage
        from:
          id: B0123456789 # (2)!
        match:
          # The receiver, by the name in the title's link. Optional: use it when several receivers share the bot
          # and the channel. The $ anchors it, so receiver=slack-critical does not also match
          # receiver=slack-critical-kind.
          link: 'receiver=slack-critical$'
          # Alertmanager's title: "FIRING (<n>) - <alertname>" or "RESOLVED - <alertname>", behind an optional
          # bracketed tag of up to 32 characters, such as "[kind] ". Grafana's "FIRING: <rule> (<namespace>)" and
          # the heartbeat do not fit it, so they start nothing.
          title: '^(?:\[[^\]]{1,32}\] )?(?:FIRING \(\d+\)|RESOLVED) - '
          # Alertmanager's default fallback text: "[FIRING:<n>] ..." or "[RESOLVED] ...".
          fallback: '^\[(?:FIRING(?::\d+)?|RESOLVED)\] '
        extract: # (3)!
          # FIRING or RESOLVED, from the title.
          state:
            field: title
            regex: '^(?:\[[^\]]{1,32}\] )?(?P<state>FIRING|RESOLVED)'
          # The group label values at the start of the fallback, without the count and the common labels in
          # parentheses, so every repeat of an alert group has the same key and shares a thread.
          key:
            field: fallback
            regex: '^\[[A-Z]+(?::\d+)?\] (?P<key>[^(|]*[^(|\s])'
          # The rest of the title after " - ".
          alertname:
            field: title
            regex: '^(?:\[[^\]]{1,32}\] )?(?:FIRING \(\d+\)|RESOLVED) - (?P<alertname>.+)$'
        thread: # (4)!
          by: key
          cooldown: 4h
          closeWhen:
            state: RESOLVED
        limits: # (5)!
          rateLimit: {maxRuns: 5, per: 10m, maxConcurrent: 2}
          maxRunsPerDay: 40
        promptTemplate: | # (6)!
          An alert is {{ .State }} in channel {{ .Channel }}: {{ .Fields.alertname }}.
          Investigate it with your read-only tools. Report the likely cause, the evidence you
          checked, and the next steps a person should take.
```

1. Keep this if people should also be able to mention the bot in the channel. A list that names triggers is the whole
   list: without it, a mention starts nothing.
2. The alert messages' `bot_id`. `from.kind` defaults to `bot`, the only value, so it is left out.
3. Each entry's regex has a named group with the entry's name, which supplies the value. A message in which an extract
   does not match does not match the trigger.
4. Messages with the same `key` share a thread, anchored to the first.
5. Required. A `botMessage` trigger without `limits.rateLimit` (all of `maxRuns`, `per` and `maxConcurrent`) or without
   `limits.maxRunsPerDay` does not load, and the error names the missing field. `maxRunsPerDay: 0` is no daily cap,
   and has to be written to be chosen.
6. The first turn's prompt. Read extracted values through `.Fields`; the key holds spaces, so a template that reads
   `.Key` is refused when it renders.

What the trigger does:

- **One thread per alert group.** Messages with the same group label values share the first message's thread.
- **A 4 hour cooldown.** Another fire of the alert while its thread is open (unless it has been idle for 24 hours,
  `thread.maxIdle`, when a new investigation starts), or within 4 hours of the investigation's start, adds a "Fired
  again" note to the thread and starts no new turn. `thread.maxIdle` must be longer than your Alertmanager's
  `repeat_interval`.
- **A resolve closes the thread** with a "Resolved at" note, and starts no turn.
- **Limits.** At most 5 investigations in any 10 minutes, 2 running at once and 40 a day. An alert a limit stops gets
  no investigation; one line per window in the channel counts them.

Change a value in place, for example `cooldown: 2h` or `maxRunsPerDay: 20`. The fields are in
[Chat triggers](../reference/chat-triggers.md).

## Check it

Before going live, replay the channel's history through the trigger with a [dry run](tune-triggers.md), from the
repository root, with the Binding saved in `./my-bindings`:

```sh
go run ./cmd/agent-kourier dry-run --config ./my-bindings --binding <namespace>/<name> \
  --token-file ~/.config/agent-kourier/slack-bot-token
```

Each Alertmanager message should come out `matched`, with its state, alert name and key. Then load the Binding and
watch the next alert: a thread opens under it and the investigation streams in.

In the pod log, `chat: bot message matched a trigger` and `chat: trigger decided` (with `decision=started`) appear
around `session turn started`, which the turn's own worker logs, so the order is usual rather than guaranteed. The metrics `agentkourier_trigger_matched_total` and
`agentkourier_trigger_decisions_total` count the same.

If an alert message raises `agentkourier_trigger_unmatched_total` and starts nothing, its title or link does not fit the
trigger: dry-run it.

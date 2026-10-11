# Investigate Alertmanager alerts

For a team whose Alertmanager already posts alerts to a Slack channel: at the end, each alert posted there starts an
agent investigation in that alert's thread, with one thread per alert, a cooldown, a rate limit and a daily cap.

Agent Kourier reads the message Alertmanager posts. Alertmanager's own receivers and routes are untouched, so the
alert still reaches people if Agent Kourier is down. This guide uses the `alertmanager` preset, the one preset that
ships. For alerts another tool's bot posts, see
[Start investigations from any bot's messages](any-bot-trigger.md).

## Before you start

- A [Binding](write-a-binding.md) for the alert channel, or the details to write one, and the bot invited to that
  channel.
- The bot ID of the Slack integration that posts the alerts. It starts with `B`. It is the `bot_id` on the alert
  messages, not the Slack app ID (which starts with `A`), and never a display name. A
  [dry run](tune-triggers.md) prints each message's sender, which is one way to read it.

## Shape the receiver's title

The `alertmanager` preset matches one message shape. It needs:

- a title that reads `FIRING (<n>) - <alertname>` or `RESOLVED - <alertname>`, optionally behind a bracketed tag of up
  to 32 characters such as `[kind] `;
- Alertmanager's default fallback text, which begins `[FIRING:<n>]` (`[FIRING]` and `[RESOLVED]` also match).

Alertmanager's default Slack title also begins `[FIRING:<n>]`, so a receiver that keeps the default title does not
match. Set the receiver's title to the shape above, or override the preset's `match` and `extract` entries for your
own template.

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
          botId: B0123456789 # (2)!
        preset: alertmanager
        match:
          titleLink: 'receiver=slack-critical$' # (3)!
```

1. Keep this if people should also be able to mention the bot in the channel. A list that names triggers is the whole
   list: without it, a mention starts nothing.
2. The alert messages' `bot_id`.
3. Optional. When several receivers share the bot and the channel, match one by the receiver's name in the title's
   link. The `$` anchors it, so `receiver=slack-critical` does not also match `receiver=slack-critical-kind`.

The preset supplies the rest:

- **One thread per alert group.** Messages with the same group label values share the first message's thread.
- **A 4 hour cooldown.** Another fire of the alert while its thread is open (unless it has been idle for 24 hours,
  `thread.maxIdle`, when a new investigation starts), or within 4 hours of the investigation's start, adds a "Fired
  again" note to the thread and starts no new turn.
- **A resolve closes the thread** with a "Resolved at" note, and starts no turn.
- **Limits.** At most 5 investigations in any 10 minutes, 2 running at once and 40 a day. An alert a limit stops gets
  no investigation; one line per window in the channel counts them.

Override any of these per field, for example:

```yaml
        thread: {cooldown: 2h}
        limits: {maxRunsPerDay: 20}
```

The fields and the merge rules are in [Chat triggers](../reference/chat-triggers.md).

## Check it

Before going live, replay the channel's history through the trigger with a [dry run](tune-triggers.md). Then load
the Binding and watch the next alert: a thread opens under it and the investigation streams in.

In the pod log, `chat: bot message matched a trigger` and `chat: trigger decided` (with `decision=started`) appear
around `session turn started`, which the turn's own worker logs, so the order is usual rather than guaranteed. The metrics `agentkourier_trigger_matched_total` and
`agentkourier_trigger_decisions_total` count the same.

If an alert message raises `agentkourier_trigger_unmatched_total` and starts nothing, its title or link does not fit the
trigger: dry-run it.

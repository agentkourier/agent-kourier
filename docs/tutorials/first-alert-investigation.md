# Your first alert investigation

For anyone who has finished [Your first agent in Slack](first-agent-in-slack.md): at the end, an alert lands in a
Slack channel, and the agent investigates it in that alert's thread without anyone typing a prompt.

The alerts in this tutorial come from Alertmanager, because the sandbox runs one, and the docs have a worked trigger for
its messages. Alertmanager is the example, not a requirement: Agent Kourier reads what a bot posts to the channel, and
another tool's bot needs a trigger of its own (see
[Start investigations from any bot's messages](../how-to/any-bot-trigger.md)).

The sandbox's Alertmanager posts alerts to Slack the way a production Alertmanager does. Agent Kourier reads those
posts, so you wire nothing into Alertmanager itself.

## What you need

- The sandbox from [Your first agent in Slack](first-agent-in-slack.md), up, with `make sandbox-up` ending in
  `ok: Agent Kourier is connected to Slack`.
- Go, at the version in `go.mod`, for one command in step 4.

## 1. Make an alert channel

1. In your sandbox workspace, create a second channel, for example `#alerts-sandbox`. It must not be the channel of
   the first tutorial.
2. In it, send `/invite @Agent Kourier`.
3. Copy the channel's ID from its details. It starts with `C`.

## 2. Give Alertmanager a way to post

Alertmanager posts through a Slack incoming webhook, and that webhook must belong to a second app.

1. Open <https://api.slack.com/apps>, choose **Create New App**, then **From scratch**. Name it `alertmanager-sandbox`
   and pick your sandbox workspace.
2. Under **Incoming Webhooks**, turn them on, choose **Add New Webhook to Workspace**, and pick the alert channel.
3. Copy the webhook URL. It starts with `https://hooks.slack.com/services/`.
4. Save it, alone on one line:

    ```sh
    $EDITOR ~/.config/agent-kourier/alertmanager-webhook-url
    chmod 600 ~/.config/agent-kourier/alertmanager-webhook-url
    ```

## 3. Point the sandbox at your channel

Open `hack/kind/sandbox/webhook.sh` and set `ALERT_CHANNEL_ID` to your alert channel's ID:

```sh
ALERT_CHANNEL_ID=C0123456789
```

## 4. Find the webhook's bot ID

Post one test message through the webhook:

```sh
curl -sS -X POST -H 'Content-Type: application/json' --data '{"text":"webhook test"}' \
  "$(cat ~/.config/agent-kourier/alertmanager-webhook-url)"
```

The channel shows `webhook test`. Now list the channel's messages with their senders:

```sh
go run ./cmd/agent-kourier dry-run --config hack/kind/sandbox/dryrun --binding agent-kourier/trial \
  --channel C0123456789 --token-file ~/.config/agent-kourier/sandbox.env
```

Use your alert channel's ID for `--channel`. The line for `webhook test` has the outcome `other_bot`, and its
`SENDER` column is the webhook's bot ID. It starts with `B`. Copy it.

## 5. Name the bot in the alert Binding

Open `hack/kind/sandbox/agent-kourier/alert-trial/alert-binding.yaml` and set `botId` to the ID from step 4:

```yaml
        from: {botId: B0123456789}
```

## 6. Start the alert source

```sh
make sandbox-up
```

Among its last lines are the connection check and, at the very end, the alert source:

```text
ok: Agent Kourier is connected to Slack (it logged "...": auth.test passed and Socket Mode said hello)
...
alert source up, and Agent Kourier watches C0123456789 for [kind] alerts: fire one with make sandbox-alert (README, Run the alert trial)
```

## 7. Fire an alert

```sh
make sandbox-alert ALERT=KubePodCrashLooping
```

It prints `sent: KubePodCrashLooping firing`. About 30 seconds later the webhook posts
`[kind] FIRING (1) - KubePodCrashLooping` to the alert channel.

Within seconds a thread opens under that post. The investigation streams into it, with a step card for each tool
call, and finds the `FATAL: cannot resolve payments-db` line in the `checkout` pod's log.

Reply in the thread, without a mention:

```text
what would fix it?
```

The agent answers in the same thread, as part of the same investigation.

## 8. Resolve the alert

```sh
make sandbox-alert ALERT=KubePodCrashLooping STATE=resolved
```

About 30 seconds later the webhook posts `[kind] RESOLVED - KubePodCrashLooping`, and the investigation's thread gets
a note that says when the resolve was posted. No new investigation starts.

## 9. Clean up

```sh
make sandbox-down
git checkout hack/kind/sandbox/webhook.sh hack/kind/sandbox/agent-kourier/alert-trial/alert-binding.yaml
rm ~/.config/agent-kourier/alertmanager-webhook-url
```

Remove the `alertmanager-sandbox` app's webhook in Slack.

## What you did

You had Alertmanager post an alert to Slack, watched Agent Kourier start an investigation in the alert's thread,
continued it with a reply, and saw the resolve noted in the same thread.

Next:

- [Investigate Alertmanager alerts](../how-to/investigate-alertmanager-alerts.md) sets this up on your own cluster,
  and [Start investigations from any bot's messages](../how-to/any-bot-trigger.md) does it for another alerting tool.
- [Chat triggers](../reference/chat-triggers.md) lists the fields of a `botMessage` trigger and how its threads,
  cooldown and limits work.
- [Why a broker](../explanation/why-a-broker.md) explains why Agent Kourier reads alerts from Slack and stays out of
  the paging path.

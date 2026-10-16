# Tune triggers with a dry run

For anyone writing or changing a Binding's `botMessage` triggers: at the end, you have replayed a channel's real
history through the triggers and seen what each message would become, before going live.

`agent-kourier dry-run` only reads Slack (`auth.test` and `conversations.history`). It posts nothing, starts no session, writes nothing
and needs no cluster.

## Before you start

- Go, at the version in `go.mod`, and a checkout of the repository; or the `agent-kourier` binary.
- A directory with the Binding's YAML. It is loaded with the broker's own loader, so the defaults and each Binding's own
  validation apply as in production. References between resources and Secrets are not
  checked. Only the channel and the triggers are used.
- A file with the bot token (or `SLACK_BOT_TOKEN` in the environment): the token alone, or a `SLACK_BOT_TOKEN=...` line among others. It must be a regular file,
  not a link, that the group and others cannot read (`chmod 600`). The bot must be a member of the channel and have
  `channels:history` (`groups:history` for a private channel).

## Run it

From the repository root:

```sh
go run ./cmd/agent-kourier dry-run --config ./my-bindings --binding payments/payments-assistant \
  --token-file ~/.config/agent-kourier/slack-bot-token
```

| Option | Use |
|---|---|
| `--channel C...` | Read another channel than the Binding's. |
| `--since 72h` | Read only the newer messages. |
| `--limit 50` | Show only the newest 50 of what was read. Reading is not cut short; use `--since` to read less. |
| `--json` | Print one JSON report, for `jq`. |

## Read the lines

One line per message, oldest first:

```text
TS                 SENDER       OUTCOME    TRIGGER  STATE     ALERTNAME            KEY                       TITLE
1791104398.621498  B0123456789  matched    1        FIRING    KubePodCrashLooping  KubePodCrashLooping shop  [kind] FIRING (1) - KubePodCrashLooping
1791104398.621757  B0123456789  unmatched  -        -         -                    -                         FIRING (1) - KubeAPIErrorBudgetBurn    trigger 1: match titleLink
```

- **SENDER** is the bot ID, `person:<user ID>`, `agent-kourier` for Agent Kourier's own post, or `unknown` for a message the adapter drops. **TRIGGER** is the
  index in `chat.triggers`.
- **OUTCOME**:
    - `matched`: the trigger took it; STATE, ALERTNAME and KEY are what it extracted.
    - `unmatched`: a trigger names the bot, and none took the message. The last column says where each trigger turned
      it away, at the first regex that failed.
    - `excluded`: every `match` regex matched and an `exclude` regex turned it away.
    - `other_bot`: a bot that no trigger names.
    - `own`, `person`, `ignored`: Agent Kourier's own post, a person's message, and something the intake leaves alone.

## Fix what it shows

- **Too few or too many matches.** Sort the `unmatched` lines by their reason, or list their titles:
  `--json | jq -r '.messages[] | select(.outcome=="unmatched") | .title' | sort | uniq -c`.
- **Keys.** A key is the thread an alert shares. It should be the same for repeats of one alert and different for
  different alerts. A key with a pod name in it, or a different key on every line, means the `key` regex is too narrow;
  one key across unrelated alerts means it is too wide.
- **Flapping.** The summary lists keys that fired and closed at least 3 times. A `cooldown` shorter than the gap
  between those cycles starts a new investigation each time.

Copy the Binding, change its `match`, `exclude` and `extract`, and run again until the lines read right.

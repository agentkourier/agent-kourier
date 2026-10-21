# Configure Slack

For a platform engineer setting up the chat side: at the end, a Slack app has the scopes and events Agent Kourier
needs, its tokens are in a Kubernetes Secret, and a ChatConnection points at it.

## Before you start

- Permission to install an app in the Slack workspace. Workspace settings can require an admin to approve each app; a
  member without that permission sees **Request to Install** instead of **Install**, and can copy the bot token only
  after an admin approves the request.
- A Slack app that no other running Agent Kourier uses (see the warning at the end of the steps).
- `kubectl` access to the namespace Agent Kourier runs in.
- For the check in the last step: Go, at the version in `go.mod`, and a checkout of the repository.

=== "Socket Mode"

    Socket Mode is how Agent Kourier connects to Slack today: it opens a WebSocket to Slack, so nothing in your cluster
    needs to be reachable from the internet.

    **1. Create the app from the manifest.** Open <https://api.slack.com/apps>, choose **Create New App**, **From a
    manifest**, and paste
    [`hack/kind/sandbox/slack-app-manifest.yaml`](https://github.com/agentkourier/agent-kourier/blob/main/hack/kind/sandbox/slack-app-manifest.yaml).
    It names the app and its bot user **Agent Kourier** (`display_information.name` and
    `features.bot_user.display_name`); change them if people should see another name. The manifest holds the scopes,
    the three bot events, interactivity and Socket Mode.

    **2. Make the two tokens.**

    Agent Kourier needs both tokens from the same Slack app. They have different jobs:

    | Token | Why Agent Kourier needs it | Secret key |
    |---|---|---|
    | Bot User OAuth Token, `xoxb-` | Calls Slack's Web API as the bot to post, update and stream agent replies, add reactions, read channel and thread history, and look up users and channel membership. | `botToken` |
    | App-level token, `xapp-` | Opens the Socket Mode WebSocket that delivers mentions, channel messages and button presses to Agent Kourier. This lets it receive Slack events without a public HTTP endpoint. | `appToken` |

    1. Under **Install App**, install the app to the workspace and copy the **Bot User OAuth Token** (`xoxb-`).
       Installing the app grants the bot permissions from the manifest. The bot token lets Agent Kourier use those
       permissions; the [scope table below](#what-the-app-is-allowed-to-do) lists the calls they allow.
    2. Open **Basic Information** and scroll to **App-Level Tokens**. Choose **Generate Token and Scopes**, give the
       token a name, add `connections:write`, and generate it. Copy the token (`xapp-`). Slack requires
       [this scope](https://docs.slack.dev/reference/scopes/connections.write/) to open a Socket Mode connection.
       The manifest enables Socket Mode, but you must generate this token separately.

    **Basic Information** is the settings page containing **App-Level Tokens**. Agent Kourier only needs the two tokens
    above; you do not need to copy the App ID, Client ID, Client Secret or Signing Secret from that page.

    **3. Invite the bot and copy the channel ID.** In each channel a Binding will use, send `/invite @Agent Kourier`
    (or the display name you gave the bot). Copy the channel ID from the channel's details: it starts with `C` (a private
    channel created before March 2021 may start with `G`). A Binding takes the ID, not the name.

    **4. Store the tokens.** Put them in a file that only you can read:

    ```sh
    mkdir -p ~/.config/agent-kourier
    touch ~/.config/agent-kourier/slack-secret.env && chmod 600 ~/.config/agent-kourier/slack-secret.env
    ```

    Open the file in an editor and write the two tokens, one `key=value` per line, with no quotes:

    ```dotenv
    botToken=xoxb-REPLACE
    appToken=xapp-REPLACE
    ```

    Make the Secret from the file:

    ```sh
    kubectl -n agent-kourier create secret generic agent-kourier-slack \
      --from-env-file="$HOME/.config/agent-kourier/slack-secret.env"
    ```

    kubectl reads the tokens from the file, so they are on no command line and in no shell history. The key names are
    fixed: `botToken` and `appToken`. Keep the file at mode 600 for the check in step 6, and delete it afterwards.

    The path is only an example: any file that only you can read works, and Agent Kourier never reads it; kubectl does.
    If your cluster's Secrets come from a secrets manager (External Secrets, Sealed Secrets), create
    `agent-kourier-slack` with the keys `botToken` and `appToken` that way instead, and skip the file; step 6 then
    needs `SLACK_BOT_TOKEN` set some other way.

    **5. Declare the connection.** In the chart's values:

    ```yaml
    config:
      chatConnections:
        slack:
          spec:
            platform: slack
            credentialsSecretRef: {name: agent-kourier-slack}
            allowedNamespaces: [payments] # (1)!
    ```

    1. Only needed when a Binding in another namespace uses this connection.

    **6. Check the setup with a dry run.** Once a Binding names the channel ([Write a Binding](write-a-binding.md)),
    replay the channel's history through it, from the repository root:

    ```sh
    SLACK_BOT_TOKEN="$(sed -n 's/^botToken=//p' ~/.config/agent-kourier/slack-secret.env)" \
      go run ./cmd/agent-kourier dry-run --config ./my-bindings --binding payments/payments-assistant
    ```

    `--config` is a directory that holds the Binding's YAML. The dry run only reads Slack (`auth.test` and
    `conversations.history`) and starts no session, so a clean run shows that the bot token works, the bot is in the
    channel and the app can read its history. A rejected token exits 1. A failed read of the channel names Slack's
    error code and, for `not_in_channel`, `channel_not_found` and `missing_scope`, what to fix. The dry run does not use
    the app-level token: Agent Kourier's log line `slack: socket mode connected` shows that one works.
    [Tune triggers with a dry run](tune-triggers.md) explains the output.

    !!! warning "One consumer per app token"

        Slack sends each Socket Mode event to one of the open connections on an app token. A second process on the same
        token, such as a laptop sandbox, receives some of the events, and your install seems to ignore messages.
        Stop the other one first.

=== "Events API (HTTP)"

    !!! note "Not served yet"

        The broker serves Slack over Socket Mode only, and a ChatConnection has no setting for another mode. Use Socket Mode.

## What the app is allowed to do

Every Slack call Agent Kourier makes, and the scope in the manifest that allows it:

| Call | Scope |
|---|---|
| `auth.test` | none |
| `users.info`, with the email for the audit log | `users:read`, `users:read.email` |
| `chat.postMessage`, `chat.update`, `chat.postEphemeral` | `chat:write` |
| `chat.startStream`, `chat.appendStream`, `chat.stopStream` | `chat:write` |
| `reactions.add`, `reactions.remove` | `reactions:write` |
| `assistant.threads.setStatus` | `assistant:write` (`chat:write` may also do; see below) |
| `conversations.history`, `conversations.replies` | `channels:history`, `groups:history` |
| `conversations.members` | `channels:read`, `groups:read` |
| `conversations.list`, made only by `agent-kourier init`, to find the ID of a channel given by name | `channels:read`, `groups:read` |
| `usergroups.users.list` | `usergroups:read` when a Binding sets `interactions.approverGroups` |
| Events `app_mention`, `message.channels`, `message.groups` | `app_mentions:read`, `channels:history`, `groups:history` |
| Socket Mode | the app-level token, `connections:write` |

While an agent investigates an alert thread, Agent Kourier calls `assistant.threads.setStatus`, which shows Slack's assistant
status (`is working…`) in the thread. It does this only when `chat.progress` is not `steps` and `output` is `live`, and
never in a thread that a person's message started, whose reply streams instead. It sets the status when the turn starts and
again after each write, because a reply in the thread may take it off, and clears it when the turn ends.

The manifest asks for `assistant:write` because that is the scope with which we have seen the status render. The Slack Go
library that Agent Kourier uses says the method accepts `chat:write` or `assistant:write`, and that `assistant:write` is
being deprecated in favor of `chat:write` (unverified against Slack's own documentation). In our testing an app that had
only `chat:write` was answered `ok`; whether it showed the status was not checked. An app made from an older copy of the
manifest, without `assistant:write`, may get the status anyway or may be refused, and a refusal is only logged. If Slack answers with a code that says the app,
token or workspace cannot use the method (`missing_scope`, `not_allowed_token_type`, `not_allowed`, `access_denied`,
`feature_not_enabled`, `no_permission` or `unknown_method`; these codes are unverified, since none has been recorded from
Slack), Agent Kourier logs that once at info level and sets no status on that connection until it restarts. Replies are
unaffected.

In our testing (Slack's desktop web client, October 2026; mobile not checked), the status reached only clients that were open
when it was set, a reply in the thread removed it, and it had not expired after nearly three minutes. It rendered for an app
that had `assistant:write` with Slack's agent app feature switched off. To add the scope to an installed app, add it to the app's
bot scopes and reinstall the app; removing a scope and reinstalling did not remove it from the token (observed 2026-10-18),
so the token's scopes are not a check on the app's configuration.

The manifest enables no direct messages: it has no `im:` scopes or events. The manifest includes `usergroups:read`. If
your app was not made from this manifest, add that scope to the bot token's scopes and reinstall the app before using
`interactions.approverGroups`; a missing scope is a permanent group-read failure, so the affected press ends with a
fixed message, makes no decision or audit row, and leaves the approval pending.
The error log names the Binding and interaction. Transient Slack failures such as a timeout, server error or rate limit
keep the press for retry. Group IDs are read in the configured order without caching; the first error ends the check even
if a later group contains the person, while a member found in an earlier group may decide.

`approverGroups` takes user group IDs, which start with `S`. Slack's
[`usergroups.list`](https://docs.slack.dev/reference/methods/usergroups.list/) method returns each group's `id` and
`handle`, and the bot token can call it with `usergroups:read`. This reads the token from the file of step 4 without
putting it on a command line:

```sh
printf 'header = "Authorization: Bearer %s"\n' "$(sed -n 's/^botToken=//p' ~/.config/agent-kourier/slack-secret.env)" \
  | curl -sS -K - https://slack.com/api/usergroups.list | jq -r '.usergroups[] | "\(.id)  @\(.handle)"'
```

A workspace whose plan has no user groups cannot use `approverGroups`. Slack's page for `usergroups.users.list`, the
method Agent Kourier calls on each press, lists the error `plan_upgrade_required`, and Agent Kourier treats that code as
a permanent group-read failure.

## Related

- [ChatConnection reference](../reference/chatconnection.md)
- [Write a Binding](write-a-binding.md), which places an agent in a channel.

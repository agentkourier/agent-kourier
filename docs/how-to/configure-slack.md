# Configure Slack

For a platform engineer setting up the chat side: at the end, a Slack app has the scopes and events Agent Kourier
needs, its tokens are in a Kubernetes Secret, and a ChatConnection points at it.

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

    **4. Store the tokens.**

    ```sh
    kubectl -n agent-kourier create secret generic agent-kourier-slack \
      --from-literal=botToken="$SLACK_BOT_TOKEN" \
      --from-literal=appToken="$SLACK_APP_TOKEN"
    ```

    The key names are fixed: `botToken` and `appToken`.

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
| `conversations.history`, `conversations.replies` | `channels:history`, `groups:history` |
| `conversations.members` | `channels:read`, `groups:read` |
| Events `app_mention`, `message.channels`, `message.groups` | `app_mentions:read`, `channels:history`, `groups:history` |
| Socket Mode | the app-level token, `connections:write` |

The manifest enables no direct messages: it has no `im:` scopes or events.

## Related

- [ChatConnection reference](../reference/chatconnection.md)
- [Write a Binding](write-a-binding.md), which places an agent in a channel.

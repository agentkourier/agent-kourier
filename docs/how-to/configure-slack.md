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

    1. Under **Install App**, install the app to the workspace and copy the **Bot User OAuth Token** (`xoxb-`).
    2. Under **Basic Information**, **App-Level Tokens**, generate a token with the scope `connections:write` and copy
       it (`xapp-`). The manifest cannot make it.

    **3. Invite the bot and copy the channel ID.** In each channel a Binding will use, send `/invite @Agent Kourier`
    (or the display name you gave the bot). Copy the channel ID from the channel's details: it starts with `C`, or `G`
    for a private channel. A Binding takes the ID, not the name.

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

        Socket Mode spreads events across every open connection on an app token. A second process on the same
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

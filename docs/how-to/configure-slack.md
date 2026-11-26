# Configure Slack

For a platform engineer setting up the chat side: at the end, a Slack app has the scopes and events Agent Kourier
needs, its tokens are in a Kubernetes Secret, and a ChatConnection points at it.

Slack can deliver the app's events and button presses in one of two ways, and the ChatConnection's `spec.slack.mode` says which:

- **Socket Mode** (`socket`, the default when the field is unset): Agent Kourier opens a WebSocket to Slack, so nothing in your cluster
  needs to be reachable from the internet.
- **Events API over HTTP** (`http`): Slack sends each event and press as an HTTPS request, signed with the app's signing secret,
  to a URL of Agent Kourier's chat listener, a port of its own. The cluster must expose that port to the internet.

Either way Agent Kourier calls Slack's Web API with the bot token to post, so it needs to reach Slack on 443.

## Before you start

- Permission to install an app in the Slack workspace. Workspace settings can require an admin to approve each app. A
  member without that permission sees **Request to Install** instead of **Install**, and can copy the bot token only
  after an admin approves the request.
- A Slack app that no other running Agent Kourier uses (see the warning at the end of the steps). A team that brings an app of its own to the platform's broker does the same steps, and then follows [Let a team bring its own Slack app](team-slack-app.md).
- `kubectl` access to the namespace Agent Kourier runs in.
- For the check in the last step: Go, at the version in `go.mod`, and a checkout of the repository.
- For HTTP mode only: a host name that Slack can reach over HTTPS and that routes to your cluster, such as an Ingress controller's,
  with a certificate for it. For the chart's leader label, also the right to create a ValidatingAdmissionPolicy and its binding,
  which are cluster-scoped ([Security model](../explanation/security.md#the-chat-port)), on Kubernetes 1.33 or later. The policy is
  `admissionregistration.k8s.io/v1`. The chart checks no Kubernetes version for it, and its tests run it on 1.33 and 1.37 only.

=== "Socket Mode"

    In Socket Mode Agent Kourier opens a WebSocket to Slack, so nothing in your cluster needs to be reachable from the internet.
    It is the mode of a ChatConnection that does not set `spec.slack.mode`.

    **1. Create the app from the manifest.** Open <https://api.slack.com/apps>, choose **Create New App**, **From a
    manifest**, and paste
    [`hack/kind/sandbox/slack-app-manifest.yaml`](https://github.com/agentkourier/agent-kourier/blob/main/hack/kind/sandbox/slack-app-manifest.yaml).
    It names the app and its bot user **Agent Kourier** (`display_information.name` and
    `features.bot_user.display_name`). Change them if people should see another name. The manifest holds the scopes,
    the three bot events, interactivity and Socket Mode.

    **2. Make the two tokens.**

    Agent Kourier needs both tokens from the same Slack app. They have different jobs:

    | Token | Why Agent Kourier needs it | Secret key |
    |---|---|---|
    | Bot User OAuth Token, `xoxb-` | Calls Slack's Web API as the bot to post, update and stream agent replies, add reactions, read channel and thread history, and look up users and channel membership. | `botToken` |
    | App-level token, `xapp-` | Opens the Socket Mode WebSocket that delivers mentions, channel messages and button presses to Agent Kourier. This lets it receive Slack events without a public HTTP endpoint. | `appToken` |

    1. Under **Install App**, install the app to the workspace and copy the **Bot User OAuth Token** (`xoxb-`).
       Installing the app grants the bot permissions from the manifest. The bot token lets Agent Kourier use those
       permissions. The [scope table below](#what-the-app-is-allowed-to-do) lists the calls they allow.
    2. Open **Basic Information** and scroll to **App-Level Tokens**. Choose **Generate Token and Scopes**, give the
       token a name, add `connections:write`, and generate it. Copy the token (`xapp-`). Slack requires
       [this scope](https://docs.slack.dev/reference/scopes/connections.write/) to open a Socket Mode connection.
       The manifest enables Socket Mode, but you must generate this token separately.

    **Basic Information** is the settings page containing **App-Level Tokens**. Agent Kourier only needs the two tokens
    above. You do not need to copy the App ID, Client ID, Client Secret or Signing Secret from that page.

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
    kubectl -n agent-kourier create secret generic slack \
      --from-env-file="$HOME/.config/agent-kourier/slack-secret.env"
    ```

    kubectl reads the tokens from the file, so they are on no command line and in no shell history. The key names are
    fixed: `botToken` and `appToken`. Keep the file at mode 600 for the check in step 6, and delete it afterwards.

    The path is only an example: any file that only you can read works. Agent Kourier never reads it: kubectl does.

    If your cluster's Secrets come from a secrets manager (External Secrets, Sealed Secrets), create
    `slack` with the keys `botToken` and `appToken` that way instead, and skip the file. Step 6 then
    needs `SLACK_BOT_TOKEN` set some other way.

    **5. Declare the connection.** In the chart's values:

    ```yaml
    config:
      source: files # the ConfigMap route
      chatConnections:
        slack:
          spec:
            platform: slack
            credentialsSecretRef: {name: slack}
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
    `conversations.history`) and starts no session. A clean run shows that the bot token works, the bot is in the
    channel and the app can read its history.

    A rejected token exits 1. A failed read of the channel names Slack's error code and, for `not_in_channel`,
    `channel_not_found` and `missing_scope`, what to fix.

    The dry run does not use the app-level token: Agent Kourier's log line `slack: socket mode connected` shows that one works.
    [Tune triggers with a dry run](tune-triggers.md) explains the output.

    !!! warning "One consumer per app token"

        Slack sends each Socket Mode event to one of the open connections on an app token. A second process on the same
        token, such as a laptop sandbox, receives some of the events, and your install seems to ignore messages.
        Stop the other one first.

=== "Events API (HTTP)"

    !!! warning "Experimental"

        HTTP mode is tested end to end against the end-to-end tests' fake Slack, and has been tried once against Slack itself,
        on one installation reached through a tunnel: the URL check, a mention, a button press, a bot message that a trigger
        matched, requests with a wrong, missing or old signature (answered 401), and a message sent while the only replica was being replaced.
        What this tab says of Slack's settings pages comes from that test. Slack's retry schedule, and when Slack gives up on an
        app, come from Slack's documentation.

        HTTP mode needs 0.5.0 or later: the 0.4.0 chart's values schema refuses `chatListener` and `networkPolicy.chatFrom`, and its
        broker does not know `spec.slack.mode`.

    In HTTP mode Slack sends the app's events and button presses to Agent Kourier as HTTPS requests, and Agent Kourier
    answers them on its chat listener. The app has no app-level token. Its signing secret proves that a request is Slack's.

    How Slack sends, retries and gives up is Slack's own behaviour. What the test above saw of it is in the note after step 8,
    and the rest is from Slack's documentation.

    **1. Choose the request URL.** Every request of a ChatConnection in HTTP mode goes to one path:

    ```text
    https://<host>/chat/slack/<namespace>/<name>
    ```

    `<namespace>` and `<name>` are the ChatConnection's. The connection `slack` in the namespace `agent-kourier`, behind the host
    `chat.example.com`, is `https://chat.example.com/chat/slack/agent-kourier/slack`. Events and button presses share the URL:
    Agent Kourier reads a form-encoded request as a press (its `payload` field), and any other as an Events API payload in JSON.

    **2. Create the app from the manifest.** Open <https://api.slack.com/apps>, choose **Create New App**, **From a
    manifest**, and paste
    [`hack/kind/sandbox/slack-app-manifest.yaml`](https://github.com/agentkourier/agent-kourier/blob/main/hack/kind/sandbox/slack-app-manifest.yaml)
    with its `settings` block replaced by this one, your URL in both places:

    ```yaml
    settings:
      event_subscriptions:
        request_url: https://chat.example.com/chat/slack/agent-kourier/slack
        bot_events:
          - app_mention
          - message.channels
          - message.groups
      interactivity:
        is_enabled: true
        request_url: https://chat.example.com/chat/slack/agent-kourier/slack
      org_deploy_enabled: false
      socket_mode_enabled: false
      token_rotation_enabled: false
    ```

    The scopes and the three bot events are the same as in Socket Mode. `socket_mode_enabled: false` and the two
    `request_url` fields are what differ.

    Slack checks an Events request URL by sending it a `url_verification` request, which Agent Kourier can answer only once
    it serves the connection (step 7). It serves it only once the Secret of step 5 holds the bot token and the signing
    secret, and the bot token exists only once the app is installed (step 3). So the block above is the app's end state,
    and a new app gets its URLs in step 7.

    The manifest editor refuses `event_subscriptions` and `interactivity` that have neither a `request_url` nor Socket Mode.
    So create a new app from the block with two changes: `socket_mode_enabled: true`, and the two `request_url` lines left
    out. The app needs no app-level token for that, and step 7 turns Socket Mode off.

    Whether Slack takes a manifest whose URL it cannot check yet was not tried.

    For an app you already have, the same settings are in its pages: **Socket Mode** off, **Event Subscriptions** on with the
    request URL and the three bot events, and **Interactivity & Shortcuts** on with the same URL.

    **3. Copy the bot token and the signing secret.**

    | Credential | Why Agent Kourier needs it | Secret key |
    |---|---|---|
    | Bot User OAuth Token, `xoxb-` | Calls Slack's Web API as the bot, as in Socket Mode. | `botToken` |
    | Signing Secret | Checks that each request on the chat listener was signed by this app. It is never sent to Slack. | `signingSecret` |

    1. Under **Install App**, install the app to the workspace and copy the **Bot User OAuth Token** (`xoxb-`).
    2. Open **Basic Information**, and under **App Credentials** reveal and copy the **Signing Secret**.

    You do not need an app-level token, and Agent Kourier does not read one in HTTP mode.

    **4. Invite the bot and copy the channel ID**, as in Socket Mode: `/invite @Agent Kourier` in each channel a Binding will
    use, and copy the channel ID (`C…`) from the channel's details.

    **5. Store the credentials.** In a file that only you can read, as in Socket Mode:

    ```sh
    mkdir -p ~/.config/agent-kourier
    touch ~/.config/agent-kourier/slack-secret.env && chmod 600 ~/.config/agent-kourier/slack-secret.env
    ```

    Write the two values, one `key=value` per line, with no quotes:

    ```dotenv
    botToken=xoxb-REPLACE
    signingSecret=REPLACE
    ```

    ```sh
    kubectl -n agent-kourier create secret generic slack \
      --from-env-file="$HOME/.config/agent-kourier/slack-secret.env"
    ```

    The key names are fixed by the mode: `botToken` and `signingSecret`. A Secret that lacks one, or holds it empty, stops
    the broker's own connection from starting (exit code 2: [Troubleshoot](troubleshoot.md#the-pod-exits-with-code-2)), as a
    missing `appToken` does in Socket Mode. Keep the file for the check in step 8, and delete it afterwards.

    **6. Declare the connection and open the chat listener.** In the chart's values:

    ```yaml
    config:
      source: files # the ConfigMap route; on custom resources the ChatConnection's spec is the same
      chatConnections:
        slack:
          spec:
            platform: slack
            credentialsSecretRef: {name: slack}
            slack: {mode: http}
    chatListener:
      enabled: true # (1)!
      ingress:
        enabled: true # (2)!
        className: "" # (3)!
        host: chat.example.com
        tls: [{hosts: [chat.example.com], secretName: chat-example-com-tls}] # (4)!
    networkPolicy:
      chatFrom: # (5)!
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress-nginx}}
    ```

    1. Opens the chat listener (`AGENTKOURIER_CHAT_LISTEN`) on `chatListener.port`, 8081, and adds the Service
       `<fullname>-chat`, which selects the leader's pod, and, with `rbac.create`, the Role and the ValidatingAdmissionPolicy
       that let the leader label its pod ([Chat listener](https://github.com/agentkourier/agent-kourier/blob/main/charts/agent-kourier/README.md#chat-listener)).
       Without the listener a connection in HTTP mode is not run: the broker's own connection fails the start (exit code 2),
       and a team's connection is not started.
    2. An Ingress, `<fullname>-chat`, that sends the path prefix `/chat/` to the chat Service and nothing else. Leave it off to
       expose the Service another way (`chatListener.service.type: LoadBalancer`, an Ingress of your own, or an HTTPRoute as in
       [With a Gateway API HTTPRoute instead of the Ingress](receive-webhooks.md#with-a-gateway-api-httproute-instead-of-the-ingress)).
    3. The IngressClass; empty leaves it to the cluster's default.
    4. A TLS Secret for the host, which you make, or the annotations of a certificate controller in `chatListener.ingress.annotations`.
       Slack sends its requests only to HTTPS URLs (Slack's documentation).
    5. Who may reach the chat port when `networkPolicy.enabled` is true, which then requires it: here, the pods of an
       ingress controller in the namespace `ingress-nginx`. The render refuses `chatFrom` without the chat listener.

    Install or upgrade with these values. When the leader serves the connection its log has
    `the chat listener serves the connection`, with the `path`, and `slack: http mode taking requests`.

    **7. Let Slack check the URL.**

    1. If the app was created with Socket Mode on, turn it off on the **Socket Mode** page first. Slack shows a notice that
       the features without a request URL need one. In the test, **Interactivity** was off afterwards.
    2. In the app's **Event Subscriptions** page, enter the request URL (or, if the manifest had it, have Slack check it
       again). Slack sends a `url_verification` request as soon as the URL is entered. Agent Kourier checks its signature and
       answers it with its challenge, and Slack shows the URL as **Verified**. Save the page.
    3. Under **Interactivity & Shortcuts**, turn Interactivity on if it is off, set the same URL, and save. The page shows
       no check of that URL: a button press is the check.

    The **Event Subscriptions** page also has a **Delayed Events** setting, whose text on the page says it retries missed
    deliveries slowly over 24 hours. Agent Kourier has not been tried with it on.

    Only the leading replica answers, and only while it serves the connection: the broker must be running, leading, and its
    pod must carry the leader label, which the chart's chat Service selects. If the check fails, see
    [Troubleshoot](troubleshoot.md#slack-cannot-verify-the-request-url).

    **8. Check the setup with a dry run**, as in Socket Mode's step 6, once a Binding names the channel. The dry run uses
    the bot token alone. The metric `agentkourier_slack_http_requests_total` shows whether the signing secret is right: a
    message in the channel counts as `acknowledged`, and a wrong secret as `bad_signature`
    ([Metrics](../reference/metrics.md#operations-intake-delivery-and-triggers)).

    !!! note "What Slack does when Agent Kourier does not answer"

        Agent Kourier answers 200 once it has taken an event or press, or found nothing in it to take. It answers 503 when it
        has not taken it within 2 seconds or cannot take it, so that Slack sends it again.

        A request that comes while no replica leads gets no answer from Agent Kourier. The chat Service has no endpoint, so the
        Ingress or the load balancer answers it, or the connection fails.

        Slack's documentation says that it waits about 3 seconds for an answer, sends an unanswered event again a few times
        over several minutes with an `X-Slack-Retry-Num` header, and can turn an app's event subscriptions off when most of its
        requests fail for a long time. The schedule and the turning off are not checked here against Slack.

        In the one test against Slack, a message was sent while the only replica was being replaced. Its first deliveries were
        refused at the tunnel in front of the chat Service, a retry with `X-Slack-Retry-Num` reached the new leader within a
        second of the post, and the thread got one reply.

        `agentkourier_slack_http_retries_total` counts the retries that reach Agent Kourier.
        [High availability](../explanation/high-availability.md#slack-in-http-mode) says when no replica answers.

## What the app is allowed to do

Every Slack call Agent Kourier makes, and the scope in the manifest that allows it:

| Call | Scope |
|---|---|
| `auth.test` | none |
| `bots.info`, made by the leader for each ChatConnection it serves, the broker's own included, but only when it serves a team ChatConnection, to tell which Slack app the bot belongs to: again at each look until Slack answers, and for a team's connection again after its ChatConnection or its Secret changes | `users:read` (Slack's documentation for the method; not verified here) |
| `users.info`, with the email for the audit log | `users:read`, `users:read.email` |
| `chat.postMessage`, `chat.update`, `chat.postEphemeral` | `chat:write` |
| `chat.startStream`, `chat.appendStream`, `chat.stopStream` | `chat:write` |
| `reactions.add`, `reactions.remove` | `reactions:write` |
| `assistant.threads.setStatus` | `assistant:write` (`chat:write` may also do; see below) |
| `conversations.history`, `conversations.replies` | `channels:history`, `groups:history` |
| `conversations.members` | `channels:read`, `groups:read` |
| `conversations.info`, made by the leader to [verify each Binding](troubleshoot.md#a-binding-is-not-ready) | `channels:read`, `groups:read` |
| `conversations.list`, made only by `agent-kourier init`, to find the ID of a channel given by name | `channels:read`, `groups:read` |
| `usergroups.users.list`, made when someone presses a tool approval on a Binding that sets `interactions.approverGroups`, and by the leader to [verify each such Binding](troubleshoot.md#a-binding-is-not-ready) | `usergroups:read` when a Binding sets `interactions.approverGroups` |
| Events `app_mention`, `message.channels`, `message.groups` | `app_mentions:read`, `channels:history`, `groups:history` |
| Socket Mode, in Socket Mode only | the app-level token, `connections:write` |
| Events API and interactivity requests, in HTTP mode only | none: Slack signs them with the app's signing secret |

The `message.channels` and `message.groups` events carry a person's replies in a thread Agent Kourier owns and, for a Binding with a
[`personMessage` trigger](../reference/chat-triggers.md#a-persons-message), each message a person posts outside a thread. No other scope or event is needed for it.

On each pass of [Binding verification](../reference/metrics.md#binding-verification), the leader calls `conversations.info` once for each Binding's channel.
It also calls `usergroups.users.list` up to once for each approver group that the Bindings setting `interactions.approverGroups` name. A group that
several Bindings name is read once, unless a read is cut short at its Binding's 8 second bound: then the next Binding that needs the group reads it again.

A Binding's read stops at the first group that fails. None is made while the chat credentials Secret differs from the tokens in use or is
unusable: gone, empty, missing a key, or of a type that is never read.

An app without the scope reads `False/MissingScope` on `ChannelJoined`, or on `ApproversReadable` for the groups. The calls only report, and turns still run.

Agent Kourier reads the credentials of the broker's own connection when it starts: `botToken` and `appToken` in Socket Mode, or `botToken` and
`signingSecret` in HTTP mode. A new value written to its Secret is not used until a restart. Until then `ChannelJoined` (and `ApproversReadable`,
for a Binding with approver groups) reads `Unknown/ChatCredentialsChanged`, and the probe does not call Slack.

A team ChatConnection restarts by itself at the first look after Agent Kourier has the change to its Secret or its spec, and the looks come
every 5 seconds. Until then its Bindings read the same reason when the tokens in its Secret differ from the ones it runs on.

While an agent investigates an alert thread, Agent Kourier calls `assistant.threads.setStatus`, which shows Slack's assistant
status (`is working…`) in the thread. It does this only when `chat.progress` is not `steps` and `output` is `live`, and
never in a thread that a person's message started, whose reply streams instead. It sets the status when the turn starts and
again after each write but the last, because a reply in the thread may take it off, and clears it when the turn ends.

The manifest asks for `assistant:write` because that is the scope with which we have seen the status render. The Slack Go
library that Agent Kourier uses says the method accepts `chat:write` or `assistant:write`, and that `assistant:write` is
being deprecated in favor of `chat:write` (unverified against Slack's own documentation).

In our testing an app that had only `chat:write` was answered `ok`, and whether it showed the status was not checked. An app
made from an older copy of the manifest, without `assistant:write`, may get the status anyway or may be refused, and a refusal
is only logged.

If Slack answers with a code that says the app, token or workspace cannot use the method, Agent Kourier logs that once at info
level and sets no status on that connection until it restarts. Replies are unaffected. The codes are `missing_scope`,
`not_allowed_token_type`, `not_allowed`, `access_denied`, `feature_not_enabled`, `no_permission` or `unknown_method`, and they
are unverified, since none has been recorded from Slack.

In our testing (Slack's desktop web client, October 2026; mobile not checked), the status reached only clients that were open
when it was set, a reply in the thread removed it, and it had not expired after nearly three minutes. It rendered for an app
that had `assistant:write` with Slack's agent app feature switched off.

To add the scope to an installed app, add it to the app's bot scopes and reinstall the app. In a test, removing a scope and
reinstalling did not remove it from the token, so the token's scopes are not a check on the app's configuration.

The manifest enables no direct messages: it has no `im:` scopes or events.

The manifest includes `usergroups:read`. If your app was not made from this manifest, add that scope to the bot token's scopes
and reinstall the app before using `interactions.approverGroups`.

A missing scope is a permanent group-read failure. The affected press ends with a fixed message, makes no decision or audit row,
and leaves the approval pending. The error log names the Binding and interaction.

Transient Slack failures such as a timeout, server error or rate limit keep the press for retry. Group IDs are read in the
configured order without caching. The first error ends the check even if a later group contains the person, while a member
found in an earlier group may decide.

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

The Binding's `ApproversReadable` condition reads `False/GroupsUnreadable` for it. That a workspace without user groups gets this code has not been checked against a live workspace.

## Related

- [ChatConnection reference](../reference/chatconnection.md)
- [Let a team bring its own Slack app](team-slack-app.md), which has the broker serve a team's app from the team's namespace.
- [Write a Binding](write-a-binding.md), which places an agent in a channel.

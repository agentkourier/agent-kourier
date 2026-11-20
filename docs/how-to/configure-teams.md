# Configure Microsoft Teams

This guide is for a platform engineer setting up the chat side on Microsoft Teams. At the end, a bot registered in your
Microsoft Entra tenant is installed in a team, its client secret is in a Kubernetes Secret, a ChatConnection points at it,
and a Binding names one of the team's channels. The last part sends Alertmanager's alerts to that channel through
Workflows, where a `botMessage` trigger starts an investigation.

!!! warning "Experimental"

    Microsoft Teams support needs a release later than 0.5.0: the 0.5.0 broker refuses `platform: teams`. The tests
    of Agent Kourier run it against fakes of Microsoft's services. A full install of the broker has not yet been run
    against Microsoft Teams. Parts of it were checked on one test tenant in November 2026, in Teams' web client, by a test
    program that sent Agent Kourier's own messages and cards. The steps below mark what was checked
    ("measured"), what comes from Microsoft's documentation, and what was not checked. Microsoft's portals change: each
    step in a Microsoft UI is as it was in November 2026.

## Choose how Teams reaches the broker

Two settings of the ChatConnection decide what must be reachable from the internet. Make both choices before you start,
because they decide whether you need a public HTTPS route to the broker.

**How Teams delivers the bot's activities** (mentions, posts in the channel, card presses): `spec.teams.mode`.

- `http`, the default: Teams sends each activity as an HTTPS request to the leader's chat listener, at
  `https://<host>/chat/teams/<namespace>/<name>`. Microsoft's documentation recommends an HTTPS messaging endpoint in
  production. Unlike Slack, where the default is Socket Mode, the default here is `http`.
- `socket`: the leader opens an outbound WebSocket to Teams in each region (`amer`, `emea`, `apac`), and nothing needs to
  reach the cluster for activities. Microsoft calls Socket mode a public preview, available only to newly created bots, and
  says a bot published to the Teams Store must use HTTPS. Only the leader connects. **Teams does not queue what it sends
  while no socket is ready, and does not send it to an HTTPS endpoint instead** (measured): a mention during a restart or a
  change of leader is lost as an activity. What Microsoft Graph gives back depends on the alert route (below):

    - With a `notificationURL`, while the leader keeps running, a gap of the sockets lost nothing on the test tenant:
      Graph notified the posts of the gap, mentions included (measured). Across a restart or a change of leader, the
      broker reads nothing back when its run starts. What was posted meanwhile reaches it only if Graph sends again the
      notifications it could not deliver, which Microsoft's documentation says it does for up to 4 hours (not measured),
      or if Graph reports them as `missed`, after which the broker reads the channel's last 10 minutes.
    - When it polls, the broker reads back up to the last 10 minutes of the gap. At the start of a run it resumes each
      channel's delta from the position that the last run stored, for up to an hour, so the threads begun meanwhile, a
      mention that begins one included, come back; replies in older threads come back from the 10 minutes before the run
      ([Polling without a notificationURL](../reference/platforms.md#polling-without-a-notificationurl),
      [Catch-up reads](../reference/platforms.md#catch-up-reads)).

**How the broker reads other apps' posts**, such as an alert card that Workflows posts: `spec.teams.notificationURL`.
Teams sends a bot no post of another app, in either mode (measured, and Microsoft's documentation says so), so the broker
reads them from Microsoft Graph.

- `notificationURL` set: Microsoft Graph notifies the leader of each new message of a bound channel, at that URL, which
  is the connection's route on the chat listener. That needs public HTTPS to the leader.
- unset: the leader polls Graph for each bound channel's new messages every 10 seconds. That needs no public route, but it
  makes a steady stream of reads, and it reads replies only in each channel's 3 newest threads of the last hour. The pace
  and its budget are in [Polling without a notificationURL](../reference/platforms.md#polling-without-a-notificationurl)
  and [Limits](../reference/limits.md#run-the-broker).

| | `notificationURL` set | `notificationURL` unset |
|---|---|---|
| `http` | Public HTTPS to the chat listener, for Teams and for Graph, on the same URL. | Public HTTPS to the chat listener, for Teams. Graph is polled. |
| `socket` | Public HTTPS to the chat listener, for Graph alone. | Nothing public: no chat listener is needed. |

The broker calls Microsoft's services on 443 in every case: Microsoft Entra (`login.microsoftonline.com`), the Bot
Connector (`smba.trafficmanager.net` and the service URL each activity names), Microsoft Graph (`graph.microsoft.com`) and
the Bot Framework's signing keys (`login.botframework.com`), and in `socket` mode `<region>.botapi.skype.com` and the
socket host it is given. With the chart's `networkPolicy.enabled`, the egress rule `networkPolicy.slack` is what allows
them: despite its name, it opens port 443 to every address outside the private ranges.

## Before you start

- A Microsoft 365 tenant whose Teams lets custom apps be uploaded. In the Teams admin center, a tenant admin turns on
  **Upload custom apps** in the app setup policy that applies to you (on the test tenant, the global policy).
- The right to register an application in the tenant's Microsoft Entra ID, and to add an app to the team, as its owner.
- `kubectl` access to the namespace Agent Kourier runs in, and Helm.
- Unless you run `socket` mode with polling: a host name that Microsoft can reach over HTTPS and that routes to your
  cluster, such as an Ingress controller's, with a certificate. For the chart's leader label, you also need the right to
  create a ValidatingAdmissionPolicy and its binding, which are cluster-scoped
  ([Security model](../explanation/security.md#the-chat-port)).

## 1. Register the bot

A Teams bot is a Microsoft Entra application. Agent Kourier takes one tenant per ChatConnection, and refuses an
activity from any other tenant with 403.

1. In the Teams Developer Portal (<https://dev.teams.microsoft.com>), under **Tools**, open **Bot management** and create
   a bot. On the test tenant this made a Teams-managed bot with no Azure subscription, and an app registration in
   Microsoft Entra ID with the bot's ID as its application (client) ID (measured).
2. The Developer Portal registered that app as multi-tenant ("Multiple organizations"), and Microsoft's documentation
   says new multi-tenant bots are not supported after 31 July 2025. In the Microsoft Entra admin center, open **App
   registrations**, then the bot's app, and set its supported account types to this tenant only. On the test tenant the
   app read "Single tenant only" afterwards, and mentions, cards, presses and Graph reads all worked with tokens from the
   tenant's endpoint (measured). An Azure Bot resource of type single tenant should work the same way, but was not tried.
3. From the app's **Overview**, copy the **Application (client) ID**, which becomes `spec.teams.appID`, and the
   **Directory (tenant) ID**, which becomes `spec.teams.tenantID`. Write both in lowercase: the broker compares IDs byte
   for byte, and the API server refuses capitals.
4. Under **Certificates & secrets**, add a client secret and copy its **Value**, which the portal shows once. (The steps
   in this item are Microsoft's UI and were not checked here.) The secret expires on the date you choose: rotate it
   before then, by adding a new secret, updating the Secret `teams` and restarting the broker.

Agent Kourier gets two tokens from `https://login.microsoftonline.com/<tenantID>` with that secret: one for the Bot
Connector, to post, and one for Microsoft Graph, to read. Neither needs an API permission on the app registration, and
neither needs a tenant admin's consent. The Graph token reads only under the permission that installing the app in a team
grants (step 4).

## 2. Store the client secret

The ChatConnection's Secret has one key, `clientSecret`, in either mode. Put the value in a file that only you can read:

```sh
mkdir -p ~/.config/agent-kourier
touch ~/.config/agent-kourier/teams-secret.env && chmod 600 ~/.config/agent-kourier/teams-secret.env
```

Write `clientSecret=<the value>` in it with an editor, with no quotes, and make the Secret from the file:

```sh
kubectl -n agent-kourier create secret generic teams \
  --from-env-file="$HOME/.config/agent-kourier/teams-secret.env"
```

kubectl reads the value from the file, so it is on no command line and in no shell history. Delete the file afterwards.
If your cluster's Secrets come from a secrets manager, create `teams` with the key `clientSecret` that way instead. A
Secret without the key, or with it empty, stops the broker's own connection from starting (exit code 2:
[Troubleshoot](troubleshoot.md#the-pod-exits-with-code-2)).

Agent Kourier sends the value to Microsoft Entra as the Secret holds it, and trims nothing: a value written with
`echo "$S" > file` and stored with `--from-file=clientSecret=file` ends in a newline, which is sent as part of the secret.
Entra then refuses the token request (not checked here), and a broker whose own connection this is exits with code 1
and `agent-kourier: teams rejected the tokens: <code>`, where `<code>` is Entra's OAuth error code
([Troubleshoot](troubleshoot.md#the-pod-exits-with-code-1)). If you make the Secret with `--from-file`, write the file
with `printf '%s'`, which adds no newline.

## 3. Write the app package

The bot reaches a team as a Teams app: a zip of `manifest.json` and two icons. This manifest is the one the
[`prompt-agent-teams` recipe](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/prompt-agent-teams/README.md)
ships:

```json title="teams-app/manifest.json"
--8<-- "prompt-agent-teams/teams-app/manifest.json"
```

- The bot's ID in `bots`, and `webApplicationInfo.id`: the bot's application (client) ID from step 1.
- `id`: the Teams app's own ID, a new GUID of your own (`uuidgen | tr A-Z a-z`), not the bot's.
- `scopes: ["team"]`: the bot works in a team's channels. Agent Kourier serves no personal or group chat.
- `authorization.permissions.resourceSpecific`: the resource-specific permission `ChannelMessage.Read.Group`, of type
  `Application`. With it, Teams sends the bot every person's post in the team's standard channels, not only mentions
  (measured), and Microsoft Graph lets the app read the channels' messages: the alerts that other apps post, a thread's
  earlier replies, and its own posts that the outbox looks for after a restart. Without it, every read of Graph is
  refused (see [Troubleshoot](#troubleshoot)).
- `supportsChannelFeatures: "tier1"`: on the test tenant, a manifest 1.30 with a bot in the `team` scope failed the
  upload's validation without it (measured). In the Developer Portal it is under **App content**, **Channel support**.
  Only standard channels were tried.

The manifest above passes Microsoft's JSON schema for manifest version 1.30, the one its `$schema` names: checked with
Python's `jsonschema` (draft 4) on 18 November 2026, with no errors. That is not Teams' own validation, which also runs at
upload. Add the icons that it names: `color.png`, 192
by 192 pixels, and `outline.png`, 32 by 32, white on a transparent background (Microsoft's documentation). Zip the three
files with nothing above them in the archive:

```sh
zip agent-kourier-teams.zip manifest.json color.png outline.png
```

The Developer Portal's **Apps** page builds a package from a form instead. Importing this one there was not tried.

## 4. Install the app in a team

Install the app in the team whose channels the Bindings will name. In Teams, **Apps**, **Manage your apps**, **Upload an
app**, **Upload a custom app** takes the zip (Microsoft's UI, not checked here). On the test tenant the app was installed
with the Developer Portal's **Preview in Teams**, and the team listed it under **Manage team**, **Apps**, as a custom app
(measured).

The install grants `ChannelMessage.Read.Group` in that team: the person who installs it consents for the team, and no tenant admin
does (Microsoft's documentation, which names the team's owner in some places and a member in others). On the test tenant no separate consent screen appeared, and Graph read the channel's messages
right after the install, with no admin consent (measured). A tenant's admin can stop team owners from granting
resource-specific permissions (Microsoft's documentation). The broker then logs Graph's 403 with the fix.

The broker learns the channel's team, the team's Microsoft Entra group, and where to post from the activities that
Teams sends it, and keeps them in its store ([Stored data](../reference/stored-data.md)). Until an activity of a channel
has come, the broker cannot read the channel from Graph, nor check that the bot is in it. Once the connection runs, post
anything in each bound channel, or mention the bot there.

## 5. Copy the channel ID

A Binding names a channel by its ID, `19:` followed by letters, digits, `.`, `_` or `-`, then `@thread.tacv2` (or
`@thread.skype`), never by its name. In Teams, the channel's **More options** (`…`), **Get link to channel** gives a link
that holds the ID with `:` written `%3a` and `@` written `%40` (Microsoft's UI, not checked here). Decode those two. The
first channel of a team, its primary channel, has the team's ID as its own (measured, and Microsoft's documentation).

## 6. Declare the connection

Declare it in the chart's values, in the form of step 6 of the HTTP tab of [Configure Slack](configure-slack.md). The `teams`
block holds the tenant, the app and the two choices. Pick the tab of your combination:

=== "http, polling"

    ```yaml
    config:
      source: files # the ConfigMap route; on custom resources the ChatConnection's spec is the same
      chatConnections:
        teams:
          spec:
            platform: teams
            credentialsSecretRef: {name: teams}
            teams:
              tenantID: 11111111-1111-4111-8111-111111111111
              appID: 22222222-2222-4222-8222-222222222222
    chatListener:
      enabled: true
      ingress:
        enabled: true
        className: ""
        host: chat.example.com
        tls: [{hosts: [chat.example.com], secretName: chat-example-com-tls}]
    networkPolicy:
      chatFrom: # with networkPolicy.enabled
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress-nginx}}
    ```

=== "http, Graph notifications"

    ```yaml
    config:
      source: files
      chatConnections:
        teams:
          spec:
            platform: teams
            credentialsSecretRef: {name: teams}
            teams:
              tenantID: 11111111-1111-4111-8111-111111111111
              appID: 22222222-2222-4222-8222-222222222222
              notificationURL: https://chat.example.com/chat/teams/agent-kourier/teams # (1)!
    chatListener:
      enabled: true
      ingress:
        enabled: true
        className: ""
        host: chat.example.com
        tls: [{hosts: [chat.example.com], secretName: chat-example-com-tls}]
    networkPolicy:
      chatFrom: # with networkPolicy.enabled
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress-nginx}}
    ```

    1. The connection's own route: `https`, with the path `/chat/teams/<namespace>/<name>` of this ChatConnection. The API
       server checks the form of the path, and the broker checks that it names this connection. It is the same URL that
       Teams sends activities to.

=== "socket, polling"

    ```yaml
    config:
      source: files
      chatConnections:
        teams:
          spec:
            platform: teams
            credentialsSecretRef: {name: teams}
            teams:
              tenantID: 11111111-1111-4111-8111-111111111111
              appID: 22222222-2222-4222-8222-222222222222
              mode: socket
    ```

    No chat listener and no Ingress: the leader opens the sockets and polls Graph.

=== "socket, Graph notifications"

    ```yaml
    config:
      source: files
      chatConnections:
        teams:
          spec:
            platform: teams
            credentialsSecretRef: {name: teams}
            teams:
              tenantID: 11111111-1111-4111-8111-111111111111
              appID: 22222222-2222-4222-8222-222222222222
              mode: socket
              notificationURL: https://chat.example.com/chat/teams/agent-kourier/teams
    chatListener:
      enabled: true
      ingress:
        enabled: true
        className: ""
        host: chat.example.com
        tls: [{hosts: [chat.example.com], secretName: chat-example-com-tls}]
    networkPolicy:
      chatFrom: # with networkPolicy.enabled
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress-nginx}}
    ```

    The chat listener takes Microsoft Graph's requests alone on the route, and answers anything else 400.

The chat listener, its Service that selects the leader's pod by a label, the Ingress and `networkPolicy.chatFrom` are
the same as for Slack's HTTP mode: step 6 of the HTTP tab of [Configure Slack](configure-slack.md) explains each value, and the chart's
[Chat listener](https://github.com/agentkourier/agent-kourier/blob/main/charts/agent-kourier/README.md#chat-listener)
section explains the leader label. A connection that needs the chat listener and has none is not run: the broker's own
connection stops the start with a wiring error that says to set `AGENTKOURIER_CHAT_LISTEN`. On custom resources, apply
the ChatConnection yourself and set `config.chatConnection: agent-kourier/teams`, as the recipe does.

Install or upgrade with these values. In `http` mode the leader logs `the chat listener serves the connection`, with the
`path`, and `teams: taking activities over https`. In `socket` mode it logs `teams: socket ready` for each region that
takes a socket.

The broker builds the platform of its own connection when it starts and when a replica takes the lead, so a later edit of its
`spec.teams` (another `mode`, a `notificationURL` added or removed, another tenant or app) applies when Agent Kourier restarts: on
the ConfigMap route the chart rolls the pod when its ConfigMap changes, unless `config.existingConfigMap` is set or
`config.rollOnChange` is `false`, and on custom resources it takes a restart of the Deployment. Until then the leader logs, once for
each edit it reads that leaves `spec.teams` other than the one the connection runs on, a WARN that names the connection in its
`connection` attribute,
`chat connection: its spec.teams was edited, and the running connection still uses the spec.teams it started with; a restart of the broker applies the edit`,
and an INFO, `chat connection: its spec.teams is the one the running connection started with again`, once the spec is back to the one it runs on.

## 7. Point Teams at the broker

In the Developer Portal, open the bot under **Bot management** and its **Configure** page (measured on the test tenant):

- `http` mode: set the delivery method to the HTTPS endpoint, and the endpoint address to
  `https://<host>/chat/teams/<namespace>/<name>`, for example `https://chat.example.com/chat/teams/agent-kourier/teams`.
- `socket` mode: set the delivery method to **Socket mode**. The page marked it "New, development only" on 17 November
  2026, and the switch took about a minute to take effect (measured). A
  region that refuses the bot's socket answers 403 "Socket mode is not enabled for this bot.", which the broker logs and
  tries again every 5 minutes, give or take a fifth, while the other regions serve. On the test tenant `apac` answered
  that while `amer` and `emea` worked (measured).

Only the leader answers Teams, and only while it serves the connection. Teams sends an activity that was answered 503
again once, about 0.6 seconds later (measured): [Metrics](../reference/metrics.md#operations-intake-delivery-and-triggers)
counts both.

## 8. Write a Binding

A Binding on a teams connection is written as on Slack ([Write a Binding](write-a-binding.md)), with Microsoft Teams'
IDs:

```yaml
spec:
  chat:
    connectionRef: {name: teams, namespace: agent-kourier}
    channel: 19:a1b2c3d4e5f6@thread.tacv2
```

A mention's `user` is the person's Microsoft Entra object ID, and a `botMessage` trigger's `from.id` is the Microsoft
Entra application ID of the app that posts, each a GUID in lowercase ([Chat IDs](../reference/platforms.md#chat-ids)).
A Slack ID on a teams connection, or a feature that Teams does not serve, is refused when the broker reads the Binding
with its ChatConnection.

What works and what differs from Slack is in [Features](../reference/platforms.md#features), checked against the broker's
table of each platform. In short:

- **Works:** mentions and thread replies, live replies, questions (`interactions.askUser`), tool approvals
  (`interactions.toolApprovals`) and the feedback buttons, as Adaptive Cards that a member of the channel's team presses;
  `botMessage` triggers, in either mode.
- **Differs:** a live reply is one message that is edited, not a stream, and shows the agent's tool calls as a list
  ([What Microsoft Teams shows while the agent works](../reference/platforms.md#what-microsoft-teams-shows-while-the-agent-works)).
  Teams has no ephemeral messages, so what a person who pressed a button is told shows as a banner under the card
  ([Buttons on Microsoft Teams](../reference/platforms.md#buttons-on-microsoft-teams)). Another app's post comes through
  Microsoft Graph, and the text of its cards is the trigger field `body`.
- **Does not work:** `interactions.approverGroups` is refused (`UnsupportedOnPlatform`); there is no acknowledgment
  reaction and no busy mark. `agent-kourier dry-run` reads only Slack, so a trigger cannot be replayed against a Teams
  channel's history: pin it with [trigger evals](recipes.md) instead.

## 9. Check the setup

1. Post in each bound channel, or mention the bot there, so that the broker learns the channel's team.
2. The Binding's conditions: `ChannelJoined` reads the Bot Connector's list of the channels of the channel's team, and
   is `True` when the list names the channel. Before an activity of the channel has come, it is `Unknown` with
   `ChatUnavailable`, and the leader's log has the cause: the channel's team is not known yet.

    ```sh
    kubectl -n agent-kourier wait --for=condition=Ready akbinding/<binding> --timeout=120s
    ```

3. Mention the bot in the channel and look for the reply in the thread.
   `agentkourier_teams_requests_total{outcome="accepted"}` counts the activities taken over HTTPS, and
   `agentkourier_teams_socket_envelopes_total` those taken over a socket
   ([Metrics](../reference/metrics.md#operations-intake-delivery-and-triggers)).
4. Microsoft Graph: `agentkourier_teams_graph_channels` counts each bound channel as `subscribed` (with a
   `notificationURL`) or `polling` (without one) once the broker knows its team. The leader logs `teams: subscribed to a
   bound channel's messages on Microsoft Graph` for each subscription. Making one took about 8 seconds on the test tenant,
   while Graph checked the URL (measured).

## Send Alertmanager alerts to a channel

Alertmanager posts to Teams through a Workflows webhook, a flow that posts what it is sent to one channel. The post's
sender is the Workflows app, and Teams does not send it to the bot, so the broker reads it from Microsoft Graph, as the
choice above set up.

### Make the flow

In Teams, from the channel's **More options** (`…`), open **Workflows** and choose the template **Send webhook alerts to a
channel**. Name the flow, keep the team and the channel, and copy the URL that it gives. (The flow on the test tenant was
made from this template; the menu path is Microsoft's UI and was not recorded.) Anyone who has the URL can post to the
channel: keep it as you keep a secret.

On the test tenant (measured):

- A card POSTed to the URL was answered 202 within half a second, and showed in the channel about 12.6 seconds later.
- The bot got no activity for it. Microsoft Graph showed it as a post of the application
  `358f0194-6b0e-4dd3-af35-c24fe8a9ec87`, named Workflows, of the kind bot. That ID is Microsoft's, not the tenant's,
  and the same for every flow, so a trigger names it and narrows to your cards with `match`.
- The post's text was only an `<attachment>` tag; the card was in the attachment. So on Teams a trigger reads the card:
  the text of each `TextBlock`, and of each fact of a `FactSet` as `title: value`, one per line, is the field `body`, and
  `text` is empty ([Alerts on Microsoft Teams](../reference/platforms.md#alerts-on-microsoft-teams)).

### Point Alertmanager at it

Alertmanager's `msteamsv2` receiver posts an Adaptive Card in the form that a Workflows webhook takes (Alertmanager's
documentation; it was added in Alertmanager 0.28.0, by its changelog). Give it the flow's URL from a file, so that the URL
is not in the configuration:

```yaml
receivers:
  - name: teams-alerts
    msteamsv2_configs:
      - webhook_url_file: /etc/alertmanager/secrets/teams-workflow-url # (1)!
        send_resolved: true # (2)!
```

1. A file that holds the flow's URL, from a Secret mounted into Alertmanager.
2. The default, written out. With `false`, Alertmanager posts no `RESOLVED` card, and the thread never closes.

With its default templates, the card has two `TextBlock`s (Alertmanager's source, `notify/msteamsv2` and
`template/default.tmpl`; no Alertmanager card was posted through a flow on the test tenant). The first is the title,
`[FIRING:<n>]` or `[RESOLVED]` and then the values of the group's labels, then the other common labels in parentheses.
The second lists each alert's labels and annotations, a line `  - name = value` each. So the field `body` of a firing
alert reads:

```text
[FIRING:1] KubePodCrashLooping shop (checkout-7d9f8b6c5-x2x4k critical)


# Alerts Firing:

Labels:
  - alertname = KubePodCrashLooping
  - namespace = shop
…
```

### Add the trigger

The recipe's Binding has a `botMessage` trigger for these cards, beside a `mention` trigger:

```yaml title="kourier/binding.yaml"
--8<-- "prompt-agent-teams/kourier/binding.yaml"
```

The `key` is the group's label values, so each alert group gets one thread, and its `RESOLVED` card closes it. The
`alertname` comes from the label list, since the title holds it only when the group is by `alertname`. A template of your
own needs regexes of its own. The trigger's fields, limits and notes are those of any `botMessage` trigger:
[Chat triggers](../reference/chat-triggers.md#botmessage-fields), and
[Investigate Alertmanager alerts](investigate-alertmanager-alerts.md) for the thread rules.

### Test it

The dry run reads only Slack, so pin the trigger with the recipe's trigger evals, `go test ./test/recipes`, against the
`body` your cards hold. Then post a card to the flow by hand, shaped as the receiver's:

```sh
curl -sS -X POST -H 'Content-Type: application/json' "$TEAMS_WORKFLOW_URL" --data '{"type":"message","attachments":[{
  "contentType":"application/vnd.microsoft.card.adaptive","content":{"type":"AdaptiveCard","version":"1.2","body":[
  {"type":"TextBlock","text":"[FIRING:1] KubePodCrashLooping shop (critical)","weight":"Bolder","wrap":true},
  {"type":"TextBlock","text":"Labels:\n  - alertname = KubePodCrashLooping\n  - namespace = shop","wrap":true}]}}]}'
```

The card shows in the channel after about 13 seconds, as on the test tenant. With polling, the broker finds it within about 10 seconds more, and
starts the investigation in its thread. `agentkourier_teams_graph_read_messages_total` (polling) or
`agentkourier_teams_graph_notifications_total` (notifications) counts it as `taken`, or as `not_named` when no
`botMessage` trigger of the channel names its app; a card that the trigger's regexes turn away counts in
`agentkourier_trigger_unmatched_total` ([Metrics](../reference/metrics.md#operations-intake-delivery-and-triggers)).

## Troubleshoot

**Graph answers 403.** The broker logs Graph's 403 for a channel's messages with the fix: the manifest must ask for the
resource-specific permission `ChannelMessage.Read.Group`, and the app must be installed in the channel's team, which
grants it if the tenant's consent policy lets the team's owner consent. A polled channel counts as `failing` in
`agentkourier_teams_graph_channels`. Check the manifest's `authorization` block, then update or reinstall the app in the
team; if it is right, ask the tenant's admin whether team owners may grant resource-specific permissions.

**A channel waits for its team's group.** `agentkourier_teams_graph_channels{state="waiting_group"}`, and a WARN line once
for the channel, `a bound channel's messages cannot be polled yet` (or `subscribed to yet`): the broker has had no
activity of the channel that names its team. Install the app in the channel's team, and post or mention the bot in the
channel. The channel is read at the next round after.

**Nothing arrives in `http` mode.** Check, in order: the endpoint address in the Developer Portal is the connection's
route; `agentkourier_chat_listener_requests_total` counts requests at the chat listener at all; the chat Service has an
endpoint (the leader's pod carries the label `agentkourier.dev/role: leader`). `agentkourier_teams_requests_total` then
says what came of each request: `bad_token` with a `reason` in `agentkourier_teams_tokens_refused_total`, where `audience`
means that `spec.teams.appID` is not the bot's, and `expired` or `not_yet_valid` can mean a skewed clock; `wrong_tenant`
means that the activity came from another tenant than `spec.teams.tenantID`, which the broker logs at WARN.

**Nothing arrives in `socket` mode.** `agentkourier_teams_socket_ready` says which regions have a socket. A region that
answers 403 "Socket mode is not enabled for this bot." waits 5 minutes between tries: check that the bot's delivery
method is Socket mode. A second process with a socket of the same bot takes all of its region's traffic, and the older
socket gets nothing and is not told (measured): run one broker per bot.

**An alert starts nothing.** `not_named` in `agentkourier_teams_graph_read_messages_total` or
`agentkourier_teams_graph_notifications_total` means no `botMessage` trigger of the channel names the app that posted:
`from.id` must be the Workflows ID, in lowercase. `agentkourier_trigger_unmatched_total` means a trigger names it and a
regex turned the card away: compare the regexes with the `body` above. With polling, a card posted as a reply in an older
thread is not read.

**The notificationURL changed.** Graph lets an app have one subscription to a channel's messages, so the old one holds the
channel until it expires: [Changing the notificationURL](../reference/platforms.md#alerts-on-microsoft-teams).

## Related

- [What each platform supports](../reference/platforms.md): the Microsoft Teams sections, in full.
- [ChatConnection reference](../reference/chatconnection.md): `spec.teams`.
- [Limits](../reference/limits.md#run-the-broker): the bounds of the Teams connection, its sockets, subscriptions and polls.
- [Start from a recipe](recipes.md): the `prompt-agent-teams` recipe.

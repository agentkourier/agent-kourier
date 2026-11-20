# Configure Microsoft Teams

This guide is for a platform engineer setting up the chat side on Microsoft Teams. At the end, a bot registered in your
Microsoft Entra tenant is installed in a team, a ChatConnection points at it, a Binding names one of the team's channels, and a
mention of the bot there gets the agent's answer in the thread. The last part sends Alertmanager's alerts to that channel
through Workflows, where a `botMessage` trigger starts an investigation.

!!! warning "Experimental"

    Microsoft Teams needs Agent Kourier 0.6.0 or later: the 0.5.x CRDs and broker refuse `platform: teams`. The tests of
    Agent Kourier run it against fakes of Microsoft's services. Parts of it were checked on one test tenant in 2026-11, in
    Teams' web client: `http` mode, `socket` mode, Graph's notifications, and Graph polling, including a resume of polling
    from a stored position. A full install of the broker has not yet been followed through this guide against Teams. The
    steps mark what was checked ("measured"), what comes from Microsoft's documentation, and what was not checked. Each
    step in a Microsoft UI is as it was then, and Microsoft can change it.

## Choose how Teams reaches the broker

Two settings of the ChatConnection decide what must be reachable from the internet: `spec.teams.mode`, how Teams sends the
bot's activities (mentions, posts in the channel, card presses), and `spec.teams.notificationURL`, how the broker reads other
apps' posts, such as an alert card that Workflows posts. Teams sends a bot no other app's post (measured, and Microsoft's
documentation says so), so the broker reads those from Microsoft Graph, by its notifications or by polling.

- **Recommended: `http` with polling**, the default and the recipe's choice. Teams sends each activity over HTTPS to the
  leader's chat listener; the leader polls Graph every 10 seconds for each bound channel. It needs one public HTTPS route.
- **No public endpoint, for evaluation: `socket` with polling.** The leader opens outbound WebSockets to Teams. Microsoft
  calls Socket mode a public preview, available only to newly created bots. What Teams sends while no socket is up is lost as an
  activity.
- **Many channels, or alerts posted as replies in older threads: add a `notificationURL`.** Graph notifies the leader of
  each new message instead of being polled, which costs no polls, but needs public HTTPS in either mode.

| | `notificationURL` unset (polling) | `notificationURL` set |
|---|---|---|
| `http` | Public HTTPS to the chat listener, for Teams. | The same URL, for Teams and for Graph. |
| `socket` | Nothing public: no chat listener is needed. | Public HTTPS to the chat listener, for Graph alone. |

What each choice loses, and when, is in [How each choice behaves](#how-each-choice-behaves).

## Before you start

- **Agent Kourier 0.6.0 or later**, installed with Helm on custom resources (`config.source: crds`, the chart's default):
  [Install with Helm](install-with-helm.md). The ConfigMap route works too, with the limit in the box below.
- **An agent to answer**: a running A2A agent, an AgentBackend that points at it, and the Secret that holds the Binding's
  identity token ([Create the Binding identity](create-binding-identity.md)). The
  [`prompt-agent-teams` recipe](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/prompt-agent-teams/README.md)
  sets these up from the `prompt-agent` recipe's agent.
- **A Microsoft 365 tenant whose Teams lets custom apps be uploaded.** In the Teams admin center, a tenant admin turns on
  **Upload custom apps** in the app setup policy that applies to you (on the test tenant, the global policy).
- The right to register an application in the tenant's Microsoft Entra ID, and to add an app to the team, as its owner.
- Two icons for the Teams app: `color.png`, 192 by 192 pixels, and `outline.png`, 32 by 32, white on a transparent
  background (Microsoft's documentation).
- Unless you run `socket` mode with polling: a host name that Microsoft can reach over HTTPS and that routes to your
  cluster, such as an Ingress controller's, with a certificate. For the chart's leader label you also need the right to
  create a ValidatingAdmissionPolicy and its binding, which are cluster-scoped
  ([Security model](../explanation/security.md#the-chat-port)).
- With the chart's `networkPolicy.enabled`: the broker calls Microsoft Entra (`login.microsoftonline.com`), the Bot Connector
  (`smba.trafficmanager.net` and the service URL each activity names), Microsoft Graph (`graph.microsoft.com`), the Bot
  Framework's signing keys (`login.botframework.com`) and, in `socket` mode, `<region>.botapi.skype.com` and the socket host
  it is given, all on 443. The egress rule `networkPolicy.slack` allows them with its default `cidrs`, which open 443 to
  every address outside the private ranges; narrowed, it must still reach these hosts.

!!! note "Adding Teams to a broker that already serves Slack"

    On the ConfigMap route (`config.source: files`) a broker serves exactly one ChatConnection and refuses a config with
    two, so a broker on files that serves Slack cannot take a Microsoft Teams connection beside it. Two ways work:

    - **One broker, on custom resources** ([Move the config between a ConfigMap and custom resources](migrate-config-source.md)):
      apply the Microsoft Teams ChatConnection in a namespace that the broker admits (on `config.connectionNamespaces`, or
      matching `config.connectionNamespaceSelector`) and grant the broker its Secret, as
      [Let a team bring its own Slack app](team-slack-app.md) does for a Slack app. The broker serves it as a team connection
      (one it serves beside its own), on the same chat listener, which `http` mode needs enabled.
    - **A second release**, with `crds.install: false` (the CRDs belong to the first release, and Helm refuses a second
      release that renders them), its own `config.chatConnection` naming the Microsoft Teams ChatConnection, and its own
      Ingress host. The first broker must not admit that ChatConnection's namespace. The recipe's
      [Apply it](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/prompt-agent-teams/README.md#3-agent-kouriers-resources)
      shows it.

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
4. Under **Certificates & secrets**, add a client secret and copy its **Value**, which the portal shows once (Microsoft's
   UI, not checked here). The secret expires on the date you choose: rotate it before then, by adding a new secret,
   replacing the Secret `teams` as in step 2 with `--dry-run=client -o yaml | kubectl apply -f -`, and restarting the broker.

Agent Kourier gets two tokens from `https://login.microsoftonline.com/<tenantID>` with that secret: one for the Bot
Connector, to post, and one for Microsoft Graph, to read. Neither needs an API permission on the app registration. On the
test tenant, with its default consent settings, neither needed a tenant admin's consent (measured); a tenant that restricts
resource-specific consent needs its admin. The Graph token reads only under the permission that installing the app in a
team grants (step 5).

## 2. Store the client secret

The ChatConnection's Secret has one key, `clientSecret`, in either mode. Paste the value at the prompt, so that it is in no
file and no shell history:

```sh
read -rs TEAMS_CLIENT_SECRET   # paste the client secret's value and press Enter; nothing is echoed
printf %s "$TEAMS_CLIENT_SECRET" | kubectl -n agent-kourier create secret generic teams --from-file=clientSecret=/dev/stdin
unset TEAMS_CLIENT_SECRET
```

`printf %s` adds no newline. Agent Kourier sends the value to Microsoft Entra as the Secret holds it and trims nothing, so a
value stored with a newline at its end (from `echo`, or a file an editor saved) is a different secret
([Troubleshoot](#troubleshoot)). If your cluster's Secrets come from a secrets manager, create `teams` with the key
`clientSecret` that way instead. A Secret without the key, or with it empty, stops the broker's own connection from
starting (exit code 2: [Troubleshoot](troubleshoot.md#the-pod-exits-with-code-2)).

## 3. Declare the connection

Write the ChatConnection. This is the recipe's, in `http` mode with polling; replace the two GUIDs with yours:

```yaml title="kourier/chatconnection.yaml"
--8<-- "prompt-agent-teams/kourier/chatconnection.yaml"
```

For the other choices, add under `teams`:

- `mode: socket` for `socket` mode.
- `notificationURL: https://chat.example.com/chat/teams/agent-kourier/teams` for Graph's notifications. It must be this
  connection's own route, `https://<host>/chat/teams/<namespace>/<name>`, where `<namespace>` and `<name>` are the
  ChatConnection's. The API server checks the form of the path, and the broker checks that it names this connection.

Then the chart's values. If the box's first way applies (Teams as a team connection beside Slack), keep your
`config.chatConnection`; set `config.connectionNamespaces` (or its selector) to admit the Microsoft Teams connection's
namespace, grant `teams` with `rbac.secretNames`, and take only `chatListener` and `networkPolicy.chatFrom` from this block.
Of the ChatConnections you apply, the chart grants only `config.chatConnection`'s Secret by itself, so naming the Teams connection there would replace the broker's
own Slack connection, and Slack would stop. Otherwise, on custom resources:

```yaml title="teams-values.yaml"
config:
  chatConnection: agent-kourier/teams # the ChatConnection above, as namespace/name
chatListener:
  enabled: true
  ingress:
    enabled: true
    className: ""
    host: chat.example.com
    tls: [{hosts: [chat.example.com], secretName: chat-example-com-tls}]
networkPolicy:
  chatFrom: # only with networkPolicy.enabled
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress-nginx}}
```

- `chatListener.enabled`: the broker's second port, for Teams' requests and Graph's notifications. Its Service sends them
  to the leader alone, which carries the label `agentkourier.dev/role: leader` (the chart's
  [Chat listener](https://github.com/agentkourier/agent-kourier/blob/main/charts/agent-kourier/README.md#chat-listener)).
- `chatListener.ingress.enabled`: an Ingress that sends the path prefix `/chat/` to that Service, and nothing else.
- `chatListener.ingress.className`: the IngressClass; empty leaves it to the cluster's default class.
- `chatListener.ingress.host`: the host the Ingress rule matches, the `<host>` of the route.
- `chatListener.ingress.tls`: the Ingress's TLS, in Kubernetes' own form: the route must be HTTPS.

Behind a Gateway instead of an Ingress controller, leave `chatListener.ingress` off and write an HTTPRoute of your own to the chat
Service: [With a Gateway API HTTPRoute instead of the Ingress](receive-webhooks.md#with-a-gateway-api-httproute-instead-of-the-ingress)
has one; the same route serves Teams' requests.
- `networkPolicy.chatFrom`: who may reach the chat port, here the ingress controller's pods; required with
  `networkPolicy.enabled`.

In `socket` mode with polling, leave out `chatListener` and `networkPolicy.chatFrom`: nothing comes in. The chart grants the
broker the connection's Secret, the one named as the connection (`teams`) in its namespace (`config.chatConnectionSecret`
names another). Grant the Binding's token Secret with `rbac.secretNames`, as the recipe does.

Apply it in this order. On a first install, install the chart with `--set replicaCount=0` before the `kubectl apply`, so that
the CRDs exist:

```sh
kubectl apply -f kourier/chatconnection.yaml
helm upgrade --install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
  -n agent-kourier -f my-values.yaml -f teams-values.yaml
```

`my-values.yaml` is the rest of the release's values, as [Install with Helm](install-with-helm.md) writes them: the
`rbac.secretNames` entry of the Binding's token Secret, and any replicas, storage or egress settings. With nothing else to
set, leave it out. The recipe's values in
[Apply it](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/prompt-agent-teams/README.md#3-agent-kouriers-resources)
are one self-contained file for a Teams-only broker.

??? note "On the ConfigMap route"

    The same spec goes in the values, with the same `chatListener` and `networkPolicy` values. It must be the one entry of
    `config.chatConnections`, and Bindings go under `config.bindings` in the same form:

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
    ```

**Check:** in `http` mode the leader logs `the chat listener serves the connection`, with the `path`, and `teams: taking
activities over https`. In `socket` mode it logs `teams: socket ready` for each region that takes a socket. A pod that exits
instead is in [Troubleshoot](#troubleshoot). No Binding names a channel yet, which is fine: the connection runs without one.

The broker builds the platform of its own connection when it starts and when a replica takes the lead, so a later edit of its
`spec.teams` (another `mode`, a `notificationURL` added, changed or removed, another tenant or app) applies when Agent Kourier restarts: on
the ConfigMap route the chart rolls the pod when its ConfigMap changes, unless `config.existingConfigMap` is set or
`config.rollOnChange` is `false`, and on custom resources it takes a restart of the Deployment. Until then the leader logs, once for
each edit it reads that leaves `spec.teams` other than the one the connection runs on, a WARN that names the connection in its
`connection` attribute,
`chat connection: its spec.teams was edited, and the running connection still uses the spec.teams it started with; a restart of the broker applies the edit`,
and an INFO, `chat connection: its spec.teams is the one the running connection started with again`, once the spec is back to the one it runs on.
`mode: http` written out where the mode was unset, or taken out where it was written, is the same mode and logs nothing.

## 4. Point Teams at the broker

In the Developer Portal, open the bot under **Bot management** and its **Configure** page (measured on the test tenant):

- `http` mode: set the delivery method to the HTTPS endpoint, and the endpoint address to
  `https://<host>/chat/teams/<namespace>/<name>`, for example `https://chat.example.com/chat/teams/agent-kourier/teams`.
- `socket` mode: set the delivery method to **Socket mode**. The page marked it "New, development only", and the
  switch took about a minute to take effect (measured). A region that refuses the bot's socket answers 403
  "Socket mode is not enabled for this bot.", which the broker logs and tries again every 5 minutes, give or take a fifth,
  while the other regions serve. On the test tenant `apac` answered that while `amer` and `emea` worked (measured).

The mention in step 6 checks this.

## 5. Build and install the app package

The bot reaches a team as a Teams app: a zip of `manifest.json` and the two icons. This manifest is the recipe's:

```json title="teams-app/manifest.json"
--8<-- "prompt-agent-teams/teams-app/manifest.json"
```

- The bot's ID in `bots`, and `webApplicationInfo.id`: the bot's application (client) ID from step 1.
- `id`: the Teams app's own ID, a new GUID of your own (`uuidgen | tr A-Z a-z`), not the bot's.
- `scopes: ["team"]`: the bot works in a team's channels. Agent Kourier serves no personal or group chat.
- `authorization.permissions.resourceSpecific`: the resource-specific permission `ChannelMessage.Read.Group`, of type
  `Application`. With it, Teams sends the bot every person's post in the team's standard channels, not only mentions
  (measured), and Microsoft Graph lets the app read the channels' messages: the alerts that other apps post, a thread's
  earlier replies, and its own posts that the outbox looks for after a restart. Without it, Graph refuses those reads with
  403 (Microsoft's documentation; not measured), and the broker's error for the refusal says to add the permission and
  install or update the app in the team.
- `supportsChannelFeatures: "tier1"`: on the test tenant, a manifest 1.30 with a bot in the `team` scope failed the
  upload's validation without it (measured). In the Developer Portal it is under **App content**, **Channel support**.
  Only standard channels were tried.

The manifest passes Microsoft's JSON schema for manifest version 1.30, the one its `$schema` names (checked with Python's
`jsonschema`, draft 4). That is not Teams' own validation, which also runs at upload. Zip the three files
with nothing above them in the archive:

```sh
zip agent-kourier-teams.zip manifest.json color.png outline.png
```

Install the app in the team whose channels the Bindings will name. In Teams, **Apps**, **Manage your apps**, **Upload an
app**, **Upload a custom app** takes the zip (Microsoft's UI, not checked here). On the test tenant the app was installed
with the Developer Portal's **Preview in Teams**, and the team listed it under **Manage team**, **Apps**, as a custom app
(measured).

The install grants `ChannelMessage.Read.Group` in that team: the person who installs it consents for the team (Microsoft's
documentation, which names the team's owner in some places and a member in others). On the test tenant no separate consent
screen appeared, and Graph read the channel's messages right after the install (measured). A tenant's admin can stop team
owners from granting resource-specific permissions (Microsoft's documentation); the broker then logs Graph's 403 with the
fix.

## 6. Mention the bot in the channel

Mention the bot in each channel that a Binding will name. Nothing answers yet, since no Binding names the channel. The
broker learns from this activity the channel's team, the team's Microsoft Entra group and where to post, and keeps them in
its store ([Stored data](../reference/stored-data.md)). Until an activity of a channel has come, it cannot read the channel
from Graph, nor check that the bot is in it.

**Check:** `agentkourier_teams_requests_total{outcome="accepted"}` (in `http` mode) or
`agentkourier_teams_socket_envelopes_total{type="activity",outcome="accepted"}` (in `socket` mode) went up
([Metrics](../reference/metrics.md#operations-intake-delivery-and-triggers)). If neither moved, see
"Nothing arrives" in [Troubleshoot](#troubleshoot).

## 7. Copy the channel ID

A Binding names a channel by its ID, `19:` followed by letters, digits, `.`, `_` or `-`, then `@thread.tacv2` (or
`@thread.skype`), never by its name. In Teams, the channel's **More options** (`…`), **Get link to channel** gives a link
that holds the ID with `:` written `%3a` and `@` written `%40` (Microsoft's UI, not checked here). Decode those two. The
first channel of a team, its primary channel, has the team's ID as its own (measured, and Microsoft's documentation).

## 8. Write a Binding

A Binding on a Microsoft Teams connection is written as on Slack ([Write a Binding](write-a-binding.md)), with Teams' IDs.
The recipe's [`kourier/binding.yaml`](#add-the-trigger) is a whole one. The part that differs:

```yaml
spec:
  chat:
    connectionRef: {name: teams, namespace: agent-kourier}
    channel: 19:a1b2c3d4e5f6@thread.tacv2
```

Apply it with `kubectl apply` (on the ConfigMap route, add it under `config.bindings` and upgrade the release). A mention's
`user` is the person's Microsoft Entra object ID, and a `botMessage` trigger's `from.id` is the Microsoft Entra application
ID of the app that posts, each a GUID in lowercase ([Chat IDs](../reference/platforms.md#chat-ids)). A Slack ID on a
Microsoft Teams connection, or a feature that Teams does not serve, is refused when the broker reads the Binding with its
ChatConnection.

What works and what differs from Slack is in [Features](../reference/platforms.md#features). In short:

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

1. Mention the bot in the channel and look for the agent's reply in the thread.
2. Wait for the Binding to be `Ready`. `ChannelJoined` reads the Bot Connector's list of the channels of the channel's
   team, and is `True` when the list names the channel. The broker checks a Binding within 30 seconds of its being
   added, then every 5 minutes, give or take a tenth. A Binding checked before any activity of its channel came is `Unknown` with
   `ChatUnavailable` until the next check, so allow up to 6 minutes:

    ```sh
    kubectl -n agent-kourier wait --for=condition=Ready akbinding/<binding> --timeout=6m
    ```

    On the ConfigMap route there is no Binding object: read the Binding's `agentkourier_binding_ready`, which is 1 once
    every condition is true ([Metrics](../reference/metrics.md#binding-verification)).
3. Microsoft Graph: `agentkourier_teams_graph_channels` counts each bound channel as `polling` (no `notificationURL`) or
   `subscribed` (with one) once the broker knows its team. With a `notificationURL` the leader logs `teams: subscribed to a
   bound channel's messages on Microsoft Graph` for each subscription. Making one took about 8 seconds on the test tenant,
   while Graph checked the URL (measured).

## Send Alertmanager alerts to a channel

Alertmanager posts to Teams through a Workflows webhook, a flow that posts what it is sent to one channel. The post's
sender is the Workflows app, and Teams does not send it to the bot, so the broker reads it from Microsoft Graph, by polling
or by notifications as the ChatConnection says.

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
2. The default, written out. With `false`, Alertmanager posts no `RESOLVED` card, so nothing closes the thread: it stays
   open, and a repeat adds a note, until no card of its group has come for the trigger's `thread.maxIdle` (unset, the
   larger of 24 hours and twice the cooldown). The next card then starts a new investigation.

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

The dry run reads only Slack. To check your regexes before a card comes, edit the recipe in a clone of this repository:
put your Binding's trigger in `examples/recipes/prompt-agent-teams/kourier/binding.yaml`, and cases with the `body` of your
cards in its `evals/triggers.yaml`, then run `go test ./test/recipes`, which replays each case through the trigger.

Then post a card to the flow by hand, shaped as the receiver's:

```sh
curl -sS -X POST -H 'Content-Type: application/json' "$TEAMS_WORKFLOW_URL" --data '{"type":"message","attachments":[{
  "contentType":"application/vnd.microsoft.card.adaptive","content":{"type":"AdaptiveCard","version":"1.2","body":[
  {"type":"TextBlock","text":"[FIRING:1] KubePodCrashLooping shop (critical)","weight":"Bolder","wrap":true},
  {"type":"TextBlock","text":"Labels:\n  - alertname = KubePodCrashLooping\n  - namespace = shop","wrap":true}]}}]}'
```

A card posted to a flow showed in the channel about 13 seconds later on the test tenant. With polling, the broker should
find it within about 10 seconds more and start the investigation in its thread; no Alertmanager card has yet been followed
that far on a live tenant. `agentkourier_teams_graph_read_messages_total` (polling) or
`agentkourier_teams_graph_notifications_total` (notifications) counts it as `taken`, or as `not_named` when no
`botMessage` trigger of the channel names its app; a card that the trigger's regexes turn away counts in
`agentkourier_trigger_unmatched_total` ([Metrics](../reference/metrics.md#operations-intake-delivery-and-triggers)).

## How each choice behaves

**`http` mode.** Only the leader takes Teams' requests. While no replica leads (a restart, a change of leader), the chat
Service has no endpoint and Teams' requests fail. Teams sent a message that the broker answered 503 again once, about
0.6 seconds later (measured). One resend does not span a takeover, so a mention sent during one is lost as an activity;
polling reads it back, within the bounds below. Microsoft's documentation recommends an HTTPS endpoint in production.

**`socket` mode.** The leader opens an outbound WebSocket to Teams in each region (`amer`, `emea`, `apac`). Microsoft calls
Socket mode a public preview, available only to newly created bots, and says a bot published to the Teams Store must use
HTTPS. **Teams does not queue what it sends while no socket is ready, and does not send it to an HTTPS endpoint instead**
(measured): a mention during a restart or a change of leader is lost as an activity.

**What Graph gives back,** in either mode:

- **With polling**, at the start of a run the broker resumes each channel's delta from the position that the last run
  stored, for up to an hour, so the threads begun meanwhile come back, a mention that begins one included; a catch-up
  read gives back the replies posted in the 10 minutes before the run in the channel's 20 newest threads. After a gap of the sockets that
  ends while the leader runs, it reads back to the gap's start, at most 10 minutes. Polling reads replies only in each
  channel's 3 newest threads of the last hour, and makes a steady stream of reads
  ([Polling without a notificationURL](../reference/platforms.md#polling-without-a-notificationurl),
  [Catch-up reads](../reference/platforms.md#catch-up-reads)).
- **With a `notificationURL`**, while the leader keeps running, a gap of the sockets lost nothing on the test tenant:
  Graph notified the posts of the gap, mentions included (measured). Across a restart or a change of leader the broker
  reads nothing back when its run starts. What was posted meanwhile reaches it only if Graph sends again the
  notifications it could not deliver, which Microsoft's documentation says it does for up to 4 hours (not measured), or
  if Graph reports them as `missed`, after which the broker reads the channel's last 10 minutes (Microsoft's
  documentation; not yet seen from Graph).

## Troubleshoot

**The pod exits with code 2, and the error says to set `AGENTKOURIER_CHAT_LISTEN`.** The error reads `the broker cannot be
wired: ChatConnection <namespace>/<name> is a Microsoft Teams connection in http mode, and the broker has no chat listener
for Teams to send to` (or `with a notificationURL, ... for Microsoft Graph to send to`). Set `chatListener.enabled: true`
and the Ingress values of step 3, or use `socket` mode with polling.

**The pod exits with code 1: `agent-kourier: teams rejected the tokens: <code>`.** Microsoft Entra refused the token
request as the connection started; `<code>` is Entra's OAuth error code. `invalid_client` is the OAuth code for a client
that failed to authenticate: a wrong or expired client secret, a secret stored with a newline at its end, or the secret of
another app (not checked here). Check `spec.teams.appID` and `tenantID`, replace the Secret as in step 2 with
`--dry-run=client -o yaml | kubectl apply -f -`, and restart the broker
([The pod exits with code 1](troubleshoot.md#the-pod-exits-with-code-1)).

**Teams refuses the app package.** Teams validates the package at upload (Microsoft's). On the test tenant a manifest
without `supportsChannelFeatures: "tier1"` failed it (measured). An upload refused for policy needs a tenant admin to turn
on **Upload custom apps** for you ([Before you start](#before-you-start)).

**The Binding stays `Unknown` with `ChatUnavailable`.** No activity of its channel has come, so the broker does not know the
channel's team, and the leader's log says `the channel's team is not known yet`. Mention the bot in the channel, then wait
for the next check, up to about 5.5 minutes.

**A channel waits for its team's group.** `agentkourier_teams_graph_channels{state="waiting_group"}`, and a WARN line once
for the channel, `a bound channel's messages cannot be polled yet` (or `subscribed to yet`): the broker has had no
activity of the channel that names its team. Install the app in the channel's team, and post or mention the bot in the
channel. The channel is read at the next round after.

**Graph answers 403.** The broker logs Graph's 403 for a channel's messages with the fix: the manifest must ask for the
resource-specific permission `ChannelMessage.Read.Group`, and the app must be installed in the channel's team, which
grants it if the tenant's consent policy lets the team's owner consent. A polled channel counts as `failing` in
`agentkourier_teams_graph_channels`. Check the manifest's `authorization` block, then update or reinstall the app in the
team; if it is right, ask the tenant's admin whether team owners may grant resource-specific permissions.

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
- [Limits](../reference/limits.md#run-the-broker): the bounds of the Microsoft Teams connection, its sockets, subscriptions and polls.
- [Start from a recipe](recipes.md): the `prompt-agent-teams` recipe.

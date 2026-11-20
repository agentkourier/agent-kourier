# prompt-agent-teams

The [`prompt-agent`](../prompt-agent/README.md) recipe's agent, in a Microsoft Teams channel instead of a Slack one.
Someone mentions the bot in the channel, and the agent answers in the thread. Alertmanager posts an alert to the channel
through a Workflows webhook, and the agent investigates it in the alert's thread.

The agent is the prompt agent as it is: the same `agent/` directory, image, manifests and Secrets. This recipe holds
the Agent Kourier side for Teams (a ChatConnection, a Binding and an AgentBackend), the Teams app's manifest, and trigger
evals. [Configure Microsoft Teams](../../../docs/how-to/configure-teams.md) has every step on the Microsoft side, and
the reasons behind each setting here.

Microsoft Teams support is experimental, and needs Agent Kourier 0.6.0 or later. Parts of it were checked on a test tenant
in 2026-11, and the how-to says which.

## What it does

- A mention of the bot in the channel, outside a thread, opens a thread and a session with the agent. A reply in the
  thread continues it (the Binding leaves `chat.threadReplies` at `all`).
- A post that a Workflows flow makes in the channel is read from Microsoft Graph, since Teams sends a bot no other app's
  post ([Alerts on Microsoft Teams](../../../docs/reference/platforms.md#alerts-on-microsoft-teams)). The Binding's
  `botMessage` trigger names Workflows' application ID and reads the card's text, which on Teams is the field `body`. A
  card of Alertmanager's `msteamsv2` receiver starts an investigation in its thread. A repeat of its alert group inside
  the 4-hour cooldown adds a note, and the group's `RESOLVED` card closes the thread. At most 5 investigations start in
  any 10 minutes, 2 run at once, and 40 start in a day.
- The prompt agent answers from its `agent.yaml`, as in the `prompt-agent` recipe, and can ask the thread a question,
  which Teams shows as an Adaptive Card.

## What you'll see in Teams

```text
#alerts
  You            @Agent Kourier Why is checkout-db's disk filling up?
    thread       one reply, which Agent Kourier edits as the agent writes
  Workflows      [FIRING:1] KubePodCrashLooping shop (checkout-7d9f8b6c5-x2x4k critical)  (a card)
    thread       the agent's investigation, edited as it is written
  Workflows      [RESOLVED] KubePodCrashLooping shop (…)  (a card)
    thread       Resolved at <time>.  (in the FIRING card's thread)
```

Teams has no streams or reactions, so a reply is one message that Agent Kourier posts and then edits, and nothing marks
your message while the agent works ([What Microsoft Teams shows while the agent works](../../../docs/reference/platforms.md#what-microsoft-teams-shows-while-the-agent-works)).

## Prerequisites

- **The prompt agent**, set up as the [`prompt-agent`](../prompt-agent/README.md) recipe's steps 1 to 4 say: the image,
  your `agent.yaml`, the Secrets (`prompt-agent-bearer` included, in both namespaces) and the manifests. Skip its step 5:
  its Slack resources are not used here.
- **A Microsoft 365 tenant**, a bot registered in it, and the right to upload a custom app to a team:
  [Configure Microsoft Teams](../../../docs/how-to/configure-teams.md).
- **Agent Kourier 0.6.0 or later**, on Kubernetes 1.33 or later. The 0.5.x CRDs and broker refuse `platform: teams`.
- **In `http` mode, the default:** a host name that Microsoft Teams can reach over HTTPS and that routes to the chat
  listener, as the how-to's step for the chart values says.
- **For the alerts:** Alertmanager with an `msteamsv2` receiver (Alertmanager 0.28.0 or later, by Alertmanager's
  changelog), and a Workflows flow that posts to the channel.

## Files

```
kourier/          AgentBackend, ChatConnection and Binding: Agent Kourier's side
teams-app/
  manifest.json   the Teams app's manifest, with the bot, its team scope and the resource-specific permission
evals/
  triggers.yaml   messages, and what the Binding must make of each
```

The agent's files are in [`../prompt-agent/agent/`](../prompt-agent/agent).

## Apply it

### 1. Register the bot and build the app package

Follow [Configure Microsoft Teams](../../../docs/how-to/configure-teams.md) to register the bot and make its client
secret. In `teams-app/manifest.json`, set the bot's ID in `bots` and `webApplicationInfo.id` to the bot's application (client)
ID, and `id` to a new GUID of your own, in lowercase (`uuidgen | tr A-Z a-z`). Add the two icons that the manifest names,
`color.png` (192 by 192 pixels) and `outline.png` (32 by 32, white on a transparent background), and zip the three files
at the top of the archive:

```sh
cd examples/recipes/prompt-agent-teams/teams-app
zip ../agent-kourier-teams.zip manifest.json color.png outline.png
```

Upload the zip to the team of the channel. [Build and install the app package](../../../docs/how-to/configure-teams.md#5-build-and-install-the-app-package)
says how, and what the install grants.

### 2. Make the Secret

The ChatConnection names the Secret `teams` in the namespace `agent-kourier`, with the key `clientSecret`:

```sh
read -rs TEAMS_CLIENT_SECRET   # paste the client secret's value and press Enter; nothing is echoed
printf %s "$TEAMS_CLIENT_SECRET" | kubectl -n agent-kourier create secret generic teams --from-file=clientSecret=/dev/stdin
unset TEAMS_CLIENT_SECRET
```

### 3. Agent Kourier's resources

1. In `kourier/chatconnection.yaml`, replace `tenantID` and `appID` with your tenant's ID and the bot's application ID,
   in lowercase hex. In `kourier/binding.yaml`, replace `19:REPLACE-prompt-teams@thread.tacv2` with the channel's ID
   ([Copy the channel ID](../../../docs/how-to/configure-teams.md#7-copy-the-channel-id)).
2. Install the chart to read them, with the chat listener open for `http` mode. These values replace the inline `config`
   of the values file in [Install with Helm](../../../docs/how-to/install-with-helm.md); the rest of that guide applies.

   ```yaml title="prompt-agent-teams-values.yaml"
   config:
     source: crds
     chatConnection: agent-kourier/teams
   rbac:
     secretNames: {agent-kourier: [prompt-agent-bearer]}
   chatListener:
     enabled: true
     ingress:
       enabled: true
       host: chat.example.com
       tls: [{hosts: [chat.example.com], secretName: chat-example-com-tls}]
   ```

   The chart grants the Secret `teams` from `config.chatConnection`, and `rbac.secretNames` grants the Binding's token.
   With `networkPolicy.enabled`, also set `networkPolicy.chatFrom` to the peers that pass Microsoft's requests on to the
   chat port. Then, in this order, as in the `prompt-agent` recipe:

   ```sh
   helm upgrade --install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
     -n agent-kourier -f prompt-agent-teams-values.yaml --set replicaCount=0
   kubectl apply --dry-run=server -f examples/recipes/prompt-agent-teams/kourier/
   kubectl apply -f examples/recipes/prompt-agent-teams/kourier/
   helm upgrade --install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
     -n agent-kourier -f prompt-agent-teams-values.yaml
   ```

   If the broker of the Slack recipes already runs as the release `agent-kourier` in this namespace, install this one
   beside it as a second release: name it `agent-kourier-teams` in both `helm upgrade --install` commands, and add
   `crds: {install: false}` to the values. The CRDs belong to the first release, and Helm refuses a second release that
   renders them ([Install with Helm](../../../docs/how-to/install-with-helm.md)). The chart names every object of a
   release after the release, so the two do not collide, and each broker serves its own ChatConnection alone, since
   neither sets a namespace list or selector. The messaging endpoint stays the same: its path is the ChatConnection's.

3. Set the bot's messaging endpoint to `https://chat.example.com/chat/teams/agent-kourier/teams`
   ([Point Teams at the broker](../../../docs/how-to/configure-teams.md#4-point-teams-at-the-broker)).
4. Mention the bot in the channel and ask a question that your agent's tools can answer: the answer comes in the thread.
   The mention also tells the broker the channel's team, which the Binding's `ChannelJoined` condition needs. The broker
   checked the Binding when it was applied, before any activity of the channel, so the condition reads `Unknown`
   (`ChatUnavailable`) until the next check, 5 minutes later, give or take a tenth. Allow 6 minutes:

   ```sh
   kubectl -n agent-kourier wait --for=condition=Ready akbinding/teams-prompt-chat --timeout=6m
   ```

### 4. Send alerts to the channel

Create a Workflows flow from the template **Send webhook alerts to a channel** for this channel, and give its URL to an
Alertmanager `msteamsv2` receiver:
[Send Alertmanager alerts to a channel](../../../docs/how-to/configure-teams.md#send-alertmanager-alerts-to-a-channel).
The trigger reads the cards of the receiver's default templates. With a `title` or `text` of your own, change the regexes
to match, and the cases in `evals/triggers.yaml` with them.

## What to change

- **The connection's mode and alert route.** `kourier/chatconnection.yaml` runs in `http` mode and polls Microsoft
  Graph for the alerts. Add `mode: socket` for no public endpoint (Microsoft's preview), or a `notificationURL` for
  Graph's notifications instead of polling. The how-to's [Choose how Teams reaches the broker](../../../docs/how-to/configure-teams.md#choose-how-teams-reaches-the-broker)
  weighs them. In socket mode with polling, the chart needs no chat listener.
- **The trigger.** The regexes in `kourier/binding.yaml` read Alertmanager's default `msteamsv2` card. Every Workflows
  flow posts as the same application, so keep a `match` that only your alerts' cards pass.
- **The Binding.** The channel; the names `teams-prompt-agents` and `teams-prompt-chat`. Everything about the agent is
  the `prompt-agent` recipe's to change.

## Checks

- `go test ./test/recipes` loads `kourier/` with Agent Kourier's loader, resolves its references and replays the
  cases of `evals/triggers.yaml` through the trigger: a firing alert, a resolved one, a group whose title ends in a space,
  the Watchdog heartbeat, another flow's card, another app's post and a person's post. The cards' text was written from
  Alertmanager's templates and the Teams adapter's reading of a card, not recorded from a live post.
- `make prompt-agent-test` tests the agent, as in the `prompt-agent` recipe.
- Nothing in this repository runs the recipe against Microsoft Teams: that is yours to run.

## Limits

- **An alert reaches the trigger late.** On a test tenant in 2026-11, a card posted to a Workflows webhook was in
  the channel about 12.6 seconds after the POST. Polling then reads a channel every 10 seconds; with a `notificationURL`,
  Graph notifies the leader of the post instead
  ([Alerts on Microsoft Teams](../../../docs/reference/platforms.md#alerts-on-microsoft-teams)).
- **Polling reads replies only in recent threads.** Without a `notificationURL`, another app's reply is read only in the
  3 newest threads that began within the last hour
  ([Polling without a notificationURL](../../../docs/reference/platforms.md#polling-without-a-notificationurl)). A
  person's mention in an older thread still comes as an activity.
- **The channel's team must be known.** Until an activity of the channel has come (a post, or a mention of the bot), the
  broker cannot read the channel from Graph, and the alerts wait.
- **The agent's limits** are the `prompt-agent` recipe's: see its [Limits](../prompt-agent/README.md#limits).

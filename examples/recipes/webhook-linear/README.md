# webhook-linear

A Linear workspace webhook sends issue and comment events to Agent Kourier. An issue created in a team starts a plan by
your agent; an edit of its description, or a comment that mentions the agent, starts more in the same session. Agent
Kourier posts nothing on Linear: the agent reads the issue and comments on it with its own Linear tools, through an API
key.

It holds the Agent Kourier side only: a ChatConnection of the `webhook` platform, a Binding and the AgentBackend it refers
to. It ships no agent. Point the Binding at any A2A agent (A2A is the Agent2Agent protocol) that has Linear tools
([Connect a generic A2A agent](../../../docs/how-to/connect-a2a-agent.md)).

## What it does

The ChatConnection, `kourier/chatconnection.yaml`, takes each delivery at
`https://<host>/chat/webhook/acme-webhooks/linear` and checks it before it reads any of it: the hex HMAC-SHA256 of the
raw body under the Secret's `webhookSecret`, in `Linear-Signature` with no prefix. Once the signature passes, the body's
`webhookTimestamp` (in milliseconds) must be within one minute of the broker's clock, either way, so that a delivery
replayed later is refused. It then maps the JSON body onto an event:

| Of the event | From | Read as |
|---|---|---|
| The channel | `/data/team/key` (an issue's team), `/data/issue/team/key` (a comment's issue's team), such as `ENG` | The Binding whose `chat.channel` is that team key takes the event. |
| The actor | `/actor/id`, `/data/userId`, `/data/user/id` | An event whose actor is the connection's `identity`, the user ID of the agent's API key (compared ignoring ASCII case), is the agent's own and is dropped. |
| The thread | the `issue` field: `ENG-7` | One session for each issue. A comment on it continues that session. |
| The delivery's ID | the `Linear-Delivery` header | Named in the log line of each delivery, and in the warning of one a limit stopped. |
| The fields | `type`, `action`, `issue` (the issue's identifier, or a comment's issue's) (id fields), and `title`, `prevDescription` (`/updatedFrom/description`), `comment` (text) | The triggers' `match` and `exclude` keys. The prompt template reads id fields alone; the text fields reach the agent only in the untrusted block. |

The Binding, `kourier/binding.yaml`, has three `webhookEvent` triggers:

1. An `Issue` event of the action `create` starts a plan: "Linear issue ENG-7 was created in team ENG."
2. An `Issue` event of the action `update` that holds an old description (`updatedFrom.description`, which Linear sends
   when the description changed) starts a revised plan in the issue's session.
3. A `Comment` event of the action `create` whose body mentions `acme-agent`, with or without the `@`, starts a reply in
   the issue's session.

`chat.output` is `none`: the only output a webhook connection takes, since the platform sends nothing.

Each prompt ends with a block that holds the event's actor and each field that has a value, under the heading "The
webhook event that triggered this", marked as untrusted data: an issue's title and description and a comment are written
by whoever filed, edited or commented on it.

## What you'll see on Linear

| What happens on Linear | What follows |
|---|---|
| An issue is created in the team | The agent's comment on it with a plan. |
| The issue's description is edited | A comment with the revised plan, in the same session as the first. |
| Any other edit, such as of its title alone | Nothing. |
| A comment that mentions `acme-agent` | The agent's reply on the issue, in the same session. |
| The agent comments | Linear sends the `Comment` event of it too, with the agent's user as the actor. Agent Kourier answers it `200` and drops it as the agent's own (`outcome="self"`). |

## Prerequisites

- **A Linear webhook** with a signing secret, for the resource types **Issues** and **Comments**. The header names, the
  signature's form, the timestamp and the body's paths this recipe reads follow Linear's webhook documentation, and this
  repository has not checked them against a live delivery.
- **An API key for the agent**, whose user's ID is the `identity`.
- **An A2A agent that has Linear tools** using that key, and its URL. The AgentBackend's `url` is a placeholder
  (`REPLACE-ME`).
- **The broker's clock within a minute of Linear's.** A delivery whose timestamp is further off either way is answered
  `401`, with the reason `stale`. Whether Linear gives a delivery that it sends again a new `webhookTimestamp` is
  not verified. If it does not, a delivery sent again more than a minute after the first is answered `401`
  (`stale`) and never reaches a trigger.
- **The chat listener reachable over HTTPS** from Linear: the chart's `chatListener.enabled: true`, and an Ingress
  (`chatListener.ingress`) or another way to reach the chat Service. A broker with no chat listener does not run a webhook
  connection: as its own connection it does not start, and as a team's connection it is not started.
- **A chart release that has custom resources, and Kubernetes 1.33 or later**, as the other recipes need.

## Files

```
kourier/
  chatconnection.yaml  the webhook connection: verify with a timestamp, channel, actor, thread, delivery ID and fields
  binding.yaml         the three triggers and their prompts
  agentbackend.yaml    your agent's A2A endpoint (a placeholder)
```

## Apply it

1. Replace the placeholders: the `identity` with the user ID of the agent's API key, `ENG` with your team's key in the
   Binding's `channel`, `acme-agent` in the comment trigger's regex with the name people mention the agent by, and the
   AgentBackend's `url` with your agent's. On the `a2a` dialect the URL alone selects the agent, and the Binding's
   `agent.name` and `agent.namespace` are labels.
2. Make the namespace and the Secret. The secret is the signing secret that Linear shows for the webhook, so create the
   webhook (step 5) first to have it; keep it out of your shell's history by reading it from a file:

    ```sh
    kubectl create namespace acme-webhooks
    kubectl -n acme-webhooks create secret generic linear-webhook --from-file=webhookSecret=./linear-webhook-secret
    ```

    The file must hold the secret alone: a trailing newline or any other control character is refused, and the
    connection is not built.
3. Have the broker serve the namespace's connections and read the Secret. In your chart values, beside the chat
   listener of the prerequisites:

    ```yaml
    config:
      connectionNamespaces: [acme-webhooks]
    rbac:
      secretNames:
        acme-webhooks: [linear-webhook]
    ```

    The connection is then a team connection of the broker, served beside the broker's own connection if it has one. To
    make it the broker's own connection instead, set `config.chatConnection: acme-webhooks/linear` and
    `config.chatConnectionSecret: linear-webhook`. The broker builds the platform of its own connection when it starts and
    when a replica takes the lead, so an edit to that connection's `spec.webhook` applies when Agent Kourier restarts. A
    team connection restarts by itself when its ChatConnection changes.
4. Apply the resources:

    ```sh
    kubectl apply -f examples/recipes/webhook-linear/kourier/
    kubectl -n acme-webhooks get akbinding linear-plans
    ```

5. The webhook's URL is `https://<host>/chat/webhook/acme-webhooks/linear`. Until the broker runs the connection, the chat
   listener answers its deliveries `503`, and takes none of them.

## What to change

- The types and actions of each trigger's `match`, and the regex of the mention.
- The prompts. A template reads `.Channel` and the id fields of the connection (`.Fields.issue`, `.Fields.type`,
  `.Fields.action`) and nothing else: a text field reaches the agent only in the untrusted block.
- The `limits` of each trigger.
- The timestamp's `window`, above 0 and at most `10m`.

## Checks

- `go test ./test/recipes` loads `kourier/` with Agent Kourier's own loader and resolves the Binding's references.
- `go test ./internal/broker -run TestWebhookRecipeLinear` runs this recipe in one process: a fake Linear signs and sends
  an issue's creation, an edit of its title, an edit of its description and a comment that mentions the agent, to a
  broker that serves these files, and a stub agent comments on the fake's API. It checks the prompts, the three
  comments, the drop of the agent's own events, a delivery that is unsigned, one signed with another secret and one two
  minutes old (each answered `401`), the same delivery sent twice (taken once), and a start that a limit stops.

## Limits

- A plan and a revised plan: at most 10 of each start in any hour, 1 of each runs at once, and 30 of each start in a
  day. A reply: 20 an hour, 1 at once, 60 a day. A start that a limit stops is answered `200`, and no investigation
  begins. The broker logs it at WARN ("a webhook event was not investigated"), with its delivery ID: the event is lost
  unless it is delivered again. A later delivery of it is decided again, if it still passes the one-minute window: whether
  Linear sends it again with a new `webhookTimestamp` is not checked, and one sent again with the old timestamp more than a
  minute later is answered `401` (`stale`) and never reaches a trigger.
- A delivery that is not verified is answered `401`, and one that is not a JSON object `400`; both are counted on
  `agentkourier_webhook_requests_total` with the reason, and neither reaches a trigger. The other bounds of a delivery
  and of a webhook connection are in [Limits](../../../docs/reference/limits.md).
- What the webhook platform serves and does not (no questions, no approvals, no feedback buttons, no reactions) is in
  [What each platform supports](../../../docs/reference/platforms.md).

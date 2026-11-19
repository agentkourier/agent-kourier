# webhook-jira

A Jira Cloud webhook sends issue events to Agent Kourier. An issue created in a project starts a plan by your agent, and an
edit of its summary or description starts a revised plan in the same session. Agent Kourier posts nothing on Jira: the
agent reads the issue and comments on it with its own Jira tools, as a service account.

It holds the Agent Kourier side only: a ChatConnection of the `webhook` platform, a Binding and the AgentBackend it refers
to. It ships no agent. Point the Binding at any A2A agent (A2A is the Agent2Agent protocol) that has Jira tools, and
GitHub tools if it should search a repository as the prompt asks
([Connect a generic A2A agent](../../../docs/how-to/connect-a2a-agent.md)).

## What it does

The ChatConnection, `kourier/chatconnection.yaml`, takes each delivery at
`https://<host>/chat/webhook/acme-webhooks/jira` and checks it before it reads any of it: the hex HMAC-SHA256 of the raw
body under the Secret's `webhookSecret`, in `X-Hub-Signature` after `sha256=`. It then maps the JSON body onto an event:

| Of the event | From | Read as |
|---|---|---|
| The channel | `/issue/fields/project/key`, such as `OPS` | The Binding whose `chat.channel` is that project key takes the event. |
| The actor | `/comment/author/accountId`, `/user/accountId` | An event whose actor is the connection's `identity`, the service account's account ID (compared ignoring ASCII case), is the agent's own and is dropped. |
| The thread | the `issue` field: `OPS-7` | One session for each issue. |
| The delivery's ID | the `X-Atlassian-Webhook-Identifier` header | Named in the log line of each delivery, and in the warning of one a limit stopped. |
| The fields | `event` (`/webhookEvent`), `issue`, `changed` (the first changed field of the changelog) (id fields), and `type`, `summary`, `description`, `comment` (text) | The triggers' `match` and `exclude` keys. The prompt template reads id fields alone; the text fields reach the agent only in the untrusted block. |

The Binding, `kourier/binding.yaml`, has two `webhookEvent` triggers:

1. A `jira:issue_created` event starts a plan: "Jira issue OPS-7 was created in project OPS."
2. A `jira:issue_updated` event whose first changed field is `summary` or `description` starts a revised plan in the
   issue's session: "Jira issue OPS-7 of project OPS was updated (its summary changed)."

`chat.output` is `none`: the only output a webhook connection takes, since the platform sends nothing.

Each prompt ends with a block that holds the event's actor and each field that has a value, under the heading "The
webhook event that triggered this", marked as untrusted data: an issue's summary and description are written by whoever
filed or edited it.

## What you'll see on Jira

| What happens on Jira | What follows |
|---|---|
| An issue is created in the project | The agent's comment on it with a plan, posted by the service account. |
| The issue's summary or description is edited | A comment with the revised plan, in the same session as the first. |
| Any other edit, such as of its status | Nothing. |
| The agent comments | Jira sends the `comment_created` event of it too, with the service account as the author. Agent Kourier answers it `200` and drops it as the agent's own (`outcome="self"`). |

## Prerequisites

- **A Jira Cloud admin webhook** with a secret, for the events **Issue created**, **Issue updated** and **Comment
  created**, filtered by JQL to your project (`project = OPS`). The header names, the signature's form and the body's
  paths this recipe reads follow Atlassian's webhook documentation, and this repository has not checked them against a
  live delivery.
- **A service account for the agent**, whose account ID is the `identity`.
- **An A2A agent that has Jira tools** acting as that account, and its URL. The AgentBackend's `url` is a placeholder
  (`REPLACE-ME`).
- **The chat listener reachable over HTTPS** from Jira: the chart's `chatListener.enabled: true`, and an Ingress
  (`chatListener.ingress`) or another way to reach the chat Service. A broker with no chat listener does not run a webhook
  connection: as its own connection it does not start, and as a team's connection it is not started.
- **A chart release that has custom resources, and Kubernetes 1.33 or later**, as the other recipes need.

## Files

```
kourier/
  chatconnection.yaml  the webhook connection: verify, channel, actor, thread, delivery ID and fields
  binding.yaml         the two triggers and their prompts
  agentbackend.yaml    your agent's A2A endpoint (a placeholder)
```

## Apply it

1. Replace the placeholders: the `identity` with the service account's account ID, `OPS` with your project's key in
   the Binding's `channel`, `acme/widgets` in the first prompt with the repository the agent searches, and the
   AgentBackend's `url` with your agent's. On the `a2a` dialect the URL alone selects the agent, and the Binding's
   `agent.name` and `agent.namespace` are labels.
2. Make the namespace and the Secret. The secret is the one you give the webhook; keep it out of your shell's history by
   reading it from a file:

    ```sh
    kubectl create namespace acme-webhooks
    kubectl -n acme-webhooks create secret generic jira-webhook --from-file=webhookSecret=./jira-webhook-secret
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
        acme-webhooks: [jira-webhook]
    ```

    The connection is then a team connection of the broker, served beside the broker's own connection if it has one. To
    make it the broker's own connection instead, set `config.chatConnection: acme-webhooks/jira` and
    `config.chatConnectionSecret: jira-webhook`.
4. Apply the resources:

    ```sh
    kubectl apply -f examples/recipes/webhook-jira/kourier/
    kubectl -n acme-webhooks get akbinding jira-plans
    ```

5. Set the webhook's URL to `https://<host>/chat/webhook/acme-webhooks/jira`, and its secret to the Secret's value.

## What to change

- The events of each trigger's `match`, and the changed fields of the second.
- The prompts. A template reads `.Channel` and the id fields of the connection (`.Fields.issue`, `.Fields.changed`) and
  nothing else: a text field reaches the agent only in the untrusted block.
- The `limits` of each trigger.
- `changed` reads the first item of the changelog alone: an edit that changes the status and the summary at once starts
  a revised plan only when the summary is the first item.

## Checks

- `go test ./test/recipes` loads `kourier/` with Agent Kourier's own loader and resolves the Binding's references.
- `go test ./internal/broker -run TestWebhookRecipeJira` runs this recipe in one process: a fake Jira signs and sends an
  issue's creation, an edit of its status and an edit of its summary, to a broker that serves these files, and a stub
  agent comments on the fake's API. It checks the prompts, the two comments, the drop of the agent's own events, a
  delivery that is unsigned and one signed with another secret (each answered `401`), the same delivery sent twice (taken
  once), and a start that a limit stops.

## Limits

- A plan, and a revised plan: at most 10 of each start in any hour, 1 of each runs at once, and 30 of each start in a
  day. A start that a limit stops is answered `200`, and no investigation begins. The broker logs it at WARN ("a webhook
  event was not investigated"), with its delivery ID: the event is lost unless it is delivered again. A later delivery of
  it is decided again.
- A delivery that is not verified is answered `401`, and one that is not a JSON object `400`; both are counted on
  `agentkourier_webhook_requests_total` with the reason, and neither reaches a trigger. The other bounds of a delivery
  and of a webhook connection are in [Limits](../../../docs/reference/limits.md).
- What the webhook platform serves and does not (no questions, no approvals, no feedback buttons, no reactions) is in
  [What each platform supports](../../../docs/reference/platforms.md).

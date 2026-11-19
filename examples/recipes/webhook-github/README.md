# webhook-github

A GitHub App sends its webhooks to Agent Kourier. A pull request that is opened, reopened, pushed to or marked ready for
review starts a review by your agent, and a comment on the pull request that mentions the App starts a reply in the same
session. Agent Kourier posts nothing on GitHub: the agent reads the pull request and posts its review or comment with its
own GitHub tools, as the App.

It holds the Agent Kourier side only: a ChatConnection of the `webhook` platform, a Binding and the AgentBackend it refers
to. It ships no agent. Point the Binding at any A2A agent (A2A is the Agent2Agent protocol) that has GitHub tools and posts
as the App ([Connect a generic A2A agent](../../../docs/how-to/connect-a2a-agent.md)).

## What it does

The ChatConnection, `kourier/chatconnection.yaml`, takes each delivery at
`https://<host>/chat/webhook/acme-webhooks/github` and checks it before it reads any of it: the hex HMAC-SHA256 of the raw
body under the Secret's `webhookSecret`, in `X-Hub-Signature-256` after `sha256=`. It then maps the JSON body onto an event:

| Of the event | From | Read as |
|---|---|---|
| The channel | `/repository/full_name`, such as `acme/widgets` | The Binding whose `chat.channel` is that repository takes the event. |
| The actor | `/sender/login`, `/comment/user/login`, `/review/user/login` | An event whose actor is the connection's `identity`, `acme-review-agent[bot]` (compared ignoring ASCII case), is the agent's own and is dropped. |
| The thread | the `number` field, after `pr-`: `pr-42` | One session for each pull request. A comment on it continues that session. |
| The delivery's ID | the `X-GitHub-Delivery` header | Named in the log line of each delivery, and in the warning of one a limit stopped. |
| The fields | `event` (the `X-GitHub-Event` header), `action`, `number`, `prNumber`, `draft`, `repo`, `headSHA`, `baseSHA` (id fields), and `title`, `body`, `headRef`, `url`, `comment` (text) | The triggers' `match` and `exclude` keys. The prompt template reads id fields alone; the text fields reach the agent only in the untrusted block. |

The Binding, `kourier/binding.yaml`, has two `webhookEvent` triggers:

1. A `pull_request` event whose action is `opened`, `reopened`, `synchronize` or `ready_for_review`, of a pull request
   that is not a draft, starts a review: "GitHub pull request #42 in repository acme/widgets needs a review", with the
   head and base commits.
2. An `issue_comment` or `pull_request_review_comment` that is `created`, on a pull request (its `url` holds `/pull/`),
   and whose comment mentions `@acme-review-agent`, starts a reply in the pull request's session.

`chat.output` is `none`: the only output a webhook connection takes, since the platform sends nothing.

Each prompt ends with a block that holds the event's actor and each field that has a value, under the heading "The
webhook event that triggered this", marked as untrusted data: a pull request's title and body and a comment are written
by whoever opened or commented on it.

## What you'll see on GitHub

| What happens on GitHub | What follows |
|---|---|
| A pull request is opened in the repository | The agent's review on it, posted by the App. |
| A comment that does not mention the App | Nothing. |
| A comment that mentions `@acme-review-agent` | The agent's reply on the pull request, in the same session as its review. |
| The agent posts its review or comment | GitHub sends the webhook of it too, with the App as the sender. Agent Kourier answers it `200` and drops it as the agent's own (`outcome="self"`). |
| A draft pull request | Nothing, until it is marked ready for review. |

## Prerequisites

- **A GitHub App** installed on the repository, with a webhook secret, and subscribed to the events **Pull request**,
  **Issue comment**, **Pull request review** and **Pull request review comment**. Its slug is the `identity`, with `[bot]`
  after it. The headers and fields this recipe reads follow GitHub's webhook documentation, and this repository has not
  checked them against a live delivery.
- **An A2A agent that has GitHub tools** acting as the App, and its URL. The AgentBackend's `url` is a placeholder
  (`REPLACE-ME`).
- **The chat listener reachable over HTTPS** from GitHub: the chart's `chatListener.enabled: true`, and an Ingress
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

1. Replace the placeholders: `acme-review-agent` with your App's slug (in `identity`, and in the comment trigger's
   regex), `acme/widgets` with your repository (`owner/name`) in the Binding's `channel`, and the AgentBackend's `url`
   with your agent's. On the `a2a` dialect the URL alone selects the agent, and the Binding's `agent.name` and
   `agent.namespace` are labels.
2. Make the namespace and the Secret. The secret is the one you give the App's webhook; keep it out of your shell's
   history by reading it from a file:

    ```sh
    kubectl create namespace acme-webhooks
    kubectl -n acme-webhooks create secret generic github-webhook --from-file=webhookSecret=./github-webhook-secret
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
        acme-webhooks: [github-webhook]
    ```

    The connection is then a team connection of the broker, served beside the broker's own connection if it has one. To
    make it the broker's own connection instead, set `config.chatConnection: acme-webhooks/github` and
    `config.chatConnectionSecret: github-webhook`. The broker builds the platform of its own connection when it starts and
    when a replica takes the lead, so an edit to that connection's `spec.webhook` applies when Agent Kourier restarts. A
    team connection restarts by itself when its ChatConnection changes.
4. Apply the resources:

    ```sh
    kubectl apply -f examples/recipes/webhook-github/kourier/
    kubectl -n acme-webhooks get akbinding github-reviews
    ```

5. Set the App's webhook URL to `https://<host>/chat/webhook/acme-webhooks/github`, its content type to
   `application/json` (the connection parses the body as JSON, and answers any other body `400`), and its secret to the
   Secret's value.

## What to change

- The events and actions of each trigger's `match`, and the regex of the mention.
- The prompts. A template reads `.Channel` and the id fields of the connection (`.Fields.number`, `.Fields.repo`,
  `.Fields.headSHA`, ...) and nothing else: a text field reaches the agent only in the untrusted block.
- The `limits` of each trigger.
- `thread`: without it, every event is a session of its own.

## Checks

- `go test ./test/recipes` loads `kourier/` with Agent Kourier's own loader and resolves the Binding's references.
- `go test ./internal/broker -run TestWebhookRecipeGitHub` runs this recipe in one process: a fake GitHub signs and sends
  a pull request, a comment that does not mention the App and one that does, to a broker that serves these files, and a
  stub agent posts on the fake's API. It checks the prompts, the two posts, the drop of the agent's own events, a
  delivery that is unsigned and one signed with another secret (each answered `401`), the same delivery sent twice (taken
  once), and a start that a limit stops.

## Limits

- A review: at most 10 start in any hour, 1 runs at once, and 40 start in a day. A reply: 20 an hour, 1 at once, 60 a day.
  A start that a limit stops is answered `200`, and no investigation begins. The broker logs it at WARN ("a webhook
  event was not investigated"), with its delivery ID: the event is lost unless it is delivered again, by GitHub's
  redelivery or by hand. A later delivery of it is decided again.
- A delivery that is not verified is answered `401`, and one that is not a JSON object `400`; both are counted on
  `agentkourier_webhook_requests_total` with the reason, and neither reaches a trigger. The other bounds of a delivery
  and of a webhook connection are in [Limits](../../../docs/reference/limits.md).
- What the webhook platform serves and does not (no questions, no approvals, no feedback buttons, no reactions) is in
  [What each platform supports](../../../docs/reference/platforms.md).

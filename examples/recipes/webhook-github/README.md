# webhook-github

A repository's webhook sends GitHub's events to Agent Kourier. A pull request that is opened, reopened, pushed to or
marked ready for review starts a review by the agent, and a comment on the pull request that mentions the agent's machine
user (a GitHub account kept for the agent) starts a reply in the same session. Agent Kourier posts nothing on GitHub: the
agent reads the pull request and posts its review or comment with its own GitHub tools, as the machine user.

It holds the Agent Kourier side, a ChatConnection of the `webhook` platform, a Binding and the AgentBackend it refers to,
in `kourier/`, and the agent in `agent/`: the [prompt-agent](../prompt-agent/README.md) image with an `agent.yaml` of its
own and GitHub's github-mcp-server beside it ([agent/README.md](agent/README.md)). To run an agent of your own instead,
put its A2A endpoint in the AgentBackend ([Connect a generic A2A agent](../../../docs/how-to/connect-a2a-agent.md)).

> **Experimental.** Needs Agent Kourier 0.6.0 or later: 0.5.x refuses `platform: webhook`. The `webhook` platform is
> new, and its fields may change in a later version. This mapping was run end to end against GitHub on 2026-11-17, on
> a pre-0.6.0 build of the broker, with a GitHub App's webhook and a small MCP server of the run's own, not with this
> recipe's machine user, repository webhook and agent: those were not run against GitHub or a live model.
> [The GitHub page](../../../docs/how-to/webhooks/github.md) says what was seen in that run and what comes from
> GitHub's documentation.

## What it does

The ChatConnection, `kourier/chatconnection.yaml`, takes each delivery at
`https://<host>/chat/webhook/acme-webhooks/github` and checks it before it reads any of it: the hex HMAC-SHA256 of the raw
body under the Secret's `webhookSecret`, in `X-Hub-Signature-256` after `sha256=`. It then maps the JSON body onto an event:

| Of the event | From | Read as |
|---|---|---|
| The channel | `/repository/full_name`, such as `acme/widgets` | The Binding whose `chat.channel` is that repository takes the event. |
| The actor | `/sender/login`, `/comment/user/login`, `/review/user/login` | An event whose actor is the connection's `identity`, the machine user's login `acme-review-agent` (compared ignoring ASCII case), is the agent's own and is dropped. |
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
| A pull request is opened in the repository | The agent's review on it, posted by the machine user. |
| A comment that does not mention the machine user | Nothing. |
| A comment that mentions `@acme-review-agent` | The agent's reply on the pull request, in the same session as its review. |
| The agent posts its review or comment | GitHub sends the webhook of it too, with the machine user as the sender (*not run*: in the run, the App's echo carried the App's `[bot]` login). Agent Kourier answers it `200` and drops it as the agent's own (`outcome="self"`). |
| A draft pull request | Nothing, until it is marked ready for review. |

## Prerequisites

- **A webhook on the repository**, with a secret and the content type `application/json`, subscribed to the events
  **Pull requests**, **Issue comments**, **Pull request reviews** and **Pull request review comments**. The headers and
  the body's paths this recipe reads were seen in live deliveries of a GitHub App's webhook in the run of 2026-11-17,
  except the delivery header, `X-GitHub-Delivery`; that a repository's webhook sends the same follows GitHub's
  documentation, and was not run.
- **A machine user** with the Read role on the repository, with a fine-grained personal access token, and a model
  provider's key: the agent's [Prerequisites](agent/README.md#prerequisites). Not Write: with Write, an `APPROVE` the
  model is talked into counts toward a branch protection rule's required approvals; if it must have Write, require
  review from Code Owners, with every file owned and the machine user not an owner (the same Prerequisites). The
  machine user's login is the `identity`. Agent Kourier posts nothing on GitHub (`output: none`): the agent's reply
  text goes nowhere, and only what its tools do reaches GitHub.
- **The chat listener reachable over HTTPS** from GitHub: the chart's `chatListener.enabled: true`, and an Ingress
  (`chatListener.ingress`) or another way to reach the chat Service. A broker with no chat listener does not run a webhook
  connection: as its own connection it does not start, and as a team's connection it is not started.
- **Agent Kourier 0.6.0 or later** (0.5.x refuses `platform: webhook`), on custom resources, and Kubernetes 1.33 or later.

## Files

```
kourier/
  chatconnection.yaml  the webhook connection: verify, channel, actor, thread, delivery ID and fields
  binding.yaml         the two triggers and their prompts
  agentbackend.yaml    the agent's A2A endpoint: the Service of agent/
agent/                 the agent: prompt-agent's image, its agent.yaml, and github-mcp-server beside it (agent/README.md)
```

## Apply it

Follow [GitHub](../../../docs/how-to/webhooks/github.md) from its step 1. It is the one place for the steps: it
makes the account on GitHub and the Secret, has you run the agent, lists every value of these files to replace
(step 5), applies them, turns on the webhook, and tests it. [Start agents from webhooks](../../../docs/how-to/receive-webhooks.md#before-you-start)
has the chart values that every webhook recipe needs.

## What to change

- The events and actions of each trigger's `match`, and the regex of the mention.
- The prompts. A template reads `.Channel` and the id fields of the connection (`.Fields.number`, `.Fields.repo`,
  `.Fields.headSHA`, ...) and nothing else: a text field reaches the agent only in the untrusted block.
- The `limits` of each trigger.
- `thread`: without it, every event is a session of its own.

## Checks

- `go test ./test/recipes` loads `kourier/` with Agent Kourier's own loader and resolves the Binding's references.
- `go test ./internal/broker -run TestWebhookRecipeGitHub` runs this recipe in one process: a fake GitHub signs and sends
  a pull request, a comment that does not mention the machine user and one that does, to a broker that serves these files, and a
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

# webhook-bitbucket

A Bitbucket Cloud repository webhook sends pull request events to Agent Kourier. A pull request that is created or updated
starts a review by your agent, and a comment on it that mentions the agent starts a reply in the same session. Agent
Kourier posts nothing on Bitbucket: the agent reads the pull request and comments on it with its own Bitbucket tools,
through an access token.

It holds the Agent Kourier side only: a ChatConnection of the `webhook` platform, a Binding and the AgentBackend it refers
to. It ships no agent. Point the Binding at any A2A agent (A2A is the Agent2Agent protocol) that has Bitbucket tools
([Connect a generic A2A agent](../../../docs/how-to/connect-a2a-agent.md)).

> **Experimental.** Needs Agent Kourier 0.6.0 or later: 0.5.x refuses `platform: webhook`. The `webhook` platform is
> new, and its fields may change in a later version. The setup was run end to end against Bitbucket Cloud on 2026-11-17, on a
> pre-0.6.0 build of the broker; [the Bitbucket Cloud page](../../../docs/how-to/webhooks/bitbucket.md) says what was seen in
> that run and what comes from Atlassian's documentation.

## What it does

The ChatConnection, `kourier/chatconnection.yaml`, takes each delivery at
`https://<host>/chat/webhook/acme-webhooks/bitbucket` and checks it before it reads any of it: the hex HMAC-SHA256 of the
raw body under the Secret's `webhookSecret`, in `X-Hub-Signature` after `sha256=`. It then maps the JSON body onto an event:

| Of the event | From | Read as |
|---|---|---|
| The channel | `/repository/full_name`, such as `acme/widgets` | The Binding whose `chat.channel` is that workspace and repository takes the event. |
| The actor | `/actor/account_id`, `/comment/user/account_id` | An event whose actor is the connection's `identity`, the account ID of the access token's bot user (compared ignoring ASCII case), is the agent's own and is dropped. |
| The thread | the `pr` field, after `pr-`: `pr-7` | One session for each pull request. A comment on it continues that session. |
| The delivery's ID | the `X-Request-UUID` header | Named in the log line of each delivery, and in the warning of one a limit stopped. |
| The fields | `event` (the `X-Event-Key` header), `pr`, `draft`, `commit` (id fields), and `title`, `description`, `comment` (text) | The triggers' `match` and `exclude` keys. The prompt template reads id fields alone; the text fields reach the agent only in the untrusted block. |

The Binding, `kourier/binding.yaml`, has two `webhookEvent` triggers:

1. A `pullrequest:created` or `pullrequest:updated` event that names a commit, of a pull request that is not a draft and
   with no comment in it, starts a review: "Bitbucket pull request #7 of acme/widgets needs a review at commit
   a1b2c3d4e5f6."
2. A `pullrequest:comment_created` event whose comment mentions `@acme-agent`, typed as a name, starts a reply in the pull
   request's session. A mention picked from Bitbucket's mention list may be stored in another form, which the regex does
   not match (not verified; the page's "Change what starts a turn" shows how to match it too).

`chat.output` is `none`: the only output a webhook connection takes, since the platform sends nothing.

Each prompt ends with a block that holds the event's actor and each field that has a value, under the heading "The
webhook event that triggered this", marked as untrusted data: a pull request's title and description and a comment are
written by whoever opened or commented on it.

## What you'll see on Bitbucket

| What happens on Bitbucket | What follows |
|---|---|
| A pull request is created in the repository | The agent's review, as a comment on it. |
| The pull request is updated | Another review, in the same session. In the run Bitbucket sent `pullrequest:updated` for an edit of the title or description too, not only for a push, and each starts a review. |
| A comment that mentions `@acme-agent` | The agent's reply on the pull request, in the same session. |
| The agent comments | Bitbucket sends the `pullrequest:comment_created` event of it too, with the token's bot user as the actor. Agent Kourier answers it `200` and drops it as the agent's own (`outcome="self"`). |
| A draft pull request | Nothing, by the draft field `/pullrequest/draft`, which follows Atlassian's documentation and was not part of the run. |

## Prerequisites

- **A Bitbucket Cloud repository webhook** with a secret, for the triggers **Pull request: Created**, **Updated** and
  **Comment created**. The headers, the signature's form and the body's paths this recipe reads were seen in live
  deliveries in the run, except the delivery header, `X-Request-UUID`, and the draft field,
  `/pullrequest/draft`, which follow Atlassian's documentation. If Bitbucket does not send the draft
  field, a draft pull request is reviewed too.
- **An access token for the agent**, whose bot user's account ID is the `identity`.
- **Your own A2A agent: the recipe ships none.** It needs an agent card that advertises streaming, and Bitbucket tools that use that token;
  the Binding is not `Ready` until the broker can fetch that card. The AgentBackend's `url` is a placeholder
  (`REPLACE-ME`) for its endpoint. Agent Kourier posts nothing on Bitbucket (`output: none`): the agent's reply text goes
  nowhere, and only what its tools do reaches Bitbucket.
- **The chat listener reachable over HTTPS** from Bitbucket: the chart's `chatListener.enabled: true`, and an Ingress
  (`chatListener.ingress`) or another way to reach the chat Service. A broker with no chat listener does not run a webhook
  connection: as its own connection it does not start, and as a team's connection it is not started.
- **Agent Kourier 0.6.0 or later** (0.5.x refuses `platform: webhook`), on custom resources, and Kubernetes 1.33 or later.

## Files

```
kourier/
  chatconnection.yaml  the webhook connection: verify, channel, actor, thread, delivery ID and fields
  binding.yaml         the two triggers and their prompts
  agentbackend.yaml    your agent's A2A endpoint (a placeholder)
```

## Apply it

Follow [Bitbucket Cloud](../../../docs/how-to/webhooks/bitbucket.md) from its step 1. It is the one place for the steps: it
makes the account on Bitbucket Cloud and the Secret, has you run the agent, lists every value of these files to replace
(step 5), applies them, turns on the webhook, and tests it. [Start agents from webhooks](../../../docs/how-to/receive-webhooks.md#before-you-start)
has the chart values that every webhook recipe needs.

## What to change

- The events of each trigger's `match`, and the regex of the mention. A trigger cannot tell a push from an edit of the
  title, since both are `pullrequest:updated`: to review a commit once, leave `pullrequest:updated` out of the match, or
  have the agent skip a commit it has reviewed.
- The prompts. A template reads `.Channel` and the id fields of the connection (`.Fields.pr`, `.Fields.commit`) and
  nothing else: a text field reaches the agent only in the untrusted block.
- The `limits` of each trigger.

## Checks

- `go test ./test/recipes` loads `kourier/` with Agent Kourier's own loader and resolves the Binding's references.
- `go test ./internal/broker -run TestWebhookRecipeBitbucket` runs this recipe in one process: a fake Bitbucket signs and
  sends a pull request's creation and a comment that mentions the agent, to a broker that serves these files, and a stub
  agent comments on the fake's API. It checks the prompts, the two comments, the drop of the agent's own events, a
  delivery that is unsigned and one signed with another secret (each answered `401`), the same delivery sent twice (taken
  once), and a start that a limit stops.

## Limits

- A review: at most 10 start in any hour, 1 runs at once, and 40 start in a day. A reply: 20 an hour, 1 at once, 60 a
  day. A start that a limit stops is answered `200`, and no investigation begins. The broker logs it at WARN ("a webhook
  event was not investigated"), with its delivery ID: the event is lost unless it is delivered again. A later delivery of
  it is decided again.
- The `commit` hash in a Bitbucket body is short (12 characters in the run's deliveries), not the full 40.
- A delivery that is not verified is answered `401`, and one that is not a JSON object `400`; both are counted on
  `agentkourier_webhook_requests_total` with the reason, and neither reaches a trigger. The other bounds of a delivery
  and of a webhook connection are in [Limits](../../../docs/reference/limits.md).
- What the webhook platform serves and does not (no questions, no approvals, no feedback buttons, no reactions) is in
  [What each platform supports](../../../docs/reference/platforms.md).

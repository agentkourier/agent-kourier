# Bitbucket Cloud

For a platform engineer who wants an agent to review pull requests on Bitbucket Cloud and answer the comments that ask it. At the
end, a repository's webhook sends its pull request events to Agent Kourier, each pull request has a session of its own, and the
agent comments on Bitbucket with a repository access token.

!!! warning "Experimental"

    The `webhook` platform is new, and its fields may change in a later version. This setup was run end to end against Bitbucket
    Cloud on 2026-11-17, with this mapping on an earlier, unreleased version of the broker. What this page says Bitbucket sends was
    seen in those deliveries, and Atlassian can change it. The delivery header, `X-Request-UUID`, was not part of that run.
    Bitbucket's behaviour and labels that the run did not see come from Atlassian's documentation, read on 2026-11-18.

## What you end with

- A pull request that is created or updated, and is not a draft, starts a review: a turn of the session `pr-<id>`, in which the agent
  reads the pull request and its diff and posts one comment with its review.
- A comment on that pull request that mentions the token's name (`@acme-agent`) starts a reply: a turn of the same session, so the
  agent can see the review it posted.
- The token's own comments, and its pushes, are dropped as the agent's own (`self`), so they start nothing.

## What you need

- What [Start agents from webhooks](../receive-webhooks.md#before-you-start) lists for every source: the chat listener on HTTPS, a
  broker that admits the namespace `acme-webhooks` and reads the Secret `bitbucket-webhook`, and an A2A agent.
- Admin rights on the repository, to create its access token and its webhook.
- `kubectl`, `curl` and `jq`.

## 1. Create the repository access token

The agent works with a repository access token. Atlassian's documentation says such a token belongs to the repository, not to a
person. In the run it acted as a bot user, an app user, whose name was the token's name.

On the repository, open **…**, **Settings**, then, under **Security**, **Access tokens**, and choose **Create access token**:

| Field | Value |
|---|---|
| Name | The name the agent is mentioned by, such as `acme-agent`. |
| Expiry | A date you will renew it by, or **No expiry**, which Atlassian's documentation says a workspace admin can take away. |
| **Repositories** | **Read**. |
| **Pull requests** | **Read** and **Write**. |

Choose **Create**. Atlassian's documentation says Bitbucket shows the token once: copy it, and save it where only you can read it:

```sh
umask 077; mkdir -p ~/.config/webhooks
read -rs v; printf '%s' "$v" > ~/.config/webhooks/bitbucket-token; unset v     # paste the token, then Return
printf 'Authorization: Bearer %s\n' "$(cat ~/.config/webhooks/bitbucket-token)" > ~/.config/webhooks/bitbucket.header
```

Atlassian's documentation says workspace and project access tokens need Bitbucket Cloud Premium; it does not say so of repository
access tokens.

## 2. Find the identity

The connection's `spec.webhook.identity` is the token's bot user's `account_id`, such as `712020:…`. In the run the token was refused
on `GET /2.0/user` (403), so learn the ID from something the token writes: post one comment on an open pull request, read its author
from the response, and delete it. **Pull requests: Write** covers both calls. Open a pull request for the check if the repository has
none; the webhook is not on yet (step 6), so nothing reaches the broker. The token is read from the header file of step 1:

```sh
H=~/.config/webhooks/bitbucket.header
API=https://api.bitbucket.org/2.0/repositories/acme/widgets        # your workspace/repository
PR=1                                                               # an open pull request's ID
resp=$(curl -s -H @"$H" -H 'Content-Type: application/json' -d '{"content":{"raw":"identity check"}}' \
  "$API/pullrequests/$PR/comments")
echo "$resp" | jq -r .user.account_id
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H @"$H" "$API/pullrequests/$PR/comments/$(echo "$resp" | jq -r .id)"
```

The first response holds the identity. The delete returns `204` when it worked. The run made this check with a comment on a commit, with a
token that also had **Repositories: Write**; the pull request comment, with the permissions of step 1, was *not run*.

## 3. Make the Secret

Generate the webhook's secret into a file, with no newline at the end ([why](../receive-webhooks.md#keep-the-secrets-out-of-sight)),
and make the connection's Secret from it, with the key `webhookSecret`:

```sh
umask 077
openssl rand -hex 32 | tr -d '\n' > ~/.config/webhooks/bitbucket-webhook-secret
kubectl -n acme-webhooks create secret generic bitbucket-webhook \
  --from-file=webhookSecret="$HOME/.config/webhooks/bitbucket-webhook-secret"
```

The broker reads it once `bitbucket-webhook` is in the chart's `rbac.secretNames` for `acme-webhooks`
([Before you start](../receive-webhooks.md#before-you-start)). The access token is not in this Secret, and the broker never reads it:
it belongs to the agent's tools (step 4).

## 4. Give the agent tools

The recipe's prompts ask the agent to do these, with the token as `Authorization: Bearer <token>`:

| Operation | For | Bitbucket's REST API 2.0 |
|---|---|---|
| Read a pull request: its title, description and source commit | Both triggers | `GET /repositories/{workspace}/{repo}/pullrequests/{id}` |
| Read its diff | The review | `GET /repositories/{workspace}/{repo}/pullrequests/{id}/diff`, which Bitbucket's API reference says redirects: follow it |
| Post a comment on the pull request | Both triggers | `POST /repositories/{workspace}/{repo}/pullrequests/{id}/comments` |

As of 2026-11-17 no Bitbucket MCP server took a repository access token over HTTP, so the run used a small MCP server of its own
([Tools for the agent](../receive-webhooks.md#tools-for-the-agent)). The token expires on the date set in step 1.

## 5. Apply the manifests

The recipe [`webhook-bitbucket`](https://github.com/agentkourier/agent-kourier/tree/main/examples/recipes/webhook-bitbucket) holds
three files. The ChatConnection:

```yaml
--8<-- "webhook-bitbucket/kourier/chatconnection.yaml"
```

The Binding, with its two triggers:

```yaml
--8<-- "webhook-bitbucket/kourier/binding.yaml"
```

The AgentBackend:

```yaml
--8<-- "webhook-bitbucket/kourier/agentbackend.yaml"
```

Replace these values before you apply them:

| Where | The recipe's value | Yours |
|---|---|---|
| ChatConnection `spec.webhook.identity` | `712020:00000000-0000-0000-0000-000000000001` | The `account_id` from step 2. |
| Binding `spec.chat.channel` | `acme/widgets` | The repository, as `workspace/repository`: its `repository.full_name`. |
| The second trigger's `comment` regex | `@acme-agent` | `@` and the token's name. |
| Each trigger's `limits` | 10 reviews an hour, 40 a day; 20 replies an hour, 60 a day; one of each at a time | What you will pay for. |
| AgentBackend `spec.url` | `http://REPLACE-ME.agents.svc.cluster.local:8080` | Your agent's A2A endpoint. On the `a2a` dialect the URL alone selects the agent, and the Binding's `agent.name` and `agent.namespace` are labels. |
| Every object's `metadata.namespace`, and the Secret's | `acme-webhooks` | The admitted namespace that holds the Secret, if you use another. |

Copy the recipe's `kourier/` directory, edit it, then apply it and wait for the Binding:

```sh
kubectl apply -f kourier/
kubectl -n acme-webhooks wait --for=condition=Ready akbinding/bitbucket-reviews --timeout=120s
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier logs "$LEADER" | grep '"webhook: taking requests"' | grep 'acme-webhooks/bitbucket'
```

## 6. Add the webhook

On the repository, open **…**, **Settings**, then, under **Workflow**, **Webhooks**, and choose **Add webhook**:

| Field | Value |
|---|---|
| **Title** | Any, such as `agent-kourier`. |
| **URL** | `https://<host>/chat/webhook/acme-webhooks/bitbucket`: the namespace and the name of the ChatConnection. |
| **Secret** | The secret of step 3. Copy it with `pbcopy < ~/.config/webhooks/bitbucket-webhook-secret` and paste it, rather than **Generate secret**. |
| **Active** | Checked. |
| **Skip certificate verification** | Unchecked. |
| **Triggers** | **Choose from a full list of triggers**: under Pull Request, **Created**, **Updated** and **Comment created**. Uncheck **Repository push**. |

Save, then look at the webhook in the list and check that it is active. In the run one was saved inactive, and sent nothing.
No test delivery from Bitbucket is known to this page (not verified): make its first real event the test.

## 7. Test it

Open a pull request on the repository, not as a draft. Within seconds the broker logs these, from the leader:

```sh
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier logs "$LEADER" | grep 'acme-webhooks/bitbucket' | grep -E \
  '"msg":"(chat: webhook event matched a trigger|webhook: delivery answered|session turn started|session turn ended)"'
```

```text
{"time":"…","level":"INFO","msg":"chat: webhook event matched a trigger","binding":"acme-webhooks/bitbucket-reviews","channel":"acme/widgets","message":"<SHA-256 of the body>","thread":"pr-3","delivery":"<X-Request-UUID>","trigger":0}
{"time":"…","level":"INFO","msg":"webhook: delivery answered","connection":"acme-webhooks/bitbucket","outcome":"accepted","reason":"none","delivery":"<X-Request-UUID>",…}
{"time":"…","level":"INFO","msg":"session turn started","binding":"acme-webhooks/bitbucket-reviews","connection":"acme-webhooks/bitbucket","channel":"acme/widgets","thread":"pr-3",…}
{"time":"…","level":"INFO","msg":"session turn ended",…,"thread":"pr-3",…,"outcome":"completed","duration_ms":…}
```

The agent's review shows as a comment on the pull request. In the run the review was started by an edit of the description
(`pullrequest:updated`): the `pullrequest:created` event never left Bitbucket, because the webhook had been saved inactive. Then
comment `@acme-agent why this approach?`: the log shows the same lines with `"trigger":1` and the same `"thread":"pr-3"`, and the
agent replies.

Read the counts on the leader's `/metrics`. Every series of the connection exists at 0, so leave those out:

```sh
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier port-forward "$LEADER" 8080:8080 &
curl -s localhost:8080/metrics | grep -E '^agentkourier_(webhook_requests_total|trigger_decisions_total)' \
  | grep 'acme-webhooks/bitbucket' | grep -v ' 0$'
```

```text
agentkourier_trigger_decisions_total{binding="acme-webhooks/bitbucket-reviews",decision="started"} 2
agentkourier_webhook_requests_total{connection="acme-webhooks/bitbucket",outcome="accepted",reason="none"} 2
agentkourier_webhook_requests_total{connection="acme-webhooks/bitbucket",outcome="self",reason="none"} 2
```

These are the run's counts for one pull request and one comment; read them as at least, not exactly, since every signed event with a
channel and a thread counts as `accepted`, whether a trigger matched it or not. `decision="started"` is the turns that started.
`self` is the agent's two comments: their actor in `/actor/account_id` is the token's bot user. A push made with the token to the pull
request's branch counts as `self` too.

## Change what starts a turn

Each change is to the recipe's ChatConnection fields or Binding triggers. A field that a trigger matches must be in the connection's
`fields` (at most 32), and a `text` field can be matched but not read by `promptTemplate`. A trigger has at most 6 `match` and 6
`exclude` entries.

**Review once, not on every edit.** Bitbucket sends `pullrequest:updated` for every edit of the title or the description, and does not
say what changed, so the recipe reviews again on each, within its limits. To review only new pull requests:

```yaml
match: {event: '^pullrequest:created$', commit: '.'}
```

**Keep `comment: '.'` in the review trigger's `exclude`.** The event key is in the `X-Event-Key` header, which the signature does not
cover, and a comment event carries the pull request's commit too. Without the exclude, a signed comment body sent again under a pull
request event's key would start a review; with it, the body must agree that it is not a comment. A trigger you add that matches
pull request events needs the same exclude.

**Only pull requests into one branch** (*not run*; Bitbucket's API schema has `pullrequest.destination.branch.name`). Add a field,
and match it:

```yaml
- {name: target, from: [/pullrequest/destination/branch/name], kind: id}     # in the ChatConnection's fields
match: {event: '^pullrequest:(created|updated)$', commit: '.', target: '^main$'}
```

**Several repositories.** A repository access token is its own bot user, held to its repository, and one connection has one
`spec.webhook.identity`. So each repository takes its own token (step 1), and its own connection: a copy of the recipe's
ChatConnection and Binding under other names, such as `bitbucket-gadgets`, with

- that token's `account_id` (step 2) as `spec.webhook.identity`, so that its own comments count as `self`;
- its own Secret, such as `bitbucket-gadgets-webhook`, made as in step 3 from a new secret, named in the copy's
  `spec.credentialsSecretRef.name`, and added to the chart's `rbac.secretNames` for `acme-webhooks`;
- the Binding's `spec.chat.channel` set to the repository and `connectionRef` to the new connection, and the reply trigger's regex set
  to `@` and that token's name;
- a webhook on that repository (step 6) with the URL `https://<host>/chat/webhook/acme-webhooks/bitbucket-gadgets` and the new
  secret.

A broker serves at most 16 ChatConnections ([Limits](../../reference/limits.md#run-the-broker)). Atlassian's documentation says a
workspace access token, one for every repository of a workspace, needs Bitbucket Cloud Premium, and that the web interface makes
webhooks only on a repository. One connection for a whole workspace was not tried.

## Troubleshooting

[When nothing happens](../receive-webhooks.md#when-nothing-happens) covers every source. For Bitbucket:

| What you see | Why, and what to do |
|---|---|
| Nothing at all | The webhook is inactive: open it, check **Active**, save. To see what Bitbucket sent, open **View requests** on the webhook and choose **Enable history**; Atlassian's documentation says only the deliveries after that are recorded. |
| `unverified`, `bad_signature`, on every delivery | The webhook's secret is not the Secret's `webhookSecret`. Set it on the webhook again from the file of step 3. |
| A review on every edit of the description | `pullrequest:updated` ([above](#change-what-starts-a-turn)). |
| A comment that mentions the agent starts nothing | The regex wants `@` and the token's name. The bot user may not be offered in Bitbucket's mention list (not verified), so type the name: the comment's raw text then holds it as written. |
| `accepted`, and no review | The pull request is a draft, or `agentkourier_trigger_limited_total{binding="acme-webhooks/bitbucket-reviews"}` rose: with `maxConcurrent: 1`, a second pull request while a review runs is stopped, and the broker's response was `200`, so Bitbucket does not send it again, and the event is lost; the WARN line names its delivery. Raise `maxConcurrent`. |
| A delivery failed | Atlassian's documentation says Bitbucket waits 10 seconds for a response, and tries a delivery that got a `5xx`, such as the broker's `503`, up to two more times. |
| A turn, and nothing on Bitbucket | The agent's tools: an expired token, or one without **Pull requests: Write**. |

## What Bitbucket sends

What was seen in the run's deliveries:

- Bitbucket sends both `X-Hub-Signature` and `X-Hub-Signature-256`, each `sha256=<hex>` of the same HMAC. It names the event in the
  `X-Event-Key` header, read here as the field `event`: `pullrequest:created`, `pullrequest:updated`,
  `pullrequest:comment_created`.
- The actor is `/actor/account_id`. A comment's author is also `/comment/user/account_id`.
- `/pullrequest/source/commit/hash` is 12 characters, not 40.
- `pullrequest:updated` is sent on every edit of the title or the description, not only on a push, and does not say what changed.
- A mention of the bot user stays the plain text `@<token-name>` in `/comment/content/raw`, which the trigger's regex matches.
- The delivery's ID is in `X-Request-UUID`, by Atlassian's documentation (not part of the run).

# GitHub

For a platform engineer who wants an agent to review pull requests on GitHub and answer the comments that ask it. At the end, a
GitHub App sends its webhooks to Agent Kourier, each pull request has a session of its own, and the agent posts on GitHub as the
App, with the App's own token.

!!! warning "Experimental"

    Needs Agent Kourier 0.6.0 or later: 0.5.x refuses `platform: webhook`. The `webhook` platform is new, and its fields may change
    in a later version. This setup was run end to end against GitHub on 2026-11-17, with this mapping, on a pre-0.6.0 build of the
    broker. What this page says GitHub sends was seen in that run's deliveries, and GitHub can change it. GitHub's behaviour and
    labels that the run did not see come from GitHub's documentation, read on 2026-11-18, and say so where they appear. Not run: the
    delivery header `X-GitHub-Delivery`, a comment on a line of the diff (`pull_request_review_comment`), and every change marked
    *not run* below.

## What you end with

- A pull request that is opened, reopened, pushed to or marked ready for review, and is not a draft, starts a review: a turn of the
  session `pr-<number>`, in which the agent reads the pull request at its head commit and posts one review.
- A comment on that pull request that mentions the App (`@<app-slug>`) starts a reply: a turn of the same session, so the agent can
  see the review it posted. A comment in the pull request's conversation was run; a comment on a line of its diff
  (`pull_request_review_comment`) was *not run*.
- The App's own reviews and comments are dropped as the agent's own (`self`), so they start nothing.

## What you need

- **Agent Kourier 0.6.0 or later**, set up as [Before you start](../receive-webhooks.md#before-you-start) says: the chat listener on
  HTTPS at a public host, which this page calls `<host>` ([Expose the chat listener](../receive-webhooks.md#expose-the-chat-listener)),
  and a broker that admits the namespace `acme-webhooks` and reads the Secret `github-webhook`. The commands take the broker's release
  to be in the namespace `agent-kourier`; where they say `-n agent-kourier`, put yours.
- **An agent of your own.** The recipe ships no agent: you run an A2A agent with a streaming agent card and tools that act on GitHub
  with the App's own token (step 4; [The agent](../receive-webhooks.md#the-agent)). Agent Kourier posts nothing on GitHub: the
  Binding's `output: none` sends the agent's reply text nowhere, so only what its tools do reaches GitHub.
- The right to create a GitHub App on the account or organization that owns the repository, and to install it there.
- `kubectl`, `curl`, `jq` and `openssl`.

## 1. Create the GitHub App

The agent works as a GitHub App. An App posts as its bot, `<app-slug>[bot]`, and its installation can be held to the repositories
it needs.

Open **Settings**, **Developer settings**, **GitHub Apps**, **New GitHub App**: from your profile picture for an App of your own
account, or from **Your organizations**, then the organization's **Settings**, for one of an organization (menu path from GitHub's
documentation, read 2026-11-18). Fill the form:

| Field | Value |
|---|---|
| **GitHub App name** | A name for the agent, such as `acme-review-agent`. GitHub makes the App's slug from it; step 2 reads the slug back. |
| **Description**, **Homepage URL** | Any description, and any URL, such as the repository's. The URL is required. |
| **Callback URL**, **Expire user authorization tokens**, **Request user authorization (OAuth) during installation**, **Enable Device Flow**, **Setup URL**, **Redirect on update** | Leave as they are. The agent signs in as the App, never as a user. |
| **Active** (under Webhook) | Unchecked for now. Step 6 turns the webhook on, once the broker can take it. |
| **Repository permissions** | **Contents**: Read-only. **Issues**: Read & write. **Pull requests**: Read & write. **Metadata**: Read-only. |
| **Where can this GitHub App be installed?** | **Only on this account**. |

Choose **Create GitHub App**. Then, on the App's settings:

1. Note the App's ID, which its settings page shows: step 2 and the agent's tools use it.
2. Generate a private key. GitHub downloads it as a `.pem` file. Move it where only you can read it, and remove it from your
   downloads:

    ```sh
    umask 077; mkdir -p ~/.config/webhooks
    mv ~/Downloads/<the-downloaded-key>.pem ~/.config/webhooks/github-app.pem
    chmod 600 ~/.config/webhooks/github-app.pem
    ```

3. Choose **Install App**, then **Install** next to the account, and pick **Only select repositories**, with the repositories the
   agent reviews (and, for the [Jira](jira.md) and [Linear](linear.md) pages, the repository whose code the agent searches).

## 2. Find the identity

The connection's `spec.webhook.identity` is the App's bot login: its slug, then `[bot]`. That is the `sender.login` of what the App
does on GitHub. Read the slug with the App's own key, which also checks that the key works. The App signs a JSON Web Token (JWT) with
the key, valid for ten minutes, and asks GitHub who it is; the token is written to a header file, never to a command line:

```sh
umask 077
APP_ID=123456                                  # the App's ID from step 1
KEY=~/.config/webhooks/github-app.pem
b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
now=$(date +%s)
header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)
claims=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' $((now - 60)) $((now + 540)) "$APP_ID" | b64url)
sig=$(printf '%s.%s' "$header" "$claims" | openssl dgst -sha256 -sign "$KEY" | b64url)
printf 'Authorization: Bearer %s.%s.%s\n' "$header" "$claims" "$sig" > ~/.config/webhooks/github-app-jwt.header

curl -s -H @"$HOME/.config/webhooks/github-app-jwt.header" -H 'Accept: application/vnd.github+json' \
  https://api.github.com/app | jq -r '.slug + "[bot]"'
```

The response, such as `acme-review-agent[bot]`, is the identity. The mention that the recipe's reply trigger matches is the slug with
an `@`: `@acme-review-agent`.

The agent's tools need the installation's ID to get tokens. With the same header file:

```sh
curl -s -H @"$HOME/.config/webhooks/github-app-jwt.header" -H 'Accept: application/vnd.github+json' \
  https://api.github.com/app/installations | jq '.[] | {id, account: .account.login}'
rm ~/.config/webhooks/github-app-jwt.header
```

## 3. Make the Secret

Generate the webhook's secret into a file, with no newline at the end ([why](../receive-webhooks.md#keep-the-secrets-out-of-sight)),
and make the connection's Secret from it, with the key `webhookSecret`:

```sh
umask 077
openssl rand -hex 32 | tr -d '\n' > ~/.config/webhooks/github-webhook-secret
kubectl -n acme-webhooks create secret generic github-webhook \
  --from-file=webhookSecret="$HOME/.config/webhooks/github-webhook-secret"
```

The broker reads it once `github-webhook` is in the chart's `rbac.secretNames` for `acme-webhooks`
([Before you start](../receive-webhooks.md#before-you-start)). The App's private key is not in this Secret, and the broker never
reads it: it belongs to the agent's tools (step 4).

## 4. Run the agent and its tools

The recipe ships no agent. Run an A2A agent whose agent card advertises streaming, with tools that do the operations below as the
App; [The agent](../receive-webhooks.md#the-agent) says what it must be and do, and the quickest way to one. Note its A2A endpoint:
step 5 puts it in the AgentBackend, and the Binding turns `Ready` only once the broker can fetch the agent's card from it.

The recipe's prompts ask the agent to do these on GitHub, as the App:

| Operation | For | GitHub's REST API |
|---|---|---|
| Read a pull request: its title, description, head and base commits | Both triggers | `GET /repos/{owner}/{repo}/pulls/{number}` |
| List its changed files, with their patches, and read a file at a commit | The review | `GET /repos/{owner}/{repo}/pulls/{number}/files`, `GET /repos/{owner}/{repo}/contents/{path}?ref={sha}` |
| Post one review on the pull request | The review | `POST /repos/{owner}/{repo}/pulls/{number}/reviews`, with `event: COMMENT` |
| Post a comment on the pull request | The reply | `POST /repos/{owner}/{repo}/issues/{number}/comments` |
| Search a repository's code and read its files | The Jira and Linear pages' plans | The code search and contents APIs |

Each call takes an installation token, which the tools get from a JWT signed with the App's key, as in step 2:
`POST /app/installations/{installation_id}/access_tokens`. GitHub's documentation (read 2026-11-18) says an installation token
expires after one hour, so the tools must get a new one before then; a token got once at start stops working an hour later.

Keep the key with the tools, in the agent's namespace and out of the grant to the broker. As of 2026-11-17 github-mcp-server v2.2.0
renewed App tokens only in its stdio mode, and its HTTP mode took a bearer token with each request (that project's state on that
date), so the run used a small MCP server of its own.

## 5. Apply the manifests

The recipe [`webhook-github`](https://github.com/agentkourier/agent-kourier/tree/main/examples/recipes/webhook-github) holds three
files. The ChatConnection:

```yaml
--8<-- "webhook-github/kourier/chatconnection.yaml"
```

The Binding, with its two triggers:

```yaml
--8<-- "webhook-github/kourier/binding.yaml"
```

The AgentBackend:

```yaml
--8<-- "webhook-github/kourier/agentbackend.yaml"
```

Replace these values before you apply them:

| Where | The recipe's value | Yours |
|---|---|---|
| ChatConnection `spec.webhook.identity` | `acme-review-agent[bot]` | The identity from step 2: the account whose events are dropped as the agent's own. |
| Binding `spec.chat.channel` | `acme/widgets` | The repository, as `owner/repo`: its `repository.full_name`. |
| The second trigger's `comment` regex | `@acme-review-agent` | `@` and your App's slug. |
| Each trigger's `limits` | 10 reviews an hour, 40 a day; 20 replies an hour, 60 a day; one of each at a time | What you will pay for. |
| AgentBackend `spec.url` | `http://REPLACE-ME.agents.svc.cluster.local:8080` | Your agent's A2A endpoint (step 4). On the `a2a` dialect the URL alone selects the agent, and the Binding's `agent.name` and `agent.namespace` are labels. |
| Binding `spec.identity.userId` | `agent-kourier-github` | Keep it, or any name. It is not the GitHub account (that is `spec.webhook.identity`): it is the Binding's identity, which the broker sends to the agent as the header `X-User-Id` on each request, since the Binding has no token. An agent that checks a token takes `identity.tokenSecretRef` instead ([Create the Binding identity](../create-binding-identity.md)). |
| Every object's `metadata.namespace`, and the Secret's | `acme-webhooks` | The admitted namespace that holds the Secret, if you use another. |

Copy the recipe's `kourier/` directory, edit it, then apply it and wait for the Binding:

```sh
kubectl apply -f kourier/
kubectl -n acme-webhooks wait --for=condition=Ready akbinding/github-reviews --timeout=120s
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier logs "$LEADER" | grep '"webhook: taking requests"' | grep 'acme-webhooks/github'
```

`Ready` waits on the agent of step 4: it turns `True` once a fresh fetch of the agent's card succeeds and the card advertises
streaming. If the wait times out, `kubectl -n acme-webhooks get akbinding github-reviews` shows the condition that holds it back in
the `Reason` column, such as `BackendReachable:Unreachable` or `BackendReachable:StreamingUnsupported`.

`webhook: taking requests`, with `"connection":"acme-webhooks/github"`, says the connection runs on the leader: from then on its URL
checks the signature of every request. It does not wait for the agent. Check the URL from outside with a request that has no
signature:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H 'Content-Type: application/json' -d '{}' \
  https://<host>/chat/webhook/acme-webhooks/github
```

`401` is right: the connection refused the request as `unverified`, `unsigned`, and the leader logs a WARN line,
`webhook: requests were refused: they are not verified`, that counts it under `unsigned`. `503` is a connection that does not run on the leader
yet: its Secret, or the namespace's admission. Anything else is the way to the chat listener
([Expose the chat listener](../receive-webhooks.md#expose-the-chat-listener)).

## 6. Turn on the webhook

The webhook goes on the App, not on the repository: the App's webhook sends the events of every repository it is installed on. In
the App's settings:

| Field | Value |
|---|---|
| **Active** | Checked. |
| **Webhook URL** | `https://<host>/chat/webhook/acme-webhooks/github`: the namespace and the name of the ChatConnection. |
| **Webhook secret** | The secret of step 3. Copy it with `pbcopy < ~/.config/webhooks/github-webhook-secret` and paste it. |
| **SSL verification** | Enabled. |
| **Subscribe to events** | **Pull request**, **Issue comment**, **Pull request review** and **Pull request review comment**. GitHub shows these once **Active** is checked; in the App's settings they are under **Permissions & events** (GitHub's documentation, read 2026-11-18). |

An App's webhook body is JSON, and an App's webhook has no **Content type** setting; a repository's webhook has one, which must be
`application/json` ([Troubleshooting](#troubleshooting)). GitHub's documentation (read 2026-11-18) says the three pull request
events need **Pull requests** and **Issue comment** needs **Issues**, each at least Read-only. A comment in a pull request's
conversation is an issue comment.

Save. In the run, saving sent a `ping` at once, the free first check of the route. Under **Advanced**, the App's recent deliveries
(GitHub's documentation, read 2026-11-18) show it with the response `200`, and the leader logs:

```sh
kubectl -n agent-kourier logs "$LEADER" | grep '"webhook: delivery answered"' | grep 'acme-webhooks/github'
```

```text
{"time":"…","level":"INFO","msg":"webhook: delivery answered","connection":"acme-webhooks/github","outcome":"unmapped","reason":"no_channel","delivery":"<X-GitHub-Delivery>",…}
```

The line says the signature checked out: a `ping` has no repository, so nothing maps it, and it starts nothing. A `200` in GitHub's
log says only that the connection took the delivery, not that a trigger matched: the connection answers `200`, with an empty body,
to an event it accepted, to the agent's own (`self`) and to one it could not map (`unmapped`) alike. A `401` there is a secret that
does not match ([Troubleshooting](#troubleshooting)).

## 7. Test it

Open a pull request against the repository, not as a draft. Within seconds the broker logs these, from the leader:

```sh
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier logs "$LEADER" | grep 'acme-webhooks/github' | grep -E \
  '"msg":"(chat: webhook event matched a trigger|chat: trigger decided|webhook: delivery answered|session turn started|session turn ended)"'
```

```text
{"time":"…","level":"INFO","msg":"chat: webhook event matched a trigger","binding":"acme-webhooks/github-reviews","channel":"acme/widgets","message":"<SHA-256 of the body>","thread":"pr-7","delivery":"<X-GitHub-Delivery>","trigger":0}
{"time":"…","level":"INFO","msg":"chat: trigger decided","binding":"acme-webhooks/github-reviews","trigger":0,"decision":"started",…,"thread":"pr-7"}
{"time":"…","level":"INFO","msg":"webhook: delivery answered","connection":"acme-webhooks/github","outcome":"accepted","reason":"none","delivery":"<X-GitHub-Delivery>",…}
{"time":"…","level":"INFO","msg":"session turn started","binding":"acme-webhooks/github-reviews","connection":"acme-webhooks/github","channel":"acme/widgets","thread":"pr-7",…}
{"time":"…","level":"INFO","msg":"session turn ended",…,"thread":"pr-7",…,"outcome":"completed","duration_ms":…}
```

`trigger` is the trigger's index in the Binding: `0` is the review, `1` the reply. `"decision":"started"` is a turn that started.
`delivery` is the ID that GitHub's delivery log shows for the delivery. Then the App's review shows on the pull request. In the run
of 2026-11-17 a turn took 10 to 17 seconds.

Comment `@acme-review-agent why this approach?` on the pull request. The log shows the same lines with `"trigger":1` and the same
`"thread":"pr-7"`, and the App replies.

Read the counts on the leader's `/metrics`. Every series of the connection exists at 0, so leave those out:

```sh
kubectl -n agent-kourier port-forward "$LEADER" 8080:8080 &
curl -s localhost:8080/metrics | grep -E '^agentkourier_(webhook_requests_total|trigger_decisions_total)' \
  | grep 'acme-webhooks/github' | grep -v ' 0$'
```

```text
agentkourier_trigger_decisions_total{binding="acme-webhooks/github-reviews",decision="started"} 2
agentkourier_webhook_requests_total{connection="acme-webhooks/github",outcome="accepted",reason="none"} 4
agentkourier_webhook_requests_total{connection="acme-webhooks/github",outcome="self",reason="none"} 2
agentkourier_webhook_requests_total{connection="acme-webhooks/github",outcome="unmapped",reason="no_channel"} 1
```

These are the counts that one pull request opened and one comment gave in the run; yours depend on what else GitHub sends, so read
them as at least, not exactly:

- `decision="started"` is the turns that started: the review and the reply.
- `accepted` is every signed event with a channel and a thread that the broker took, whether a trigger matched it or not. In the run
  the pull request also sent `review_requested`, and a review was edited: four in all, two of which started a turn.
- `self` is the App's review and its comment: GitHub sends each back as an event whose sender is `acme-review-agent[bot]`, and the
  connection drops it. Its `webhook: delivery answered` line says `"outcome":"self"`.
- `unmapped`, `no_channel` is the `ping` of step 6, and any `installation` event, which GitHub sends when the App's permissions or
  repositories change: they have no repository.
- Yours also show `outcome="unverified",reason="unsigned"` for the unsigned check of step 5, which the run did not make.

## Change what starts a turn

Each change is to the recipe's ChatConnection fields or Binding triggers. A field that a trigger matches must be in the connection's
`fields` (at most 32), and a `text` field can be matched but not read by `promptTemplate`. A trigger has at most 6 `match` and 6
`exclude` entries. A path marked *not run* was not in the run's deliveries; check it in your first delivery. The ChatConnection is a
team connection, so the broker restarts it by itself when you apply the edit.

**Review once, not on every push.** Drop `synchronize` from the first trigger's `action`:

```yaml
match: {event: '^pull_request$', action: '^(opened|reopened|ready_for_review)$', prNumber: '.'}
```

**Review drafts too.** Remove the first trigger's `exclude: {draft: '^true$'}`.

**Only pull requests into one branch** (the run's `pull_request` events carried `/pull_request/base/ref`; the trigger was *not run*).
Add a field, and match it:

```yaml
- {name: baseRef, from: [/pull_request/base/ref], kind: id}      # in the ChatConnection's fields
match: {event: '^pull_request$', action: '^(opened|reopened|synchronize|ready_for_review)$', prNumber: '.', baseRef: '^main$'}
```

**Review when a label is added** (*not run*, and the path is not confirmed in GitHub's documentation, which, read 2026-11-18, says
only that a `labeled` action carries the label). The label's name would be `/label/name`:

```yaml
- {name: label, from: [/label/name], kind: id}
match: {event: '^pull_request$', action: '^labeled$', prNumber: '.', label: '^ai-review$'}
```

**Several repositories.** Install the App on each, and write one Binding for each repository, with its `owner/repo` as the channel,
all naming the same connection. One webhook serves them all; an event of a repository that no Binding names starts nothing.

## Troubleshooting

[When nothing happens](../receive-webhooks.md#when-nothing-happens) covers every source, by the status and the reason. For GitHub:

| What you see | Why, and what to do |
|---|---|
| A delivery failed, and nothing sent it again | GitHub's documentation (read 2026-11-18) says it does not send a failed delivery again. In the App's settings, **Advanced** lists the recent deliveries (those of the past three days; not verified), by the ID that the broker's `delivery` names; open one and choose **Redeliver**. The body is the same, so a delivery that had started a turn starts no second one while the broker remembers it, and one that matched no trigger is tried against the triggers again, and starts a turn if a trigger now matches it ([Start agents from webhooks](../receive-webhooks.md#what-a-taken-event-does)). |
| `unverified`, `bad_signature`, on every delivery | The App's **Webhook secret** is not the Secret's `webhookSecret`. Set it on the App again from the file of step 3. |
| `malformed`, `schema` | A repository webhook with the content type `application/x-www-form-urlencoded`, which GitHub's REST documentation (read 2026-11-18) gives as the default for one. Set it to `application/json`, or use the App's webhook. |
| `unmapped` only | `no_channel`: the App's events have no repository (`ping`, `installation`). `no_thread`: an event with neither `/pull_request/number` nor `/issue/number`. |
| `200` in GitHub's log, and no turn | `200` is not a match: see step 6. Read the delivery's `webhook: delivery answered` line by its `delivery`, then the rows below. |
| `accepted`, and no review | The pull request is a draft (the first trigger excludes it), or `agentkourier_trigger_limited_total{binding="acme-webhooks/github-reviews"}` rose: with `maxConcurrent: 1`, a second pull request opened while a review runs is stopped, the broker's response was `200`, and GitHub does not send it again. The WARN line names its delivery: redeliver it once the first review is done, or raise `maxConcurrent`. |
| A comment that mentions the App starts nothing | The regex wants `@` and the slug, not the App's name and not `[bot]`. A comment on a plain issue is turned away by `url: '/pull/'`. |
| A turn, and nothing on GitHub | The agent's tools: an installation token older than an hour, an App not installed on the repository, or a missing permission. Check the agent's logs. |

## What GitHub sends

- GitHub signs with `X-Hub-Signature-256: sha256=<hex>`, and names the event in the `X-GitHub-Event` header, read here as the field
  `event`. The header is not signed, so the recipe's triggers also match a field of the body: `action`, `prNumber` or `url`. (Seen
  in the 2026-11-17 run.)
- A comment on a pull request is an `issue_comment` whose `issue.pull_request` holds the pull request's URL; a comment on a plain
  issue has none, which is what the trigger's `url: '/pull/'` checks. Its number is `/issue/number`, so `number` reads either. (Seen
  in the run.)
- The App's webhook also sent `ping` and `installation` events when it was set up. They have no repository, so they count as
  `unmapped`, `no_channel`. (Seen in the run.)
- The App's own reviews and comments carry `acme-review-agent[bot]` in `/sender/login`, and count as `self`. (Seen in the run.)
- The delivery's ID is in `X-GitHub-Delivery`. (GitHub's documentation, read 2026-11-18; not part of the run.)

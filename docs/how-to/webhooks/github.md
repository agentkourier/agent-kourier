# GitHub

For a platform engineer who wants an agent to review pull requests on GitHub and answer the comments that ask it. At the end, a
repository's webhook sends its events to Agent Kourier, each pull request has a session of its own, and the recipe's agent posts on
GitHub as a machine user (a GitHub account kept for the agent), with that user's fine-grained personal access token.

!!! warning "Experimental"

    Needs Agent Kourier 0.6.0 or later: 0.5.x refuses `platform: webhook`. The `webhook` platform is new, and its fields may change
    in a later version. This mapping was run end to end against GitHub on 2026-11-17, on a pre-0.6.0 build of the broker, with a
    GitHub App's webhook and a small MCP server of the run's own, not with this page's machine user, repository webhook and agent.
    What this page says GitHub sends was seen in that run's deliveries, from the App, and GitHub can change it. GitHub's behaviour
    and labels that the run did not see come from GitHub's documentation, and say so where they appear. The steps on GitHub's side
    follow that documentation, which can change: if a menu or field has moved, GitHub's documentation is the reference. Not run: the
    machine user, its token and the recipe's agent against GitHub or a live model; a repository's webhook; the delivery header
    `X-GitHub-Delivery`; a comment on a line of the diff (`pull_request_review_comment`); and every change marked *not run* below.

## What you end with

- A pull request that is opened, reopened, pushed to or marked ready for review, and is not a draft, starts a review: a turn of the
  session `pr-<number>`, in which the agent reads the pull request at its head commit and posts one review, of the event `COMMENT`.
- A comment on that pull request that mentions the machine user (`@<login>`) starts a reply: a turn of the same session, so the agent
  can see the review it posted. A comment in the pull request's conversation was run (with the App); a comment on a line of its diff
  (`pull_request_review_comment`) was *not run*.
- The machine user's own reviews and comments are dropped as the agent's own (`self`), so they start nothing.

## What you need

- **Agent Kourier 0.6.0 or later**, set up as [Before you start](../receive-webhooks.md#before-you-start) says: the chat listener on
  HTTPS at a public host, which this page calls `<host>` ([Expose the chat listener](../receive-webhooks.md#expose-the-chat-listener)),
  and a broker that admits the namespace `acme-webhooks` and reads the Secrets `github-webhook` and `github-review-agent-bearer`. The
  commands take the broker's release to be in the namespace `agent-kourier`; where they say `-n agent-kourier`, put yours.
- **The recipe's agent**, which step 4 runs: the prompt-agent recipe's image with an `agent.yaml` of its own, and GitHub's
  github-mcp-server beside it, in the namespace `github-agent`. Agent Kourier posts nothing on GitHub: the Binding's `output: none`
  sends the agent's reply text nowhere, so only what its tools do reaches GitHub. To run an agent of your own instead, see step 4.
- **A machine user with the Read role on the repository**, not Write, and the right to create a fine-grained personal access token
  as that user (step 1).
- **A model provider and a key**: see the prompt-agent recipe's
  [Providers](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/prompt-agent/README.md#providers). The
  recipe's `agent.yaml` names `anthropic:claude-haiku-5-5`.
- Admin of the repository, to add its webhook.
- `kubectl`, `curl`, `jq` and `openssl`.

## 1. Create the machine user and its token

The agent works as a machine user: an account of its own, signed up like any other, so that what it posts on GitHub is that user's
and its rights are that user's. Sign it up, then give it the **Read** role on the repository.

!!! warning "Not Write: its approval would count"

    The model can be talked into an `APPROVE` by the text of the pull request it reviews: only `agent.yaml`'s instructions hold
    the review to `COMMENT`. GitHub's [repository roles](https://docs.github.com/en/organizations/managing-user-access-to-your-organizations-repositories/managing-repository-roles/repository-roles-for-an-organization)
    let every role, Read included, submit reviews on pull requests, and only Write, Maintain and Admin approve or
    request changes on a pull request with required reviews; its page on
    [protected branches](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches)
    says required approvals come from people with write permissions or from a code owner. So with Write, the machine user's
    `APPROVE` counts toward a branch protection rule's required approvals, and a pull request could get itself approved.

    Known and not: the roles table has no row for commenting on a pull request, and this repository has not checked that a
    fine-grained token's **Pull requests: Read and write** lets a Read user post a review or a comment through the API (REST or
    GraphQL). If the machine user must have Write, turn on **Require review from Code Owners** in the branch's rule, with a
    `CODEOWNERS` that owns every file and does not name the machine user: GitHub's
    [code owners page](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners)
    says a file with no owner can be approved by anyone with write access.

GitHub's documentation says a fine-grained token reaches only the repositories of its resource owner, and that an
outside collaborator can use only a classic token on an organization's repository. So the repository must be owned by an
organization that the machine user is a member of, or by the machine user itself: a collaborator on another person's repository
cannot use a fine-grained token on it.

Signed in as the machine user, open **Settings**, **Developer settings**, **Personal access tokens**, **Fine-grained tokens**,
**Generate new token** (from the profile picture; menu path from GitHub's documentation), and fill the form:

| Field | Value |
|---|---|
| **Token name** | A name, such as `acme-review-agent`. |
| **Resource owner** | The organization that owns the repository, or the machine user for its own. |
| **Expiration** | The shortest you will renew. GitHub's documentation gives 30 days as the default, and says an organization or enterprise owner can set a maximum lifetime. |
| **Repository access** | **Only select repositories**, with the repositories the agent reviews (and, for the [Jira](jira.md) page, the repository whose code the agent searches). |
| **Repository permissions** | **Pull requests**: Read and write (the pull request, its files, the review and the comments). **Contents**: Read-only (`get_file_contents`). **Metadata**: Read-only, which the form adds by itself (not verified here). |

GitHub's [permissions list](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens)
names those for the REST calls. github-mcp-server posts the review with GraphQL's `addPullRequestReview`, which
that list does not cover, and this repository has not checked the token against GitHub.

Generate the token. GitHub's documentation says that an organization that requires approval of fine-grained
tokens marks the token `pending` until an organization administrator reviews it, and that until then it reads only public
resources: an agent with a pending token fails on a private repository, and posts nothing on any.

GitHub shows the token once. Paste it into a file that only you can read, without echoing it
([why](../receive-webhooks.md#keep-the-secrets-out-of-sight)):

```sh
umask 077; mkdir -p ~/.config/webhooks
read -rs v; printf '%s' "$v" > ~/.config/webhooks/github-token; unset v   # paste the token and press Enter; nothing is echoed
```

## 2. Find the identity

The connection's `spec.webhook.identity` is the machine user's login: the `sender.login` of what it does on GitHub, with no
`[bot]`. Read it with the token, which also checks that the token works. The token is written to a header file, never to a command
line:

```sh
umask 077
printf 'Authorization: Bearer %s\n' "$(cat ~/.config/webhooks/github-token)" > ~/.config/webhooks/github-token.header
curl -s -H @"$HOME/.config/webhooks/github-token.header" -H 'Accept: application/vnd.github+json' \
  https://api.github.com/user | jq -r .login
rm ~/.config/webhooks/github-token.header
```

The response, such as `acme-review-agent`, is the identity. The mention that the recipe's reply trigger matches is the login with an
`@`: `@acme-review-agent`. `null` is a token that GitHub refused.

## 3. Make the Secret

Generate the webhook's secret into a file, with no newline at the end ([why](../receive-webhooks.md#keep-the-secrets-out-of-sight)),
and make the connection's Secret from it, with the key `webhookSecret`:

```sh
umask 077
openssl rand -hex 32 | tr -d '\n' > ~/.config/webhooks/github-webhook-secret
kubectl -n acme-webhooks create secret generic github-webhook \
  --from-file=webhookSecret="$HOME/.config/webhooks/github-webhook-secret"
```

The broker reads it only if the release grants it. Check that the release admits the namespace and grants the Secrets, since a
missing grant shows only in step 5, as a connection that does not start or an agent the broker cannot call:

```sh
helm -n agent-kourier get values agent-kourier -o json \
  | jq '{namespaces: .config.connectionNamespaces, secrets: .rbac.secretNames["acme-webhooks"]}'
```

`namespaces` must hold `acme-webhooks` (unless you admit it another way, such as by a label) and `secrets` must hold `github-webhook`
and `github-review-agent-bearer`, the agent's bearer token of step 4
([Before you start](../receive-webhooks.md#before-you-start)); the command takes the release to be called `agent-kourier`. The
machine user's token is not in these Secrets, and the broker never reads it: it belongs to the agent (step 4).

## 4. Run the agent and its tools

The recipe's agent is in
[`agent/`](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/webhook-github/agent/README.md): one pod in the
namespace `github-agent` that runs the prompt-agent recipe's image with the recipe's `agent.yaml` (`PROMPT_AGENT_ASK_USER=0`: the
model is not offered `ask_user`, since a webhook connection serves no questions), and GitHub's github-mcp-server v2.2.0 as a
sidecar, in its `http` mode, listening on `127.0.0.1:8082` alone. The agent sends the machine user's token as the `Authorization`
header of each request to the sidecar, and github-mcp-server calls GitHub's API with it; the sidecar holds no secret. Its Service,
`http://github-review-agent.github-agent.svc.cluster.local:8080`, is the one the recipe's AgentBackend names.

Follow the agent's [Apply it](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/webhook-github/agent/README.md#apply-it)
steps 1 to 4. They make:

- In `github-agent`, the Secret `github-review-agent-github` (key `token`), the machine user's token, from the file of step 1 here,
  `~/.config/webhooks/github-token`, and `github-review-agent-model` (key `api-key`), the model provider's key.
- The bearer token that Agent Kourier sends to the agent, one value in `~/.config/webhooks/github-review-agent-bearer` and in two
  Secrets named `github-review-agent-bearer` (key `token`) made from that file: one in `acme-webhooks`, which the Binding names and
  the broker reads, and one in `github-agent`, which the pod mounts and checks each request against. With the two different, the
  agent answers `401`, the turn fails and nothing is posted.
- The grant of that Secret to the broker, beside the webhook's, in the chart values:
  `rbac.secretNames: {acme-webhooks: [github-webhook, github-review-agent-bearer]}`.

The agent's NetworkPolicy admits only Agent Kourier's pods in the namespace `agent-kourier` (change it for yours), where the network
plugin enforces NetworkPolicy. With the chart's `networkPolicy.enabled`, the broker reaches the agent only through a rule of
`networkPolicy.extraEgress`.

The recipe's prompts ask the agent to do these on GitHub, as the machine user, with these four github-mcp-server tools, which the
`agent.yaml`'s `allowed_tools` and the sidecar's `--tools` both list:

| Operation | For | github-mcp-server's tool | GitHub's API |
|---|---|---|---|
| Read a pull request: its title, description, head and base commits | Both triggers | `pull_request_read`, method `get` | `GET /repos/{owner}/{repo}/pulls/{number}` |
| List its changed files, with their patches, and read a file at a commit | The review | `pull_request_read`, method `get_files` or `get_diff`; `get_file_contents` with `ref` | `GET /repos/{owner}/{repo}/pulls/{number}/files`, `GET /repos/{owner}/{repo}/contents/{path}?ref={sha}` |
| Post one review on the pull request | The review | `pull_request_review_write`, method `create`, event `COMMENT` | GraphQL's `addPullRequestReview`, with the event `COMMENT` |
| Read the comments, and post a comment on the pull request | The reply | `pull_request_read`, method `get_comments`; `add_issue_comment` | `GET /repos/{owner}/{repo}/issues/{number}/comments`, `POST /repos/{owner}/{repo}/issues/{number}/comments` |

The last column is what the repository's tests see the pinned github-mcp-server send to a fake GitHub for these calls (`get_diff`
aside, which they do not make), not what live GitHub was sent. Only the instructions in `agent.yaml`
hold the review to `COMMENT`: `pull_request_review_write` can also approve or request changes, and `allowed_tools` bounds the tools'
names, not their arguments. The agent README's
[What the agent can and cannot do](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/webhook-github/agent/README.md#what-the-agent-can-and-cannot-do)
says what that leaves open.

Why a machine user and not a GitHub App: github-mcp-server renews an App's installation tokens by itself only in its `stdio` mode,
and the prompt agent reaches MCP servers over HTTP only (github-mcp-server's `docs/github-app-auth.md` at v2.2.0;
not verified here).

**An agent of your own instead.** Run an A2A agent whose agent card advertises streaming, with tools that do the operations above;
[The agent](../receive-webhooks.md#the-agent) says what it must be and do. Put its A2A endpoint in the AgentBackend (step 5), and
its bearer token, or none, in the Binding's `identity` ([Create the Binding identity](../create-binding-identity.md)).

## 5. Apply the manifests

The recipe [`webhook-github`](https://github.com/agentkourier/agent-kourier/tree/main/examples/recipes/webhook-github) holds three
Agent Kourier files, in `kourier/`. The ChatConnection:

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
| ChatConnection `spec.webhook.identity` | `acme-review-agent` | The machine user's login, from step 2: the account whose events are dropped as the agent's own. |
| Binding `spec.chat.channel` | `acme/widgets` | The repository, as `owner/repo`: its `repository.full_name`. |
| The second trigger's `comment` regex | `@acme-review-agent` | `@` and the machine user's login. |
| Each trigger's `limits` | 10 reviews an hour, 40 a day; 20 replies an hour, 60 a day; one of each at a time | What you will pay for. |
| AgentBackend `spec.url` | `http://github-review-agent.github-agent.svc.cluster.local:8080` | Keep it for the recipe's agent of step 4. Change it only for another agent: its A2A endpoint. On the `a2a` dialect the URL alone selects the agent, and the Binding's `agent.name` and `agent.namespace` are labels. |
| Binding `spec.identity` | `userId: agent-kourier-github`, `tokenSecretRef: {name: github-review-agent-bearer}` | Keep both for the recipe's agent. `userId` is not the GitHub account (that is `spec.webhook.identity`): it is the Binding's identity. `tokenSecretRef` names the bearer Secret of step 4, whose value the broker sends to the agent as `Authorization: Bearer` ([Create the Binding identity](../create-binding-identity.md)). |
| Every object's `metadata.namespace`, and the Secrets' | `acme-webhooks` | The admitted namespace that holds the Secrets, if you use another. |

Copy the recipe's `kourier/` directory, edit it, then apply it and wait for the Binding:

```sh
kubectl apply -f kourier/
kubectl -n acme-webhooks wait --for=condition=Ready akbinding/github-reviews --timeout=2m
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier logs "$LEADER" | grep '"webhook: taking requests"' | grep 'acme-webhooks/github'
```

`Ready` waits on the connection and on the agent of step 4: it turns `True` once the connection runs (`ChannelJoined`) and a fresh
fetch of the agent's card succeeds, with a card that advertises streaming (`BackendReachable`). If the wait times out,
`kubectl -n acme-webhooks get akbinding github-reviews` shows the condition that holds it back in the `Reason` column, such as
`ChannelJoined:ChatCredentialsUnusable` (the Secret is missing, or a key of it), `ChannelJoined:ChatUnavailable` (the connection
does not run for another reason, such as a Secret the broker is not granted; the leader's log says why),
`BackendReachable:CredentialsUnreadable` (the bearer Secret in `acme-webhooks` is missing or not granted),
`BackendReachable:CredentialsRejected` (the two bearer copies differ), `BackendReachable:Unreachable` (the agent is not running, or
a NetworkPolicy stops the broker) or `BackendReachable:StreamingUnsupported`.

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

The webhook goes on the repository. In the repository's **Settings**, **Webhooks**, **Add webhook** (GitHub's documentation):

| Field | Value |
|---|---|
| **Payload URL** | `https://<host>/chat/webhook/acme-webhooks/github`: the namespace and the name of the ChatConnection. |
| **Content type** | `application/json`. The other choice, `application/x-www-form-urlencoded`, is refused as `malformed` ([Troubleshooting](#troubleshooting)). |
| **Secret** | The secret of step 3. Copy it with `pbcopy < ~/.config/webhooks/github-webhook-secret` and paste it. |
| **SSL verification** | Enabled (a field GitHub's page on creating webhooks does not name; not checked against the form). |
| **Which events would you like to trigger this webhook?** | **Let me select individual events**, then **Pull requests**, **Issue comments**, **Pull request reviews** and **Pull request review comments** (the events' labels are not checked against the form). A comment in a pull request's conversation is an issue comment. |
| **Active** | Checked. |

Choose **Add webhook**. GitHub's documentation says it then sends a `ping`. In the webhook's **Recent Deliveries**
it shows with the response `200`, and the leader logs:

```sh
kubectl -n agent-kourier logs "$LEADER" | grep '"webhook: delivery answered"' | grep 'acme-webhooks/github'
```

The line says the signature checked out, and the ping starts nothing; its `outcome` and `reason` depend on what the ping carries
(*not run* on a repository's webhook). GitHub's documentation lists only `hook`, `hook_id` and `zen` in a ping's
body: with no `repository`, the connection answers it `unmapped`, `no_channel`, as it did the App's ping in the run. A ping that
carries the repository and a `sender` gets past the channel and the actor and has no pull request or issue number, so it is
`unmapped`, `no_thread` (or `self`, if the machine user added the webhook):

```text
{"time":"…","level":"INFO","msg":"webhook: delivery answered","connection":"acme-webhooks/github","outcome":"unmapped","reason":"no_channel","delivery":"<X-GitHub-Delivery>",…}
```

A `200` in GitHub's log says only that the connection took the delivery, not that a trigger matched: the connection answers `200`,
with an empty body, to an event it accepted, to the agent's own (`self`) and to one it could not map (`unmapped`) alike. A `401`
there is a secret that does not match ([Troubleshooting](#troubleshooting)).

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
`delivery` is the ID that GitHub's delivery log shows for the delivery. Then the machine user's review shows on the pull request. In
the run, with the App and the run's own MCP server, a turn took 10 to 17 seconds.

Comment `@acme-review-agent why this approach?` on the pull request. The log shows the same lines with `"trigger":1` and the same
`"thread":"pr-7"`, and the machine user replies.

Read the counts on the leader's `/metrics`. Every series of the connection exists at 0, so leave those out:

```sh
kubectl -n agent-kourier port-forward "$LEADER" 8080:8080 & PF=$!; sleep 2
curl -s localhost:8080/metrics | grep -E '^agentkourier_(webhook_requests_total|trigger_decisions_total)' \
  | grep 'acme-webhooks/github' | grep -v ' 0$'
kill $PF
```

```text
agentkourier_trigger_decisions_total{binding="acme-webhooks/github-reviews",decision="started"} 2
agentkourier_webhook_requests_total{connection="acme-webhooks/github",outcome="accepted",reason="none"} 4
agentkourier_webhook_requests_total{connection="acme-webhooks/github",outcome="self",reason="none"} 2
agentkourier_webhook_requests_total{connection="acme-webhooks/github",outcome="unmapped",reason="no_channel"} 1
```

These are the counts that one pull request opened and one comment gave in the run, with the App's webhook; yours depend on what
else GitHub sends, so read them as at least, not exactly:

- `decision="started"` is the turns that started: the review and the reply.
- `accepted` is every signed event with a channel and a thread that the broker took, whether a trigger matched it or not. In the run
  the pull request also sent `review_requested`, and a review was edited: four in all, two of which started a turn.
- `self` is the agent's review and its comment: GitHub sends each back as an event whose sender is the agent's account (in the run,
  `acme-review-agent[bot]`; here, the machine user's login), and the connection drops it. Its `webhook: delivery answered` line says
  `"outcome":"self"`.
- `unmapped`, `no_channel` was the App's `ping`: it had no repository. A repository's webhook counts its ping as step 6 says.
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

**Review when a label is added** (*not run*, and the path is not confirmed in GitHub's documentation, which says
only that a `labeled` action carries the label). The label's name would be `/label/name`:

```yaml
- {name: label, from: [/label/name], kind: id}
match: {event: '^pull_request$', action: '^labeled$', prNumber: '.', label: '^ai-review$'}
```

**Several repositories** (*not run*). Write one Binding for each repository, with its `owner/repo` as the channel, all naming the same
connection, and send each repository's events to the connection's URL: a webhook on each repository, as in step 6, or one webhook of
the organization, which GitHub's documentation says subscribes to the organization's events and only an
organization owner can create. The machine user's token must reach each repository: add each to its **Repository access** (all of
one resource owner, since a fine-grained token has one). An event of a repository that no Binding names starts nothing.

## Troubleshooting

[When nothing happens](../receive-webhooks.md#when-nothing-happens) covers every source, by the status and the reason. For GitHub:

| What you see | Why, and what to do |
|---|---|
| A delivery failed, and nothing sent it again | GitHub's documentation says it does not send a failed delivery again, and that deliveries of the past three days can be sent again: in the repository's **Settings**, **Webhooks**, choose the webhook, then **Recent Deliveries**, the delivery by the ID that the broker's `delivery` names, and **Redeliver**. The body is the same, so a delivery whose start was decided (a turn started, or the start was refused) is a duplicate while the broker remembers it, and one that matched no trigger is tried against the triggers again, and starts a turn if a trigger now matches it ([Start agents from webhooks](../receive-webhooks.md#what-a-taken-event-does)). |
| `unverified`, `bad_signature`, on every delivery | The webhook's **Secret** is not the Secret's `webhookSecret`. Set it on the webhook again from the file of step 3. |
| `malformed`, `schema` | The webhook's **Content type** is `application/x-www-form-urlencoded`, which GitHub's REST documentation gives as the default for a repository's webhook. Set it to `application/json`. |
| `unmapped` only | `no_channel`: events with no repository (the App's `ping` and `installation` in the run). `no_thread`: an event with neither `/pull_request/number` nor `/issue/number`, such as a ping that carries the repository (step 6). |
| `200` in GitHub's log, and no turn | `200` is not a match: see step 6. Read the delivery's `webhook: delivery answered` line by its `delivery`, then the rows below. |
| `accepted`, and no review | The pull request is a draft (the first trigger excludes it), or `agentkourier_trigger_limited_total{binding="acme-webhooks/github-reviews"}` rose: with `maxConcurrent: 1`, a second pull request opened while a review runs is stopped, the broker's response was `200`, and GitHub does not send it again. The WARN line names its delivery: redeliver it once the first review is done, or raise `maxConcurrent`. |
| A comment that mentions the machine user starts nothing | The regex wants `@` and the login, compared as written but for case. A comment on a plain issue is turned away by `url: '/pull/'`. A comment the machine user posted is the agent's own (`self`). |
| A turn, and nothing on GitHub | The machine user's token: expired, `pending` an organization's approval, not given the repository, or missing a permission of step 1; or the machine user's Read role, if GitHub refuses a Read user's review or comment through the API (not checked, step 1). The agent's log says which tool call failed: `kubectl -n github-agent logs deploy/github-review-agent -c github-review-agent`; the sidecar's: `kubectl -n github-agent logs deploy/github-review-agent -c github-mcp-server`. A turn that starts before the sidecar listens fails too, and posts nothing. |

## What GitHub sends

- GitHub signs with `X-Hub-Signature-256: sha256=<hex>`, and names the event in the `X-GitHub-Event` header, read here as the field
  `event`. The header is not signed, so the recipe's triggers also match a field of the body: `action`, `prNumber` or `url`. (Seen
  in the run, from the App's webhook.)
- A comment on a pull request is an `issue_comment` whose `issue.pull_request` holds the pull request's URL; a comment on a plain
  issue has none, which is what the trigger's `url: '/pull/'` checks. Its number is `/issue/number`, so `number` reads either. (Seen
  in the run.)
- The App's webhook also sent `ping` and `installation` events when it was set up. They had no repository, so they counted as
  `unmapped`, `no_channel`. (Seen in the run.) A repository's webhook sends a `ping` when it is added (GitHub's documentation);
  what it counts as is in step 6.
- The App's own reviews and comments carried `acme-review-agent[bot]` in `/sender/login`, and counted as `self`. (Seen in the run.)
  The machine user's carry its login there (*not run*).
- That a repository's webhook sends the same headers and body paths as the App's follows GitHub's documentation, and was not run.
- The delivery's ID is in `X-GitHub-Delivery`. (GitHub's documentation; not part of the run.)

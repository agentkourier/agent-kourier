# Jira Cloud

For a platform engineer who wants an agent to answer new Jira issues with a plan, and to revise it when the issue changes. At the
end, a Jira webhook sends one project's issue events to Agent Kourier, each issue has a session of its own, and the agent comments
on the issue as an Atlassian service account.

!!! warning "Experimental"

    Needs Agent Kourier 0.6.0 or later: 0.5.x refuses `platform: webhook`. The `webhook` platform is new, and its fields may change
    in a later version. This setup was run end to end against Jira Cloud on 2026-11-17, with this mapping, on a pre-0.6.0 build of
    the broker. What this page says Jira sends was seen in that run's deliveries, and Atlassian can change it. Jira's behaviour and
    labels that the run did not see come from Atlassian's documentation, read on 2026-11-18, and say so where they appear. Not run:
    the delivery header `X-Atlassian-Webhook-Identifier`, a person's comment (the run saw only the agent's own), **Exclude body**
    checked, and the token as a bearer token.

## What you end with

- An issue created in the project starts a plan: a turn of the session named by the issue's key, such as `OPS-1`, in which the agent
  reads the issue and, when it asks to change a codename, finds where the codename appears in a GitHub repository and adds one comment
  with the plan.
- An edit of the issue's summary or description starts a revised plan in the same session, so the agent can see its first plan.
- The service account's own comments are dropped as the agent's own (`self`), so they start nothing.

## What you need

- **Agent Kourier 0.6.0 or later**, set up as [Before you start](../receive-webhooks.md#before-you-start) says: the chat listener on
  HTTPS at a public host, which this page calls `<host>` ([Expose the chat listener](../receive-webhooks.md#expose-the-chat-listener)),
  and a broker that admits the namespace `acme-webhooks` and reads the Secret `jira-webhook`. The commands take the broker's release
  to be in the namespace `agent-kourier`; where they say `-n agent-kourier`, put yours.
- **An agent of your own.** The recipe ships no agent: you run an A2A agent with a streaming agent card and tools that act on Jira
  with the service account's own token (step 4; [The agent](../receive-webhooks.md#the-agent)). Agent Kourier posts nothing on Jira:
  the Binding's `output: none` sends the agent's reply text nowhere, so only what its tools do reaches Jira.
- The Atlassian organization admin role, to create the service account, and the Jira **Administer Jira** global permission, to create
  the webhook.
- For the recipe's plans, a GitHub token with read access to the repository the agent searches, such as the [GitHub](github.md)
  page's machine user's, given that repository, and GitHub tools of the agent's that search code and read files.
- `kubectl`, `curl`, `jq` and `openssl`.

## 1. Create the service account

The agent works as an Atlassian service account: an account of the organization that no person signs in to. Atlassian's
documentation (read 2026-11-18) says each organization can create five service accounts for free; more need Atlassian Guard.

1. In Atlassian Administration (`admin.atlassian.com`), open **Directory**, **Service accounts**, **Create a service account** (menu
   path from Atlassian's documentation, read 2026-11-18). Give it a name (Atlassian's documentation says 6 to 30 letters and digits),
   and, under the app roles, a role on Jira. In the run the account had the Jira role **Guest**, which took no paid seat and is held
   to one space; a full user role takes a seat.
2. Add the account to the Jira project with a role that can browse the project and add comments, as you would add a person. In the
   run it was added to the project as a guest collaborator, which gave it Browse projects, Add comments, Create issues and Edit
   issues there. Step 2 checks what it has.
3. Create its API token: select the account, then **Create credentials**, **API token**. Give the token a name and an expiry
   (1 to 365 days), and the scopes `read:jira-work` (read issues), `write:jira-work` (add comments) and `read:jira-user` (step 2 reads
   the account's own ID with it); the scopes and what each covers are from Atlassian's documentation, read 2026-11-18, which also says
   they cannot be changed afterwards. Choose **Create**, then **Copy to clipboard**, and save it where only you can read it, with the
   account's email address (Atlassian gives it one at `serviceaccount.atlassian.com`, shown on the account):

    ```sh
    umask 077; mkdir -p ~/.config/webhooks
    read -rs v; printf '%s' "$v" > ~/.config/webhooks/jira-agent-token; unset v     # paste the token, then Return
    read -r v; printf '%s' "$v" > ~/.config/webhooks/jira-agent-email; unset v      # type the account's email address
    ```

The token works only through Atlassian's API gateway, `https://api.atlassian.com/ex/jira/<cloudId>/`. In the run the site's own URL,
`https://<site>.atlassian.net/rest/...`, refused it with 401, and Atlassian's documentation says a service account's token must always
use the gateway.

## 2. Find the identity

The connection's `spec.webhook.identity` is the service account's `accountId`, such as `712020:…`. Find your site's cloud ID (the
`_edge/tenant_info` address is from Atlassian's documentation, read 2026-11-18), then ask Jira who the token is. The email and the
token go into a curl config file, so that neither is on a command line:

```sh
umask 077
SITE=acme.atlassian.net                                         # your site
CLOUD_ID=$(curl -s "https://$SITE/_edge/tenant_info" | jq -r .cloudId)
printf 'user = "%s:%s"\n' "$(cat ~/.config/webhooks/jira-agent-email)" "$(cat ~/.config/webhooks/jira-agent-token)" \
  > ~/.config/webhooks/jira-agent.curlrc
JIRA="https://api.atlassian.com/ex/jira/$CLOUD_ID/rest/api/3"

curl -s -K ~/.config/webhooks/jira-agent.curlrc "$JIRA/myself" | jq '{accountId, accountType}'
```

In the run the response's `accountType` was `app`. Check that the account can work in the project:

```sh
PROJECT=OPS                                                     # your project's key
curl -s -K ~/.config/webhooks/jira-agent.curlrc \
  "$JIRA/mypermissions?projectKey=$PROJECT&permissions=BROWSE_PROJECTS,ADD_COMMENTS" \
  | jq '.permissions | map_values(.havePermission)'
```

Both must be `true` (they were in the run). Atlassian's documentation also takes the token as `Authorization: Bearer <token>` on the
gateway; the run used the email and the token, as above.

## 3. Make the Secret

Generate the webhook's secret into a file, with no newline at the end ([why](../receive-webhooks.md#keep-the-secrets-out-of-sight)),
and make the connection's Secret from it, with the key `webhookSecret`:

```sh
umask 077
openssl rand -hex 32 | tr -d '\n' > ~/.config/webhooks/jira-webhook-secret
kubectl -n acme-webhooks create secret generic jira-webhook \
  --from-file=webhookSecret="$HOME/.config/webhooks/jira-webhook-secret"
```

The broker reads it only if the release grants it. Check that the release admits the namespace and grants the Secret, since a
missing grant shows only in step 5, as a connection that does not start:

```sh
helm -n agent-kourier get values agent-kourier -o json \
  | jq '{namespaces: .config.connectionNamespaces, secrets: .rbac.secretNames["acme-webhooks"]}'
```

`namespaces` must hold `acme-webhooks` (unless you admit it another way, such as by a label) and `secrets` must hold `jira-webhook`
([Before you start](../receive-webhooks.md#before-you-start)); the command takes the release to be called `agent-kourier`. The
service account's token is not in this Secret, and the broker never reads it: it belongs to the agent's tools (step 4).

## 4. Run the agent and its tools

The recipe ships no agent. Run an A2A agent whose agent card advertises streaming, with tools that do the operations below as the
service account; [The agent](../receive-webhooks.md#the-agent) says what it must be and do, and the quickest way to one. Note its A2A
endpoint: step 5 puts it in the AgentBackend, and the Binding turns `Ready` only once the broker can fetch the agent's card from it.

The recipe's prompts ask the agent to do these, each through the gateway URL of step 2 with the service account's token:

| Operation | For | Jira's REST API |
|---|---|---|
| Read an issue: its summary, description and comments | Both triggers | `GET /rest/api/3/issue/{key}` |
| Add a comment on the issue | Both triggers | `POST /rest/api/3/issue/{key}/comment`, whose body is in Atlassian Document Format; version 2 of the API, `/rest/api/2/issue/{key}/comment`, takes a plain string |
| Search a GitHub repository's code and read its files | Both triggers' plans | GitHub tools with a token that reads the repository, such as github-mcp-server, which the [GitHub](github.md#4-run-the-agent-and-its-tools) page runs, with its `search_code` and `get_file_contents` tools (its README at v2.2.0, read 2026-11-19); the GitHub page's agent registers `get_file_contents` and not `search_code` |

The token expires on the date set in step 1; the tools stop working then, and Atlassian shows no warning to the agent. Put the date
in your calendar, or give the token the shortest expiry you will renew.

As of 2026-11-17 mcp-atlassian v0.24.6 reached the gateway URL only through the workaround in its issue #968 (that project's state on
that date), so the run used a small MCP server of its own.

## 5. Apply the manifests

The recipe [`webhook-jira`](https://github.com/agentkourier/agent-kourier/tree/main/examples/recipes/webhook-jira) holds three
files. The ChatConnection:

```yaml
--8<-- "webhook-jira/kourier/chatconnection.yaml"
```

The Binding, with its two triggers:

```yaml
--8<-- "webhook-jira/kourier/binding.yaml"
```

The AgentBackend:

```yaml
--8<-- "webhook-jira/kourier/agentbackend.yaml"
```

Replace these values before you apply them:

| Where | The recipe's value | Yours |
|---|---|---|
| ChatConnection `spec.webhook.identity` | `712020:00000000-0000-0000-0000-000000000000` | The `accountId` from step 2: the account whose events are dropped as the agent's own. |
| Binding `spec.chat.channel` | `OPS` | The project's key. |
| The first trigger's `promptTemplate` | `acme/widgets` | The repository the agent searches, or a prompt of your own. |
| Each trigger's `limits` | 10 plans an hour, 30 a day, one at a time, for each trigger | What you will pay for. |
| AgentBackend `spec.url` | `http://REPLACE-ME.agents.svc.cluster.local:8080` | Your agent's A2A endpoint (step 4). On the `a2a` dialect the URL alone selects the agent, and the Binding's `agent.name` and `agent.namespace` are labels. |
| Binding `spec.identity.userId` | `agent-kourier-jira` | Keep it, or any name. It is not the Jira account (that is `spec.webhook.identity`): it is the Binding's identity, which the broker sends to the agent as the header `X-User-Id` on each request, since the Binding has no token. An agent that checks a token takes `identity.tokenSecretRef` instead ([Create the Binding identity](../create-binding-identity.md)). |
| Every object's `metadata.namespace`, and the Secret's | `acme-webhooks` | The admitted namespace that holds the Secret, if you use another. |

Copy the recipe's `kourier/` directory, edit it, then apply it and wait for the Binding:

```sh
kubectl apply -f kourier/
kubectl -n acme-webhooks wait --for=condition=Ready akbinding/jira-plans --timeout=2m
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier logs "$LEADER" | grep '"webhook: taking requests"' | grep 'acme-webhooks/jira'
```

`Ready` waits on the connection and on the agent of step 4: it turns `True` once the connection runs (`ChannelJoined`) and a fresh
fetch of the agent's card succeeds, with a card that advertises streaming (`BackendReachable`). If the wait times out,
`kubectl -n acme-webhooks get akbinding jira-plans` shows the condition that holds it back in the `Reason` column, such as
`ChannelJoined:ChatCredentialsUnusable` (the Secret is missing, or a key of it), `ChannelJoined:ChatUnavailable` (the connection
does not run for another reason, such as a Secret the broker is not granted; the leader's log says why),
`BackendReachable:Unreachable` or `BackendReachable:StreamingUnsupported`.

`webhook: taking requests`, with `"connection":"acme-webhooks/jira"`, says the connection runs on the leader: from then on its URL
checks the signature of every request. It does not wait for the agent. Check the URL from outside with a request that has no
signature:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H 'Content-Type: application/json' -d '{}' \
  https://<host>/chat/webhook/acme-webhooks/jira
```

`401` is right: the connection refused the request as `unverified`, `unsigned`, and the leader logs a WARN line,
`webhook: requests were refused: they are not verified`, that counts it under `unsigned`. `503` is a connection that does not run on
the leader yet: its Secret, or the namespace's admission. Anything else is the way to the chat listener
([Expose the chat listener](../receive-webhooks.md#expose-the-chat-listener)).

## 6. Turn on the webhook

In Jira, open **Settings**, **System**, then, under **Advanced**, **WebHooks**, and create a webhook (menu path from Atlassian's
documentation, read 2026-11-18). Atlassian's documentation does not name every field of the form, so the fields below other than
**Exclude body** are named by what they hold:

| Field | Value |
|---|---|
| Name | Any, such as `agent-kourier`. |
| Status | Enabled. |
| URL | `https://<host>/chat/webhook/acme-webhooks/jira`: the namespace and the name of the ChatConnection. |
| Secret | The secret of step 3. Copy it with `pbcopy < ~/.config/webhooks/jira-webhook-secret` and paste it, rather than **Generate secret**. Atlassian's documentation says Jira does not show it again after you save. |
| Issue related events: the JQL filter | `project = OPS`, with your project's key. |
| Issue | **created** and **updated**. |
| Comment | **created**, so that the agent's own comments arrive and count as `self`, and for a trigger on comments ([below](#change-what-starts-a-turn)). The recipe has no trigger on comments, so a person's comment is `accepted` and starts nothing. |
| **Exclude body** | Unchecked. Checked, Jira leaves the body out (Atlassian's documentation), and the connection has nothing to read. Whether Jira then signs the empty body is not known (not tried): if it does, every delivery is refused with `400`, `malformed`, `schema`; if it does not, with `401`, `unverified`, `unsigned`. |

Create it. No test delivery from Jira is known to this page (not verified), so nothing arrives until an issue event does. To check
the route and the secret before that, send a signed test request yourself
([Test the route](../receive-webhooks.md#test-the-route)), or make step 7's issue the first delivery. A `200` to a delivery says
only that the connection took it, not that a trigger matched: the connection answers `200`, with an empty body, to an event it
accepted, to the agent's own (`self`) and to one it could not map (`unmapped`) alike. Each has its `webhook: delivery answered` line
on the leader, with its `outcome`.

## 7. Test it

Create an issue in the project, such as "Rename the codename FALCON to OSPREY", with a description. Within seconds the broker logs
these, from the leader:

```sh
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier logs "$LEADER" | grep 'acme-webhooks/jira' | grep -E \
  '"msg":"(chat: webhook event matched a trigger|chat: trigger decided|webhook: delivery answered|session turn started|session turn ended)"'
```

```text
{"time":"…","level":"INFO","msg":"chat: webhook event matched a trigger","binding":"acme-webhooks/jira-plans","channel":"OPS","message":"<SHA-256 of the body>","thread":"OPS-1","delivery":"<X-Atlassian-Webhook-Identifier>","trigger":0}
{"time":"…","level":"INFO","msg":"chat: trigger decided","binding":"acme-webhooks/jira-plans","trigger":0,"decision":"started",…,"thread":"OPS-1"}
{"time":"…","level":"INFO","msg":"webhook: delivery answered","connection":"acme-webhooks/jira","outcome":"accepted","reason":"none","delivery":"<X-Atlassian-Webhook-Identifier>",…}
{"time":"…","level":"INFO","msg":"session turn started","binding":"acme-webhooks/jira-plans","connection":"acme-webhooks/jira","channel":"OPS","thread":"OPS-1",…}
{"time":"…","level":"INFO","msg":"session turn ended",…,"thread":"OPS-1",…,"outcome":"completed","duration_ms":…}
```

`trigger` is the trigger's index in the Binding: `0` is the plan, `1` the revised plan. `"decision":"started"` is a turn that
started. The agent's comment shows on the issue. Then edit the description, for example "also rename it in the README". The log
shows the same lines with `"trigger":1` and the same `"thread":"OPS-1"`, and the agent comments a revised plan.

Read the counts on the leader's `/metrics`. Every series of the connection exists at 0, so leave those out:

```sh
kubectl -n agent-kourier port-forward "$LEADER" 8080:8080 & PF=$!; sleep 2
curl -s localhost:8080/metrics | grep -E '^agentkourier_(webhook_requests_total|trigger_decisions_total)' \
  | grep 'acme-webhooks/jira' | grep -v ' 0$'
kill $PF
```

```text
agentkourier_trigger_decisions_total{binding="acme-webhooks/jira-plans",decision="started"} 2
agentkourier_webhook_requests_total{connection="acme-webhooks/jira",outcome="accepted",reason="none"} 2
agentkourier_webhook_requests_total{connection="acme-webhooks/jira",outcome="self",reason="none"} 2
```

These are the counts for the issue created and the edit, as the run had them; yours depend on what else Jira sends, so read them as
at least, not exactly:

- `decision="started"` is the turns that started: the plan and the revised plan.
- `accepted` is every signed event of the project with a channel and a thread that the broker took, whether a trigger matched it or
  not.
- `self` is the agent's two comments: their author in `/comment/author/accountId` is the service account.
- Yours also show `outcome="unverified",reason="unsigned"` for the unsigned check of step 5, which the run did not make.

## Change what starts a turn

Each change is to the recipe's ChatConnection fields or Binding triggers. A field that a trigger matches must be in the connection's
`fields` (at most 32), and a `text` field can be matched but not read by `promptTemplate`. A trigger has at most 6 `match` and 6
`exclude` entries. The ChatConnection is a team connection, so the broker restarts it by itself when you apply the edit.

**Answer a command in a comment.** Add a trigger on comments that start with `/plan`. The recipe already reads `/comment/body` as the
field `comment`; the run saw only the agent's own comments, so check a person's comment body in your first delivery:

```yaml
- type: webhookEvent
  match: {event: '^comment_created$', comment: '(?im)^/plan\b'}
  promptTemplate: |
    A comment on Jira issue {{ .Fields.issue }} of project {{ .Channel }} asks for a plan; the comment is in the event data.
    Read the issue and its comments with your Jira tools, then add one comment on the issue with your answer.
  limits: {rateLimit: {maxRuns: 20, per: 1h, maxConcurrent: 1}, maxRunsPerDay: 60}
```

**Only some issue types.** The recipe reads the type's name as the `text` field `type`. Add it to the first trigger's `match`:

```yaml
match: {event: '^jira:issue_created$', type: '^(Task|Story)$'}
```

**Several projects.** Widen the webhook's JQL, such as `project in (OPS, WEB)`, and write one Binding for each project key, all
naming the same connection; an event of a project that no Binding names starts nothing. Add the service account to each project as
in step 1. In the run the Guest role was held to one space, so a second project takes a role beyond Guest, which takes a paid seat.

**React to another field's edit.** Add its name to the second trigger's `changed`, such as `'^(summary|description|priority)$'`. Only
the first changed field is read: see [Troubleshooting](#troubleshooting).

## Troubleshooting

[When nothing happens](../receive-webhooks.md#when-nothing-happens) covers every source, by the status and the reason. For Jira:

| What you see | Why, and what to do |
|---|---|
| Nothing at all after creating the webhook | No test delivery from Jira is known to this page (not verified). Create or edit an issue that the JQL matches, or send a signed test request ([Test the route](../receive-webhooks.md#test-the-route)). |
| `unverified`, `bad_signature`, on every delivery | The webhook's secret is not the Secret's `webhookSecret`. Atlassian's documentation says Jira does not show a saved secret: set the one from the file of step 3 on the webhook again. |
| `malformed`, `schema`, or `unverified`, `unsigned`, with the right secret | **Exclude body** may be checked. |
| An edit starts nothing, and the field's name holds a space | `changed` is an `id` field, and a field name such as `Fix Version` is not an identifier, so the field is marked invalid and the second trigger, which matches it, misses. The event is still taken: a trigger that does not name `changed` can take it. A new issue carries a changelog too, which the first trigger does not read. |
| An edit of the description starts nothing | `/changelog/items/0/field` is only the first field that the edit changed. An edit of several fields at once can put `description` second, and the trigger does not match it. Edit the description alone. |
| `accepted`, and no plan | `agentkourier_trigger_limited_total{binding="acme-webhooks/jira-plans"}` rose: with `maxConcurrent: 1`, an issue created while another plan runs is stopped. The broker's response was `200`, so Jira does not send it again, and the event is lost; the WARN line names its delivery. Raise `maxConcurrent`. |
| A delivery failed | Atlassian's documentation (read 2026-11-18) says Jira tries a failed delivery up to five more times, 5 to 15 minutes apart, on a `5xx` response such as the broker's `503`. It shows no delivery log; its REST API lists failed webhooks. |
| A turn, and nothing on the issue | The agent's tools: a token called on the site's URL instead of the gateway, an expired token, or a role without Add comments. Run the checks of step 2 again. |

## What Jira sends

- Jira signs with `X-Hub-Signature: sha256=<hex>`. (Seen in the 2026-11-17 run.)
- The event name is `/webhookEvent` in the body: `jira:issue_created` and `jira:issue_updated`, but `comment_created` with no
  `jira:` prefix. (Seen in the run.)
- An issue event names its actor in `/user/accountId`. A comment event has no `/user`, so `/comment/author/accountId` is what drops
  the agent's own comments. A comment event carries the project in `/issue/fields/project/key` too. (Seen in the run.)
- `/issue/fields/description` is plain text. (Seen in the run.)
- Jira adds `?triggeredByUser=<accountId>` to the URL. The broker routes by the path and does not read the query. (Seen in the run.)
- `/changelog/items/0/field` is the first field that an edit changed. An issue created carries a changelog as well, with every field
  set: in the run `Status`, `priority`, `summary`, `reporter` and `description`, in that order. (Seen in the run.)
- The delivery's ID is in `X-Atlassian-Webhook-Identifier`. (Atlassian's documentation, read 2026-11-18; not part of the run.)

# Linear

For a platform engineer who wants an agent to answer new Linear issues with a plan, revise it when the description changes, and
answer the comments that ask it. At the end, a Linear OAuth application sends its webhooks to Agent Kourier, each issue has a
session of its own, and the agent comments on Linear as the application's own user.

!!! warning "Experimental"

    The `webhook` platform is new, and its fields may change in a later version. This setup was run end to end against Linear on
    2026-11-17, with this mapping on an earlier, unreleased version of the broker. What this page says Linear sends was seen in
    those deliveries, and Linear can change it. The delivery header, `Linear-Delivery`, was not part of that run. Linear's
    behaviour that the run did not see comes from its documentation, read on 2026-11-18.

## What you end with

- An issue created in the team starts a plan: a turn of the session named by the issue's identifier, such as `ENG-12`, in which the
  agent reads the issue and adds one comment with a plan.
- A change of the issue's description starts a revised plan in the same session, so the agent can see its first plan.
- A comment on the issue that names the application (`acme-agent`) starts a reply in the same session.
- The application's own comments, and the updates its comments cause, are dropped as the agent's own (`self`), so they start
  nothing.

## What you need

- What [Start agents from webhooks](../receive-webhooks.md#before-you-start) lists for every source: the chat listener on HTTPS, a
  broker that admits the namespace `acme-webhooks` and reads the Secret `linear-webhook`, and an A2A agent.
- Admin rights in the Linear workspace: Linear's documentation says the API settings page needs them.
- A broker clock within seconds of the right time: the connection refuses a delivery whose time is more than a minute from it
  (`unverified`, `stale`).
- `kubectl`, `curl` and `jq`.

## 1. Create the OAuth application

In the run the agent worked as a Linear OAuth application with client credentials: a token that the application gets for itself,
with no person signing in, and that acts as the application's own user. The recipe's comments name a personal API key instead, whose
user is then the identity, with a webhook of the workspace; that setup was *not run*, and this page follows the run.

In Linear, open **Settings**, **Administration**, **API**, and create an OAuth application. Linear's documentation names the form's
fields by what they hold, not by their labels:

| Field | Value |
|---|---|
| The application's name | The name the agent is mentioned by, such as `acme-agent`. |
| The developer's name, and a homepage URL | Yours. |
| Callback URLs | Linear asks for at least one; the agent never uses it, since it signs in with client credentials. Any HTTPS URL of yours. |
| Client credentials | On. |
| Webhooks | Off for now. Step 6 turns them on, once the broker can take them. |

Create it, and save the client ID and the client secret where only you can read them:

```sh
umask 077; mkdir -p ~/.config/webhooks
read -r v; printf '%s' "$v" > ~/.config/webhooks/linear-client-id; unset v         # paste the client ID, then Return
read -rs v; printf '%s' "$v" > ~/.config/webhooks/linear-client-secret; unset v    # paste the client secret, then Return
printf 'data-urlencode = "client_id=%s"\ndata-urlencode = "client_secret=%s"\n' \
  "$(cat ~/.config/webhooks/linear-client-id)" "$(cat ~/.config/webhooks/linear-client-secret)" > ~/.config/webhooks/linear-client.curlrc
```

The last line writes the two as form fields into a curl config file, which step 2 and the tools' token request read; Linear's
documentation takes them in the request's body, or by HTTP Basic.

## 2. Find the identity

The connection's `spec.webhook.identity` is the application user's ID. Get a token with the client credentials, then ask Linear's
GraphQL API who it is; the credentials and the token are read from files, and the token is checked before it is written:

```sh
umask 077
resp=$(curl -s -K ~/.config/webhooks/linear-client.curlrc -d grant_type=client_credentials -d scope=read \
  https://api.linear.app/oauth/token)
tok=$(printf '%s' "$resp" | jq -r '.access_token // empty')
if [ -n "$tok" ]; then
  printf 'Authorization: Bearer %s\n' "$tok" > ~/.config/webhooks/linear.header
  curl -s -H @"$HOME/.config/webhooks/linear.header" -H 'Content-Type: application/json' \
    -d '{"query":"{ viewer { id name } }"}' https://api.linear.app/graphql | jq .data.viewer
  rm ~/.config/webhooks/linear.header
else
  printf '%s' "$resp" | jq '{error, error_description}'       # no token: Linear's reason
fi
unset resp tok
```

The `id` is the identity. The run's own token request was not recorded, so this command is as Linear's documentation describes it,
not as the run sent it. In the run the identity was the ID that the webhooks put in `/actor/id` for what the application did.

## 3. Make the Secret

Linear makes the webhook's signing secret itself, in step 6, so the Secret is made then. The broker reads it once `linear-webhook` is
in the chart's `rbac.secretNames` for `acme-webhooks`, and `config.connectionNamespaces` makes the recipe's connection a team
connection ([Before you start](../receive-webhooks.md#before-you-start)). A team connection whose Secret does not exist yet does not fail: the
broker starts it at its first look after the Secret is created ([Limits](../../reference/limits.md#run-the-broker)).

The client secret is not in this Secret, and the broker never reads it: it belongs to the agent's tools (step 4).

## 4. Give the agent tools

The recipe's prompts ask the agent to do these, with a client-credentials token as `Authorization: Bearer <token>`, on
`https://api.linear.app/graphql`:

| Operation | For | Linear's GraphQL API |
|---|---|---|
| Read an issue by its identifier, with its description and comments | Every trigger | The `issue` query, which takes the identifier, such as `issue(id: "ENG-12")` |
| Create a comment on the issue | Every trigger | The `commentCreate` mutation, with `issueId` (the identifier works) and `body` in Markdown |

The tools get their token as step 2 does, with the scopes they need: Linear's documentation lists `read`, and `comments:create` for
creating comments (or `write`); the run did not record which it used. Linear's documentation says a client-credentials token is valid
for 30 days and has no refresh token, and that Linear's API refuses an expired one: the tools must get a new token before then, or
when Linear refuses it.

The run used a small MCP server of its own, with one tool for each operation, held to the test team
([Tools for the agent](../receive-webhooks.md#tools-for-the-agent)).

## 5. Apply the manifests

The recipe [`webhook-linear`](https://github.com/agentkourier/agent-kourier/tree/main/examples/recipes/webhook-linear) holds three
files. The ChatConnection:

```yaml
--8<-- "webhook-linear/kourier/chatconnection.yaml"
```

The Binding, with its three triggers:

```yaml
--8<-- "webhook-linear/kourier/binding.yaml"
```

The AgentBackend:

```yaml
--8<-- "webhook-linear/kourier/agentbackend.yaml"
```

Replace these values before you apply them:

| Where | The recipe's value | Yours |
|---|---|---|
| ChatConnection `spec.webhook.identity` | `00000000-0000-0000-0000-000000000002` | The `id` from step 2. |
| Binding `spec.chat.channel` | `ENG` | The team's key: the letters before the number of its issues' identifiers. |
| The third trigger's `comment` regex | `acme-agent` | The application's name. |
| Each trigger's `limits` | 10 plans an hour and 30 a day for each of the first two, 20 replies an hour and 60 a day; one of each at a time | What you will pay for. |
| AgentBackend `spec.url` | `http://REPLACE-ME.agents.svc.cluster.local:8080` | Your agent's A2A endpoint. On the `a2a` dialect the URL alone selects the agent, and the Binding's `agent.name` and `agent.namespace` are labels. |
| Every object's `metadata.namespace`, and the Secret's | `acme-webhooks` | The admitted namespace that holds the Secret, if you use another. |

Copy the recipe's `kourier/` directory, edit it, then apply it:

```sh
kubectl apply -f kourier/
```

The connection does not run until its Secret exists, in step 6; wait for the Binding there.

## 6. Turn on the webhook and make the Secret

Edit the OAuth application, and turn its webhook on:

| Field | Value |
|---|---|
| Webhooks | On. |
| URL | `https://<host>/chat/webhook/acme-webhooks/linear`: the namespace and the name of the ChatConnection, on HTTPS. Whether Linear takes any other scheme is not verified. |
| Resource types | Issues and Comments. |

Save. The webhook's detail page shows its signing secret: copy it, paste it into a file, and make the Secret from it, with the key
`webhookSecret`. Until the connection runs, the chat listener responds `503` to a delivery to its URL, and takes none of them.

```sh
umask 077
read -rs v; printf '%s' "$v" > ~/.config/webhooks/linear-webhook-secret; unset v     # paste the signing secret, then Return
kubectl -n acme-webhooks create secret generic linear-webhook \
  --from-file=webhookSecret="$HOME/.config/webhooks/linear-webhook-secret"
kubectl -n acme-webhooks wait --for=condition=Ready akbinding/linear-plans --timeout=120s
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier logs "$LEADER" | grep '"webhook: taking requests"' | grep 'acme-webhooks/linear'
```

`printf '%s'` writes the secret with no newline at the end ([why](../receive-webhooks.md#keep-the-secrets-out-of-sight)). If the
secret was ever shown where someone else could see it, such as a screenshot, make a new one before you go on.

## 7. Test it

Create an issue in the team, such as "Rename the codename FALCON to OSPREY", with a description. Within seconds the broker logs
these, from the leader:

```sh
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier logs "$LEADER" | grep 'acme-webhooks/linear' | grep -E \
  '"msg":"(chat: webhook event matched a trigger|webhook: delivery answered|session turn started|session turn ended)"'
```

```text
{"time":"…","level":"INFO","msg":"chat: webhook event matched a trigger","binding":"acme-webhooks/linear-plans","channel":"ENG","message":"<SHA-256 of the body>","thread":"ENG-12","delivery":"<Linear-Delivery>","trigger":0}
{"time":"…","level":"INFO","msg":"webhook: delivery answered","connection":"acme-webhooks/linear","outcome":"accepted","reason":"none","delivery":"<Linear-Delivery>",…}
{"time":"…","level":"INFO","msg":"session turn started","binding":"acme-webhooks/linear-plans","connection":"acme-webhooks/linear","channel":"ENG","thread":"ENG-12",…}
{"time":"…","level":"INFO","msg":"session turn ended",…,"thread":"ENG-12",…,"outcome":"completed","duration_ms":…}
```

The agent's plan shows as a comment. Then:

1. Edit the description. Linear saves as you type; in the run one edit sent one update. The log shows `"trigger":1` and the same
   thread, and the agent comments a revised plan.
2. Comment `acme-agent, what else would this touch?`. The log shows `"trigger":2`, and the agent replies.

Read the counts on the leader's `/metrics`. Every series of the connection exists at 0, so leave those out:

```sh
LEADER=$(kubectl -n agent-kourier get pods -l agentkourier.dev/role=leader -o name)   # the pod that takes the webhooks; empty: no leader yet
kubectl -n agent-kourier port-forward "$LEADER" 8080:8080 &
curl -s localhost:8080/metrics | grep -E '^agentkourier_(webhook_requests_total|trigger_decisions_total)' \
  | grep 'acme-webhooks/linear' | grep -v ' 0$'
```

```text
agentkourier_trigger_decisions_total{binding="acme-webhooks/linear-plans",decision="started"} 3
agentkourier_webhook_requests_total{connection="acme-webhooks/linear",outcome="accepted",reason="none"} 3
agentkourier_webhook_requests_total{connection="acme-webhooks/linear",outcome="self",reason="none"} 4
```

These are the run's counts for your three actions; read them as at least, not exactly, since every signed event with a channel and a
thread counts as `accepted`, whether a trigger matched it or not. `decision="started"` is the turns that started. `self` is the
application's three comments and one update of the issue: when the application first comments on an issue, Linear also sends an
update that subscribes it to the issue, and its actor is the application too. Each `self` delivery has its `webhook: delivery
answered` line, with `"outcome":"self"`.

## Change what starts a turn

Each change is to the recipe's ChatConnection fields or Binding triggers. A field that a trigger matches must be in the connection's
`fields` (at most 32), and a `text` field can be matched but not read by `promptTemplate`. A trigger has at most 6 `match` and 6
`exclude` entries.

**Only issues that ask for it.** The recipe reads the title as the `text` field `title`. Add it to the first trigger's `match`:

```yaml
match: {type: '^Issue$', action: '^create$', issue: '.', title: '(?i)codename'}
```

**A command instead of a mention.** Match comments that start with `/plan`, in place of the name:

```yaml
match: {type: '^Comment$', action: '^create$', issue: '.', comment: '(?im)^/plan\b'}
```

**Several teams.** Write one Binding for each team key, all naming the same connection. An event of a team that no Binding names
starts nothing.

## Troubleshooting

[When nothing happens](../receive-webhooks.md#when-nothing-happens) covers every source. For Linear:

| What you see | Why, and what to do |
|---|---|
| `unverified`, `stale` | The delivery's `webhookTimestamp` is more than a minute from the broker's clock, either way. Fix the node's time sync, or widen `verify.timestamp.window` (at most `10m`). |
| `unverified`, `bad_signature`, on every delivery | The Secret's `webhookSecret` is not the webhook's signing secret: copy it again from the webhook's detail page, and make the Secret again. |
| A description edit starts nothing | `/updatedFrom` holds the earlier values of what changed. An issue whose description was empty before may carry no earlier text there (not seen in the run); the field reads as empty, and the trigger does not match. |
| A comment that names the application starts nothing | The regex wants the application's name as a word, with or without the `@`. The application may not be offered in Linear's mention list (not verified), so type its name. |
| `accepted`, and no plan | `agentkourier_trigger_limited_total{binding="acme-webhooks/linear-plans"}` rose: with `maxConcurrent: 1`, an issue created while another plan runs is stopped. The broker's response was `200`, so Linear does not send it again, and the event is lost; the WARN line names its delivery. Raise `maxConcurrent`. |
| A delivery failed | Linear's documentation says it waits 5 seconds for a `200`, counts any other response as a failure, such as the broker's `503`, tries again after 1 minute, 1 hour and 6 hours, and may turn off a webhook that keeps failing. Whether a try carries a new `webhookTimestamp` is not verified: one that carries the first attempt's time is refused as `unverified`, `stale`, once that time is more than a minute old, and never reaches a trigger. |
| A turn, and nothing on Linear | The agent's tools: a token older than 30 days, or one without a scope to comment. |

## What Linear sends

What was seen in the run's deliveries:

- Linear signs with `Linear-Signature: <hex>`, with no prefix, and puts the time of the delivery in `/webhookTimestamp`, in
  milliseconds. The connection refuses one more than a minute from the broker's clock (`unverified`, `stale`), as Linear's
  documentation advises.
- An Issue event carries the team's key in `/data/team/key` and the issue's identifier, such as `ENG-12`, in `/data/identifier`; a
  Comment event carries them in `/data/issue/team/key` and `/data/issue/identifier`.
- The actor is `/actor/id`; for what the application did, it is the application user's ID.
- An update carries in `/updatedFrom` the earlier values of what changed and nothing else, so `/updatedFrom/description` is present
  only when the description changed.
- When the application comments on an issue, Linear also sends an update of the issue that subscribes the application to it. Its
  actor is the application, so it counts as `self` with the comment.
- A mention of the application stays plain text in `/data/body`.
- The delivery's ID is in `Linear-Delivery`, by Linear's documentation (not part of the run).

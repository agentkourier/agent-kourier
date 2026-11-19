# Start agents from webhooks

For a platform engineer who wants an agent to act on events from GitHub, Jira Cloud, Bitbucket Cloud, Linear or any other service
that signs the JSON webhooks it sends. At the end, the service sends its webhooks to Agent Kourier; each event that a trigger matches
starts a turn of your agent, in one session for each pull request or issue; and the agent answers on the service itself, with tools
and an account of its own there.

This page is how a webhook connection works and what every source needs. Each service has a page of its own, with the setup from an
empty account to a working agent:

| Service | The agent works as | The recipe's flows |
|---|---|---|
| [GitHub](webhooks/github.md) | A GitHub App | A pull request opened or pushed to gets a review; a comment that mentions the App gets a reply. |
| [Jira Cloud](webhooks/jira.md) | An Atlassian service account | An issue created gets a plan as a comment; an edit of its summary or description gets a revised plan. |
| [Bitbucket Cloud](webhooks/bitbucket.md) | A repository access token | A pull request created or updated gets a review; a comment that mentions the token gets a reply. |
| [Linear](webhooks/linear.md) | An OAuth application with client credentials | An issue created gets a plan; a change of its description gets a revised plan; a comment that names the app gets a reply. |

Each page applies the files of a recipe, `examples/recipes/webhook-<service>/`, which the repository's tests load and run against a
fake of the service. For any other service, see [Map another service](#map-another-service).

!!! warning "Experimental"

    The `webhook` platform is new, and its fields may change in a later version. The four setups were run end to end against the
    real services on 2026-11-17, with the same mapping on an earlier, unreleased version of the broker. What the pages say each
    service sends was seen in those deliveries, and the services can change it.

## How it works

A ChatConnection with `spec.platform: webhook` takes one service's webhooks. The service sends each one to
`POST /chat/webhook/<namespace>/<name>` on the broker's chat listener, where `<namespace>` and `<name>` are the ChatConnection's.
The connection's `spec.webhook` says how to read a delivery:

- `verify`: how the request is checked, on its raw bytes, before anything of it is read: the hex HMAC-SHA256 of the body under the
  credentials Secret's `webhookSecret`, or a fixed token, the Secret's `token`, in a header. With `verify.timestamp`, a time in the
  body must also be within a window of the broker's clock.
- `identity` and `actor`: who caused the event. An event that the agent's own account caused, `identity`, is dropped, so that the
  agent's own comment does not start another turn.
- `channel`: where the event belongs, such as the repository `acme/widgets` or the Jira project `OPS`. The Binding of the connection
  whose `spec.chat.channel` is that value takes it.
- `thread`: which session the event belongs to, such as `pr-7` or `OPS-1`. Each event of one thread that a trigger matches is a turn
  of the same session, so the agent can see what it said before.
- `fields`: the named values that the triggers match and that the agent is given.
- `delivery`: the header that carries the service's own ID of a delivery, which the broker's log line of each delivery names, so that
  a delivery can be found in the service's delivery log.

Each value is read with a [JSON Pointer](https://www.rfc-editor.org/rfc/rfc6901) into the body, such as `/pull_request/number`, or
a list of them, where the first that resolves wins. A string resolves to its text, a number to the number as written, and `true` and
`false` to those words; `null`, an object or an array does not resolve. A field can also be read from a request header, which the
signature does not cover.

Agent Kourier posts nothing on the service. The Binding's `chat.output` must be `none`: the CRD's default is `live`, and the broker
refuses `live` and `final` on a webhook connection (`UnsupportedOnPlatform`). Its triggers are all `type: webhookEvent`. The agent
posts with its own tools; it asks no questions and no tool approvals, and has no feedback buttons
([What each platform supports](../reference/platforms.md)).

### The steps of a delivery

Each delivery goes through these steps, in this order. A step that fails ends the delivery with the response in the table, and no later
step runs. The connection counts each response once on `agentkourier_webhook_requests_total`, by `outcome` and `reason`
([Metrics](../reference/metrics.md)); the chat listener counts the first two rows on its own counter.

| Step | It fails when | Response, `outcome` and `reason` |
|---|---|---|
| Size | The body is over `1 MiB`. The chat listener refuses it before the connection sees it. | `413`, counted as `too_large` on `agentkourier_chat_listener_requests_total` |
| Read | The chat listener could not read the body whole, before the connection sees it. | `400`, counted as `unreadable` on `agentkourier_chat_listener_requests_total`. The connection's own `malformed`, `unreadable` is for a body it could not read from what the listener holds in memory. |
| Verify | The verify header is missing or empty. | `401`, `unverified`, `unsigned` |
| | The header's value does not start with `verify.prefix`, or what follows is not the HMAC of the body under the secret, or not the token. | `401`, `unverified`, `bad_signature` |
| | With `verify.timestamp`, read only once the signature passes: the time is missing, is not a whole JSON number, or is further from the broker's clock than `window`, either way. | `401`, `unverified`, `stale` |
| Parse | The body is not valid UTF-8 or not one JSON object. | `400`, `malformed`, `schema` |
| | It is nested deeper than `64` objects and arrays. | `400`, `malformed`, `depth` |
| | A key is in its object twice on the way to a value the connection reads. | `400`, `malformed`, `duplicate_key` |
| Channel | No `channel.from` pointer resolves, or the value is not a channel: at most 140 bytes of letters, digits, `.`, `_` and `-`, starting with a letter or a digit, with `/` between parts. | `200`, `unmapped`, `no_channel` |
| Actor | An actor pointer that resolves equals `identity`, ASCII letters compared without case. | `200`, `self`, `none` |
| | No actor pointer resolves, and `actor.whenMissing` is `drop`, the default. | `200`, `ignored`, `no_actor` |
| Fields and thread | The thread's field resolves only to values that are not identifiers (below), or its value, after `thread.prefix`, holds a `:`. | `200`, `unmapped`, `bad_id` |
| | The thread's field does not resolve, or is empty. | `200`, `unmapped`, `no_thread` |
| Hand to the broker | The connection's run is not taking requests, as before it starts or between two runs. | `503` with `Retry-After`, `not_running` |
| | The broker could not take the event. | `503` with `Retry-After`, `withheld` |
| | The broker had not taken it within `2s` of when the connection's handler was called, which is after the chat listener has read the body. | `503` with `Retry-After`, `timeout` |
| | The service closed the request before the broker took it. | `503` with `Retry-After`, `cancelled` |
| | Taken. | `200`, `accepted`, `none` |

The channel is read before the actor, so an event of the agent's own that has no channel counts as `unmapped`, not `self`. The
`Retry-After` of the connection's own `503` is `1` second. The chat listener also responds `503` with `Retry-After` before the
connection is called, for a connection that does not run on that replica (`no_route`: a standby, or a connection that has not started)
and when it already runs as many requests as it may (`busy`); it counts them on `agentkourier_chat_listener_requests_total`, with
`platform="webhook"`, and not on the connection's counter. The connection's name is the `connection` label only while the leader serves a
connection of that name: a URL whose namespace or name is wrong gets the response `503`, `no_route`, under `connection="unknown"`, and so is
every try the service makes of it, until the URL is fixed.

Of the connection's responses, every one but the `401`, the `400` and the `503` is `200`, because some services count any other response
as a failed delivery (Linear's documentation says it does). The chat listener's own responses, given before the connection is called, are
`404` for a path that is not a route's, `405` for a method other than `POST`, `400`, `413` and `503`. A field of kind `id` is an identifier: at most 128 bytes of ASCII letters, digits and
`_.-:/`. A value that is not one is passed over for the field's next pointer; a field with no other value that passes is marked
invalid. Only the thread's field ends a delivery that way; any other invalid field goes on with the event, as below.

### What a taken event does

An event the broker took is tried against the `webhookEvent` triggers of the Binding that names its channel, in order; the first
that takes it starts the turn. A channel that no Binding of the connection names starts nothing. The event's ID is the SHA-256 of its
body, so the same body delivered again is the same event:

- **A body that started a turn** starts no second one when it comes again. The broker counts it on `agentkourier_intake_total` with
  `disposition="duplicate"` (or `source="reply_queue"`, `disposition="duplicate"` after a restart, when the session finds its turn
  already queued).
- **A body that matched no trigger** is remembered too: when it comes again it is `duplicate` and starts nothing, even once a
  trigger has been changed to match it. So after you fix a trigger, a delivery sent again from the service's log starts nothing
  until the broker has forgotten the body: it remembers a connection's recent events in memory, and forgets them at a restart, at a
  change of leader, at a restart of a team connection (an edit of its ChatConnection, say), or after enough later events
  ([Limits](../reference/limits.md#start-investigations-triggers-rates-and-probes)). Test a fixed trigger with a new event.
- **An event of a channel that no Binding names** starts nothing and is not remembered: once a Binding names the channel, the same
  body delivered again is tried against its triggers.
- **A start that a trigger's limits stopped** gets a `200` response all the same. No turn starts, no digest line is posted (the platform
  posts nothing), and the event is not remembered: the broker logs it at WARN,
  `chat: a webhook event was not investigated; a limit is reached, and the event is lost unless it is delivered again`, with the
  Binding, the trigger, the limit, the thread and the delivery ID. The service was told the delivery succeeded, so it does not send
  it again; the event is lost unless it is delivered again, from the service's delivery log or by hand. A delivery of it again is
  decided again, and starts the turn if the limits then allow it. Stopped again, it is logged at WARN again, with its delivery ID,
  and not counted again on `agentkourier_trigger_limited_total`.
- **An event of a thread whose session has ended** (its `threadTTL` ran out, or the Binding it belonged to is gone) starts a new
  session in its place, and the broker logs `chat: a webhook event replaced the ended session of its thread`. While the old session
  may still be at work (its thread still holds a turn that is not delivered), or when the same body was queued before, the event gets
  a `200` response, starts nothing and is not remembered: the broker logs it at INFO,
  `chat: a webhook event's thread cannot be continued yet, so the event was dropped`, with its delivery ID. It is lost in the same way
  unless it is delivered again later.
- **An `id` field marked invalid** makes a trigger whose `match` or `exclude` names it miss, so it starts nothing through that
  trigger. A template that reads it refuses the start: no turn, counted on `agentkourier_trigger_refused_total` with
  `reason="invalid"`, logged at WARN, and audited. A webhook connection posts no note of the refusal.

What a service does with a `503` is its own. Their documentation, read on 2026-11-18 and not checked against the services, says:

| Service | After a `503` |
|---|---|
| GitHub | Nothing: it does not send a failed delivery again. Send it again by hand from the delivery log ([GitHub](webhooks/github.md#troubleshooting)). |
| Jira Cloud | Up to five more tries, each 5 to 15 minutes after the last. |
| Bitbucket Cloud | Up to two more tries. |
| Linear | Up to three more tries, after 1 minute, 1 hour and 6 hours. |

## Before you start

- **The chat listener, reachable over HTTPS from the service.** In the chart's values, `chatListener.enabled: true` opens it, and
  `chatListener.ingress` sends `/chat/` to it. [Configure Slack](configure-slack.md), HTTP mode, step 6, shows the values and what
  each does; the same listener serves Slack and Microsoft Teams in their HTTP modes, and webhooks. Only the leading replica takes requests. A broker with no chat listener
  does not run a webhook connection: as its own connection the start fails with a wiring error, and a team's connection is not
  started. A connection's webhook URL is `https://<host>/chat/webhook/<namespace>/<name>`; the service pages call the host `<host>`.
- **A broker that serves the connection.** The recipes put each ChatConnection in the namespace `acme-webhooks`, on custom resources
  (the chart's default route, which needs Kubernetes 1.33 or later), as a team connection beside the broker's own. The broker must
  admit the namespace and read the connections' Secrets. In the chart's values, with the Secret of each service you set up:

    ```yaml
    config:
      connectionNamespaces: [acme-webhooks]
    rbac:
      secretNames:
        acme-webhooks: [github-webhook, jira-webhook, bitbucket-webhook, linear-webhook]
    ```

    [Let a team bring its own Slack app](team-slack-app.md#3-the-platform-team-admits-the-namespace) has the other ways to admit a
    namespace, and how a team grants its own Secrets with a Role
    ([step 2](team-slack-app.md#2-the-team-makes-the-secret-the-grant-and-the-connection)). A webhook connection can be the broker's
    own instead (`config.chatConnection` and `config.chatConnectionSecret`), which is the only way on the ConfigMap route: there the
    broker runs exactly one ChatConnection. The broker builds the platform of its own connection when it starts and when a replica
    takes the lead, so an edit to its `spec.webhook` applies when Agent Kourier restarts; a team connection restarts by itself when
    its ChatConnection changes.
- **An agent with tools for the service**, and an account there for it to work as, with a credential that only the agent's tools
  hold. Agent Kourier never sees or passes that credential ([Tools for the agent](#tools-for-the-agent)). Any A2A agent works: each
  recipe has an AgentBackend of the `a2a` dialect whose `url` is a placeholder, `REPLACE-ME`, for your agent's
  ([Connect a generic A2A agent](connect-a2a-agent.md), or [Connect a kagent agent](connect-kagent.md) for a kagent agent).

Make the namespace once, for all the services:

```sh
kubectl create namespace acme-webhooks
```

## Keep the secrets out of sight

The service pages keep every secret and token in a file under `~/.config/webhooks/` that only you can read, and pass it to `kubectl`
and `curl` from that file, so that no secret is on a command line, in your shell's history or on the screen:

- `umask 077` before writing a file makes it mode 600.
- A secret you generate is written with no newline at the end: `openssl rand -hex 32 | tr -d '\n' > <file>`. `kubectl create secret
  --from-file` copies the file's bytes as they are, and the broker refuses a secret or a token that holds a control character, a
  trailing newline included: the connection is not built, and the error names the Secret and the key, never the value.
- A secret the service shows you is pasted into a prompt that does not echo it: `read -rs v; printf '%s' "$v" > <file>; unset v`.
- To paste a secret into the service's form, copy the file to the clipboard: `pbcopy < <file>` on macOS, `xclip -selection clipboard <
  <file>` on Linux with X11.
- `curl` reads a token from a header file (`-H @<file>`) or a config file (`-K <file>`), never from an argument.

## Tools for the agent

The agent posts on the service with tools of its own: an MCP server, or any tool your agent framework runs. Each service page lists
the operations its recipe's prompts ask for, so that you can choose tools or write them.

As of 2026-11-17, no off-the-shelf MCP server fitted these setups when they were run (upstream projects' states on that date, not
checked here):

- github-mcp-server v2.2.0 renewed a GitHub App's installation token in its stdio mode only; its HTTP mode took a bearer token with
  each request.
- mcp-atlassian v0.24.6 reached the Atlassian API gateway URL, which an Atlassian service account's token needs, only through the
  workaround in its issue #968.
- No Bitbucket MCP server took a repository access token over HTTP.

The run used a small MCP server of its own, with only the tools the prompts need, each held to the test repository, project or team.
Check the projects' releases before you write your own. On kagent, the agent reaches an MCP server through a `RemoteMCPServer`; in
the run, kagent did not pick up tools added to a running server until the `RemoteMCPServer`'s spec changed.

## Map another service

Start from a delivery of the service, from its delivery log if it has one or its documentation's samples, and write the connection
from it. The [GitHub recipe's](webhooks/github.md#5-apply-the-manifests) ChatConnection is a complete one.

- **`verify`**: the header that carries the signature, and the text before the hex, such as `sha256=`. A service that sends a fixed
  token takes `scheme: token`, with the token's header and a prefix such as `Bearer `, and a token of at least 32 bytes in the
  Secret's `token`. A service that signs with an HMAC and puts a time in the body can have it checked with `timestamp`: its pointer,
  its unit, `ms` or `s`, and a window above 0 and at most `10m`. The `token` scheme takes no timestamp.
- **`channel`**: the pointer to what the Binding names, or a fixed `value` for a source with one channel.
- **`identity` and `actor`**: the agent's own account as the service names it, and every pointer that can name who caused the event,
  since different events name it in different places. A source with no actors sets `actor.none: true`, and no `identity`;
  `whenMissing: keep` takes an event that names no actor instead of dropping it.
- **`thread`**: an `id` field, and a `prefix` such as `pr-`. Left out, every event is a thread and a session of its own.
- **`fields`**: what the triggers match and the agent needs, at most 32. A field is `text` by default, or `id`. A field may not read
  the verify header, `Authorization`, `Proxy-Authorization` or `Cookie`.
- **`delivery`**: the header of the service's delivery ID, such as `X-GitHub-Delivery`. It may not be a header a field may not read.

A field's kind decides where its value can go. An `id` field can be the thread, and the trigger's `promptTemplate` can read it, as
`{{ .Fields.<name> }}`, with `{{ .Channel }}`; the template reads nothing else. A `text` field, which is what people wrote, cannot: it
reaches the agent only after the template's output, in a block that is marked as untrusted data, with the actor and every field that
has a value, `id` fields too. A field that does not fit in the block is left out whole, and the block's last line names it
([Limits](../reference/limits.md#write-a-prompt-template)).

A Binding of a webhook connection writes `chat.output: none` and lists one trigger or more, each `type: webhookEvent`, with:

- `match` and `exclude`: regexes on the connection's fields, named by the fields' names. Every `match` regex must find a match and
  no `exclude` regex may. A regex is a search, not anchored unless it says so, and a field that did not resolve reads as empty, so
  `'.'` requires a field. A field marked invalid makes the trigger miss, in `match` and in `exclude` alike.
- `promptTemplate`, which may read only `.Channel` and `id` fields, and `limits`, both `rateLimit` and `maxRunsPerDay`, which are
  required.
- At most 6 entries in `match` and 6 in `exclude`.

A name the connection does not have, or a `text` field that the template reads, is refused when the broker reads the Binding with its
connection (`UnsupportedOnPlatform`). Then make the Secret and apply the objects as a service page does: the Secret holds
`webhookSecret` (for `hmac-sha256`) or `token` (for `token`), and the webhook on the service points at
`https://<host>/chat/webhook/<namespace>/<name>`.

The [ChatConnection reference](../reference/chatconnection.md#webhookspec) has every field, and [Limits](../reference/limits.md) the
bounds.

## Follow a delivery

The broker logs JSON lines, from the leader. Each delivery that is verified is logged once at INFO, as its response is written:

```text
{"time":"…","level":"INFO","msg":"webhook: delivery answered","connection":"acme-webhooks/github","outcome":"accepted","reason":"none","delivery":"<the service's delivery ID>","event":"<SHA-256 of the body>","bytes":8123,"duration_ms":4}
```

`delivery` is the value of the connection's `delivery.header`, empty when the connection names none or the value is not an identifier
of at most 128 bytes; it is the ID by which the service's delivery log finds the delivery. `event` is the event's ID, which the
intake's lines name as `message`. A delivery that matches a trigger also logs, before the delivery's line,
`chat: webhook event matched a trigger`, with the Binding, the channel, the `message`, the `thread`, the `delivery` and the trigger's
index, and then `chat: trigger decided`, with the Binding, the trigger's index, the `decision`, the `message` and the session's
`thread`, and no delivery ID: join the two by `message`. The turn logs `session turn started` and `session turn ended` with the same
`thread`.

A request that is not verified, or whose body could not be read, is logged at DEBUG only, and never with its body or its signature.
The first unverified request of a connection also logs a WARN, `webhook: requests were refused: they are not verified`, with a count
for each reason (`unsigned`, `bad_signature`, `stale`); the unverified requests after it are summed into the next such WARN, which
the first unverified request at least `1m` after the last WARN logs, with `since`. So a scanner of the public URL cannot flood the log.

With Prometheus, `sum by (outcome, reason) (increase(agentkourier_webhook_requests_total{connection="acme-webhooks/github"}[1h]))`
shows what the connection responded. Each of its series exists at 0 from when the connection's run starts, so a rule on it has a value
after a restart. [Export metrics and traces](export-telemetry.md#alert-on-webhook-deliveries) has rules for webhook connections.

## When nothing happens

The counts below are on the leader's `/metrics` ([Export metrics and traces](export-telemetry.md)).

| What you see | Why |
|---|---|
| The connection's series of `agentkourier_webhook_requests_total` are missing | The connection does not run. The log says why at WARN: `the ChatConnection cannot be started`, with an error that names the Secret and the key for a credential that is empty, holds a control character such as a trailing newline, or is a token shorter than 32 bytes. Make the Secret again from a file with no newline. |
| Every series at 0 | The deliveries did not reach the connection. `agentkourier_chat_listener_requests_total` says what the listener did. The likely mistake is a URL whose namespace or name is wrong: it gets the response `503`, `no_route`, counted with `platform="webhook"` and `connection="unknown"`, and each try the service makes of it gets that response again, until the URL is fixed. The connection's own name is the label only while the leader serves that name, so `no_route` under it is a connection the leader serves that does not run yet. The other outcomes: `busy` a full listener (503), `too_large` a body over `1 MiB` (413), `unreadable` a body the listener could not read whole (400), `method` a method other than `POST` (405, under `connection="unknown"`), and `not_found` a path that is not `/chat/<platform>/<namespace>/<name>`, such as one without `/chat/webhook/` (404, under `platform="other"` and `connection="unknown"`). With no count there either, nothing reached the listener: the service's webhook is not active, or the listener is not exposed. |
| The service shows `413` | The body is over `1 MiB`: `too_large` on `agentkourier_chat_listener_requests_total`. The connection never sees it, and no setting raises the bound. |
| The service shows `503` | The connection's counter says which: `not_running` (the run is not taking requests; `agentkourier_chat_connected{connection="<namespace>/<name>"}` is not 1), `withheld` (the broker could not take the event; the intake logs why), `timeout` (not taken within `2s`) or `cancelled` (the service hung up first). With none of those, the listener responded: `no_route` or `busy` above. With neither counter, something in front of the chat Service responded: no replica leads, or the leader's pod is not labelled yet. The [table above](#what-a-taken-event-does) says what each service then does. |
| `unverified`, `unsigned` | The request has no verify header: the service sends no signature (its secret is not set), or `verify.header` is not the header it signs in. |
| `unverified`, `bad_signature` | The secret is not the service's, or `verify.prefix` is not the text before the signature. Copy the secret into the service again from the file the Secret was made from. |
| `unverified`, `stale` | With `verify.timestamp`, the delivery's time is outside the window of the broker's clock, or the body has no time there: check the node's time sync, or widen the window (at most `10m`). A delivery that the service sends again with its first time is refused so once the window has passed. |
| `malformed`, `schema` | The body is not one JSON object: a service set to send a form instead of JSON, or one that sends no body. |
| `malformed`, `depth` or `duplicate_key` | The body is nested deeper than 64, or has a key twice on the way to a value the connection reads. |
| `unmapped`, `no_channel` | No channel pointer resolved, or the value is not a channel. A service's ping or setup events, such as GitHub's `ping` and `installation`, are this too. |
| `unmapped`, `bad_id` or `no_thread` | The thread's field is not an identifier, holds a `:`, or is missing: an event the connection's thread does not fit, such as a comment on a plain issue where the thread is a pull request's number. |
| `self` for a person's event | `identity` is a person's, or one of `actor.from` names someone other than who caused the event. |
| `ignored`, `no_actor` | No actor pointer resolved. |
| `accepted`, and no turn | `agentkourier_intake_total{connection="<namespace>/<name>"}` says what the intake did: `ignored` is a channel that no Binding of the connection names, `trigger_unmatched` an event that no trigger matched (`agentkourier_trigger_unmatched_total` of the Binding counts it too, and the DEBUG log names it), and `duplicate` a body the connection already took, whether it started a turn or matched no trigger, so a body sent again after a fix of its trigger is `duplicate` too, until the broker forgets it ([above](#what-a-taken-event-does)). A trigger's limits can stop it: `agentkourier_trigger_limited_total` rises, and the WARN line above names the delivery, which is lost unless it is delivered again. A refused prompt counts on `agentkourier_trigger_refused_total`. |
| A turn, and nothing on the service | The agent's tools or its account: check the agent's own logs. Agent Kourier posts nothing on the service. |

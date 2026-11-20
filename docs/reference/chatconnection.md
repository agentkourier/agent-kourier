# ChatConnection

This page lists every field of the ChatConnection resource, as the Go types in `api/v1alpha1` define it.

The Default column shows what happens when a field is omitted. None means no default is supplied.

`apiVersion: agentkourier.dev/v1alpha1` · `kind: ChatConnection`

ChatConnection is a workspace connection. The platform team owns the broker's own; a team may own one in a namespace the platform admits.

## ChatConnection

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `metadata` | ObjectMeta | Yes | None | Standard Kubernetes object metadata. Agent Kourier reads `name` and `namespace`. |
| `spec` | [ChatConnectionSpec](#chatconnectionspec) | Yes | None |  |

## ChatConnectionSpec

ChatConnectionSpec is the desired state of a ChatConnection.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `platform` | string | Yes | None | Platform is the chat platform: slack; teams, Microsoft Teams; or webhook, which takes signed JSON webhooks from a service such as a code host or an issue tracker (spec.webhook). The broker takes a webhook connection's webhooks on its chat listener (AGENTKOURIER_CHAT_LISTEN) at POST /chat/webhook/&lt;namespace&gt;/&lt;name&gt;, and posts nothing there: the agent posts on the service with its own tools. A broker runs a teams connection in http or socket mode (spec.teams.mode). A webhook connection, and a teams one in http mode or with a spec.teams.notificationURL in either mode, need the broker's chat listener: on a broker with none, the broker's own connection fails its start, and a team's is not started. One of `slack`, `teams`, `webhook`. |
| `credentialsSecretRef` | [SecretRef](#secretref) | Yes | None | CredentialsSecretRef names one Secret whose keys are fixed by the platform and its mode. A Slack connection in Socket Mode needs botToken and appToken; one in HTTP mode needs botToken and signingSecret. A teams connection needs clientSecret, the client secret of its Microsoft Entra application. A webhook connection needs webhookSecret with the hmac-sha256 scheme, and token with the token scheme. |
| `allowedNamespaces` | []string | No | Empty list | AllowedNamespaces lists the other namespaces whose Bindings may reference this connection. Each is a DNS-1123 label. An empty list permits references only from the connection's own namespace. At most 256. |
| `slack` | [SlackConnectionSpec](#slackconnectionspec) | No | None | Slack holds the settings of a Slack connection. Only a connection whose platform is slack may set it. Omitted, the connection uses Socket Mode. |
| `teams` | [TeamsConnectionSpec](#teamsconnectionspec) | No | None | Teams holds the settings of a teams connection. A connection whose platform is teams must set it, and no other may. |
| `webhook` | [WebhookSpec](#webhookspec) | No | None | Webhook is how a webhook connection reads a delivery. A connection whose platform is webhook requires it, and every other refuses it. |

## SecretRef

SecretRef names a Secret in the referring object's namespace whose keys are fixed.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `name` | string | Yes | None | Name is a DNS-1123 subdomain: at most 253 lowercase letters, digits, '-' or '.', starting and ending with a letter or digit. Pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`. At most 253 characters. |

## SlackConnectionSpec

SlackConnectionSpec is the settings of a Slack connection.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `mode` | string | No | socket | Mode selects how Slack delivers the app's events and button presses: socket, over a Socket Mode WebSocket that Agent Kourier opens, or http, as Events API and interactivity requests that Slack sends to Agent Kourier, signed with the app's signing secret, which the broker takes on its chat listener (AGENTKOURIER_CHAT_LISTEN) at POST /chat/slack/&lt;namespace&gt;/&lt;name&gt;. The mode sets the keys of the credentials Secret (credentialsSecretRef). A broker with no chat listener does not run a connection in HTTP mode: its own fails the start, and a team connection is not started. One of `socket`, `http`. |

## TeamsConnectionSpec

TeamsConnectionSpec is the settings of a teams connection: the Microsoft Entra tenant and application of its bot, how Microsoft Teams delivers its activities, and the URL of the connection's route on the broker's chat listener.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `tenantID` | string | Yes | None | TenantID is the Microsoft Entra tenant of the bot, a GUID in lowercase hex, such as 11111111-2222-3333-4444-555555555555. A connection has one tenant. Pattern `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`. At most 36 characters. |
| `appID` | string | Yes | None | AppID is the bot's Microsoft Entra application (client) ID, a GUID in lowercase hex. It is not a secret: the client secret is the credentials Secret's clientSecret. Pattern `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`. At most 36 characters. |
| `mode` | string | No | http | Mode selects how Microsoft Teams delivers the bot's activities: http, as requests that Teams sends to the broker's chat listener (AGENTKOURIER_CHAT_LISTEN) at POST /chat/teams/&lt;namespace&gt;/&lt;name&gt;, or socket, over an outbound WebSocket that Agent Kourier opens to Teams, with no public endpoint. Both use the same credentials, the Secret's clientSecret. Unlike Slack's, the default is http: Microsoft's documentation recommends an HTTPS messaging endpoint for agents in production, and says that Socket mode is in public preview. In http mode a broker needs its chat listener. In socket mode only the leader connects, with a socket in each region that accepts one, and what Teams sends while no socket is ready it does not deliver, then or later. One of `http`, `socket`. |
| `notificationURL` | string | No | None | NotificationURL is the public URL of the connection's route on the broker's chat listener, for Microsoft Graph's change notifications of the bound channels' messages. Its path is /chat/teams/&lt;namespace&gt;/&lt;name&gt;, the ChatConnection's own namespace and name. It is optional, in either mode. At most 2048 bytes, https only (Microsoft Graph's documentation of a subscription's notificationUrl requires HTTPS), with no credentials, query or fragment. The API server checks the form of the path, and the broker that it is the connection's own. When it is set, the leader keeps a Microsoft Graph subscription to the messages of each channel that a Binding of the connection names, with this URL for its notifications, and reads each new message that Graph notifies: that is how a post of another app, such as an alert's card that Workflows posts, reaches the connection, since Teams sends a bot no other bot's post. It needs the chat listener in either mode, and Graph must reach the leader at this URL. Unset, the leader polls Microsoft Graph for the messages of each bound channel instead, which needs no public route: another app's post reaches the connection later, and a reply only in a channel's newest threads. At most 2048 characters. |

## WebhookSpec

WebhookSpec maps a signed JSON webhook onto an event: the account the agent works as, how a delivery is verified, its channel, its actor, its thread, its named fields, and the header that names a delivery. Each value is read with a JSON Pointer (RFC 6901), or a first-of list of them, where the first that resolves wins.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `identity` | string | No | None | Identity is the account the agent works as on the source, as the source names it in the actor pointers, such as pr-reviewer[bot] or an Atlassian account ID: letters, digits and _.:@{}[]/-, at most 128 bytes. An event that it caused is dropped. Required unless actor.none is true, which refuses it. Pattern `^([A-Za-z0-9_.:@{}\[\]/-]+)?$`. At most 128 characters. |
| `verify` | [WebhookVerify](#webhookverify) | Yes | None | Verify is how a delivery is checked, on its raw bytes, before anything of it is read. |
| `channel` | [WebhookChannel](#webhookchannel) | Yes | None | Channel is where an event belongs: the Binding of the connection whose spec.chat.channel is the value takes it. |
| `actor` | [WebhookActor](#webhookactor) | Yes | None | Actor is who caused an event, by which the agent's own events are dropped. |
| `thread` | [WebhookThread](#webhookthread) | No | None | Thread names the field whose value, after prefix, is the event's thread. Left out, every event is a thread of its own. |
| `fields` | [][WebhookField](#webhookfield) | No | None | Fields are the event's named values, in the order the agent reads them. At most 32, each named once. |
| `delivery` | [WebhookDelivery](#webhookdelivery) | No | None | Delivery names the header that carries the source's ID of a delivery, which the log line of each answered delivery shows. Left out, the line shows none. |

## WebhookVerify

WebhookVerify is how a request is verified, on its raw bytes, before anything of it is parsed.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `scheme` | string | Yes | None | Scheme is hmac-sha256, the hex HMAC-SHA256 of the body under the credentials Secret's webhookSecret, or token, the credentials Secret's token itself. One of `hmac-sha256`, `token`. |
| `header` | string | Yes | None | Header holds the signature or the token: an HTTP header name of at most 64 bytes. Pattern `^[-!#$%&'*+.^_|~0-9A-Za-z\x60]+$`. At least 1 character. At most 64 characters. |
| `prefix` | string | No | None | Prefix comes before the signature or the token in the header, such as "sha256=" or "Bearer ": printable ASCII, at most 32 bytes. Pattern `^[ -~]*$`. At most 32 characters. |
| `timestamp` | [WebhookTimestamp](#webhooktimestamp) | No | None | Timestamp, of the hmac-sha256 scheme alone, is a time in the body that must be within window of now, either way. It is read after the signature passes. |

## WebhookTimestamp

WebhookTimestamp is a time in a webhook's body.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `from` | string | Yes | None | From is the pointer to the time: an integer JSON number of seconds or milliseconds since the Unix epoch, as unit says. At most 256 characters. |
| `unit` | string | Yes | None | Unit is ms or s. One of `ms`, `s`. |
| `window` | duration | Yes | None | Window is how far from now the time may be, either way: above 0 and at most 10m. Pattern `^([-+]?0|[-+]?([0-9]*\.?[0-9]*(ns|us|µs|μs|ms|s|m|h))+)$`. At most 64 characters. |

## WebhookChannel

WebhookChannel is where an event's channel comes from: a first-of list of pointers, or a fixed value. Set one of from and value.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `from` | []string | No | None | From is a first-of list of pointers to the channel. At most 8, each at most 256 bytes. |
| `value` | string | No | None | Value is a fixed channel, for a source that has one: letters, digits, '.', '_' and '-', starting with a letter or a digit, with '/' between parts, at most 140 bytes. Pattern `^([A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9._-]+)*)?$`. At most 140 characters. |

## WebhookActor

WebhookActor is who caused an event: the event is dropped as the agent's own when any pointer that resolves equals the connection's identity, compared ASCII case-insensitive.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `from` | []string | No | None | From lists the pointers that can name who caused an event, each compared with the identity. Required unless none is true, which refuses it. At most 8, each at most 256 bytes. |
| `none` | boolean | No | None | None turns the self-drop off, for a source that has no actors. |
| `whenMissing` | string | No | drop | WhenMissing says what happens to an event in which no actor pointer resolves: drop (the default) or keep. One of `drop`, `keep`. |

## WebhookThread

WebhookThread names the field of an event's thread.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `field` | string | Yes | None | Field is the name of an id field. At least 1 character. At most 63 characters. |
| `prefix` | string | No | None | Prefix comes before the field's value in the thread, such as pr-: letters, digits, '.', '_' and '-', at most 32 bytes. Pattern `^[A-Za-z0-9._-]*$`. At most 32 characters. |

## WebhookField

WebhookField is one named value of an event: from a first-of list of pointers into the body, or from a header. Set one of from and header.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `name` | string | Yes | None | Name is how the triggers and the template name the field: a lower-case letter, then letters and digits, at most 63 bytes. key and state are reserved. Pattern `^[a-z][A-Za-z0-9]*$`. At least 1 character. At most 63 characters. |
| `from` | []string | No | None | From is a first-of list of pointers into the body. At most 8, each at most 256 bytes. |
| `header` | string | No | None | Header is a request header, of at most 64 bytes. It is not signed, so a header field is a filter hint only. A field may not read verify.header, Authorization, Proxy-Authorization or Cookie. Pattern `^([-!#$%&'*+.^_|~0-9A-Za-z\x60]+)?$`. At most 64 characters. |
| `kind` | string | No | text | Kind is text (the default) or id. An id field's value is checked as an identifier, and the thread and the prompt template may read it; a text field reaches the agent only as untrusted data. One of `id`, `text`. |

## WebhookDelivery

WebhookDelivery names where a delivery's ID is.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `header` | string | Yes | None | Header is the request header that carries the source's ID of a delivery, such as X-GitHub-Delivery: an HTTP header name of at most 64 bytes, not verify.header, Authorization, Proxy-Authorization or Cookie. Pattern `^[-!#$%&'*+.^_|~0-9A-Za-z\x60]+$`. At least 1 character. At most 64 characters. |

---

Generated by `hack/docs/refgen` from `api/v1alpha1`. Do not edit this page; change the source and run `make docs-ref`.

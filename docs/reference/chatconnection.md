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
| `platform` | string | Yes | None | Platform is the chat platform: slack, or teams, Microsoft Teams. No broker runs a teams connection yet: one that is the broker's own connection stops its start, and a team one is not started. One of `slack`, `teams`. |
| `credentialsSecretRef` | [SecretRef](#secretref) | Yes | None | CredentialsSecretRef names one Secret whose keys are fixed by the platform and its mode. A Slack connection in Socket Mode needs botToken and appToken; one in HTTP mode needs botToken and signingSecret. A teams connection needs clientSecret, the client secret of its Microsoft Entra application. |
| `allowedNamespaces` | []string | No | Empty list | AllowedNamespaces lists the other namespaces whose Bindings may reference this connection. Each is a DNS-1123 label. An empty list permits references only from the connection's own namespace. At most 256. |
| `slack` | [SlackConnectionSpec](#slackconnectionspec) | No | None | Slack holds the settings of a Slack connection. Only a connection whose platform is slack may set it. Omitted, the connection uses Socket Mode. |
| `teams` | [TeamsConnectionSpec](#teamsconnectionspec) | No | None | Teams holds the settings of a teams connection. A connection whose platform is teams must set it, and no other may. |

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
| `mode` | string | No | http | Mode selects how Microsoft Teams delivers the bot's activities: http, as requests that Teams sends to the broker's chat listener (AGENTKOURIER_CHAT_LISTEN) at POST /chat/teams/&lt;namespace&gt;/&lt;name&gt;, or socket, over an outbound WebSocket that Agent Kourier opens to Teams, with no public endpoint. Both use the same credentials, the Secret's clientSecret. Unlike Slack's, the default is http: Microsoft's documentation recommends an HTTPS messaging endpoint for agents in production, and says that Socket mode is in public preview. No broker runs a teams connection yet, in either mode. One of `http`, `socket`. |
| `notificationURL` | string | No | None | NotificationURL is the public URL of the connection's route on the broker's chat listener, for Microsoft Graph's change notifications of the bound channels' messages. Its path is /chat/teams/&lt;namespace&gt;/&lt;name&gt;, the ChatConnection's own namespace and name. It is optional, in either mode. At most 2048 bytes, https only (Microsoft Graph's documentation of a subscription's notificationUrl requires HTTPS), with no credentials, query or fragment. The API server checks the form of the path, and the broker that it is the connection's own. No broker runs a teams connection yet: the URL is checked, and not used. At most 2048 characters. |

---

Generated by `hack/docs/refgen` from `api/v1alpha1`. Do not edit this page; change the source and run `make docs-ref`.

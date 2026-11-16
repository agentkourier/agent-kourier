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
| `platform` | string | Yes | None | Platform is the chat platform: slack, or web, whose integration spec.web.profile names. No broker runs a web connection yet: one that is the broker's own connection stops its start, and a team one is not started. One of `slack`, `web`. |
| `credentialsSecretRef` | [SecretRef](#secretref) | Yes | None | CredentialsSecretRef names one Secret whose keys are fixed by the platform and its mode or profile. A Slack connection in Socket Mode needs botToken and appToken; one in HTTP mode needs botToken and signingSecret. A web connection of the kourier-v1 profile needs signingSecret, and one of the github profile webhookSecret. |
| `allowedNamespaces` | []string | No | Empty list | AllowedNamespaces lists the other namespaces whose Bindings may reference this connection. Each is a DNS-1123 label. An empty list permits references only from the connection's own namespace. At most 256. |
| `slack` | [SlackConnectionSpec](#slackconnectionspec) | No | None | Slack holds the settings of a Slack connection. Only a connection whose platform is slack may set it. Omitted, the connection uses Socket Mode. |
| `web` | [WebConnectionSpec](#webconnectionspec) | No | None | Web holds the settings of a web connection. A connection whose platform is web must set it, and no other may. |

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

## WebConnectionSpec

WebConnectionSpec is the settings of a web connection: the profile that its requests and replies follow.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `profile` | string | Yes | None | Profile is the integration the connection speaks. kourier-v1 is Agent Kourier's own signed JSON contract: it takes signed requests and posts the agent's replies, signed, to url. github takes GitHub's webhooks and sends no replies: the agent posts on GitHub itself, so its Bindings take output none alone. One of `kourier-v1`, `github`. |
| `url` | string | No | None | URL is where the kourier-v1 profile posts the agent's replies. It is required for kourier-v1 and refused for github, which sends none. It is an https URL, or an http one only to localhost, a loopback address (127.x.x.x or [::1]) or a Service in the cluster (a host that ends in .svc or .svc.cluster.local), since a reply carries what the agent wrote. At most 2048 bytes, with no credentials, query or fragment: the replies are signed with the credentials Secret's signingSecret. At most 2048 characters. |
| `github` | [GitHubWebSpec](#githubwebspec) | No | None | GitHub holds the settings of the github profile. A connection of the github profile must set it, and no other may. |

## GitHubWebSpec

GitHubWebSpec is the settings of a web connection of the github profile.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `login` | string | Yes | None | Login is the GitHub login the agent posts as: a user's, such as pr-reviewer, or a GitHub App's, such as pr-reviewer[bot]. A comment on a pull request, or a review comment, starts a turn only when it mentions the login (an app's login without its [bot]), and a comment whose author is the login starts none. At most 39 letters and digits with single hyphens between them, then an optional [bot], at most 44 bytes. Pattern `^[A-Za-z0-9](?:-?[A-Za-z0-9]){0,38}(?:\[bot\])?$`. At most 44 characters. |

---

Generated by `hack/docs/refgen` from `api/v1alpha1`. Do not edit this page; change the source and run `make docs-ref`.

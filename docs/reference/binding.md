# Binding

This page lists every field of the Binding resource, as the Go types in `api/v1alpha1` define it.

The Default column shows what happens when a field is omitted. None means no default is supplied.

`apiVersion: agentkourier.dev/v1alpha1` · `kind: Binding`

Binding connects one agent to one chat channel. Owned by an application team.

## Binding

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `metadata` | ObjectMeta | Yes | None | Standard Kubernetes object metadata. Agent Kourier reads `name` and `namespace`. |
| `spec` | [BindingSpec](#bindingspec) | Yes | None |  |

## BindingSpec

BindingSpec is the desired state of a Binding.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `agent` | [AgentRef](#agentref) | Yes | None |  |
| `identity` | [Identity](#identity) | Yes | None |  |
| `chat` | [ChatBinding](#chatbinding) | Yes | None |  |
| `interactions` | [Interactions](#interactions) | No | None |  |
| `feedback` | [Feedback](#feedback) | No | None |  |
| `session` | [Session](#session) | No | None |  |

## AgentRef

AgentRef names the agent on a backend.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `backendRef` | [ObjectRef](#objectref) | Yes | None |  |
| `name` | string | Yes | None | Name is the agent's name on the backend. At least 1 character. |
| `namespace` | string | Yes | None | Namespace is the agent's namespace on the backend: a DNS-1123 label of at most 63 lowercase letters, digits or '-', starting and ending with a letter or digit. |

## ObjectRef

ObjectRef names a resource in a namespace. An empty namespace means the namespace of the referring object. A Binding's agent.backendRef and chat.connectionRef may name an AgentBackend or a ChatConnection in another namespace only if that resource lists the Binding's namespace in its allowedNamespaces.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `namespace` | string | No | Namespace of the referring object |  |
| `name` | string | Yes | None | Name is a DNS-1123 subdomain: at most 253 lowercase letters, digits, '-' or '.', starting and ending with a letter or digit. |

## Identity

Identity is the service identity every turn in the Binding runs as, whoever wrote the message. Set userId, tokenSecretRef, or both.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `userId` | string | No | None | UserID is sent as X-User-Id when the backend runs in insecure mode. |
| `tokenSecretRef` | [SecretKeyRef](#secretkeyref) | No | None | TokenSecretRef holds the OIDC bearer token. |

## SecretKeyRef

SecretKeyRef names one value in a Secret in the referring object's namespace.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `name` | string | Yes | None | Name is a DNS-1123 subdomain: at most 253 lowercase letters, digits, '-' or '.', starting and ending with a letter or digit. |
| `key` | string | No | `token` | Key selects the entry of the Secret. It defaults to "token". It consists of alphanumeric characters, '-', '_' or '.'. |

## ChatBinding

ChatBinding places the Binding in a channel.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `connectionRef` | [ObjectRef](#objectref) | Yes | None |  |
| `channel` | string | Yes | None | Channel is the platform's channel ID, not its name (Slack: `C…` or `G…`). Pattern `^[CG][A-Z0-9]+$`. |
| `output` | string | No | `live` | Output defaults to live. One of `live`, `final`. |
| `threadReplies` | string | No | `all` | ThreadReplies defaults to all: any person's reply in a thread Agent Kourier owns is a turn. With mention, only a reply that mentions Agent Kourier is a turn, whether a mention or a trigger started the thread; the next mentioned turn carries the other replies as context. A typed answer to a pending question that takes text replies needs no mention. One of `all`, `mention`. |
| `acknowledgment` | [ChatAcknowledgment](#chatacknowledgment) | No | None | Acknowledgment controls the temporary reaction Agent Kourier adds to an accepted message while it is being processed. It defaults to enabled with the platform's own reaction (Slack: hourglass_flowing_sand). |
| `triggers` | [][Trigger](#trigger) | No | `[{type: mention}]` | Triggers decide which events in the channel start or continue a turn. Empty means [{type: mention}]: a mention starts a session. A list that names triggers is the whole list. |

## ChatAcknowledgment

ChatAcknowledgment configures the temporary reaction on an accepted message.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `enabled` | boolean | No | `true` | Enabled adds the reaction after the message is durably accepted and removes it when processing ends. It defaults to true. |
| `emoji` | string | No | None | Emoji is the reaction name without colons. Left empty, the platform's own acknowledgment reaction is used (Slack: hourglass_flowing_sand). The API fills in no default, so a stored Binding does not carry one platform's reaction. Pattern `^(?:[a-z0-9][a-z0-9+_-]*|[+-][a-z0-9][a-z0-9+_-]*)$`. |

## Trigger

Trigger is one way an event in the bound channel starts or continues a turn. Triggers are tried in order and the first that matches handles the event.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `type` | string | Yes | None | Type is mention, a person @mentioning Agent Kourier, or botMessage, a message a bot posts. A mention trigger takes no other field. The key is type, not on: YAML 1.1 reads a bare on as the boolean true, and the loader's decoder follows it. One of `mention`, `botMessage`. |
| `from` | [BotSource](#botsource) | No | None | From names the sender a botMessage trigger accepts. Required for botMessage. |
| `match` | map[string]string | No | None | Match maps a message field to a regex it must match. Every entry must match. Keys: `text`, `title`, `link`, `fallback`, `footer`, `body`. |
| `exclude` | map[string]string | No | None | Exclude maps a message field to a regex; a message matching any entry is ignored. Keys: `text`, `title`, `link`, `fallback`, `footer`, `body`. |
| `extract` | map[string][Extract](#extract) | No | None | Extract names the values to pull out of a message. A message in which one of these regexes does not match does not match the trigger. |
| `thread` | [TriggerThread](#triggerthread) | No | None | Thread ties messages that share an extracted key to one thread. |
| `promptTemplate` | string | No | None | PromptTemplate is a Go template over the trigger's view of the message (.Channel, .State, .Key, .Fields, .Link, .Source). Required for botMessage. |
| `limits` | [TriggerLimits](#triggerlimits) | No | None | Limits bounds how many investigations the trigger starts (rate, concurrency and a daily cap). Required for botMessage: both rateLimit and maxRunsPerDay. |

## BotSource

BotSource identifies the sender whose messages a trigger accepts: a message matches only when its sender's ID and kind both equal ID and Kind.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `id` | string | Yes | None | ID is the sender's ID on the platform, never a display name (Slack: the bot ID, `B…`). Agent Kourier's own ID is refused at runtime: the load cannot know it. Pattern `^B[A-Z0-9]+$`. |
| `kind` | string | No | `bot` | Kind is what kind of sender ID names. Only bot exists today, and it is the default. One of `bot`. |

## Extract

Extract pulls one named value out of a message field. The entry's name in Trigger.Extract must also be the name of a named group in Regex, which supplies the value.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `field` | string | Yes | None | Field is the message field the regex runs on. One of `text`, `title`, `link`, `fallback`, `footer`, `body`. |
| `regex` | string | Yes | None | Regex is a Go (RE2) regular expression of at most 512 bytes. |

## TriggerThread

TriggerThread decides which thread a message joins and when it closes the thread.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `by` | string | No | None | By is key: messages with the same extracted "key" share a thread. One of `key`. |
| `cooldown` | duration | No | None | Cooldown is how long after a turn the same key starts no new turn. Required with by, and between 1m and 168h. |
| `maxIdle` | duration | No | Larger of `24h` and twice the cooldown | MaxIdle is how long an open thread may go without a message of its key before Agent Kourier counts it abandoned, so that the key's next fire starts a new investigation instead of a note. It covers a resolve that never arrived. It needs by, is at least twice the cooldown, and is at most 720h; unset means the larger of 24h and twice the cooldown. It must be longer than the sender's repeat interval (Alertmanager's repeat_interval): an alert that keeps firing repeats at that interval, and a maxIdle shorter than it counts a live alert abandoned between two repeats and starts a second investigation for it. Twice the cooldown covers that only for a 4h cooldown against Alertmanager's 4h repeat_interval; a longer repeat_interval needs a maxIdle set above it. |
| `closeWhen` | map[string]string | No | None | CloseWhen maps an extracted field to the value that closes the thread: Agent Kourier posts a note and starts no turn. Every entry must equal. |

## TriggerLimits

TriggerLimits bounds how many investigations a botMessage trigger starts: a rate limit over a window and a cap for a day. A botMessage trigger sets both. Only a start is limited: a repeat, a close and a note never are.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `rateLimit` | [RateLimit](#ratelimit) | No | None | RateLimit allows MaxRuns starts in any window of Per, and MaxConcurrent investigations running at once. Required for a botMessage trigger, with all three fields set. |
| `maxRunsPerDay` | integer | No | None | MaxRunsPerDay caps starts in any 24 hours. Required for a botMessage trigger: an explicit 0 means no cap. Minimum 0. |

## RateLimit

RateLimit allows MaxRuns agent runs per window and MaxConcurrent at once.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `maxRuns` | integer | Yes | None | Minimum 1. |
| `per` | duration | Yes | None | Per is the window length. |
| `maxConcurrent` | integer | Yes | None | Minimum 1. |

## Interactions

Interactions configures agent pauses that wait on a person.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `askUser` | boolean | No | `false` | AskUser lets the agent ask questions in the thread. Defaults to false. |
| `toolApprovals` | boolean | No | `false` | ToolApprovals lets the agent ask a person to approve tool calls in the thread. Defaults to false. |
| `approverGroups` | []string | No | None | ApproverGroups names the platform's user groups whose members may decide a tool approval, by the platform's ID (Slack: `S…`). An empty list allows any non-bot channel member. At most 10 groups may be named. Group IDs start with S and contain uppercase letters and digits; duplicates and empty entries are refused. |
| `timeout` | duration | No | `30m` | Timeout is how long a pause waits for an answer. Defaults to 30m. |

## Feedback

Feedback configures the useful and not-useful buttons. The final message of an investigation that a trigger started always carries them; Enabled adds them to the Binding's chat answers too.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `enabled` | boolean | No | `false` | Enabled puts the buttons on the final message of an answer to a person's message in a thread. It does not switch them off for an investigation a trigger started. Defaults to false. |

## Session

Session configures thread-to-session mapping.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `threadTTL` | duration | No | `2160h` | ThreadTTL is how long a thread keeps its session after its last activity. Defaults to 2160h (90 days). |

---

Generated by `hack/docs/refgen` from `api/v1alpha1`. Do not edit this page; change the source and run `make docs-ref`.

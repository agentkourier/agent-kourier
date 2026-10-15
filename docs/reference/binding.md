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
| `channel` | string | Yes | None | Channel is the channel ID, not its name. Pattern `^[CG][A-Z0-9]+$`. |
| `output` | string | No | `live` | Output defaults to live. One of `live`, `final`. |
| `threadReplies` | string | No | `all` | ThreadReplies defaults to all: any person's reply in a thread Agent Kourier owns is a turn. With mention, only a reply that mentions Agent Kourier is a turn, whether a mention or a trigger started the thread; the next mentioned turn carries the other replies as context. A typed answer to a pending question that takes text replies needs no mention. One of `all`, `mention`. |
| `acknowledgment` | [ChatAcknowledgment](#chatacknowledgment) | No | None | Acknowledgment controls the temporary reaction Agent Kourier adds to an accepted message while it is being processed. It defaults to enabled with the hourglass_flowing_sand reaction. |
| `triggers` | [][Trigger](#trigger) | No | `[{type: mention}]` | Triggers decide which events in the channel start or continue a turn. Empty means [{type: mention}]: a mention starts a session. A list that names triggers is the whole list. |

## ChatAcknowledgment

ChatAcknowledgment configures the temporary reaction on an accepted message.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `enabled` | boolean | No | `true` | Enabled adds the reaction after the message is durably accepted and removes it when processing ends. It defaults to true. |
| `emoji` | string | No | `hourglass_flowing_sand` | Emoji is the Slack reaction name without colons. It defaults to hourglass_flowing_sand. Pattern `^(?:[a-z0-9][a-z0-9+_-]*|[+-][a-z0-9][a-z0-9+_-]*)$`. |

## Trigger

Trigger is one way an event in the bound channel starts or continues a turn. Triggers are tried in order and the first that matches handles the event.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `type` | string | Yes | None | Type is mention, a person @mentioning Agent Kourier, or botMessage, a message a bot posts. A mention trigger takes no other field. The key is type, not on: YAML 1.1 reads a bare on as the boolean true, and the loader's decoder follows it. One of `mention`, `botMessage`. |
| `from` | [BotSource](#botsource) | No | None | From names the bot a botMessage trigger accepts. Required for botMessage. |
| `preset` | string | No | None | Preset fills in match, extract, thread, limits and promptTemplate for a known sender. What the Binding sets itself overrides the preset field by field: a match, exclude or extract entry replaces the preset's entry of that name, and each part of thread and limits replaces the preset's. One of `alertmanager`. |
| `match` | map[string]string | No | From preset, if set | Match maps a message field to a regex it must match. Every entry must match. Keys: `text`, `title`, `titleLink`, `fallback`, `footer`, `attachmentText`. |
| `exclude` | map[string]string | No | From preset, if set | Exclude maps a message field to a regex; a message matching any entry is ignored. Keys: `text`, `title`, `titleLink`, `fallback`, `footer`, `attachmentText`. |
| `extract` | map[string][Extract](#extract) | No | From preset, if set | Extract names the values to pull out of a message. A message in which one of these regexes does not match does not match the trigger. |
| `thread` | [TriggerThread](#triggerthread) | No | From preset, if set | Thread ties messages that share an extracted key to one thread. |
| `promptTemplate` | string | No | From preset, if set | PromptTemplate is a Go template over the trigger's view of the message (.Channel, .State, .Key, .Fields, .Link, .Source). Required for botMessage, directly or from the preset. |
| `limits` | [TriggerLimits](#triggerlimits) | No | From preset, if set | Limits bounds how many investigations the trigger starts (rate, concurrency and a daily cap). What the Binding leaves out comes from the preset; a trigger with no preset and no limits is unlimited. |

## BotSource

BotSource identifies the bot whose messages a trigger accepts.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `botId` | string | Yes | None | BotID is the Slack bot ID, never a display name. Agent Kourier's own bot ID is refused at runtime: the load cannot know it. Pattern `^B[A-Z0-9]+$`. |

## Extract

Extract pulls one named value out of a message field. The entry's name in Trigger.Extract must also be the name of a named group in Regex, which supplies the value.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `field` | string | Yes | None | Field is the message field the regex runs on. One of `text`, `title`, `titleLink`, `fallback`, `footer`, `attachmentText`. |
| `regex` | string | Yes | None | Regex is a Go (RE2) regular expression of at most 512 bytes. |

## TriggerThread

TriggerThread decides which thread a message joins and when it closes the thread.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `by` | string | No | From preset, if set | By is key: messages with the same extracted "key" share a thread. One of `key`. |
| `cooldown` | duration | No | From preset, if set | Cooldown is how long after a turn the same key starts no new turn. Required with by, and between 1m and 168h. |
| `maxIdle` | duration | No | Larger of `24h` and twice the cooldown | MaxIdle is how long an open thread may go without a message of its key before Agent Kourier counts it abandoned, so that the key's next fire starts a new investigation instead of a note. It covers a resolve that never arrived. It needs by, is at least twice the cooldown, and is at most 720h; unset means the larger of 24h and twice the cooldown. It must be longer than the sender's repeat interval (Alertmanager's repeat_interval): an alert that keeps firing repeats at that interval, and a maxIdle shorter than it counts a live alert abandoned between two repeats and starts a second investigation for it. Twice the cooldown covers that only for the preset's 4h cooldown against Alertmanager's 4h repeat_interval; a longer repeat_interval needs a maxIdle set above it. |
| `closeWhen` | map[string]string | No | From preset, if set | CloseWhen maps an extracted field to the value that closes the thread: Agent Kourier posts a note and starts no turn. Every entry must equal. |

## TriggerLimits

TriggerLimits bounds how many investigations a botMessage trigger starts: a rate limit over a window and a cap for a day. The preset supplies the defaults, so a Binding sets only what it changes. Only a start is limited: a repeat, a close and a note never are.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `rateLimit` | [RateLimit](#ratelimit) | No | From preset, otherwise unlimited | RateLimit allows MaxRuns starts in any window of Per, and MaxConcurrent investigations running at once. What the Binding leaves out of it comes from the preset, field by field. |
| `maxRunsPerDay` | integer | No | From preset, otherwise no cap | MaxRunsPerDay caps starts in any 24 hours. An explicit 0 means no cap; leaving it out takes the preset's cap. Minimum 0. |

## RateLimit

RateLimit allows MaxRuns agent runs per window and MaxConcurrent at once.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `maxRuns` | integer | Yes | From preset, if set | Minimum 1. |
| `per` | duration | Yes | From preset, if set | Per is the window length. |
| `maxConcurrent` | integer | Yes | From preset, if set | Minimum 1. |

## Interactions

Interactions configures agent pauses that wait on a person.

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `askUser` | boolean | No | `false` | AskUser lets the agent ask questions in the thread. Defaults to false. |
| `toolApprovals` | boolean | No | `false` | ToolApprovals lets the agent ask a person to approve tool calls in the thread. Defaults to false. |
| `approverGroups` | []string | No | None | ApproverGroups names the Slack user groups whose members may decide a tool approval. An empty list allows any non-bot channel member. At most 10 groups may be named. Group IDs start with S and contain uppercase letters and digits; duplicates and empty entries are refused. |
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

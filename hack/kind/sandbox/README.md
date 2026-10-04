# The sandbox tier: courier against a real Slack and a real model

Everything else in `hack/kind/` proves courier against our own doubles: the fake Slack, mockllm, the stub agent. The
sandbox tier lets people try the chat path for real, before the Phase 1 sign-off: courier on its **production image**
in a **real Slack sandbox workspace** (Socket Mode), answered by `sandbox-sre`, a **read-only SRE agent** on a **real
model**, investigating **faults seeded** into the kind cluster.

It is an overlay on the full tier (`kind-courier-full`), never a new cluster: only one full tier fits a Docker host.

```
Slack sandbox workspace  <-- wss/https -->  courier-sandbox (courier:dev, production image)
                                                  |  the Binding's Dex ID token
                                                  v
                              kagent-frontdoor:4180 (verifies the token; the full tier's own)
                                                  v
                  kagent  ->  sandbox-sre (Substrate actor, gVisor)  -- egress gateway -->  api.anthropic.com | api.openai.com | bedrock-runtime.<region>.amazonaws.com
                                                  |  MCP
                                                  v
                  sandbox-tools (kagent's tool server, --read-only, ServiceAccount bound to `view`)  ->  namespace shop (and the cluster, read-only)
```

## Make targets

| Target | What it does |
|---|---|
| `make sandbox-up` | Checks the secrets file, and the alert webhook file if there is one (optional; see "Alert trial"), first, before it touches anything. Brings the full tier up if it is not (`make e2e-full-up`), builds and loads `courier:dev`, then applies the overlay and verifies it. Safe to re-run. Exit 0: up, courier connected to Slack. Exit 1: a refused file or a failure. **Exit 3: deployed, but Slack rejected the tokens** (everything else is up and verified; courier crash-loops until the file is fixed). |
| `make sandbox-down` | Removes the overlay, takes the Alertmanager off the alert source, and restores the full tier's own courier (the testhooks image on the fake Slack), so `make e2e-full` works again. The cluster stays. |
| `make sandbox-alert ALERT=<name> [STATE=firing\|resolved]` | Fires, or resolves, the alert of one seeded fault in the full tier's Alertmanager, which posts it to the owner's **real alert channel**, tagged `[kind]`. Needs `make sandbox-up` first. See "Alert trial". |
| `make sandbox-heal` | Restarts the full tier's **egress gateway** (`deploy/atenet-egress`) and waits for it, if its pod is more than 20 hours old or a log holds `CertificateExpired`; otherwise it says `ok` and changes nothing. For a **long session**: the gateway's certificate lives 24 hours and is never reloaded, so after about a day every agent turn fails with "not ready" (see "If it does not come up"). `make sandbox-up` does the same before it verifies. Changes nothing else; needs no secrets file. |

While the sandbox is up, `make e2e-full` and `make e2e-full-up` **refuse**
(`Run: make sandbox-down first`): the sandbox's pod carries the label the front door trusts, which the tier's own courier Deployment and
its Service also select, so a test run would drive the sandbox's courier. `kubectl logs deploy/courier` is the tier's (scaled to zero);
use `deploy/courier-sandbox`.

`make` shows all four in its help. The secrets file is `$SANDBOX_ENV`, default `~/.config/agent-courier/sandbox.env`:
`SANDBOX_ENV=/path/to/file make sandbox-up`. `hack/kind/sandbox/check-env.sh [FILE]` checks a file and nothing else.

## Setup (once)

1. **Create the Slack app.** Open <https://api.slack.com/apps>, Create New App, From a manifest, pick the sandbox
   workspace, and paste `hack/kind/sandbox/slack-app-manifest.yaml` (YAML tab). It asks for Socket Mode, interactivity, the
   `app_mention`, `message.channels` and `message.groups` events and the bot scopes below. **Use the sandbox workspace only.**
2. **Make an app-level token.** Basic Information, App-Level Tokens, Generate Token, with the scope
   `connections:write`. That is `SLACK_APP_TOKEN` (`xapp-...`). The manifest cannot make it.
3. **Install the app** to the sandbox workspace (Install App). The Bot User OAuth Token is `SLACK_BOT_TOKEN` (`xoxb-...`).
4. **Invite the bot** to a channel: `/invite @courier` in it. The channel's ID (Channel details, at the bottom; `C...`,
   or `G...` for a private channel) is `SLACK_CHANNEL_ID`. Courier answers in this channel only.
5. **Fill in the secrets file**, outside the repository:
   ```
   mkdir -p ~/.config/agent-courier
   cp hack/kind/sandbox/sandbox.env.example ~/.config/agent-courier/sandbox.env
   chmod 600 ~/.config/agent-courier/sandbox.env
   $EDITOR ~/.config/agent-courier/sandbox.env      # all six keys; MODEL_PROVIDER is anthropic, openai or bedrock (plus MODEL_REGION)
   hack/kind/sandbox/check-env.sh                   # says "ok" and names the keys, never a value
   ```
6. `make sandbox-up`, then wait for `ok: courier is connected to Slack`. That is courier's own word for it, the log line
   `slack: socket mode connected`: a Ready pod is not proof, because courier is Ready before it tries Slack.

The file is **parsed, never sourced**, and is refused if it is missing, a symbolic link, unreadable, not yours, readable or writable by
anyone else, if a key is missing, repeated, malformed or still a `REPLACE` placeholder. The error names the key and never
the value. The values go into Secret manifests on stdin to `kubectl apply -f -` (server-side, so no last-applied copy
is kept): on no command line, in no file, in no output.

### The model: `MODEL_PROVIDER`

| Provider | `MODEL_NAME` | `MODEL_API_KEY` | Also |
|---|---|---|---|
| `anthropic` | an Anthropic model name | an Anthropic API key, `sk-ant-...` | |
| `openai` | an OpenAI model name | an OpenAI API key, `sk-...` (not `sk-ant-`, which is Anthropic's) | |
| `bedrock` | a Bedrock model ID or **inference profile ID**, for example `us.anthropic.claude-haiku-4-5-20251001-v1:0` (also `global.anthropic.…` or a bare `anthropic.claude-…-v1:0`), not an ARN | a **Bedrock API key** (used as a bearer token), not AWS access keys: `ABSK...` for a long-term key, `bedrock-api-key-...` for a short-term one (12 hours at most, so it will go stale in a long session) | `MODEL_REGION`, required for bedrock and refused for the others: `^[a-z]{2}(-gov)?-[a-z]+-[0-9]$`, for example `us-east-1` |

The check refuses a key that is not the provider's kind, naming the key and the provider and never the value: `anthropic` needs `sk-ant-...`, `openai`
needs `sk-...` and not `sk-ant-`, and `bedrock` needs `ABSK...` or `bedrock-api-key-...`. A key goes to its provider's host through the egress gateway, so
a Bedrock key under `anthropic` would be sent to `api.anthropic.com`, for nothing, where it is seen. A key that is plainly another provider's says whose it
looks like: a Bedrock key under `anthropic` or `openai` ("looks like a Bedrock API key; MODEL_PROVIDER=anthropic cannot use it; set MODEL_PROVIDER=bedrock and
MODEL_REGION"), or an Anthropic or OpenAI key under `bedrock`.
(`bedrock-api-key-` is in the AWS token generator's README; `ABSK` is what a long-term key was seen to start with, and the AWS pages do not say.)

How Bedrock works here, from kagent 1.0.0-alpha5's source and CRDs, and what the first real Bedrock turn showed:
- **Auth.** kagent's Bedrock provider takes either AWS access keys (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`) or a bearer token, and it reads
  them from **fixed key names** in the Secret the ModelConfig's `apiKeySecret` names; the ModelConfig's `apiKeySecretKey` is exempt for Bedrock and is
  left out. A Bedrock API key goes in under **`AWS_BEARER_TOKEN_BEDROCK`**, which `make sandbox-up` does from `MODEL_API_KEY`. IRSA is not used.
- **Egress, and a workaround.** kagent 1.0.0-alpha5's Go ADK harness has a gap: the destination allowlist (`agentConfigDestinations`) has a case for
  OpenAI, Anthropic, Gemini and Ollama and none for Bedrock, while the Secret's bearer key makes kagent bind `Authorization: Bearer <key>` to
  `bedrock-runtime.<region>.amazonaws.com`, and kagent's Substrate policy builder refuses a binding for a host that is not also an allowed destination
  (`policy.go:32-34`). Without more, every Bedrock session fails in preparation. So `modelconfig-bedrock.yaml` carries one `defaultHeaders` entry,
  `X-Courier-Egress-Host: https://bedrock-runtime.<region>.amazonaws.com`: the allowlist also takes every URL in the serialised model, headers included, so
  the host becomes a destination and the binding is accepted. This is a **kind-only workaround**; the upstream fix is a Bedrock case in
  `agentConfigDestinations`, as `translator/claude/compiler.go:257` has for Claude on Bedrock, and the header goes with it. The header is also sent to
  Bedrock on every request (the ADK's `headerTransport`). The key itself is never in the sandbox: the SDK holds a placeholder
  (`kagent-credential-injected`) and the gateway swaps in the key.
  All of this was read from the source, and `TestBedrockModelConfigPutsItsEndpointInTheModelJSON` pins the header, its rendered host, and a copy of the walk that finds it.
- **Confirmed in dogfood 2026-10-03**, in a real Bedrock turn through the workaround (read from the egress gateway's log: `kubectl --context kind-courier-full -n ate-system logs deploy/atenet-egress -c agentgateway`):
  the gateway reached **exactly `bedrock-runtime.us-east-1.amazonaws.com`**, with the key injected; **no other AWS host** was reached; and Bedrock **ignored the extra
  `X-Courier-Egress-Host` header** (no request was rejected for it).
- UNCONFIRMED, still (the ate-api-server log's `GetActorEgressPolicy` for the actor shows the first):
  - that the policy lists that host both as an allowed destination and as the credential rule's host;
  - that kagent's Go SDK accepts the placeholder in `AWS_BEARER_TOKEN_BEDROCK` and sends it as `Authorization: Bearer`, so the gateway has a header to replace.

  If a turn fails, look at the gateway's log for a denied host and at the agent's error in the thread.

### The scopes, against what courier calls

| Call (`internal/chat/slack`) | Scope in the manifest |
|---|---|
| `auth.test` | none needed |
| `users.info` (and the email for the audit log) | `users:read`, `users:read.email` |
| `chat.postMessage`, `chat.update`, `chat.postEphemeral` | `chat:write` |
| `chat.startStream`, `chat.appendStream`, `chat.stopStream` | `chat:write` (Slack's method pages name no other scope) |
| `reactions.add`, `reactions.remove` | `reactions:write` |
| `conversations.history`, `conversations.replies` | `channels:history`, `groups:history` |
| `conversations.members` | `channels:read`, `groups:read` |
| events `app_mention`, `message.channels`, `message.groups` | `app_mentions:read`, `channels:history`, `groups:history` |
| Socket Mode | the app-level token, `connections:write` |

The manifest covers every call. **Confirmed in dogfood 2026-10-03:** `chat.stopStream` appends the stop blocks courier sends
(WP-39c), and chunk-mode streams work: the `task_update` cards of tool calls are placed in the text where the calls were made,
and a finished step shows status complete (WP-39e). Still UNCONFIRMED(WP-04): that the streaming methods need no app feature
beyond `chat:write` (the "Agents & AI Apps" feature and `assistant:write` are named on none of the three method pages, and
nothing here enables them); that `chat.startStream` accepts the channel form courier sends (`recipient_user_id` and
`recipient_team_id` are documented as required in channels, and courier sends both); and that `chat.stopStream` takes the
metadata courier sends (its page lists `blocks` and `metadata`). If `chat.startStream` fails with `missing_scope` or a feature
error, that is the finding: note the error code in the checklist's streaming row and add the scope or feature to the
manifest. Direct messages are not enabled (no `im:` scopes or events), so the app has no DM trigger.

## What `make sandbox-up` makes

| Piece | What |
|---|---|
| Faults | Namespace `shop`: six small Deployments (below). |
| Tool server | A second Helm release, `sandbox-tools` (kagent-tools 0.3.0, pinned, image by digest), in `kagent`. The full tier's own kagent release keeps `kagent-tools: enabled: false` and is not touched. |
| Agent | `sandbox-sre` (`agent.yaml`), a `ModelConfig` for `MODEL_PROVIDER`, the API key in the Secret `sandbox-model`. |
| Courier | `courier-sandbox` in namespace `courier`, the production image `courier:dev`, a ChatConnection on Socket Mode (Secret `sandbox-slack`), one Binding: `SLACK_CHANNEL_ID` to `sandbox-sre`, output live, `askUser` on, through the full tier's AgentBackend (the verifying front door, port 4180). Its pod carries the `app.kubernetes.io/name=courier` label the front door's NetworkPolicy trusts. Its own SQLite volume. The full tier's `courier` goes to zero replicas meanwhile. |
| Token refresh | A CronJob, `courier-token-refresh`, every hour (below). |

**The seeded faults**, in `shop`: `checkout` crash-loops after logging `FATAL: cannot resolve payments-db...`; `catalog` has a
bad image tag (ImagePullBackOff); `recommender` is OOM killed at its 48Mi limit; `inventory`'s Service selects pods that
do not exist (no endpoints; its pods are fine); `search` runs but its readiness probe fails (404); `frontend` is healthy,
for contrast. Nothing in the labels or annotations says which is which.

**Read-only, three ways.** The tools' ServiceAccount is bound to the built-in `view` ClusterRole (no Secrets, no writes); the
server runs with `--read-only`, which does not register its write tools; and the agent lists only read tools by name
(`k8s_get_resources`, `k8s_describe_resource`, `k8s_get_pod_logs`, `k8s_get_events`, `k8s_get_resource_yaml`,
`k8s_get_available_api_resources`). `make sandbox-up` proves the first with `kubectl auth can-i --as=system:serviceaccount:kagent:sandbox-tools`:
create, delete, patch and exec are "no", and so is get on Secrets. Note that `view` reads the whole cluster, pod logs
included, so do not point the agent at a cluster that has anything secret in its logs.

**Egress, nothing to open.** Courier's pod has no egress policy in this tier (the only NetworkPolicies are the three that fence kagent's
ingress, and Substrate's own), so it already reaches `slack.com` and `wss-primary.slack.com` over https and wss. The agent's
pod leaves through Substrate's egress gateway, whose per-actor policy kagent infers from the ModelConfig: for `sandbox-sre`
it is exactly `api.anthropic.com` (or `api.openai.com`), `kagent-controller.kagent` and `sandbox-tools.kagent.svc.cluster.local`
(read from the Substrate API's `GetActorEgressPolicy` response). The gateway also **injects the API key** into the
request from the Secret `sandbox-model`, so the key never enters the agent's sandbox. Nothing was added to any policy.
For `bedrock` the host is `bedrock-runtime.<MODEL_REGION>.amazonaws.com`, but kagent does not infer it: the ModelConfig carries a header that
puts it in the allowlist (see "The model", above). That was read from kagent's source, and the Bedrock turn of dogfood 2026-10-03 confirmed the host it reaches.

**The Binding's token.** The Binding identity's Dex ID token (the Secret `courier-agent-token`) lasts 24 hours and courier reads
it on every request. A CronJob in `courier` (hourly, `0 * * * *`) mints one and patches it in, from inside the cluster, with a ServiceAccount whose
only right is `patch` on that one Secret. `make sandbox-up` makes the first token by running that same CronJob as a Job, so the one
path that keeps the token fresh is proved at bring-up. It was chosen over a host loop because it needs no terminal that survives
sleep, and over lib.sh's `refresh_dex_token` run by hand because that is a manual step in a multi-hour session.
A sleeping laptop can still swallow runs: one that wakes late and then hits its 180 s deadline is over for good (the deadline counts wall time), and a
run missed asleep starts on wake only if that is within the hour. Hourly keeps the token fresh through any ordinary sleep, but after a closed-lid
stretch of a day or more the token can have expired (see the first row of "If it does not come up").
To force one: `kubectl --context kind-courier-full -n courier create job --from=cronjob/courier-token-refresh refresh-now`.

## The dogfood checklist

Courier answers in the one channel. Watch it while you try each row (where to look is below).

| # | Try | Look for |
|---|---|---|
| 1 | `@courier why is checkout crashlooping?` | A thread opens under your message. The reply **streams** (it grows as the model writes) and shows **tool steps** as cards (`k8s_get_pod_logs ...`, one per call, placed in the text where the call was made, status complete once it is done; confirmed in dogfood 2026-10-03). It quotes the `FATAL: cannot resolve payments-db` line, says what it checked, and changes nothing. If it streams in one piece, or fails at `chat.startStream`, that is a WP-04 finding. |
| 2 | Other questions: `why is catalog stuck?`, `is recommender healthy?`, `why does inventory have no endpoints?`, `what is wrong with search?`, `is frontend ok?` | Each finds its fault: ImagePullBackOff and the missing tag; OOMKilled and the 48Mi limit; the Service's selector against the pods' labels; the readiness probe's 404; and `frontend` fine, with how it knows. |
| 3 | **A thread reply**, in the same thread, after the answer: `and which pods does that affect?` | The answer comes in the same thread, and knows the earlier turn (same session). No new thread. |
| 4 | **A reply while a turn is running**: send a long question (`check every deployment in shop and tell me about each`), and answer or add to it before it finishes | Courier queues it: a notice says it is queued, and the answer follows when the first turn ends, in order. Never two answers at once, never a lost message. |
| 5 | **The agent asks the user a question**: `ask me which app to look at, then look at it` (the agent's `ask_user` tool) | A prompt with the question and its choices, as input blocks, and a **Submit** button. Submitting posts the answer back and the agent carries on. Only a channel member can submit, the first press wins, and an incomplete answer gets an ephemeral "answer every question", not a changed prompt. Leave one unanswered: after 30 minutes it times out and says so. If the model will not call `ask_user`, say so here: that is the model, not courier. |
| 6 | **Inert text, which gates the pilot (WP-04 item (b))**: ask the agent to quote the probe text below, one item per line | Every one stays **inert**: **no** `@channel` or `@here` notification (check with a second account, or the sandbox's notification log), no mention chip, no link rendered, no channel link. The text shows as typed (or escaped) and notifies nobody. Try it three ways: in the streamed reply, in a tool line (ask it to look up a resource named `<!channel>`: the call fails, and the tool line shows the name), and in the Submit prompt (ask it to ask you a question whose text and choices hold `<!channel>`). Also in inline code and a code block. A single notification or live link is a pilot-blocker: record where it came from. |
| 7 | **Delete the courier pod mid-turn**: send the long question, and once it is streaming, `kubectl --context kind-courier-full -n courier delete pod -l app.kubernetes.io/instance=sandbox` | A new pod starts (the volume survives). Courier **recovers** the turn from its outbox: the thread is completed or ends with an honest notice, and **there is no double post**: no duplicate of a message that was already sent, and no orphaned half-streamed message. Read the new pod's log for the `recover:` lines. |
| 8 | **Long output**: `list every resource in shop with its full YAML, then describe each pod` | The answer is split or cut cleanly, within Slack's limits, with no truncated markdown or code fence, no error from Slack in the log, and nothing lost without a notice. |
| 9 | Refusals: `delete the checkout deployment`, `scale checkout to 3`, `show me the Secrets in courier` | The agent says it cannot, says what a person would run, and nothing in `shop` changes. |

The probe text for row 6 (paste it after `quote exactly, one item per line:`):

```
<!channel>
<!here>
<@U0123ABCD>
<https://example.com|click me>
<#C0123ABCD>
#general
`<!channel>`
```

## Alert trial

**Warning: this posts to the owner's real alert channel.** The alert source is optional: it exists only if the webhook file below is there. When it is,
`make sandbox-up` makes the full tier's Alertmanager post, through the owner's own incoming webhook, to `#alerts-critical`, the channel where production's alerts also arrive. Nothing is posted until
someone fires an alert (there is no Prometheus in the tier to do it), and everything the tier posts is tagged: the title begins `[kind] `, and the
link under it says `receiver=slack-critical-kind` where production's says `receiver=slack-critical`. Fire alerts only for a trial, and
resolve them when it is done.

**What it is.** Alertmanager (`monitoring/courier`, run by prometheus-operator, pod `alertmanager-courier-0`) gets a second receiver,
`slack-critical-kind`: a copy of the owner's `slack-critical` receiver (`cluster-apps`, `templates/prometheus-operator.yaml`) with the
`[kind] ` prefix on the title and nothing else changed (same text template, `send_resolved: true`, `group_by [alertname, namespace]`, `repeat_interval 4h`).
A test compares the two. One thing differs by design, outside the receiver: `group_interval` is 30s, not production's 5m, so that a resolve shows
in the channel in under a minute and not in up to five. Its `externalUrl` is `http://alertmanager.kind.local`, which no one mistakes for production's, and it is what
Alertmanager puts in every `title_link`.

The configuration is the Alertmanager's base configuration (`alertmanager/config.yaml`), not an `AlertmanagerConfig` resource, for two reasons: that
resource cannot say `api_url_file`, which is how production reads its webhook, and the operator adds a `namespace="monitoring"` matcher to a
resource's routes (unless the Alertmanager says `alertmanagerConfigMatcherStrategy: None`, as the tier's does for its sink), which would turn
away alerts for namespace `shop`. The route matches `severity="critical"`, as production's does, and continues, so the sink's own route, which the operator puts first, still gets every alert.

**Setup (once, optional).** Without the file, `make sandbox-up` skips the alert source and says `alert source: not configured; see README Alert trial`,
and `make sandbox-alert` refuses and says the same. (Taking the file away later does not remove an alert source that is already up: `make sandbox-down` does.) The webhook URL goes in `~/.config/agent-courier/alertmanager-webhook-url`, on one line, alone:

```
mkdir -p ~/.config/agent-courier
$EDITOR ~/.config/agent-courier/alertmanager-webhook-url   # https://hooks.slack.com/services/T.../B.../...
chmod 600 ~/.config/agent-courier/alertmanager-webhook-url
```

A file that is there is checked right after the secrets file, before `make sandbox-up` touches anything, and **stops the run** (it is not skipped) if it is a symbolic link, not yours, accessible to the group or others, empty,
longer than one line, or not matching `^https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+$`. The error names the file and never the value.
`ALERTMANAGER_WEBHOOK_FILE=/path make sandbox-up` reads another file. The URL goes into the Secret `alertmanager-slack-secret` in `monitoring`
(on stdin to a server-side apply, like the other Secrets), which the Alertmanager mounts; the configuration reads it with `api_url_file`, as production does. It is in no
manifest, on no command line and in no output. `make sandbox-down` removes the Secret and the receiver.

**Fire and resolve** an alert, once the sandbox is up (each prints `sent: <alertname> <state>` and nothing else of the alert):

```
make sandbox-alert ALERT=KubePodCrashLooping                  # fires it (STATE=firing is the default)
make sandbox-alert ALERT=KubePodCrashLooping STATE=resolved   # the same alert, ended
```

| `ALERT` | The seeded fault it is about | Labels, besides `severity=critical` and `namespace=shop` |
|---|---|---|
| `KubePodCrashLooping` | `checkout`, which crash-loops | `pod` (the live pod's name), `container=checkout` |
| `KubePodNotReady` | `search`, running and never Ready | `pod` |
| `KubeContainerWaiting` | `catalog`, ImagePullBackOff | `pod`, `container=catalog`, `reason=ImagePullBackOff` |
| `KubeContainerOOMKilled` | `recommender`, OOM killed | `pod`, `container=recommender`, `reason=OOMKilled` |
| `TargetDown` | `inventory`, whose Service has no endpoints | `job=inventory`, `service=inventory` |

Only these five can be fired. The alert is posted to Alertmanager's `/api/v2/alerts` from a probe pod in the cluster, with `summary` and `description`
annotations like kube-prometheus's and the pod's real name, so what the agent finds with its read-only tools is true. How it plays out:
- Alertmanager groups it for 30 seconds, then posts `[kind] FIRING (1) - KubePodCrashLooping` to the channel.
- A fired alert ends by itself after an hour, which posts `[kind] RESOLVED - ...`; `STATE=resolved` ends it at once, and the post follows within
  30 seconds. Resolve before the pod is replaced: the resolve must carry the firing alert's labels, the pod's name included.
- Firing an alert that is already firing posts nothing: its next repeat is 4 hours on. To see the alert again, resolve it, wait for the resolved
  post, and fire it again. A second alert of the same name and namespace joins the group and is posted as an update.

**What courier should do.** The trial trigger is a `botMessage` trigger on the incoming webhook's bot ID (the same bot posts production's alerts, Grafana's and the heartbeat),
matching only `receiver=slack-critical-kind` in the title link: production's messages (`receiver=slack-critical`), the heartbeat and Grafana's must stay ignored. For a fired alert
courier starts an investigation in the alert's thread, and `sandbox-sre` answers there from the fault; the same alert again within the cooldown attaches to that thread; the resolved
post gets a note and starts no turn; people follow up in the thread without a mention. What the message looks like, for the trigger's regexes (the attachment's fields; the title
and its link are Alertmanager's, the fallback is its default):

```
title      [kind] FIRING (1) - KubePodCrashLooping    (the state is not at the start: allow the prefix, for example ^(\[kind\] )?(?P<state>FIRING|RESOLVED))
title_link http://alertmanager.kind.local/#/alerts?receiver=slack-critical-kind
fallback   [FIRING:1] KubePodCrashLooping shop (checkout-... critical) | http://alertmanager.kind.local/#/alerts?receiver=slack-critical-kind
text       *KubePodCrashLooping* - critical  Pod is crash looping.  Pod shop/checkout-... (checkout) is in waiting state (reason: "CrashLoopBackOff").
```

(Read off Alertmanager v0.34.0, run locally on this configuration against a stand-in webhook, never the real one.) The receiver's name is in the title link, and so in the fallback, which ends with that
link, and in no other field a trigger can read: match `titleLink` on `receiver=slack-critical-kind`.

**Check Alertmanager's own status:**

```
CTX=kind-courier-full
kubectl --context $CTX -n monitoring get alertmanager courier -o jsonpath='{.spec.configSecret} {.spec.externalUrl}{"\n"}'   # alertmanager-kind-config http://alertmanager.kind.local
kubectl --context $CTX -n monitoring exec alertmanager-courier-0 -c alertmanager -- wget -q -O - http://localhost:9093/api/v2/status | jq -r .config.original   # the receiver and its api_url_file path; never the URL
kubectl --context $CTX -n monitoring exec alertmanager-courier-0 -c alertmanager -- wget -q -O - http://localhost:9093/api/v2/alerts | jq -c '.[] | {alertname: .labels.alertname, state: .status.state, receivers: [.receivers[].name]}'
kubectl --context $CTX -n monitoring logs alertmanager-courier-0 -c alertmanager | grep -i -e notify -e slack   # a failed post says why; Alertmanager takes the URL out of its errors
```

A routed alert lists the receivers `monitoring/sink/sink` and `slack-critical-kind`. If nothing reaches Slack: the alert is not `severity=critical` (only the presets are fired, and they all are), the
webhook was revoked (Slack answers an error, the log says so; put a new URL in the file and re-run `make sandbox-up`, which rewrites the Secret, and the next post uses it), or the pod has
not come back after a `make sandbox-up` (`kubectl --context $CTX -n monitoring get pods`).

## Where to look

```
CTX=kind-courier-full
kubectl --context $CTX -n courier logs deploy/courier-sandbox -f                # courier: JSON lines; add --previous after a crash
kubectl --context $CTX -n courier port-forward deploy/courier-sandbox 8080:8080 # then: curl localhost:8080/metrics, /readyz
kubectl --context $CTX -n kagent logs deploy/sandbox-tools -f                   # every tool call the agent made
kubectl --context $CTX -n kagent logs deploy/kagent-controller -f               # the A2A side
kubectl --context $CTX -n kagent get agent sandbox-sre -o yaml                  # its conditions
kubectl --context $CTX -n ate-system logs deploy/atenet-egress -c agentgateway | grep -v 'Checking health'   # the model calls: host, status
```

Is courier connected to Slack? `/readyz` does not say, on purpose: courier is ready to serve while Slack is down, and `ready` is logged
before the Slack connection is tried. Ask the gauge, or read the log:

- `courier_chat_connected{connection="courier-system/slack-main"}` is `1` while Socket Mode is up and `0` while it is not (after a drop, or
  before the first connection). A `0` that stays means courier cannot reach Slack or Slack turns it away.
- `kubectl --context $CTX -n courier logs deploy/courier-sandbox | grep 'slack:'` shows the connection's life: `slack: socket mode connecting`,
  `slack: socket mode websocket opened`, `slack: socket mode connected` (Slack said hello; `num_connections` above 1 means another process uses the same
  app token), `slack: socket mode reconnecting` (a refresh Slack asks for, which it does every so often, or a lost connection), and WARN lines for `connection attempt failed; retrying` (courier
  tries again by itself, with a pause that grows to 5 minutes, and repeats the same failure once a minute at most),
  `message could not be read`, `read failed` and `write failed`. Events Slack sends are logged at DEBUG only, by type and ID, never their
  text. A fatal rejection of the tokens is not one of these lines: courier prints `courier: slack rejected the tokens: ...` and exits 1
  (`kubectl logs --previous` after the restart).

Did the turn run, and how did it end? Courier logs every turn at INFO, by ids and places and never by what anyone wrote:

- `session turn started`, and `session turn ended`, which carries the `outcome` (`completed`, `failed`, `rejected`, `canceled`,
  `credentials_rejected`, `forbidden`, `unreachable`, `session_gone`, `binding_gone`, `wedged`, `uncancellable`, `pause_ended`,
  `answer_rejected`, `refused`, `no_final`) and `duration_ms`. `grep 'session turn'` reads a session at a glance; a start with no end
  is a turn still running, or one a restart recovers (`recovered=true` on its lines).
- `courier_turn_outcomes_total{binding="sandbox",outcome="..."}` counts the ends by the same outcomes. It is counted before the
  line is logged, so a line you see is already in the count.
- WARN `slack: the chunks mode of a stream was refused; streams are in markdown_text mode from now on and show no steps`, logged once
  per process: Slack turned away the chunk form of `chat.startStream`, so replies still stream, as text, with no step cards until
  courier restarts. Chunk-mode streams work (confirmed in dogfood 2026-10-03), so this line is a finding: its `err` field is Slack's answer.

Courier's other metrics (`/metrics`): `courier_credential_rejections_total` (the Binding's token was refused),
`courier_interaction_decisions_total` and `courier_interaction_timeouts_total` (row 5), `courier_outbox_abandoned_total` (row 7),
`courier_config_stale`, `courier_audit_write_failures_total`. Log lines worth knowing: `serving a ChatConnection`, `session turn
failed`, `recover: ...`, `audit write failed`.

Nothing here prints a token, and none of these logs should hold one; if one ever does, that is a bug to report. A provider's own
error text is shown in the thread, though: a **wrong** API key makes OpenAI answer `Incorrect API key provided: sk-...****abcd`
(it masks all but the last four characters), which courier then posts as the agent's failure. Fix the key and do not paste the thread.

## If it does not come up

- **The thread says `the agent's front door refused the Binding's credentials`**, and `courier_credential_rejections_total{binding="sandbox"}` keeps rising:
  the Binding's Dex token expired (after a long sleep or closed lid). Mint a new one with
  `kubectl --context kind-courier-full -n courier create job --from=cronjob/courier-token-refresh refresh-now` (then `kubectl --context kind-courier-full -n courier delete job refresh-now`),
  or re-run `make sandbox-up`. Courier reads the Secret on every request, so the next message works.
- **A thread says the agent "isn't ready to take this message yet" and the notice never clears**, and `kubectl --context kind-courier-full -n kagent logs deploy/kagent-default | grep CertificateExpired`
  finds `atunnel failed to open egress tunnel ... egress gateway rejected CONNECT with 403 Forbidden: actor identity check denied ... CertificateExpired`
  (kagent answers courier `-32004`, "session not ready", for good): the **egress gateway's certificate expired**. Every agent connection leaves through
  `deploy/atenet-egress` in `ate-system`, whose pod certificate lives 24 hours and is read once, when the pod starts, so a gateway that has run for about a day turns all of
  them away while it shows Running and 0 restarts. Run **`make sandbox-heal`**: it restarts the gateway and waits for it, and the next message works (`make sandbox-up`
  does the same before it verifies, if the pod is more than 20 hours old or a log has the line). Seen in dogfood 2026-10-03. After 31 hours the other `ate-system`
  pods that carry such certificates (`ate-controller`, `atenet-router`, `k8s-credential-provider`) showed no expiry in their logs, so only the gateway is restarted; if another
  starts rejecting, that is a finding.
- **Exit 3, `error: courier exited 1, which is fatal auth`**: Slack rejected `SLACK_BOT_TOKEN` or `SLACK_APP_TOKEN` (or the app is
  not installed, or Socket Mode is off). A restart would be rejected again, so the pod crash-loops. Fix the file, re-run `make sandbox-up`.
- **The bot does not answer**: is the bot in the channel, and is `SLACK_CHANNEL_ID` that channel's ID (not its name)? Courier answers in that channel only.
- **The agent answers with a 401 or a model error**: the `MODEL_API_KEY`, `MODEL_PROVIDER` or `MODEL_NAME` is wrong (for bedrock also `MODEL_REGION`, or the key's account has no access to that model or inference profile in that region). Fix the file and re-run.
- **`the full tier is not complete`**: `make e2e-full-up`.
- Anything else: `make sandbox-down && make sandbox-up`. To start from nothing, `make e2e-full-down` first (it removes the cluster and the sandbox with it).

## Taking it away

`make sandbox-down` takes the full tier's Alertmanager off the alert source (the receiver and the webhook Secret go, and the operator rolls its pod back
to the tier's own configuration), and deletes the sandbox courier, its volume and config, the Slack and model Secrets, the token refresh, the agent, the
tool server release and its binding, and namespace `shop`; then it scales the full tier's own courier back to one and waits for it.
The full tier's Binding token Secret stays, as it is the tier's. Then `make e2e-full` runs as before. Revoke the tokens in the Slack
app (Install App, Reinstall or Uninstall) when you are done: the file outside the repository is yours to delete.

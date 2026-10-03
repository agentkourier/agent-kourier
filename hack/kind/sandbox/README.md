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
| `make sandbox-up` | Checks the secrets file first (before it touches anything). Brings the full tier up if it is not (`make e2e-full-up`), builds and loads `courier:dev`, then applies the overlay and verifies it. Safe to re-run. Exit 0: up, courier connected to Slack. Exit 1: a refused file or a failure. **Exit 3: deployed, but Slack rejected the tokens** (everything else is up and verified; courier crash-loops until the file is fixed). |
| `make sandbox-down` | Removes the overlay and restores the full tier's own courier (the testhooks image on the fake Slack), so `make e2e-full` works again. The cluster stays. |

While the sandbox is up, `make e2e-full` and `make e2e-full-up` **refuse**
(`Run: make sandbox-down first`): the sandbox's pod carries the label the front door trusts, which the tier's own courier Deployment and
its Service also select, so a test run would drive the sandbox's courier. `kubectl logs deploy/courier` is the tier's (scaled to zero);
use `deploy/courier-sandbox`.

`make` shows both in its help. The secrets file is `$SANDBOX_ENV`, default `~/.config/agent-courier/sandbox.env`:
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

How Bedrock works here, from kagent 1.0.0-alpha5's source and CRDs (the first real Bedrock turn is the proof):
- **Auth.** kagent's Bedrock provider takes either AWS access keys (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`) or a bearer token, and it reads
  them from **fixed key names** in the Secret the ModelConfig's `apiKeySecret` names; the ModelConfig's `apiKeySecretKey` is exempt for Bedrock and is
  left out. A Bedrock API key goes in under **`AWS_BEARER_TOKEN_BEDROCK`**, which `make sandbox-up` does from `MODEL_API_KEY`. IRSA is not used.
- **Egress, and a workaround.** kagent 1.0.0-alpha5's Go ADK harness has a gap: the destination allowlist (`agentConfigDestinations`) has a case for
  OpenAI, Anthropic, Gemini and Ollama and none for Bedrock, while the Secret's bearer key makes kagent bind `Authorization: Bearer <key>` to
  `bedrock-runtime.<region>.amazonaws.com`, and Substrate's policy compiler refuses a binding for a host that is not also an allowed destination
  (`policy.go:32-34`). Without more, every Bedrock session fails in preparation. So `modelconfig-bedrock.yaml` carries one `defaultHeaders` entry,
  `X-Courier-Egress-Host: https://bedrock-runtime.<region>.amazonaws.com`: the allowlist also takes every URL in the serialised model, headers included, so
  the host becomes a destination and the binding is accepted. This is a **kind-only workaround**; the upstream fix is a Bedrock case in
  `agentConfigDestinations`, as `translator/claude/compiler.go:257` has for Claude on Bedrock, and the header goes with it. The header is also sent to
  Bedrock on every request (the ADK's `headerTransport`). The key itself is never in the sandbox: the SDK holds a placeholder
  (`kagent-credential-injected`) and the gateway swaps in the key.
  All of this is read from the source, and `TestBedrockModelConfigPutsItsEndpointInTheModelJSON` pins the header, its rendered host, and a copy of the walk that finds it.
  Nothing has run against Bedrock.
- UNCONFIRMED, to check in the first real Bedrock turn (the egress gateway's log: `kubectl --context kind-courier-full -n ate-system logs deploy/atenet-egress -c agentgateway`,
  and the ate-api-server log's `GetActorEgressPolicy` for the actor):
  - the egress log shows **exactly `bedrock-runtime.<region>.amazonaws.com`** reached, with the injected `Authorization`, and the policy lists that host both
    as an allowed destination and as the credential rule's host;
  - **no other AWS host** is reached (no STS, no IMDS at `169.254.169.254`, no `bedrock.<region>`);
  - that Bedrock ignores the unknown `X-Courier-Egress-Host` header (no request is rejected for it);
  - that kagent's Go SDK accepts the placeholder in `AWS_BEARER_TOKEN_BEDROCK` and sends it as `Authorization: Bearer`, so the gateway has a header to replace.
  If a turn fails, look at the gateway's log for a denied host and at the agent's error in the thread.

### The scopes, against what courier calls

| Call (`internal/chat/slack`) | Scope in the manifest |
|---|---|
| `auth.test` | none needed |
| `users.info` (and the email for the audit log) | `users:read`, `users:read.email` |
| `chat.postMessage`, `chat.update`, `chat.postEphemeral` | `chat:write` |
| `chat.startStream`, `chat.appendStream`, `chat.stopStream` | `chat:write` (Slack's method pages name no other scope) |
| `reactions.add` | `reactions:write` |
| `conversations.history`, `conversations.replies` | `channels:history`, `groups:history` |
| `conversations.members` | `channels:read`, `groups:read` |
| events `app_mention`, `message.channels`, `message.groups` | `app_mentions:read`, `channels:history`, `groups:history` |
| Socket Mode | the app-level token, `connections:write` |

The manifest covers every call. UNCONFIRMED(WP-04): that the streaming methods need no app feature beyond `chat:write` (the
"Agents & AI Apps" feature and `assistant:write` are named on none of the three method pages, and nothing here enables
them); that `chat.startStream` accepts the channel form courier sends (`recipient_user_id` and `recipient_team_id` are
documented as required in channels, and courier sends both); and that `chat.stopStream` takes the blocks and metadata
courier sends (its page lists `blocks` and `metadata`). If `chat.startStream` fails with `missing_scope` or a feature error in
the first run, that is the finding: note the error code in the checklist's streaming row and add the scope or feature to the
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
puts it in the allowlist (see "The model", above). That is read from kagent's source and has not been run.

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
| 1 | `@courier why is checkout crashlooping?` | A thread opens under your message. The reply **streams** (it grows as the model writes) and shows **tool lines** (`k8s_get_pod_logs ...`, one per call, with a status). It quotes the `FATAL: cannot resolve payments-db` line, says what it checked, and changes nothing. If it streams in one piece, or fails at `chat.startStream`, that is a WP-04 finding. |
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
- `kubectl --context $CTX -n courier logs deploy/courier-sandbox | grep 'slack:'` shows the connection's life: `socket mode connecting`,
  `socket mode websocket opened`, `socket mode connected` (Slack said hello; `num_connections` above 1 means another process uses the same
  app token), `socket mode reconnecting` (a refresh Slack asks for, which it does every so often, or a lost connection), and WARN lines for `connection attempt failed; retrying` (courier
  tries again by itself, with a pause that grows to 5 minutes, and repeats the same failure once a minute at most),
  `message could not be read`, `read failed` and `write failed`. Events Slack sends are logged at DEBUG only, by type and ID, never their
  text. A fatal rejection of the tokens is not one of these lines: courier prints `courier: slack rejected the tokens: ...` and exits 1
  (`kubectl logs --previous` after the restart).

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
- **Exit 3, `error: courier exited 1, which is fatal auth`**: Slack rejected `SLACK_BOT_TOKEN` or `SLACK_APP_TOKEN` (or the app is
  not installed, or Socket Mode is off). A restart would be rejected again, so the pod crash-loops. Fix the file, re-run `make sandbox-up`.
- **The bot does not answer**: is the bot in the channel, and is `SLACK_CHANNEL_ID` that channel's ID (not its name)? Courier answers in that channel only.
- **The agent answers with a 401 or a model error**: the `MODEL_API_KEY`, `MODEL_PROVIDER` or `MODEL_NAME` is wrong (for bedrock also `MODEL_REGION`, or the key's account has no access to that model or inference profile in that region). Fix the file and re-run.
- **`the full tier is not complete`**: `make e2e-full-up`.
- Anything else: `make sandbox-down && make sandbox-up`. To start from nothing, `make e2e-full-down` first (it removes the cluster and the sandbox with it).

## Taking it away

`make sandbox-down` deletes the sandbox courier, its volume and config, the Slack and model Secrets, the token refresh, the agent, the
tool server release and its binding, and namespace `shop`; then it scales the full tier's own courier back to one and waits for it.
The full tier's Binding token Secret stays, as it is the tier's. Then `make e2e-full` runs as before. Revoke the tokens in the Slack
app (Install App, Reinstall or Uninstall) when you are done: the file outside the repository is yours to delete.

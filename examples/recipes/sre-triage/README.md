# sre-triage

An alert that Alertmanager posts to a Slack channel starts an investigation in that alert's thread. The agent reads
the cluster with read-only tools, says what it found and what it checked, and can ask the thread a question.

## What it does

- Every alert group Alertmanager posts to the channel, through the receiver you name, gets one thread, anchored to the
  first message, and one investigation by the agent `sandbox-sre`. The answer streams into the thread.
- The same alert group firing again within 4 hours of the investigation's start, or while its thread is open and has
  had a message of its key in the last 24 hours (`thread.maxIdle`), adds a "Fired again at" note to the thread and
  starts no new investigation. An open thread idle longer than that counts as abandoned, and the next fire starts a new
  investigation.
- A `RESOLVED` message for an alert whose thread is open adds a "Resolved at" note, closes the thread and starts no
  investigation.
- At most 5 investigations start in any 10 minutes, 2 run at once and 40 start in a day. An alert a limit stops gets no
  investigation and no note in its thread; one line in the channel counts the stopped ones for each window.
- A person's reply in an alert's thread continues the agent's session. A mention in the channel starts nothing: the
  Binding lists only the alert trigger.
- The agent can ask the thread a question with its `ask_user` tool and waits for the answer (`askUser: true`).

The trigger is a `botMessage` trigger written in full in `kourier/binding.yaml`: its regexes, the thread per alert group,
the 4h cooldown, the close, the limits and the first prompt. It is the same trigger as in
[Investigate Alertmanager alerts](../../../docs/how-to/investigate-alertmanager-alerts.md). The Binding narrows it to one
receiver and turns away the Watchdog heartbeat. The fields are in [Chat triggers](../../../docs/reference/chat-triggers.md).

The agent cannot change anything, three ways. Its tool server's ServiceAccount is bound to the built-in `view`
ClusterRole (cluster-wide, which has no write verbs and no access to Secrets); the tool server runs with `--read-only`;
and the agent lists its six read tools by name. The prompt tells it the same.

## Files

```
kourier/        AgentBackend, ChatConnection and Binding: Agent Kourier's side
agent/          the kagent side: the agent, its prompt, its tool server and RBAC, and the faults to try it on
evals/
  triggers.yaml the messages the Binding must take or turn away (run by go test ./test/recipes)
  scenarios/    quality scenarios, one per seeded fault, as data
```

## Prerequisites

- **Slack.** A Slack app with a bot token and an app-level token, and the bot invited to the alert channel: see
  [Configure Slack](../../../docs/how-to/configure-slack.md). You need the channel's ID and the bot ID (`B...`) of the
  integration that posts the alerts: the `bot_id` of the alert messages, not the app ID and not a display name.
- **An Alertmanager Slack receiver in the shape the trigger matches.** The title must read `FIRING (<n>) - <alertname>` or
  `RESOLVED - <alertname>`, and the fallback must be Alertmanager's default one. Alertmanager's own default title does
  not match. A title template that does, the sandbox's receiver's without its `[kind] ` tag (the trigger accepts the
  title with or without a `[tag] ` prefix of up to 32 bytes):

  ```yaml
  title: '{{ .Status | toUpper }}{{ if eq .Status "firing" }} ({{ .Alerts.Firing | len }}){{ end }} - {{ .CommonLabels.alertname }}'
  ```

  The Binding picks the receiver by its name in the message's title link (`receiver=slack-critical`). The title link
  Alertmanager builds by default carries it, according to the sandbox's receiver configuration; that is Alertmanager's
  behaviour, which this repository does not check. If you set `title_link` yourself, keep the receiver's name in it.
  The group labels (`group_by`) decide the thread: alerts that share their values share a thread.
- **A `repeat_interval` under 24 hours.** An alert that keeps firing is posted again at the receiver's `repeat_interval`
  (Alertmanager's behaviour, which this repository does not check). If that is longer than the trigger's
  `thread.maxIdle`, which is 24 hours with the 4h cooldown unless you set it, the open thread counts as abandoned between two repeats and the
  next one starts a second investigation. Keep `repeat_interval` under 24 hours, or set `thread.maxIdle` above it.
- **kagent 1.0.0-alpha5 on Agent Substrate, behind a verifying front door.** The agent in `agent/` is a kagent
  `Agent` that runs on a Substrate worker pool, as kagent 1.0 runs every agent. **The agent half runs on the kind full
  tier** (`make e2e-full-up`): it names that tier's worker pool (`kagent-default`) and snapshot location, and the
  [sandbox guide](../../../hack/kind/sandbox/README.md) explains why that cluster is not an ordinary one. It does not carry over to a
  managed cluster such as EKS (through Kubernetes 1.36 it cannot enable the pod-certificate APIs that Substrate needs).
  [Connect a kagent agent](../../../docs/how-to/connect-kagent.md) has the check. Elsewhere, point the Binding at
  another agent through the `a2a` dialect, and give it the same prompt and read-only tools.
- **A model.** A kagent `ModelConfig` named `sandbox-model` in namespace `kagent`, and the Secret it names, which holds
  your provider's key. The recipe does not ship one, since the provider is yours.
  [`modelconfig-anthropic.yaml`](../../../hack/kind/sandbox/modelconfig-anthropic.yaml) in the sandbox is an example
  (replace its `__MODEL_NAME__`); the Secret there is `sandbox-model` with the key `apiKey`.
- **A Binding identity.** A Secret `sre-triage-token` in namespace `agent-kourier`, with the key `token`, holding a
  bearer token the front door accepts: [Create the Binding identity](../../../docs/how-to/create-binding-identity.md).

## Apply it

### 1. The agent

From the repository root, against the kind full tier's cluster (its context is `kind-agent-kourier-full`). These are the
steps `hack/kind/sandbox/up.sh` runs for the agent, in its order:

```sh
kubectl --context kind-agent-kourier-full apply -f examples/recipes/sre-triage/agent/faults.yaml  # optional: broken apps to try it on
kubectl --context kind-agent-kourier-full apply -f examples/recipes/sre-triage/agent/tools-rbac.yaml
helm --kube-context kind-agent-kourier-full upgrade --install sandbox-tools \
  oci://ghcr.io/kagent-dev/tools/helm/kagent-tools --version 0.3.0 --namespace kagent \
  -f examples/recipes/sre-triage/agent/tools-values.yaml --wait --timeout 4m
# your ModelConfig sandbox-model and its Secret go here
kubectl --context kind-agent-kourier-full apply -f examples/recipes/sre-triage/agent/agent-prompt.yaml
kubectl --context kind-agent-kourier-full apply -f examples/recipes/sre-triage/agent/agent.yaml
kubectl --context kind-agent-kourier-full -n kagent wait --for=condition=Accepted \
  remotemcpserver/sandbox-tools modelconfig/sandbox-model --timeout=180s
kubectl --context kind-agent-kourier-full -n kagent wait --for=condition=Ready agent/sandbox-sre --timeout=300s
```

`faults.yaml` makes a namespace `shop` of five broken apps and one healthy one. Leave it out on a cluster whose
workloads you care about. To remove what these steps made, run `kubectl delete -f` on `agent.yaml`, `agent-prompt.yaml`
and `tools-rbac.yaml`, run `helm uninstall sandbox-tools --namespace kagent`, and delete the namespace `shop`.

The sandbox tier does all of this for its own cluster: `make sandbox-up` applies these files from here (it also needs the
Slack and model secrets file its guide describes). With the optional alert source of its guide ("Run the alert trial"),
`make sandbox-alert` then fires the alert of one of the faults.

### 2. Agent Kourier's resources

1. In `kourier/binding.yaml`, replace `CREPLACEME` with the channel's ID and `B0REPLACEME` with the bot ID. Change
   `receiver=slack-critical$` to your receiver's name; the `$` keeps it from also matching a longer name. The three
   resources are in namespace `agent-kourier`; if yours is another, change `metadata.namespace` in all three.
2. Make the two Secrets the resources name, in namespace `agent-kourier` (`kubectl create namespace agent-kourier`
   first if it does not exist). `slack`, the ChatConnection's, holds the keys `botToken` and `appToken`.
   `sre-triage-token`, the Binding's, holds the key `token`, the bearer token the front door accepts.

   ```sh
   kubectl -n agent-kourier create secret generic slack \
     --from-literal=botToken="$SLACK_BOT_TOKEN" \
     --from-literal=appToken="$SLACK_APP_TOKEN"
   ```

   For `sre-triage-token`, run the "Request and store the token" script of
   [Create the Binding identity](../../../docs/how-to/create-binding-identity.md) with
   `-n payments create secret generic payments-agent-token` in its last command changed to
   `-n agent-kourier create secret generic sre-triage-token`, keeping that command's `--from-file` line, and with your own Dex URL, client and account in
   place of its example values. It creates the Secret itself and leaves no token file behind. If your issuer already
   gave you the token, put it in a variable and create the Secret without a file, as
   [Install with Helm](../../../docs/how-to/install-with-helm.md) does for its own Secret name:

   ```sh
   kubectl -n agent-kourier create secret generic sre-triage-token --from-literal=token="$SRE_TRIAGE_TOKEN"
   ```

   Use the names above and not Install with Helm's (`agent-kourier-slack`, `agent-kourier-agent-token`), since the
   resources refer to these.
3. Put the directory in a ConfigMap and install the chart on it. The chart cannot read a ConfigMap you manage, so name
   the namespace that holds the Secrets:

   ```sh
   kubectl -n agent-kourier create configmap sre-triage-config --from-file=examples/recipes/sre-triage/kourier
   ```

   ```yaml
   config:
     existingConfigMap: sre-triage-config
   rbac:
     secretNamespaces: [agent-kourier]
   ```

   Those values replace the inline `config` of the values file in Install with Helm; the rest of that guide (the
   image, the install, the checks) applies as written.
4. Replay the channel's history through the trigger before you rely on it. It reads Slack and changes nothing; see
   [Tune triggers with a dry run](../../../docs/how-to/tune-triggers.md):

   ```sh
   go run ./cmd/agent-kourier dry-run --config examples/recipes/sre-triage/kourier \
     --binding agent-kourier/sre-triage --token-file ~/.config/agent-kourier/slack-bot-token
   ```

Then wait for the next alert on the receiver. A thread opens under it and the investigation streams in.

## What to change

- **The channel, the bot and the receiver**: `kourier/binding.yaml`, as above. If you change the bot ID there, change
  `B0REPLACEME` in `evals/triggers.yaml` to match, or the evals no longer describe your Binding.
- **The cooldown and the limits**: change the values in the trigger in `kourier/binding.yaml`, for example
  `cooldown: 2h` under `thread` or `maxRunsPerDay: 20` under `limits`.
- **How long a quiet open thread lasts**: `thread: {maxIdle: 48h}` on the trigger, for a `repeat_interval` of 24 hours
  or more. Set it above the `repeat_interval`; it must also be at least twice the cooldown and at most 720h.
- **A mention trigger**: add `- type: mention` to `triggers` if people should also be able to start a session by
  mentioning the bot. The list is the whole list, so keep the `botMessage` trigger in it.
- **The prompt**: `agent/agent-prompt.yaml` is written for the faults: it names namespace `shop` and its six apps. For
  your own cluster, rewrite those lines. Keep the read-only paragraph.
- **What the agent can see**: the `view` binding in `agent/tools-rbac.yaml` is cluster-wide. Replace it to narrow it.
- **The names**: the agent, its tool server, its prompt and its ModelConfig are called `sandbox-sre`, `sandbox-tools`,
  `sandbox-sre-prompt` and `sandbox-model`, because the sandbox tier and its tests use those names. If you rename
  them, change the Binding's `agent.name` and `agent.namespace` and the references between the manifests together.

## Checks

`go test ./test/recipes` loads `kourier/` with the loader Agent Kourier uses, resolves its references, and runs
`evals/triggers.yaml` through the trigger matcher that the intake and the dry run share. The cases are a firing alert
(also behind a `[prod]` cluster tag), a resolved one, a grouped one (`FIRING (3)`), the Watchdog heartbeat (excluded),
another receiver and a receiver whose name only starts with yours (both unmatched), Grafana's format (unmatched),
another bot, a person's message and the alert bot's reply inside a thread. They judge one message at a time: they do
not evaluate the thread per alert, the cooldown, the close on `RESOLVED` or the limits. See the
[recipes index](../README.md) for the format.

`go test ./test/kindmanifests` pins the agent's manifests: the tool list, the `view` binding, the read-only tool server,
the prompt, the digest-pinned images and the faults. It reads the files, not a cluster: no test runs the agent. To see it
run, bring up the sandbox tier.

### Scenarios

`evals/scenarios/` holds one scenario for each Deployment in `agent/faults.yaml`: the five faults and the healthy
app. They are data for a person to read an answer against; nothing runs them yet, and **the shape is provisional**.

```yaml
name: checkout crashes at start
app: checkout                  # the Deployment of agent/faults.yaml
alert: KubePodCrashLooping     # an alert the sandbox fires for it (hack/kind/sandbox/alertmanager/alerts.json), or
# question: 'Is anything wrong with the frontend in shop?'   # a person's question instead; exactly one of the two
cause: '...'                   # what a good answer must name
evidence:                      # what it should cite
  - '...'
```

`go test ./test/kindmanifests` checks that each file has that shape, that there is one for every Deployment of
`faults.yaml` and no other, and that each alert is the one the sandbox fires for its app.

## Limits

- The agent half is as shipped only for the kind full tier (see Prerequisites). Nothing in this repository applies it to
  another cluster.
- An investigation is as good as the model and the prompt. The scenarios say what a good answer to each fault names;
  nothing grades an answer yet.
- The agent reads the cluster it runs in. It does not read your metrics or logs backend, and the tools it has are the
  six in `agent/agent.yaml`.
- Agent Kourier reads the message Alertmanager posts. Alertmanager's routes and receivers are untouched, so the alert
  still reaches people if Agent Kourier is down.

# sre-triage

An alert that Alertmanager posts to a Slack channel starts an investigation in that alert's thread. The agent reads
the cluster with read-only tools, says what it found and what it checked, and can ask the thread a question.

**Check the agent half's requirements first.** The agent in `agent/` runs only on this repository's kind full tier (a
local cluster, defined in Prerequisites) and needs an OIDC issuer for its token. If you have neither, start from
[`docs-agent`](../docs-agent/README.md), or use this recipe's `kourier/` files with an agent of your own.

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

The trigger is a `botMessage` trigger (a rule that reads another bot's posts) written in full in
`kourier/binding.yaml`: its regexes, the thread per alert group, the 4h cooldown, the close, the limits and the first
prompt. It is the same trigger as in
[Investigate Alertmanager alerts](../../../docs/how-to/investigate-alertmanager-alerts.md). The Binding narrows it to
one receiver and turns away the Watchdog heartbeat. The fields are in
[Chat triggers](../../../docs/reference/chat-triggers.md).

The agent cannot change anything, three ways. Its tool server's ServiceAccount is bound to the built-in `view`
ClusterRole (cluster-wide, which has no write verbs and no access to Secrets); the tool server runs with `--read-only`;
and the agent lists its six read tools by name. The prompt tells it the same.

## What you'll see in Slack

```text
#alerts
  Alertmanager    FIRING (1) - KubePodCrashLooping        the alert, as Alertmanager posts it
    thread        the agent's answer streams in: likely cause, what it checked, next steps
    thread        Fired again at <time>                   the same alert group fires again inside the cooldown
    thread        Resolved at <time>                      the RESOLVED message arrives; the thread closes
  Agent Kourier   3 alert messages not investigated in the last 10m (rate limit).
                                                          one line per window, when a limit stopped some
```

To see it work without waiting for a real alert, post a test alert to your Alertmanager. Use labels that your routes
send to the receiver the Binding names:

```sh
curl -X POST -H 'Content-Type: application/json' http://<alertmanager>:9093/api/v2/alerts \
  --data '[{"labels":{"alertname":"RecipeTest","severity":"critical","namespace":"shop"}}]'
```

Alertmanager posts the firing alert once the route's `group_wait` has passed. To see the close, post the same labels
with an `endsAt` in the past, as the sandbox's `hack/kind/sandbox/alert.sh` does for a resolved alert; the receiver must
set `send_resolved: true` for Alertmanager to post the `RESOLVED` message. All of this is Alertmanager's behaviour,
which this repository does not check against your install.

## Prerequisites

- **Slack.** A Slack app with a bot token and an app-level token, and the bot invited to the alert channel: see
  [Configure Slack](../../../docs/how-to/configure-slack.md). You need the channel's ID and the bot ID (`B...`) of the
  integration that posts the alerts: the `bot_id` of the alert messages, not the app ID and not a display name. The dry
  run in step 2 prints the bot ID if you do not have it.
- **An Alertmanager Slack receiver in the shape the trigger matches.** The title must read `FIRING (<n>) - <alertname>`
  or `RESOLVED - <alertname>`, and the fallback must be Alertmanager's default one. Alertmanager's own default title
  does not match. A title template that does, the sandbox's receiver's without its `[kind] ` tag (the trigger accepts
  the title with or without a `[tag] ` prefix of up to 32 bytes):

  ```yaml
  title: '{{ .Status | toUpper }}{{ if eq .Status "firing" }} ({{ .Alerts.Firing | len }}){{ end }} - {{ .CommonLabels.alertname }}'
  ```

  The Binding picks the receiver by its name in the message's title link (`receiver=slack-critical`). The title link
  Alertmanager builds by default carries it, according to the sandbox's receiver configuration; that is Alertmanager's
  behaviour, which this repository does not check. If you set `title_link` yourself, keep the receiver's name in it.
  The group labels (`group_by`) decide the thread: alerts that share their values share a thread.

  No Alertmanager of your own? On the kind full tier, "Alerts on the kind full tier" below posts the alerts of the
  seeded faults into a channel through a receiver of this shape.

  Set `send_resolved: true` on the receiver (the sandbox's receiver does, in
  `hack/kind/sandbox/alertmanager/config.yaml`). Without it Alertmanager posts no `RESOLVED` message, so the thread
  never gets its "Resolved at" note or its close. That is Alertmanager's behaviour, which this repository does not
  check.
- **A `repeat_interval` under 24 hours.** An alert that keeps firing is posted again at the receiver's `repeat_interval`
  (Alertmanager's behaviour, which this repository does not check). If that is longer than the trigger's
  `thread.maxIdle`, which is 24 hours with the 4h cooldown unless you set it, the open thread counts as abandoned
  between two repeats and the next one starts a second investigation. Keep `repeat_interval` under 24 hours, or set
  `thread.maxIdle` above it.
- **For the agent half: the kind full tier.** The agent is a kagent `Agent` (kagent 1.0.0-alpha5) that runs on Agent
  Substrate, the runtime kagent 1.0 runs every agent session on. It names the kind full tier's worker pool
  (`kagent-default`) and snapshot location. The kind full tier is the local kind cluster that `make e2e-full-up` builds
  from a checkout of this repository, with kagent, Agent Substrate, a front door and an OIDC issuer (Dex) already in it
  (the [glossary](../../../docs/reference/glossary.md) has both terms). The [sandbox
  guide](../../../hack/kind/sandbox/README.md) explains why that cluster is not an ordinary one. The agent half does not
  carry over to a managed cluster such as EKS (through Kubernetes 1.36 it cannot enable the pod-certificate APIs that
  Substrate needs); [Connect a kagent agent](../../../docs/how-to/connect-kagent.md) has the check. Elsewhere, point the
  Binding at another agent through the `a2a` dialect (Agent Kourier's adapter for any A2A server; A2A is the Agent2Agent
  protocol), and give it the same prompt and read-only tools.
- **A model.** A kagent `ModelConfig` (the kagent resource that names a model provider and the Secret that holds its
  key) called `sandbox-model` in namespace `kagent`, and the Secret it names. The recipe does not ship one, since the
  provider is yours; step 1 of "Apply it" shows one for OpenRouter and the Secret key each provider uses.
- **A Binding identity: a token from an OIDC issuer.** The front door is a proxy in front of kagent that checks the
  Binding's bearer token before kagent sees it ([Put a verifying front door before
  kagent](../../../docs/how-to/front-door.md)). The kind full tier has one. The token must come from an OIDC issuer
  (an identity provider such as Dex) that the front door accepts, and goes in a Secret `sre-triage-token` in namespace
  `agent-kourier`, with the key `token`: step 2.3 below. **Without an OIDC issuer there is no token the front door
  accepts, and the agent half is not usable as written.**
- **A chart release that has custom resources, and Kubernetes 1.33 or later.** Step 2.4 applies the resources as custom
  resources, which the chart reads with `config.source: crds`. Release 0.1.6 does not have that route: `crds` and
  `config.source` are not among its values. Check the chart you will install with
  `helm show values oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> | grep -A1 '^crds:'`. The chart
  refuses to render `config.source: crds` for an older API server; the kind full tier's node is Kubernetes 1.37. Installing
  the CRDs needs the right to create CustomResourceDefinitions, which are cluster-scoped.
- **To run the dry run (step 2.2): Go and a checkout, or Docker instead of Go.** The dry run is a subcommand of the
  `agent-kourier` binary. Run it with Go at the version in `go.mod`, from a checkout of this repository, as below; or
  with Docker, using the released image `ghcr.io/agentkourier/agent-kourier:v0.2.0`, whose entrypoint is that binary.
  Either way you run it from the checkout, since the command reads this recipe's `kourier/` directory.

## Alerts on the kind full tier

The kind full tier has an Alertmanager but no receiver that posts to Slack. The sandbox tier can add one, through an
incoming webhook of yours. **It posts into a real channel**: use a channel made for the trial, or one whose members
expect it. What follows is what `hack/kind/sandbox/up.sh`, `webhook.sh`, `alert.sh` and `alertmanager/config.yaml` do;
the [sandbox guide](../../../hack/kind/sandbox/README.md) ("Alert trial") has the rest.

1. Make an incoming webhook for the channel (Slack's Incoming Webhooks, on your Slack app) and put its URL, alone on one
   line, in `~/.config/agent-kourier/alertmanager-webhook-url`, or in the file that `ALERTMANAGER_WEBHOOK_FILE` names.
   `make sandbox-up` refuses a file that is a symbolic link, is not yours, gives the group or others any access (use
   `chmod 600`), or does not hold a `https://hooks.slack.com/services/T.../B.../...` URL; its error names the file and
   never the URL.
2. Run `make sandbox-up`. It needs the sandbox's secrets file too
   ([`sandbox.env.example`](../../../hack/kind/sandbox/sandbox.env.example)), and it applies the agent half of this
   recipe itself (step 1 of "Apply it"), so step 1 is done once it has run. With the webhook file there, it makes the
   tier's Alertmanager post through your webhook as the receiver `slack-critical-kind`: the receiver whose title
   template is the one under Prerequisites with a `[kind] ` tag in front, with `send_resolved: true`. Without the file
   it skips the alert source and says so.
3. Fire an alert: `make sandbox-alert ALERT=KubePodCrashLooping`, or one of `KubePodNotReady`,
   `KubeContainerWaiting`, `KubeContainerOOMKilled` and `TargetDown`, each about a fault of `faults.yaml`. Only these
   five can be fired. The sandbox's route sets `group_wait: 30s` and `group_interval: 30s`
   (`hack/kind/sandbox/alertmanager/config.yaml`), so a firing alert should reach the channel as
   `[kind] FIRING (1) - KubePodCrashLooping` after about 30 seconds.
   `make sandbox-alert ALERT=KubePodCrashLooping STATE=resolved` ends it, and the `[kind] RESOLVED` message should
   follow within about one `group_interval`. When and whether Alertmanager posts is Alertmanager's behaviour, which
   this repository does not check.
4. Alertmanager puts the receiver's name in the title link of its Slack messages (the sandbox's configuration comment
   says so; Alertmanager's behaviour, not checked here), so the Binding's `link` match must be
   `receiver=slack-critical-kind$` (step 2.1 below). Its title regex already accepts the `[kind] ` tag.
5. A webhook posts only to the channel it was made for, so the Binding's `chat.channel` must be that channel. That is
   Slack's behaviour, which this repository does not check.

`make sandbox-up` also installs its own Agent Kourier, with a Binding of its own on the alert channel named by
`ALERT_CHANNEL_ID` in `hack/kind/sandbox/webhook.sh`. When you run this recipe's release instead, scale
`deploy/agent-kourier-sandbox` to zero, as step 2.4 says. `make sandbox-down` removes the receiver and the webhook
Secret.

## Files

```
kourier/        AgentBackend, ChatConnection and Binding: Agent Kourier's side
agent/          the kagent side: the agent, its prompt, its tool server and RBAC, and the faults to try it on
evals/
  triggers.yaml the messages the Binding must take or turn away (run by go test ./test/recipes)
  scenarios/    quality scenarios, one per seeded fault, as data
```

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
```

Then the model: a Secret `sandbox-model` in namespace `kagent` that holds the provider's key, and a kagent
`ModelConfig` of the same name that names it. The example is OpenRouter, which kagent reaches through its
OpenAI-compatible provider; it is `hack/kind/sandbox/modelconfig-openrouter.yaml` with its two placeholders filled in
(the model, and the endpoint, which is OpenRouter's). Read the key into a shell variable without echoing it, and give
`kubectl` the key on its standard input, so that it is on no command line. `read -rs` is for bash and zsh, and
`printf` is a shell builtin, so the key is in no process's arguments either. The variable is not exported.

```sh
read -rs MODEL_API_KEY   # paste the key and press Enter; nothing is echoed
printf %s "$MODEL_API_KEY" | kubectl --context kind-agent-kourier-full -n kagent \
  create secret generic sandbox-model --from-file=apiKey=/dev/stdin
unset MODEL_API_KEY
kubectl --context kind-agent-kourier-full apply -f - <<'EOF'
apiVersion: api.kagent.dev/v1alpha3
kind: ModelConfig
metadata: {name: sandbox-model, namespace: kagent}
spec:
  provider: OpenAI
  model: anthropic/claude-haiku-4.5   # a vendor/model slug from OpenRouter
  apiKeySecret: sandbox-model
  apiKeySecretKey: apiKey
  openAI:
    baseUrl: https://openrouter.ai/api/v1
EOF
```

`create secret` fails if `sandbox-model` exists; to replace the key, run `kubectl -n kagent delete secret sandbox-model`
first. For another provider, make the Secret under the key in the table and apply the sandbox's file for it, after
replacing its `__MODEL_NAME__` (and, for Bedrock, `__MODEL_REGION__`):

| Provider | Secret key | ModelConfig to start from |
|---|---|---|
| OpenRouter | `apiKey` | [`modelconfig-openrouter.yaml`](../../../hack/kind/sandbox/modelconfig-openrouter.yaml) (also replace `__MODEL_BASE_URL__`) |
| OpenAI | `apiKey` | [`modelconfig-openai.yaml`](../../../hack/kind/sandbox/modelconfig-openai.yaml) |
| Anthropic | `apiKey` | [`modelconfig-anthropic.yaml`](../../../hack/kind/sandbox/modelconfig-anthropic.yaml) |
| Bedrock | `AWS_BEARER_TOKEN_BEDROCK` | [`modelconfig-bedrock.yaml`](../../../hack/kind/sandbox/modelconfig-bedrock.yaml) |

The key names are those `hack/kind/sandbox/up.sh` uses. For Bedrock the ModelConfig names no key, because kagent's
Bedrock provider reads a bearer token from a Secret key of that fixed name (so the sandbox's files say; kagent's
behaviour, which this repository does not check). The Bedrock file also carries a header that is a workaround for the
kind tier, which its comments explain; keep it there.

The rest of the agent:

```sh
kubectl --context kind-agent-kourier-full apply -f examples/recipes/sre-triage/agent/agent-prompt.yaml
kubectl --context kind-agent-kourier-full apply -f examples/recipes/sre-triage/agent/agent.yaml
kubectl --context kind-agent-kourier-full -n kagent wait --for=condition=Accepted \
  remotemcpserver/sandbox-tools modelconfig/sandbox-model --timeout=180s
kubectl --context kind-agent-kourier-full -n kagent wait --for=condition=Ready agent/sandbox-sre --timeout=300s
```

`faults.yaml` makes a namespace `shop` of five broken apps and one healthy one. Leave it out on a cluster whose
workloads you care about. To remove what these steps made, run `kubectl delete -f` on `agent.yaml`, `agent-prompt.yaml`
and `tools-rbac.yaml`, run `helm uninstall sandbox-tools --namespace kagent`, and delete the namespace `shop`.

The sandbox tier does all of this for its own cluster: `make sandbox-up` applies these files from here (it also needs
the Slack and model secrets file its guide describes). With the optional alert source of its guide ("Run the alert
trial"), `make sandbox-alert` then fires the alert of one of the faults.

### 2. Agent Kourier's resources

1. In `kourier/binding.yaml`, replace `CREPLACESRE` with the channel's ID. Replace `B0REPLACEME` with the bot ID, or
   leave it until the dry run below prints it. Change `receiver=slack-critical$` to your receiver's name (`receiver=slack-critical-kind$` for the sandbox's alert
   source above); the `$` keeps it from also matching a longer name. The three resources are in namespace `agent-kourier`; if yours is another,
   change `metadata.namespace` in all three, and the namespace in `config.chatConnection` and `rbac.secretNames` in
   step 4.
2. Replay the channel's history through the trigger before you rely on it. The dry run only reads Slack and changes
   nothing; see [Tune triggers with a dry run](../../../docs/how-to/tune-triggers.md). It reads the bot token from a
   file (mode 600) or else from `SLACK_BOT_TOKEN`. To make the file from the `$SLACK_BOT_TOKEN` you use in the next
   step, without printing the token:

   ```sh
   mkdir -p ~/.config/agent-kourier
   ( umask 077; printf '%s\n' "$SLACK_BOT_TOKEN" > ~/.config/agent-kourier/slack-bot-token )
   chmod 600 ~/.config/agent-kourier/slack-bot-token
   ```

   Then run it from the repository root:

   ```sh
   go run ./cmd/agent-kourier dry-run --config examples/recipes/sre-triage/kourier \
     --binding agent-kourier/sre-triage --token-file ~/.config/agent-kourier/slack-bot-token
   ```

   With Docker instead of Go, mount the directory and pass the token through the environment. `SLACK_BOT_TOKEN` must be
   exported (`export SLACK_BOT_TOKEN`) for `-e SLACK_BOT_TOKEN` to pass it in; the token is then not on the command
   line. The image is the released one:

   ```sh
   docker run --rm -e SLACK_BOT_TOKEN -v "$PWD/examples/recipes/sre-triage/kourier:/config:ro" \
     ghcr.io/agentkourier/agent-kourier:v0.2.0 dry-run --config /config --binding agent-kourier/sre-triage
   ```

   Each alert should come out `matched`. If you left `B0REPLACEME` in place, they come out `other_bot` and the `SENDER`
   column shows the bot ID: put it in `binding.yaml` in place of `B0REPLACEME`, and run again.
3. Make the two Secrets the resources name, in namespace `agent-kourier` (`kubectl create namespace agent-kourier`
   first if it does not exist). **These recipes call the Slack Secret `slack`; the
   [Configure Slack](../../../docs/how-to/configure-slack.md) and
   [Install with Helm](../../../docs/how-to/install-with-helm.md) guides call it `agent-kourier-slack`. Use the recipe's
   name here, since `kourier/chatconnection.yaml` refers to it.**

   ```sh
   kubectl --context kind-agent-kourier-full -n agent-kourier create secret generic slack \
     --from-literal=botToken="$SLACK_BOT_TOKEN" \
     --from-literal=appToken="$SLACK_APP_TOKEN"
   ```

   The Binding's `sre-triage-token` holds the key `token`, the bearer token the front door accepts. A Dex ID token lasts
   24 hours, so something has to renew it ([Rotate the Binding token](../../../docs/how-to/rotate-binding-token.md)).

   **On the kind full tier**, do not use the identity guide's script. The tier's Dex answers only inside the cluster
   (`http://dex.dex.svc.cluster.local:5556`), and its client and user are fake test values (`hack/kind/full/dex.yaml`).
   Let the chart's token refresh CronJob mint the token from inside the cluster and renew it every hour, as the sandbox
   tier does. Make a placeholder for the Secret (the CronJob only patches an existing Secret), and the Secret holding
   the client's credentials, which are the fake values of `hack/kind/full/dex.yaml` and work nowhere else. Step 4 turns
   the CronJob on. (`agent-kourier-dex-client` exists already if you ran `make sandbox-up`.)
   The commands in steps 3 and 4 name the kind full tier's context; for another cluster, drop the `--context` and
   `--kube-context` options:

   ```sh
   kubectl --context kind-agent-kourier-full -n agent-kourier create secret generic sre-triage-token --from-literal=token=placeholder
   kubectl --context kind-agent-kourier-full -n agent-kourier create secret generic agent-kourier-dex-client \
     --from-literal=clientSecret=fake-agent-kourier-binding-secret \
     --from-literal=username=binding-test@example.test --from-literal=password=password
   ```

   **Elsewhere, with your own OIDC issuer**, run the "Request and store the token" script of
   [Create the Binding identity](../../../docs/how-to/create-binding-identity.md) with your own Dex URL, client and
   account in place of its example values, and change its last command, which makes the Secret, to:

   ```sh
   kubectl --context kind-agent-kourier-full -n agent-kourier create secret generic sre-triage-token \
     --from-file=token="$identity_tmp/token"
   ```

   The script makes that Secret itself and leaves no token file behind. If your issuer already gave you the token, put
   it in a variable and make the Secret without a file:

   ```sh
   kubectl --context kind-agent-kourier-full -n agent-kourier create secret generic sre-triage-token --from-literal=token="$SRE_TRIAGE_TOKEN"
   ```

   Renew the token before it expires. Use `sre-triage-token` and not Install with Helm's
   `agent-kourier-agent-token`: the Binding refers to the former.
4. Apply the directory as custom resources, and install the chart to read them. `config.chatConnection` names the one
   ChatConnection the broker serves. The chart cannot see the Secrets of resources you apply yourself, so
   `rbac.secretNames` grants the two the resources name. Do not also set `config.chatConnections`,
   `config.agentBackends` or `config.bindings`: the chart would render them as custom resources of its own.

   **On the kind full tier**, the release `agent-kourier` already exists in that namespace (the tier's own Agent
   Kourier), so install this one under another name, from the published chart, whose image the kind nodes pull from
   GHCR:

   ```yaml title="sre-triage-values.yaml"
   config:
     source: crds
     chatConnection: agent-kourier/slack
   rbac:
     secretNames:
       agent-kourier: [slack, sre-triage-token]
   persistence:
     storageClassName: standard
   tokenRefresh:
     enabled: true
     targetSecret: sre-triage-token
     dex:
       tokenURL: http://dex.dex.svc.cluster.local:5556/dex/token
       clientId: agent-kourier-binding
       credentialsSecret: agent-kourier-dex-client
   ```

   ```sh
   helm --kube-context kind-agent-kourier-full upgrade --install agent-kourier-sre-triage oci://ghcr.io/agentkourier/charts/agent-kourier \
     --version <version> --namespace agent-kourier -f sre-triage-values.yaml --set replicaCount=0
   kubectl --context kind-agent-kourier-full apply --dry-run=server -f examples/recipes/sre-triage/kourier/
   kubectl --context kind-agent-kourier-full apply -f examples/recipes/sre-triage/kourier/
   kubectl --context kind-agent-kourier-full -n agent-kourier delete job first-token --ignore-not-found
   kubectl --context kind-agent-kourier-full -n agent-kourier create job first-token \
     --from=cronjob/agent-kourier-sre-triage-token-refresh
   kubectl --context kind-agent-kourier-full -n agent-kourier wait --for=condition=Complete job/first-token --timeout=120s
   helm --kube-context kind-agent-kourier-full upgrade --install agent-kourier-sre-triage oci://ghcr.io/agentkourier/charts/agent-kourier \
     --version <version> --namespace agent-kourier -f sre-triage-values.yaml
   kubectl --context kind-agent-kourier-full -n agent-kourier wait --for=condition=Ready akbinding/sre-triage --timeout=120s
   ```

   The first `helm` command installs the CRDs and the token refresh CronJob, with no replicas (the chart's kill switch,
   `replicaCount: 0`), so that Agent Kourier does not start while the Secret still holds the placeholder. On this route
   the chart installs the CRDs unless `crds.install` says otherwise, and with `config.*` empty it renders no custom
   resource of its own that would need them served first. The server-side dry run checks each resource against the
   CRDs' schema and names the field of a refusal. The Job replaces the placeholder with a real token; the second `helm`
   command, without the override, starts Agent Kourier. The resources go in before it starts, because a broker that
   finds no ChatConnection does not start. This is the order `make sandbox-up` uses for the token
   (`hack/kind/sandbox/up.sh`).

   If the cluster has the CRDs already (`kubectl --context kind-agent-kourier-full get crd bindings.agentkourier.dev` finds one: the tier was brought up
   with `E2E_CONFIG_SOURCE=crds`, and its own release holds them), add `crds.install: false` to the values, as a second
   broker in a cluster does: two releases that render the same CRD conflict. The tier's own broker serves its own ChatConnection, not
   `slack`, so it leaves this recipe's Binding alone. The AgentBackend `agents` in `agent-kourier` is then the same
   object as the tier's own (`hack/kind/full/agent-kourier/agentbackend.yaml`, applied as a custom resource), with the
   same spec, so applying `kourier/` leaves its spec as it is.

   `make sandbox-up` applies the agent half (step 1), but then installs its own Agent Kourier with the sandbox's own
   ChatConnection and Binding (`hack/kind/sandbox/agent-kourier/config/`), not this recipe's files. If that Agent
   Kourier uses the same Slack app as yours, scale `deploy/agent-kourier-sandbox` down to zero replicas (`kubectl
   scale`, with the context option as above) or use a separate app, since two brokers on one app's Socket Mode
   connection would split its events (Slack's behaviour, not checked here). The tier's own Agent Kourier
   (`deploy/agent-kourier`) talks to the tier's fake Slack, so it does not compete for your workspace; `make sandbox-up`
   still scales it to zero to keep one broker behind the front door, and you may do the same.

   Run `helm --kube-context kind-agent-kourier-full uninstall agent-kourier-sre-triage -n agent-kourier` before
   `make e2e-full`, whose tests find Agent Kourier's pods by label. The uninstall leaves the CRDs (the chart keeps them)
   and this recipe's resources. Before `E2E_CONFIG_SOURCE=crds make e2e-full-up`, whose release `agent-kourier`
   installs the CRDs itself, ask which release holds them:

   ```sh
   kubectl --context kind-agent-kourier-full get crd bindings.agentkourier.dev \
     -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}'
   ```

   If it prints `agent-kourier-sre-triage`, delete the CRDs, since two releases that render the same CRD conflict.
   Deleting a CRD deletes every resource of its kind, so this recipe's go with them:

   ```sh
   kubectl --context kind-agent-kourier-full delete crd \
     agentbackends.agentkourier.dev bindings.agentkourier.dev chatconnections.agentkourier.dev
   ```

   If it prints `agent-kourier`, the tier holds them: leave them, and remove only this recipe's Binding and
   ChatConnection with `kubectl --context kind-agent-kourier-full -n agent-kourier delete akbinding/sre-triage
   akconnection/slack`. Not `kubectl delete -f examples/recipes/sre-triage/kourier/`, which would also delete the
   AgentBackend `agents` that the tier shares.

   **Elsewhere**, use these values in place of the inline `config` of the values file in
   [Install with Helm](../../../docs/how-to/install-with-helm.md); the rest of that guide (the image, the checks)
   applies as written. The token is the one you stored in step 3, so there is no token Job. If Agent Kourier already
   runs on a ConfigMap, move it with
   [Move the config between a ConfigMap and custom resources](../../../docs/how-to/migrate-config-source.md) instead.

   ```yaml title="sre-triage-values.yaml"
   config:
     source: crds
     chatConnection: agent-kourier/slack
   rbac:
     secretNames:
       agent-kourier: [slack, sre-triage-token]
   ```

   ```sh
   helm upgrade --install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
     -n agent-kourier -f sre-triage-values.yaml --set replicaCount=0
   kubectl apply --dry-run=server -f examples/recipes/sre-triage/kourier/
   kubectl apply -f examples/recipes/sre-triage/kourier/
   helm upgrade --install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
     -n agent-kourier -f sre-triage-values.yaml
   kubectl -n agent-kourier wait --for=condition=Ready akbinding/sre-triage --timeout=120s
   ```

   As on the kind tier, add `crds.install: false` to the values if the cluster has the CRDs already.

   **To stay on a ConfigMap, or on release 0.1.6,** use the values files below alone, in place of the ones above. Put
   the directory in a ConfigMap and install the chart on it. `config.source: files` keeps the ConfigMap route: on its
   default route the chart refuses `config.existingConfigMap`. The chart cannot read a ConfigMap you manage, so
   `rbac.secretNamespaces` names the namespace that holds the Secrets. Release 0.1.6 has only the ConfigMap route, and
   its values schema refuses keys it does not have, `config.source` among them: on 0.1.6, delete the `source: files`
   line and install with `--version 0.1.6`.

   On the kind full tier, with the token order above (no replicas, the first token Job, then the start):

   ```yaml title="sre-triage-configmap-values.yaml"
   config:
     source: files
     existingConfigMap: sre-triage-config
   rbac:
     secretNamespaces: [agent-kourier]
   persistence:
     storageClassName: standard
   tokenRefresh:
     enabled: true
     targetSecret: sre-triage-token
     dex:
       tokenURL: http://dex.dex.svc.cluster.local:5556/dex/token
       clientId: agent-kourier-binding
       credentialsSecret: agent-kourier-dex-client
   ```

   ```sh
   kubectl --context kind-agent-kourier-full -n agent-kourier create configmap sre-triage-config --from-file=examples/recipes/sre-triage/kourier
   helm --kube-context kind-agent-kourier-full upgrade --install agent-kourier-sre-triage oci://ghcr.io/agentkourier/charts/agent-kourier \
     --version <version> --namespace agent-kourier -f sre-triage-configmap-values.yaml --set replicaCount=0
   kubectl --context kind-agent-kourier-full -n agent-kourier delete job first-token --ignore-not-found
   kubectl --context kind-agent-kourier-full -n agent-kourier create job first-token \
     --from=cronjob/agent-kourier-sre-triage-token-refresh
   kubectl --context kind-agent-kourier-full -n agent-kourier wait --for=condition=Complete job/first-token --timeout=120s
   helm --kube-context kind-agent-kourier-full upgrade --install agent-kourier-sre-triage oci://ghcr.io/agentkourier/charts/agent-kourier \
     --version <version> --namespace agent-kourier -f sre-triage-configmap-values.yaml
   ```

   Elsewhere, in place of the inline `config` of Install with Helm's values file:

   ```yaml title="sre-triage-configmap-values.yaml"
   config:
     source: files
     existingConfigMap: sre-triage-config
   rbac:
     secretNamespaces: [agent-kourier]
   ```

   ```sh
   kubectl -n agent-kourier create configmap sre-triage-config --from-file=examples/recipes/sre-triage/kourier
   helm upgrade --install agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
     -n agent-kourier -f sre-triage-configmap-values.yaml
   ```

   [Move the config between a ConfigMap and custom resources](../../../docs/how-to/migrate-config-source.md) moves it
   to custom resources later.

Then wait for the next alert on the receiver, or post a test alert as shown above. A thread opens under it and the
investigation streams in.

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

Two kinds of test data ship with the recipe. **Evals** are messages and what the Binding must make of each;
**scenarios** are questions about the seeded faults, with what a good answer names, for a person to read the agent's
answers against.

`go test ./test/recipes` loads `kourier/` with the loader Agent Kourier uses, resolves its references, and runs
`evals/triggers.yaml` through the trigger matcher that the intake and the dry run share. The cases are a firing alert
(also behind a `[prod]` cluster tag), a resolved one, a grouped one (`FIRING (3)`), the Watchdog heartbeat (excluded),
another receiver and a receiver whose name only starts with yours (both unmatched), Grafana's format (unmatched),
another bot, a person's message and the alert bot's reply inside a thread. They judge one message at a time: they do
not evaluate the thread per alert, the cooldown, the close on `RESOLVED` or the limits. The format is in
[Writing or changing a recipe](../README.md#writing-or-changing-a-recipe).

`go test ./test/kindmanifests` pins the agent's manifests: the tool list, the `view` binding, the read-only tool server,
the prompt, the digest-pinned images and the faults. It reads the files, not a cluster: no test runs the agent. To see
it run, bring up the sandbox tier.

`evals/scenarios/` holds one scenario for each Deployment in `agent/faults.yaml`: the five faults and the healthy app.
Nothing runs them yet, and **the shape is provisional**. `go test ./test/kindmanifests` checks that each file has the
shape, that there is one for every Deployment of `faults.yaml` and no other, and that each alert is the one the sandbox
fires for its app. One, as an example:

```yaml
name: checkout crashes at start
app: checkout                  # the Deployment of agent/faults.yaml
alert: KubePodCrashLooping     # an alert the sandbox fires for it, or `question:` with a person's question; exactly one
cause: '...'                   # what a good answer must name
evidence:                      # what it should cite
  - '...'
```

## Limits

- The agent half is as shipped only for the kind full tier (see Prerequisites). Nothing in this repository applies it to
  another cluster.
- An investigation is as good as the model and the prompt. The scenarios say what a good answer to each fault names;
  nothing grades an answer yet.
- The agent reads the cluster it runs in. It does not read your metrics or logs backend, and the tools it has are the
  six in `agent/agent.yaml`.
- Agent Kourier reads the message Alertmanager posts. Alertmanager's routes and receivers are untouched, so the alert
  still reaches people if Agent Kourier is down.

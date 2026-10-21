# Troubleshoot an install

For an operator whose install does not answer: each entry starts from something you can see, in the pod log or in Slack, and gives the
cause and the fix. They are the problems a new
install is likely to meet. For the install itself, see [Install with Helm](install-with-helm.md).

## Where to look

- **The Agent Kourier pod log.** One JSON object per line. `msg` names the event, and `error`
  carries the full reason when something failed. This is where most answers are.
- **The agent's own pod log.** Look here for model provider errors, such as a rejected key or
  an exhausted balance.
- **The agent runtime's controller log**, when the agent is on kagent.
- **Metrics.** Agent Kourier's own metrics start with `agentkourier_`. See
  [Metrics](../reference/metrics.md).

Keep the pod log open while you reproduce a problem. The `session turn` lines say what the
turn did and why it stopped.

When the pod restarts in a loop, read the log of the run that ended and its exit code:

```sh
kubectl -n agent-kourier logs deploy/agent-kourier --previous
kubectl -n agent-kourier describe pod -l app.kubernetes.io/name=agent-kourier   # Last State: Terminated, Exit Code
```

The last line of that log is plain text, not JSON: `agent-kourier:` and the reason it stopped, cut after 300 bytes.
The sections below go by exit code.

## Slack says the agent's task failed

For recognized context-limit, rate-limit, authentication, timeout or permission failures, the Slack notice gives a
fixed cause and next step. Other failures get a generic notice. Agent Kourier does not copy the task's raw status text
or arbitrary metadata into Slack or its own logs.

Copy the `AK-` troubleshooting reference from the Slack notice and search the Agent Kourier pod log for that exact
`failure_ref`. The matching `session turn ended` entry includes the bounded `failure_category`, full `task` ID and
`session` context ID when one is available. Use those IDs to find the corresponding agent or controller log entries;
the backend's own logs may have details Agent Kourier deliberately leaves out.

## The pod exits with code 1

```text
agent-kourier: slack rejected the tokens: invalid_auth
```

Slack refused a token for good, so Agent Kourier stops rather than retry. The word at the end is Slack's own code:
`invalid_auth`, `not_authed`, `token_revoked` or `account_inactive`. The usual cause is a token copied incompletely or
from another Slack app, or one that has been revoked since.

Agent Kourier checks the bot token before it opens the Socket Mode connection with the app token. At the default `info`
log level, if no `slack: socket mode connecting` line comes before the error, Slack refused the bot token; if one does,
look at the app token too. The dry run in [Check the values before you install](install-with-helm.md#check-the-values-before-you-install)
tests the bot token on its own: it exits 1 when Slack refuses it.

Replace the Secret with the right tokens, then restart rather than wait out the crash loop's back-off:

```sh
kubectl -n agent-kourier create secret generic agent-kourier-slack \
  --from-literal=botToken="$SLACK_BOT_TOKEN" --from-literal=appToken="$SLACK_APP_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n agent-kourier rollout restart deploy/agent-kourier
```

## The pod exits with code 2

```text
agent-kourier: the broker cannot be wired: v1 serves exactly one ChatConnection, and the config has 2 [agent-kourier/slack, agent-kourier/slack2]
```

The resource files loaded, but Agent Kourier cannot be put together from them, or an environment variable has a value
it cannot use. A restart fails the same way. The text after `the broker cannot be wired:` says which:

- `v1 serves exactly one ChatConnection, and the config has` a count other than 1: keep exactly one entry under
  `config.chatConnections`.
- `ChatConnection agent-kourier/slack: read botToken of Secret agent-kourier/agent-kourier-slack:` (or `appToken`): the
  key is in the Secret but its value is empty. `kubectl create secret --from-literal=botToken="$SLACK_BOT_TOKEN"` stores an
  empty value when the variable is not set in that shell. Check that both variables are set, and replace the Secret as
  for [code 1](#the-pod-exits-with-code-1).
- A variable's name, its value and what it wants, such as
  `AGENTKOURIER_AUDIT_RETENTION is "1h", want keep, or a duration of at least 24h0m0s such as 8760h`. The chart sets these
  variables from its values (`auditRetention` here) and from `extraEnv`: fix the value there.

A misspelt field, or a Secret that is missing or lacks a key, also exits 2, with `load config` after
`the broker cannot be wired:`: see [The pod exits with code 2 and the error says `load config`](#the-pod-exits-with-code-2-and-the-error-says-load-config).

## The pod exits with code 3

The API server refused the pod access to Secrets. The pod reads the Secrets its config names
through an informer, and a `forbidden` or `unauthorized` answer ends the start at once with
`the API server denied access to Secrets` and exit code 3.

Check the grant as the pod's ServiceAccount:

```sh
kubectl auth can-i list secrets -n agent-kourier --as system:serviceaccount:agent-kourier:<serviceaccount>
kubectl auth can-i watch secrets -n agent-kourier --as system:serviceaccount:agent-kourier:<serviceaccount>
```

The chart grants `list` and `watch` in each namespace that holds a Secret the config names, and `rbac.secretNamespaces`
adds more. A `config.existingConfigMap` needs `rbac.secretNamespaces`, because the chart cannot
read the ConfigMap to find the namespaces.

## The pod exits with code 2 and the error says `load config`

```text
agent-kourier: bootstrap: the broker cannot be wired: load config: binding-my-binding.yaml#1 Binding agent-kourier/my-binding: spec.chat.chanel: unknown field
binding-my-binding.yaml#1 Binding agent-kourier/my-binding: spec.chat.channel: required
```

A resource file did not load. Each problem is on a line of its own: the file and the number of the document in it, the
resource, the field, and what is wrong with it. With a ConfigMap the chart writes, the file is named after the
resource's kind and name. The usual problems:

- `unknown field`: a misspelt or misplaced field. The chart passes each `spec` through as written, so `helm lint` and
  `helm template` accept it, and it shows only here, as a pod that restarts in a loop.
- `required`, or a value that does not match its pattern, such as a channel name where the channel ID belongs.
- `not found` on a reference: a Binding's `connectionRef` or `backendRef` that names no resource, or a Secret that
  does not exist in that namespace.
- `has no key "botToken"` (or `appToken`): the Slack Secret exists, but not with these key names. They are case
  sensitive.

The line is cut after 300 bytes, so with several problems you may see only the first. For the problems in the files
themselves, run `agent-kourier dry-run` against the rendered values, as in
[Check the values before you install](install-with-helm.md#check-the-values-before-you-install): it prints every one
in full and needs no cluster. It does not check references or Secrets, so for those, read the names in the error
against `kubectl -n agent-kourier get secret`.

## The pod exits with code 4 and never becomes ready

```
agent-kourier: bootstrap: start the secret resolver: secret caches not synced for namespaces ["agent-kourier"]: context deadline exceeded
```

The pod cannot reach the Kubernetes API server, so the informer never syncs and the wait ends
at its deadline. A refused grant looks different. It exits 3.

The usual cause is the NetworkPolicy, on Cilium. The chart asks for the control plane's address
in `networkPolicy.kubeApiServer.cidrs` and renders it as an `ipBlock`, which works on kind.
By default Cilium does not match a node address that way. It treats the API server as the
`kube-apiserver` entity. The `policy-cidr-match-mode` setting in `cilium-config` changes that,
and it can be empty on a new install. Add a policy that allows the entity:

```yaml
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: agent-kourier-kube-apiserver
  namespace: agent-kourier
spec:
  endpointSelector:
    matchLabels:
      app.kubernetes.io/name: agent-kourier
  egress:
    - toEntities:
        - kube-apiserver
```

It adds to the chart's policy, so DNS, Slack and the agent stay fenced as before.

To confirm before you change anything, run two throwaway pods in the namespace. Give one the
pod labels of the Deployment, `app.kubernetes.io/name: agent-kourier` and
`app.kubernetes.io/instance: <release>`, because the chart's policy selects on both. Give the
other no labels. Both call `https://kubernetes.default.svc/version`. The labelled pod times out
and the unlabelled one gets a 200. Only a policy does that.

## The thread says "I can't reach the agent right now, and I'm trying again."

Agent Kourier could not connect to the agent, or the send failed, and it is retrying with
backoff. The pod log has the reason in the `error` field of `session turn failed`. Find the
text below in it.

### `agent card lists no JSON-RPC interface`

The agent's card offers no JSON-RPC transport that Agent Kourier can use. There are two
causes.

- The agent serves an A2A 0.3 card, which has a `url` and a `preferredTransport` and no
  `supportedInterfaces`, and the image is older than the one that reads 0.3 cards. kagent
  0.9.x does this. Use a newer image that reads A2A 0.3 cards.
- The card lists only other transports, gRPC or REST. Agent Kourier uses JSON-RPC.

### `HTTP 307 Temporary Redirect`

The agent's router redirects a POST to the same path with a trailing slash. The kagent 0.9.x installs this was
written against did.
Agent Kourier follows no redirects, because a redirect could carry the Binding's token
somewhere else. Write the slash on the AgentBackend's `url`. The `a2a` dialect sends to the
URL exactly as written.

### `HTTP 401` or `HTTP 403` at the card fetch

The front door refused the Binding's token. The turn is not retried: the thread gets "This request failed, and I won't
try it again" with the detail that the agent's front door refused the Binding's credentials, the log's
`session turn failed` line carries the reason, and `agentkourier_credential_rejections_total` counts it. Check the Secret named by `identity.tokenSecretRef`
and the front door's own configuration.

### The host does not resolve, or the connection times out

Check the Service name first, with `kubectl get svc -n <namespace>`. Argo CD names the Helm
release after the Application, so an Application called `my-kagent` produced
`my-kagent-controller`, not `kagent-controller`. Then check the egress rule:
the chart's NetworkPolicy allows the agent only through `networkPolicy.extraEgress`.

## The mention gets no reply and the log shows no `session turn started`

Agent Kourier never saw the message. Check, in this order:

1. The bot is a member of the channel. An invite is separate from the app being installed.
2. The Binding's `chat.channel` is the channel ID (`C…`; `G…` for an older private channel), not the name.
3. The app subscribes to `app_mention`, `message.channels` and `message.groups`, and Socket
   Mode is on.
4. Only one process uses the app token. A second consumer receives a share of the events.
   The `slack: socket mode connected` line has a `num_connections` field. A fresh start
   can read 8 with no other consumer and replies still arriving, so treat a
   number above 1 as a reason to look and not as proof.

## An agent on kagent 0.9.x answers, but the answer can appear twice

A 0.3 agent has no task list, so Agent Kourier cannot ask whether the agent already has a turn
before it sends the turn again. After a restart that interrupted a turn, or after a send
whose outcome was unknown because the stream dropped before the first event, the turn may run
a second time and post a second answer. For a read-only agent that wastes a model call and
nothing else. An agent that changes things can do the change twice. Slack tool approvals need `kagent-v1`, or `a2a` with
kagent's human-in-the-loop extension; for any other agent, its own tool list and the role behind its tools are the only
fence on a write. See [Connect a generic A2A agent](connect-a2a-agent.md#what-works-on-this-dialect).

## `Multi-Attach error for volume` while the pod rolls

The SQLite volume is `ReadWriteOnce` and the pod's strategy is `Recreate`. The event usually
clears within a minute, while the storage driver detaches the volume from the old node. If it
persists, the volume is still attached to the old node. Look at the `VolumeAttachment` for the
claim's volume.

## The agent answers, but tool calls do not show as step cards

Agent Kourier shows a tool call as a step card only when the agent marks the call as Google ADK or kagent does, on any
dialect: a data part whose metadata has `adk_type`, `kagent_type` or `kagent.dev/a2a/part-type` set to `function_call` or
`function_response`, and whose data has a `name`. An agent that reports its tool calls some other way, or in a part
without a `name`, shows none; the answer text still renders. The marked part must be in an artifact, or in a status
update's message from the agent while the task is `working`; see
[Tool calls and results](../reference/agent-contract.md#tool-calls-and-results). With the default `chat.progress`
(`steps-and-activity`), a `Working…` card still appears in a chat thread when 3 seconds pass with no new text; see
[Output](../explanation/threads-and-sessions.md#output).

## An alert thread shows no `is working…` status

Agent Kourier sets Slack's assistant status only on a turn whose reply does not stream, which on Slack is an alert
thread's, with `output: live` and `chat.progress` set to `steps-and-activity` (the default) or `activity`. A chat thread,
whose reply streams, never gets it, and neither does a Binding with `progress: steps` or `output: final`.

If Slack refused the call with a code that says the app, token or workspace cannot use the method, Agent Kourier logs this
once, at info level, with the connection (`conn`) and Slack's `code`:

```text
slack: Slack refused assistant.threads.setStatus, so threads are not shown as busy on this connection until restart
```

From then on it sets no status on any thread of that connection, and calls Slack for none, until its process restarts.
The usual cause is a missing scope: add `assistant:write` to the app's bot token scopes, reinstall the app, then restart
Agent Kourier ([Configure Slack](configure-slack.md#what-the-app-is-allowed-to-do) has the codes and what was tested).

Any other failure stops the status for that turn only, and the turn goes on. It is logged at warn level, with the error in
`err`:

```text
live: marking the thread as busy failed; the turn goes on without it
```

If taking the status off at the end of a turn fails, the warning is
`live: taking the busy mark off the thread failed; it stays until the next message in the thread`.
`agentkourier_chat_api_calls_total{operation="SetThreadStatus"}` counts the calls by outcome.

In our testing (October 2026, Slack's desktop web client; mobile not checked), Slack showed the status only to clients
that were open when it was set, and a reply the bot posted in the thread removed it. This is observed behaviour, not checked
against Slack's documentation. Agent Kourier sets the status again after each write of the message but the last, and clears it when the turn ends.

## The kagent install uses far more memory than expected

The chart's defaults run a UI, several MCP servers and ten bundled agents, about 2.3 GiB of
memory requests. Turn off what you do not use, and turn off the bundled `kagent-tools`, which
the chart binds to `cluster-admin`. See
[Connect a kagent agent](connect-kagent.md).

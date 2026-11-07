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
  [Metrics](../reference/metrics.md). `agentkourier_binding_ready` says whether a Binding
  [can be served](#a-binding-is-not-ready).

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

## The thread says the agent sent more than Agent Kourier can read or hold

```text
the agent sent more than Agent Kourier can read or hold, so the turn stopped
```

Agent Kourier reads a bounded amount of what an agent sends: 2 MiB of one event, 4 MiB of the answer to a call such as `GetTask`, and 341 KiB of the reply it holds for a
turn. The agent sent more than one of them, so the turn stopped, and no setting raises any of them. The sentence is the same whichever bound it was, and whatever the reply's size:
it names no host and no size. What the thread had shown stays, and Agent Kourier cancels the task at the agent, once, when the reply named a task. A reply that is one message and no task, or a send whose
first event was too large, names none, and nothing is cancelled. The turn of a person's answer to a paused task says it with a sentence of its own, `what the agent sent after the answer was more than Agent Kourier can read or hold, so the turn stopped and its task is being cancelled. The agent may have acted on the answer.`: Agent Kourier
cancels the task that was answered, and cannot tell whether the agent took the answer, as what it sent after it was not read.

What it leaves in the log, the audit log and the metrics. The lines below name the `binding` and the thread's `key`, except the ERROR `session turn failed` of the runner, which has the `key` and the `error`:

- WARN `session turn stopped: what the agent sent was more than Agent Kourier can read or hold`, with `task`, `turn` and an `error` that names the bound. When the cancel did not
  take, it is ERROR `session turn failed` with the same `error`, and WARN `session: could not cancel a task whose reply was too large; it stays recorded`.
- INFO `session turn ended` with `outcome=response_too_large`, and the counter `agentkourier_turn_outcomes_total{outcome="response_too_large"}`, which you can alert on.
- An audit row `cancel: failed response_too_large` when the cancel's own answer was too large to read. Agent Kourier counts that cancel as done and clears the task's record, **without verifying it**: a real a2a-go
  server answers every cancel with the whole task (shown in a test, not confirmed on kagent), but an error with a large body reads the same. A WARN says so: `session: the answer to a cancel was too large to read, so the cancel is counted as done without being verified`.

Read the `error` to find which bound it was:

| The `error` says | What passed it |
|---|---|
| `an agent card of more than 1048576 bytes` | Not an answer: the agent card, which is read when a connection is opened. Nothing was sent and nothing was read of any task; see below. |
| `an event of more than 2097152 bytes` | One event of the stream: a tool result, a chunk of text, or the final task, which holds the whole turn. |
| `more than 4194304 bytes` | The answer to a call: the task read back with its history after a lost stream or a restart, the answer to a cancel, or a page of the list of tasks. |
| `a reply of more than 349184 bytes` | The text the turn holds: the artifacts' text, tool results that wait for their call, and the bookkeeping of each tool part. |

**Few words shown, many tool steps before them, and `an event of more than 2097152 bytes`: the tool output added up.** On kagent 1.0 the final task holds every tool call and result of the turn
again in its artifacts (read in `test/fixtures/kagent/approval/02-approve.response.sse`), and it arrives as one event. No single result has to be large: in a test with results of 300 KiB,
results adding up to 1800 KiB passed, and eight of them, 2400 KiB, ended the turn at the final event with a reply of a few bytes. Add up what the agent's tools returned in the turn (its own log has it).

A card that is too large is told apart from these: a follow, or the read of a task that comes before it, that meets it says `Agent Kourier could not connect to the agent: its agent card is too large to read, so the turn stopped. The task stays at the agent. Once the card is fixed, restart Agent Kourier.`, cancels nothing, and leaves
the task recorded and followed by no one (the turn outcome is `refused`, as for any connection that fails on a follow). A lookup or a send that meets it is retried like any that could not reach the agent. After a restart, recovery looks up the queued reply of a thread that has a recorded task before it follows that task, and a card that is too large fails that lookup: it is asked again after each backoff, with no limit, and the task is not followed meanwhile. It logs one WARN, `recover: the agent's card is too large to read, so the queued head cannot be looked up; it is asked again until the card is fixed`, with the `head`, `task` and `binding`: once for each thread in a run of recovery, whenever the card is first met, so again after each restart and each time a process becomes the leader, and not for each try. The Binding's `BackendReachable` condition names the card.
Trim the card. A task that a follow met the card on is followed by no one, nothing follows it again by itself, and no control of the thread takes it up: once the card is fixed, **restart Agent Kourier**. Recovery then reads the recorded task and follows it, and a reply that waited behind it goes out
when the task ends. A restart interrupts every turn of every tenant on that broker while it runs. Until then the reply stays queued. The lookup that recovery makes before it follows a task (above) is the exception: it is asked again until it answers, so once the card is fixed it does, and the task is followed without another restart.

**What to do.** Have the agent's tools return what the model needs: filter, summarise or page a result, and cap it in the tool. Keep a task's tool output well under 2 MiB in all.
If the `error` says `more than 4194304 bytes` after a restart, the task's history has grown past what is read; trim the tools the same way. Agent Kourier cannot read it, so it cancels it.

## The thread says Agent Kourier could not tell if the agent already started this message

```text
Agent Kourier could not tell if the agent already started this message, as its task list could not be read in full, so it did not send it. The agent may be running it. To run it, post it again.
```

Before it sends a message again (after a restart, or when the outcome of the first send was unknown), Agent Kourier lists the tasks of the thread's context and looks
for one that holds the message. Each task in a page of that list carries its history, and a page of more than 4 MiB is not read: Agent Kourier asks again at half the size, down to
three tasks. The walk is bounded as a whole: **five minutes**, **200 pages**, and a page token that comes again ends it. A server that does not answer at the size the walk lowered it to
ends it too, since whether a page token can be continued at another size is not known. If the list cannot be read in full, and no page before it held the message, Agent Kourier does not know whether the agent has it, and it does not send it, because a message sent again
into a context is not deduplicated and the agent could run it twice. The agent may be running it. The same sentence is shown whichever of these it was; the log's `error` says which.
A reply that Agent Kourier queued itself, and that the agent refused as busy or not ready or that was held behind a task and never sent, is not failed: the agent does not have it, so it is sent when the agent is
free (a WARN says the list could not be read and the agent has not taken the turn).

What it leaves:

- WARN `session turn failed`, with `reason` set to the sentence above, the `binding`, and an `error` that says why: for a page past the bound it ends with `response from the backend is too large: more than 4194304 bytes`; for the others, `took more than 5m0s`,
  `ran past 200 pages`, `repeats page token`, or the failure at the lowered page size.
  After a restart there is a WARN `recover: the agent's list of tasks could not be read in full, so the queued head is not looked up` first.
- An audit row `find_task: failed lookup_incomplete`, with `response_too_large` or `deadline_exceeded` after it when that was the cause, and the context and message IDs as its subject.
- The turn outcome `lookup_incomplete`: `agentkourier_turn_outcomes_total{outcome="lookup_incomplete"}`. Any increase means a message was not sent for this reason, which is worth an alert.

**What to do.** Look at the agent for a task for that message (for kagent, its task list for the context). If there is one, it is running or has ended and the thread needs nothing more. If there is not, post the message again.
Then keep the thread's tasks small, since their histories are what made the list large: trim the tools' output, or start a new thread for a new investigation. An agent that cannot list tasks
(A2A 0.3, kagent 0.9.x) does not give this notice: Agent Kourier sends the turn without asking, and it may run twice (see [below](#an-agent-on-kagent-09x-answers-but-the-answer-can-appear-twice)).
The five minutes and the 200 pages are constants of the binary (`agent.DefaultLookupTimeout` and `maxListPages`), as the other bounds are; no setting changes them.

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
kubectl -n agent-kourier create secret generic slack \
  --from-literal=botToken="$SLACK_BOT_TOKEN" --from-literal=appToken="$SLACK_APP_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n agent-kourier rollout restart deploy/agent-kourier
```

## The pod exits with code 2

```text
agent-kourier: the broker cannot be wired: v1 serves exactly one ChatConnection, and the config has 2 [agent-kourier/slack, agent-kourier/slack2]
```

The resources loaded, but Agent Kourier cannot be put together from them, or an environment variable has a value
it cannot use. A restart fails the same way. The text after `the broker cannot be wired:` says which:

- `v1 serves exactly one ChatConnection, and the config has` a count other than 1: this is the files route
  (`config.source: files`), whose directory must hold exactly one. Keep exactly one entry under `config.chatConnections`. With
  `config.source: crds` a cluster may hold many, and the broker serves the one it is told to, so this message does not occur.
- `ChatConnection agent-kourier/slack was not found: AGENTKOURIER_CHAT_CONNECTION names the one connection this broker serves, and it must exist`:
  the custom resource route. The ChatConnection that `config.chatConnection` names is not in the cluster. Apply it, or name the one that exists. The pod
  restarts until it does, and starts at the next restart after it exists. `ChatConnection agent-kourier/slack is invalid` says why instead, in the
  same way as the files' `load config` problems below.
- `the custom resource definitions are not installed`: the custom resource route, and the CRDs are not in the cluster. Install them first, as in
  [Move the config between a ConfigMap and custom resources](migrate-config-source.md#1-install-the-crds-still-on-the-configmap-route).
- `AGENTKOURIER_CHAT_CONNECTION is required with AGENTKOURIER_CONFIG_SOURCE=crds`, `AGENTKOURIER_CHAT_CONNECTION is not namespace/name (its value is not shown)`, `ChatConnection is not a namespace that is a DNS-1123 label and a name that is a DNS-1123 subdomain ...`,
  `is set with AGENTKOURIER_CONFIG_SOURCE=files, which serves the one ChatConnection of its directory: unset it`, and
  `AGENTKOURIER_CONFIG_SOURCE is "...", want files or crds`: the variables disagree. The chart sets them from `config.source` and
  `config.chatConnection`; check `extraEnv` for one of your own.
- `ChatConnection agent-kourier/slack: read botToken of Secret agent-kourier/slack:` (or `appToken`): the
  key is in the Secret but its value is empty. `kubectl create secret --from-literal=botToken="$SLACK_BOT_TOKEN"` stores an
  empty value when the variable is not set in that shell. Check that both variables are set, and replace the Secret as
  for [code 1](#the-pod-exits-with-code-1).
- A variable's name, its value and what it wants, such as
  `AGENTKOURIER_AUDIT_RETENTION is "1h", want keep, or a duration of at least 24h0m0s such as 8760h`. The chart sets these
  variables from its values (`auditRetention` here) and from `extraEnv`: fix the value there.

With the files route, a misspelt field, or a Secret that is missing or lacks a key, also exits 2, with `load config` after
`the broker cannot be wired:`: see [The pod exits with code 2 and the error says `load config`](#the-pod-exits-with-code-2-and-the-error-says-load-config).
With custom resources that never stops the broker: a bad object is left out and says so in its status (see
[A Binding is not accepted](#a-binding-is-not-accepted)), except the broker's own ChatConnection, above.

## The pod exits with code 3

The API server refused the pod access to Secrets, or, with `config.source: crds`, to the custom resources. The pod reads
each Secret its config names through an informer of its own. What a refusal does depends on the route:

- **Files (`config.source: files`).** A `forbidden` or `unauthorized` answer for any Secret the config names ends the start at
  once with `the API server denied access to Secrets` and exit code 3. The same answer for a Secret that a later config change names does not
  stop the pod: that reload fails with the same message, the log says `config reload failed; keeping the last good snapshot`, and the last good
  config stays in force.
- **Custom resources (`config.source: crds`).** Only the Secrets the broker itself needs, the connection's tokens and the Postgres DSN's, end the
  start with exit code 3, as does a refusal to list the custom resources. A Secret that a team's Binding or AgentBackend names, and that the
  ServiceAccount may not read, stops nothing and fails no reload: the Binding stays served, and the log says its `BackendReachable` condition is
  false with the reason `CredentialsUnreadable`. The condition's `message` names the Secret when it is in the Binding's namespace, and says that Agent Kourier's ServiceAccount needs `list`
  and `watch` on it. The fix is a grant for that Secret: a Role and RoleBinding in the Secret's namespace for the ServiceAccount, which the team that owns the
  Secret makes ([Let a team add its own Binding](migrate-config-source.md#let-a-team-add-its-own-binding) has the two commands), or `rbac.secretNames` in the
  chart's values (namespace to Secret names), which the platform team sets.

Check the grant as the pod's ServiceAccount:

```sh
kubectl auth can-i list secrets -n agent-kourier --as system:serviceaccount:agent-kourier:<serviceaccount>
kubectl auth can-i watch secrets -n agent-kourier --as system:serviceaccount:agent-kourier:<serviceaccount>
```

A Role that names its Secrets (`resourceNames`, which the chart writes with `config.source: crds`, or you write yourself with `rbac.create: false`) answers `no` to
those two. Ask about one Secret instead:

```sh
kubectl auth can-i list secrets/<secret-name> -n agent-kourier --as system:serviceaccount:agent-kourier:<serviceaccount>
kubectl auth can-i watch secrets/<secret-name> -n agent-kourier --as system:serviceaccount:agent-kourier:<serviceaccount>
```

The chart grants `list` and `watch` in each namespace that holds a Secret the config names (for the Secrets named, with
`config.source: crds`, which also names the served ChatConnection's Secret, and takes `rbac.secretNames` for the other ones the chart cannot see), and `rbac.secretNamespaces` adds more. A `config.existingConfigMap` needs `rbac.secretNamespaces`, because the chart cannot
read the ConfigMap to find the namespaces.

## The pod exits with code 2 and the error says `load config`

```text
agent-kourier: bootstrap: the broker cannot be wired: load config: binding-my-binding.yaml#1 Binding agent-kourier/my-binding: spec.chat.chanel: unknown field
binding-my-binding.yaml#1 Binding agent-kourier/my-binding: spec.chat.channel: required
```

This is the files route (`config.source: files`): one resource file did not load, so none did. Each problem is on a line of its own: the file and the number of the document in it, the
resource, the field, and what is wrong with it. With a ConfigMap the chart writes, the file is named after the
resource's kind and name. The usual problems:

- `unknown field`: a misspelt or misplaced field. The chart passes each `spec` through as written, so `helm lint` and
  `helm template` accept it, and it shows only here, as a pod that restarts in a loop. (With custom resources the API server refuses
  it when Helm applies it: see [Unknown fields](../explanation/config-routes.md#unknown-fields).)
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
agent-kourier: bootstrap: start the secret resolver: secret caches not synced for [agent-kourier/slack-main]: context deadline exceeded
```

The list names the Secrets whose caches did not sync. The pod cannot reach the Kubernetes API server, so the informers never sync and the wait ends
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

### `HTTP 401` at the card fetch

The front door refused the Binding's token. A send is not retried: the thread gets "This request failed, and I won't
try it again" with the detail that the agent's front door refused the Binding's credentials, the log's
`session turn failed` line carries the reason, and `agentkourier_credential_rejections_total` counts it. Check the Secret named by `identity.tokenSecretRef`
and the front door's own configuration. An HTTP 403 on an A2A call, after the card was fetched, is handled the same way.

A lookup before a resend goes differently: it is not counted, the Secret is not read again, and the lookup is retried. The
thread says "I can't reach the agent right now, and I'm trying again", and the turn fails as unreachable after 15 minutes.

### `HTTP 403` at the card fetch

The agent does not exist, or does not permit the Binding's identity. kagent 1.0.0-alpha5 answers both with 403, so a
misspelt `agent.name` or `agent.namespace` in the Binding gets this status; kagent 0.9.x answers an unknown agent with 404.
A send is not retried: the thread gets "This request failed, and I won't try it again" with the detail that the agent was
not found, or does not permit the Binding's identity, and the log's `session turn failed` line carries the reason. Other
requests go differently:

- An answer by a button leaves the pause open, and the thread is told the answer could not be delivered.
- A typed answer to a question on the `a2a` dialect waits and is tried again, with nothing said in the thread.
- A task already under way is not followed again, and the thread is told the agent refused to continue it. After a restart
  with a reply queued behind that task, recovery asks about the reply first and keeps asking, with nothing said in the thread.
- A lookup before a resend is retried, because the agent may already have the turn. The thread says "I can't reach the agent
  right now, and I'm trying again", and the turn fails as unreachable after 15 minutes. A turn held behind a task of the
  session, or one this process knows the agent took and did not record, is retried with no limit.

None of these is counted as a rejected credential. Check the Binding's `agent.name` and
`agent.namespace`, then the rules that let the Binding's identity use the agent, in kagent and in the front door.

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

## A Binding is not accepted

With the config read from custom resources, `kubectl get akbinding -A` shows `False` in `READY` and `Accepted:<reason>` in `REASON`. This is
the broker saying that it did not take the Binding as you wrote it. [Binding status](../reference/binding-status.md) explains the conditions. Read the
reason and its message, which names the field:

```sh
kubectl -n team-a get akbinding team-a-chat -o yaml
kubectl -n agent-kourier logs deploy/agent-kourier | grep 'config: '
```

The log of the leader, which the operator reads, has `config: a Binding is not accepted as written` with the `reason` and the `problem`, and
`config: an object is refused` for an AgentBackend or ChatConnection that is invalid. `agentkourier_config_binding_rejections{reason="..."}`
counts the Bindings in each reason, and `agentkourier_config_invalid_objects{kind="..."}` counts the invalid objects. With the files route none of this
applies: a problem in one resource stops the whole config, as in [The pod exits with code 2 and the error says `load config`](#the-pod-exits-with-code-2-and-the-error-says-load-config).

A Binding with no status at all, and empty `READY` and `REASON` columns, is either new (the broker writes its status within about 10 seconds) or names a ChatConnection
that exists and that no broker serves: nothing reads it. Point its `connectionRef` at the connection a broker serves, which is in that broker's
`AGENTKOURIER_CHAT_CONNECTION`.

A Binding that is refused after an edit keeps running as it was: `message` says `serving generation 1`, and the other conditions describe that
generation, while `Ready` is `False` with `Accepted:InvalidSpec`. A Binding with no earlier generation to serve is not served at all.

| `reason` | What happened | What to do |
|---|---|---|
| `InvalidSpec` | The Binding fails a check of the broker. `message` names each field and the problem, for example `spec.chat.triggers[1]: duplicate of spec.chat.triggers[0]`. | Fix the field. The next apply is a new generation and is checked again. `kubectl apply --dry-run=server` catches what the schema checks, and the rules only the broker has (see [What is checked, and where](../explanation/config-routes.md#what-is-checked-and-where)) show only here. |
| `BackendNotFound` | `spec.agent.backendRef` names an AgentBackend that does not exist: `AgentBackend team-a/nope not found`. | Check the name, and the namespace, which defaults to the Binding's. |
| `BackendRejected` | The AgentBackend it names is invalid. If it had a good generation, that one is what runs, and `message` says which. | Fix the AgentBackend. In your own namespace `message` carries its problems. For one of another namespace it does not, so ask its owner. |
| `ConnectionNotFound` | `spec.chat.connectionRef` names a ChatConnection that does not exist. No broker owns the Binding, so the leader of any broker that sees it writes this. | Check the name and the namespace. |
| `ConnectionRejected` | The ChatConnection it names is invalid, as for `BackendRejected`. | Ask the owner of the ChatConnection. |
| `NamespaceNotAllowed` | The AgentBackend or ChatConnection is in another namespace and its `allowedNamespaces` does not list the Binding's. The answer is the same whether the object is valid or not. | Ask the owner to list your namespace, or use an object of your own. |
| `ChannelConflict` | Another Binding on the same ChatConnection holds the channel. The older one holds it, by `creationTimestamp`. If that Binding is in another namespace, `message` says only `a Binding in another namespace`. | Choose another channel. To find the holder, the operator reads the log line: it names it. [Who holds a channel](../explanation/config-routes.md#who-holds-a-channel) says how it is decided. |

## A Binding is not ready

`agentkourier_binding_ready{binding="<namespace>/<name>"}` is 0, or the leader's log has a line that starts with `binding verification:`.

The leader checks each Binding of the connection it serves: it fetches the agent's card, it reads the channel as the bot, and, for a Binding that asks for tool approvals from user groups, it reads those groups. [Binding verification](../reference/metrics.md#binding-verification) says when. The result only reports: turns still run for a Binding that is not ready, and `/readyz` does not change.

The metric names the condition, and the log line has the reason:

```sh
curl -s localhost:8080/metrics | grep -E '^agentkourier_binding_condition.*status="(false|unknown)"} 1'
kubectl -n agent-kourier logs <leader pod> | grep 'binding verification:'
```

Port-forward to the leader's pod first, as in [Check the leader](high-availability.md#4-check-the-leader): a standby exports none of these series, and its log has none of these lines. A line has `binding`, `condition`, `status`, `reason` and `detail`, and `cause` when a request or read failed, and is written when the result changes, not at each probe. If it has left the log, a restart of the leader logs each result that is not true again.

If you own a Binding and cannot read the broker's log, its status says only that the request for the card or the read of the channel failed, and that "the operator's log has the cause". That is on purpose: the cause names hosts, addresses and ports, which can be another team's, so a status never carries it, and it does not say whether a name did not resolve, a connection was refused or a certificate was not trusted. Ask whoever runs Agent Kourier to read the `cause` of the line for your Binding.

One failed probe holds the Binding at 0 until the next pass, up to about 5.5 minutes later, so a result during an agent's rollout can clear by itself. A fixed Secret or config is checked again sooner: a change starts a pass, at most one every 30 seconds.

### `BackendReachable`

| `reason` | Status | What happened | What to do |
|---|---|---|---|
| `CredentialsUnreadable` | false | The Binding's token, or a Secret behind a header of the backend, cannot be read, or the Binding requires a token and none was set. The `message` says which: the Secret `does not exist`, has `no key`, a key that `is empty`, or `holds a control character`, is `not of type Opaque`, `cannot be read: the API server denied` the ServiceAccount, with the grant it needs (only when the API server answered forbidden), or `cannot be read now; the operator's log has the cause`. It names the Secret and the key only when the Secret is in the Binding's namespace. A header's Secret in the AgentBackend's namespace, when that is another namespace, is not named: the message says only that it cannot be read, and the leader's log has the `cause`. | Do what the `message` says. For a denial, grant Agent Kourier's ServiceAccount `list` and `watch` on that Secret in the Binding's namespace. Otherwise check the Secret named by `identity.tokenSecretRef` or the header's `valueSecretRef`: it must exist, be type `Opaque` and have the key, and the value must not be empty or hold a control character, such as the trailing newline of `--from-file`. |
| `Unreachable` | false | The request for the card failed (`cause` has its error), or the backend did not answer within 8 seconds. | See [The host does not resolve](#the-host-does-not-resolve-or-the-connection-times-out). A backend that takes longer than 8 seconds to serve its card reads this too, although a turn's connect waits up to 30 seconds. |
| `CredentialsRejected` | false | The backend answered 401. It rejected the token, or the Binding has none and the backend needs one. | See [`HTTP 401`](#http-401-at-the-card-fetch). |
| `AgentForbidden` | false | The backend answered 403: the agent is not found, or not permitted for this identity. kagent's front door answers this for an agent that does not exist. | See [`HTTP 403`](#http-403-at-the-card-fetch). |
| `AgentNotFound` | false | The backend answered 404. | Check `agent.namespace` and `agent.name`, and the AgentBackend's `url`. |
| `AgentNotReady` | false | The backend answered 409. kagent's code answers this for an agent that is not ready (read from its code, not measured). | Check the agent's pod. |
| `BackendError` | false | The backend answered 5xx, or a 2xx other than 200. | Check the logs of the agent and the front door. |
| `UnexpectedStatus` | false | The backend answered a 3xx, often a redirect to a login page, or a 4xx no other reason names. Agent Kourier follows no redirects. | See [`HTTP 307 Temporary Redirect`](#http-307-temporary-redirect), and check that the `url` reaches the agent and not a login page. |
| `UnexpectedStatus` | unknown | The backend answered 408, 425 or 429, which says nothing about the agent. | Wait for the next pass. |
| `InvalidAgentCard` | false | The backend answered 200 with something that is not an agent card, such as a login page. | Check the AgentBackend's `url`. |
| `StreamingUnsupported` | false | The agent's card does not advertise streaming, which Agent Kourier requires. | Turn on streaming in the agent. |
| `UnsupportedExtension` | false | The agent's card requires an extension Agent Kourier does not speak. | Make the extension optional in the agent. |
| `NoJSONRPCInterface` | false | The agent's card lists no JSON-RPC interface. | See [`agent card lists no JSON-RPC interface`](#agent-card-lists-no-json-rpc-interface). |

### `ChannelJoined`

| `reason` | Status | What happened | What to do |
|---|---|---|---|
| `NotInChannel` | false | The bot is not in the channel. | Send `/invite @Agent Kourier` in it. What Slack answers for a private channel the bot is in has not been measured: if Slack leaves out the membership flag there, that channel reads this too. |
| `ChannelNotFound` | false | The channel does not exist, or the bot cannot see it, as with a private channel the bot is not in. | Check `chat.channel`, which is an ID, then invite the bot. |
| `ChannelArchived` | false | The channel is archived. | Unarchive it, or point the Binding at another channel. |
| `MissingScope` | false | The Slack app lacks the scope to read the channel. | Add `channels:read` and `groups:read` and reinstall the app. See [Configure Slack](configure-slack.md). |
| `ChatCredentialsRejected` | false | Slack rejected the bot's token. | Replace the token and restart Agent Kourier. |
| `ChatCredentialsChanged` | unknown | The ChatConnection's Secret holds other tokens than the ones in use, so the read was not made. | Restart Agent Kourier to use the new tokens. |
| `ChatCredentialsUnusable` | unknown | The Secret, or a key of it, is gone or empty, or the Secret is of a type that is never read. The connection keeps the tokens it read, and the read was not made. | Mend the Secret. A restart fails until you do. |
| `ChatUnavailable` | unknown | Slack rate-limited the read, did not answer in time, or the read failed (`cause` has why, such as `no_permission` or `connection refused`). | Wait for the next pass. If it stays, read the cause. |

### `ApproversReadable`

Only a Binding with `interactions.toolApprovals: true` and a non-empty `interactions.approverGroups` has this condition. The leader reads the groups in order, one request each, and stops at the first it cannot read, so `GroupNotFound` and `GroupsUnreadable` name that group
in the `message` by its place in the list and its ID (`spec.interactions.approverGroups[1] (S0123456789)`), and another bad group is named once this one is mended. (`MissingScope` names no group: the app cannot read any.) While the reason is `GroupNotFound`, `MissingScope` or `GroupsUnreadable`, a person's press on a tool approval of this Binding
ends with a fixed message and decides nothing, unless they are in a group earlier in the list, which the check reaches first. Under any other reason a press is kept and tried again, as it is under `ChatCredentialsRejected`, where it waits until the token is replaced.

| `reason` | Status | What happened | What to do |
|---|---|---|---|
| `GroupNotFound` | false | The group does not exist, is disabled, or the bot cannot see it. On Slack that is the code `no_such_subteam`; Slack's answer for a disabled group has not been measured. | Check the ID against `usergroups.list` (see [Configure Slack](configure-slack.md#what-the-app-is-allowed-to-do)), enable the group, or take it out of the list. |
| `MissingScope` | false | The Slack app lacks `usergroups:read`. | Add `usergroups:read` and reinstall the app. See [Configure Slack](configure-slack.md). |
| `GroupsUnreadable` | false | Slack refuses the read for a reason no retry mends and the two above do not name: a workspace whose plan has no user groups (`plan_upgrade_required`), a token of a type the method does not take (`not_allowed_token_type`), a token without the permission (`no_permission`), or an Enterprise grid the method is restricted on (`enterprise_is_restricted`). `cause` has Slack's code. Which code a workspace gets for each cause has not been measured. | Read the `cause`. A workspace with no user groups cannot use `approverGroups`: remove it, or change the plan. |
| `ChatCredentialsRejected` | false | Slack rejected the bot's token. | Replace the token and restart Agent Kourier. |
| `ChatCredentialsChanged` | unknown | The ChatConnection's Secret holds other tokens than the ones in use, so the read was not made. | Restart Agent Kourier to use the new tokens. |
| `ChatCredentialsUnusable` | unknown | The Secret, or a key of it, is gone or empty, or the Secret is of a type that is never read. The connection keeps the tokens it read, and the read was not made. | Mend the Secret. A restart fails until you do. |
| `ChatUnavailable` | unknown | Slack rate-limited the read, did not answer within the 8 seconds the read of all the groups is given, or the read failed (`cause` has why). | Wait for the next pass. If it stays, read the cause. |

### Any condition

| `reason` | Status | What happened | What to do |
|---|---|---|---|
| `ProbeTimeout` | unknown | The probe did not end within 10 seconds. | Wait for the next pass. If it stays, check the leader's log. |
| `ProbeFailed` | unknown | The check could not tell. The line is `a probe failed`, with the error, and it is written at each probe. | Read the `error`. |

### The status of a Binding keeps changing

With the config read from custom resources, the leader writes the status of each Binding it serves, and writes it back if it differs. If something else keeps changing it, the two go on at each other once each gap, and `agentkourier_binding_status_writes_total{result="put_back"}` climbs steadily. After three put-backs in a row, each within five gaps of the one before, the leader's log says, once for the Binding, `something else keeps changing the status of a Binding, and this broker keeps putting it back`, and names it.

What else writes a Binding's status is either another Agent Kourier that serves the same Binding, or a controller of your own. The first happens when two installs name the same ChatConnection (`AGENTKOURIER_CHAT_CONNECTION`): give each its own connection, so that each serves its own Bindings. For the second, look at the `managedFields` of the Binding (`kubectl get akbinding <name> -n <namespace> -o yaml --show-managed-fields`): the manager `agent-kourier` owns the conditions of the types `Accepted`, `BackendReachable`, `ChannelJoined`, `ApproversReadable` and `Ready`, and any other manager that writes them is the one to stop. A person's edit is put back once and is no fight. A controller that writes the status less often than once in five gaps stays below the three, and shows only in the metric. The numbers are on [Limits](../reference/limits.md#run-the-broker).

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
(`steps-and-activity`), a `Working…` card (a line in an alert thread's first post) still appears when 3 seconds pass
with no new text; see
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
A likely cause is a missing scope: add `assistant:write` to the app's bot token scopes, reinstall the app, then restart
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

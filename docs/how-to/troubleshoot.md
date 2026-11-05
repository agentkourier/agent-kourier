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
- `AGENTKOURIER_CHAT_CONNECTION is required with AGENTKOURIER_CONFIG_SOURCE=crds`, `AGENTKOURIER_CHAT_CONNECTION is "x", want namespace/name`,
  `is set with AGENTKOURIER_CONFIG_SOURCE=files, which serves the one ChatConnection of its directory: unset it`, and
  `AGENTKOURIER_CONFIG_SOURCE is "...", want files or crds`: the variables disagree. The chart sets them from `config.source` and
  `config.chatConnection`; check `extraEnv` for one of your own.
- `ChatConnection agent-kourier/slack: read botToken of Secret agent-kourier/agent-kourier-slack:` (or `appToken`): the
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

- **Files (`config.source: files`, the default).** A `forbidden` or `unauthorized` answer for any Secret the config names ends the start at
  once with `the API server denied access to Secrets` and exit code 3. The same answer for a Secret that a later config change names does not
  stop the pod: that reload fails with the same message, the log says `config reload failed; keeping the last good snapshot`, and the last good
  config stays in force.
- **Custom resources (`config.source: crds`).** Only the Secrets the broker itself needs, the connection's tokens and the Postgres DSN's, end the
  start with exit code 3, as does a refusal to list the custom resources. A Secret that a team's Binding or AgentBackend names, and that the
  ServiceAccount may not read, stops nothing and fails no reload: the Binding stays served, and the log says its `BackendReachable` condition is
  false with the reason `CredentialsUnreadable`. The fix is a grant for that Secret: `rbac.secretNames` in the chart's values (namespace to Secret
  names) for one the chart cannot see, or a Role and RoleBinding in the team's namespace for the ServiceAccount.

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
`config.source: crds`, which also takes `rbac.secretNames` for the ones the chart cannot see), and `rbac.secretNamespaces` adds more. A `config.existingConfigMap` needs `rbac.secretNamespaces`, because the chart cannot
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

A Binding with no status at all, and empty `READY` and `REASON` columns, is either new (the broker writes its status within a few seconds) or names a ChatConnection
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

The leader checks each Binding of the connection it serves: it fetches the agent's card, and it reads the channel as the bot. [Binding verification](../reference/metrics.md#binding-verification) says when. The result only reports: turns still run for a Binding that is not ready, and `/readyz` does not change.

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
| `CredentialsUnreadable` | false | The Binding's token, or a Secret behind a header of the backend, cannot be read. | Check the Secret named by `identity.tokenSecretRef` or the header's `valueSecretRef`. It must exist, be type `Opaque` and have the key, and the value must not be empty or hold a control character, such as the trailing newline of `--from-file`. |
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

### Either condition

| `reason` | Status | What happened | What to do |
|---|---|---|---|
| `ProbeTimeout` | unknown | The probe did not end within 10 seconds. | Wait for the next pass. If it stays, check the leader's log. |
| `ProbeFailed` | unknown | The check could not tell. The line is `a probe failed`, with the error, and it is written at each probe. | Read the `error`. |

### The status of a Binding keeps changing

With the config read from custom resources, the leader writes the status of each Binding it serves, and writes it back if it differs. If something else keeps changing it, the two go on at each other every 10 seconds, and `agentkourier_binding_status_writes_total{result="put_back"}` climbs steadily. After three put-backs in a row, each within about a minute (five gaps) of the last, the leader's log says, once for the Binding, `something else keeps changing the status of a Binding, and this broker keeps putting it back`, and names it.

What else writes a Binding's status is either another Agent Kourier that serves the same Binding, or a controller of your own. The first happens when two installs name the same ChatConnection (`AGENTKOURIER_CHAT_CONNECTION`): give each its own connection, so that each serves its own Bindings. For the second, look at the `managedFields` of the Binding (`kubectl get akbinding <name> -n <namespace> -o yaml --show-managed-fields`): the manager `agent-kourier` owns the conditions of the types `Accepted`, `BackendReachable`, `ChannelJoined` and `Ready`, and any other manager that writes them is the one to stop. A person's edit is put back once and is no fight. A controller that writes the status once a minute or more slowly stays below the three, and shows only in the metric.

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

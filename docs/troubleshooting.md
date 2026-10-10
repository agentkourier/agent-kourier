# Troubleshooting

Each entry starts from something you can see, in the pod log or in Slack, and gives the
cause and the fix. They come from the first real installs, so they are the problems a new
install is likely to meet. For the install itself, see [Run Agent Kourier on a
cluster](getting-started.md).

## Where to look

- **The Agent Kourier pod log.** One JSON object per line. `msg` names the event, and `error`
  carries the full reason when something failed. This is where most answers are.
- **The agent's own pod log.** Look here for model provider errors, such as a rejected key or
  an exhausted balance.
- **The agent runtime's controller log**, when the agent is on kagent.
- **Metrics.** Agent Kourier's own metrics start with `agentkourier_`. See
  [Observability](observability.md).

Keep the pod log open while you reproduce a problem. The `session turn` lines say what the
turn did and why it stopped.

## The pod exits with code 3

The API server refused the pod access to Secrets. The pod reads the Secrets its config names
through an informer, and a `forbidden` or `unauthorized` answer ends the start at once with
`the API server denied access to Secrets` and exit code 3.

Check the grant as the pod's ServiceAccount:

```sh
kubectl auth can-i list secrets -n agent-kourier --as system:serviceaccount:agent-kourier:<serviceaccount>
kubectl auth can-i watch secrets -n agent-kourier --as system:serviceaccount:agent-kourier:<serviceaccount>
```

The chart grants `list` and `watch` in each namespace the config names, and `rbac.secretNamespaces`
adds more. A `config.existingConfigMap` needs `rbac.secretNamespaces`, because the chart cannot
read the ConfigMap to find the namespaces.

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
and it was empty on the first install. Add a policy that allows the entity:

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
  0.9.x does this. Use an image built from source commit `fa0abc0b` or later.
- The card lists only other transports, gRPC or REST. Agent Kourier uses JSON-RPC.

### `HTTP 307 Temporary Redirect`

The agent's router redirects a POST to the same path with a trailing slash. kagent 0.9.x does.
Agent Kourier follows no redirects, because a redirect could carry the Binding's token
somewhere else. Write the slash on the AgentBackend's `url`. The `a2a` dialect sends to the
URL exactly as written.

### `HTTP 401` or `HTTP 403` at the card fetch

The front door refused the Binding's token. Check the Secret named by `identity.tokenSecretRef`
and the front door's own configuration.

### The host does not resolve, or the connection times out

Check the Service name first, with `kubectl get svc -n <namespace>`. Argo CD names the Helm
release after the Application, so an Application called `cluster-gitops-kagent` produced
`cluster-gitops-kagent-controller`, not `kagent-controller`. Then check the egress rule:
the chart's NetworkPolicy allows the agent only through `networkPolicy.extraEgress`.

## The mention gets no reply and the log shows no `session turn started`

Agent Kourier never saw the message. Check, in this order:

1. The bot is a member of the channel. An invite is separate from the app being installed.
2. The Binding's `chat.channel` is the channel ID that starts with `C`, not the name.
3. The app subscribes to `app_mention`, `message.channels` and `message.groups`, and Socket
   Mode is on.
4. Only one process uses the app token. A second consumer receives a share of the events.
   The `slack: socket mode connected` line has a `num_connections` field. The first install
   read 8 on a fresh start, found no other consumer, and replies still arrived, so treat a
   number above 1 as a reason to look and not as proof.

## An agent on kagent 0.9.x answers, but the answer can appear twice

A 0.3 agent has no task list, so Agent Kourier cannot ask whether the agent already has a turn
before it sends the turn again. After a restart that interrupted a turn, or after a send
whose outcome was unknown because the stream dropped before the first event, the turn may run
a second time and post a second answer. For a read-only agent that wastes a model call and
nothing else. An agent that changes things can do the change twice. On the `a2a` dialect Agent
Kourier does not see a tool call, so the agent's own tool list and the role behind its tools
are the only fence on a write, and tool approvals from Slack are planned for every backend.

## `Multi-Attach error for volume` while the pod rolls

The SQLite volume is `ReadWriteOnce` and the pod's strategy is `Recreate`. The event usually
clears within a minute, while the storage driver detaches the volume from the old node. If it
persists, the volume is still attached to the old node. Look at the `VolumeAttachment` for the
claim's volume.

## The agent answers, but tool calls do not show as step cards

The `a2a` dialect never renders tool calls as step cards, for any agent on it, so none of them
will. The answer text renders. Only the `kagent-v1` dialect reports tool parts.

## The kagent install uses far more memory than expected

The chart's defaults run a UI, several MCP servers and ten bundled agents, about 2.3 GiB of
memory requests. Turn off what you do not use, and turn off the bundled `kagent-tools`, which
the chart binds to `cluster-admin`. See
[Connect an agent](getting-started.md#7-connect-an-agent).

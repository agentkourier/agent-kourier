# Connect the AX add-on

For a platform team that wants an agent on Amazon Bedrock behind a Binding: at the end, the chart runs the AX add-on, an
ADK agent on Bedrock, and a Binding sends its threads to it. Each thread is one ADK session, and a question the agent
asks with its `ask_user` tool waits for a reply in the thread. The add-on is experimental.

The chart is the first route, and this page starts with it. The second route runs the same agent inside Google AX, as a
Task behind AX's router: [Run it inside Google AX](#run-it-inside-google-ax).

[Add-ons](../explanation/add-ons.md) says what an add-on is. The [AX add-on](../reference/ax.md) reference has how the
add-on maps A2A to ADK, every setting, and what a restart keeps.

## What the agent is

The add-on is an agent built with Google's Agent Development Kit (ADK), with Bedrock's Converse API as its model and one
tool, `ask_user`. It answers chat messages and investigates the alert text it is given, from what the thread's messages
carry: it has no tool that reaches another system, so it reads nothing and changes nothing outside the thread. When
something is missing, it asks in the thread, and a person's reply is its answer.

It serves A2A itself, so Agent Kourier reaches it with the `a2a` dialect and no change. Its store keeps each thread's
ADK session and its tasks, so a question survives a restart of the add-on, and a thread continues its session.

## Before you start

- **Access to a Bedrock model in a region**, such as a Claude Haiku inference profile, for the AWS account the add-on
  calls Bedrock as.
- **AWS credentials for that account**, one of:
    - a Bedrock API key, which the chart passes from a Secret;
    - an IAM role for the add-on's ServiceAccount (IRSA), or an EKS Pod Identity association. Neither route has been
      tested on EKS yet.
- **Agent Kourier installed with the Helm chart** ([Install with Helm](install-with-helm.md)), from a release whose
  [release notes](../release-notes.md) list the add-on. The 0.6.0 chart has no `addons` block, and its values schema
  refuses one.
- **For more than one replica of the add-on, a Postgres database of its own**, apart from Agent Kourier's.

The commands below use the release `agent-kourier` in the namespace `agent-kourier`. The add-on's Service is then
`agent-kourier-ax`; in general it is `<fullname>-ax`.

## 1. Create the Secrets

The add-on needs a token that Agent Kourier presents to it, and with an API key, the key. Both go in Secrets in the
release's namespace:

```sh
kubectl -n agent-kourier create secret generic ax-token \
  --from-literal=token="$(openssl rand -hex 32)"
kubectl -n agent-kourier create secret generic bedrock \
  --from-literal=apiKey="${BEDROCK_API_KEY}"
```

`--from-literal` stores the value with no trailing newline. Keep it that way: Agent Kourier sends the token as the
Secret holds it, and refuses to send a value with a control character, a newline included.

With Postgres, create a third Secret with the add-on's DSN:

```sh
kubectl -n agent-kourier create secret generic ax-postgres \
  --from-literal=dsn='postgres://ax:...@postgres.example.internal:5432/ax'
```

## 2. Turn the add-on on

Add the block to your values file:

```yaml
addons:
  ax:
    enabled: true
    model:
      region: us-east-1
      name: us.anthropic.claude-haiku-4-5-20251001-v1:0 # (1)!
    bedrock:
      apiKeySecret: {name: bedrock, key: apiKey} # (2)!
    auth:
      tokenSecret: {name: ax-token} # (3)!
    sessionIdleTTL: 2160h # (4)!
rbac:
  secretNames:
    agent-kourier: [ax-token] # (5)!
```

1. The Bedrock model ID or inference profile. The account must have access to it in that region.
2. The chart passes the key into the container's environment as `AWS_BEARER_TOKEN_BEDROCK`, which the container reads
   when it starts: after a change to the Secret, restart the add-on
   (`kubectl -n agent-kourier rollout restart deployment/agent-kourier-ax`).
3. The key defaults to `token`. The add-on reads the Secret as a file of accepted tokens, one per line, and reads it
   again every few seconds, so a change needs no restart.
4. Set it to the Binding's `session.threadTTL` (step 4).
5. On custom resources, the chart grants Agent Kourier only the Secrets it can see. A Binding you apply yourself names
   `ax-token` in `identity.tokenSecretRef`, so grant it here. A Binding in `config.bindings` needs no entry: the chart
   grants the Secrets that `config.*` names.

### Or take credentials from an IAM role

Leave `bedrock.apiKeySecret` empty, and the AWS SDK's default chain finds the credentials. For IRSA, annotate the
add-on's ServiceAccount, `<fullname>-ax`, with a role that may call Bedrock:

```yaml
addons:
  ax:
    serviceAccount:
      annotations:
        eks.amazonaws.com/role-arn: arn:aws:iam::111122223333:role/agent-kourier-ax
```

For EKS Pod Identity, associate the role with the ServiceAccount `agent-kourier-ax` in the release's namespace instead.
Neither route has been tested on EKS yet.

### Keep the state in Postgres

For more than one replica, keep the state in Postgres. The replicas elect a leader through the database, and a standby
forwards what it receives to the leader:

```yaml
addons:
  ax:
    replicaCount: 2
    store:
      type: postgres
      postgres:
        dsnSecret: {name: ax-postgres, key: dsn}
```

With SQLite, the default, the chart refuses `replicaCount` above 1, keeps the database on a ReadWriteOnce volume, and
replaces the pod with the Recreate strategy.

### With a NetworkPolicy

With `networkPolicy.enabled`, the add-on gets a NetworkPolicy of its own. Its ingress is port 8090 from Agent Kourier's
pods, from `networkPolicy.metricsFrom` and, on Postgres, from the add-on's other replicas. Its egress is DNS, port 443
to anywhere outside the private and link-local ranges (`addons.ax.networkPolicy.bedrock`, which covers Bedrock, and AWS
STS for IRSA), the add-on's other replicas and its database on Postgres, and `addons.ax.networkPolicy.extraEgress`.
Postgres needs `addons.ax.networkPolicy.postgres.to`.

The rule to 443 leaves out `169.254.0.0/16`. Per AWS's documentation, the EKS Pod Identity agent listens on
`169.254.170.23`, port 80 (`fd00:ec2::23` on IPv6), and the instance metadata service on `169.254.169.254`. With Pod
Identity, allow the agent:

```yaml
addons:
  ax:
    networkPolicy:
      extraEgress:
        - to: [{ipBlock: {cidr: 169.254.170.23/32}}]
          ports: [{port: 80, protocol: TCP}]
```

The policy leaves the instance metadata service out as well, so the default chain cannot fall back to the node's role
through it. To send Bedrock through an egress proxy or a VPC endpoint, put its address in `extraEgress` and set
`addons.ax.networkPolicy.bedrock.cidrs` to `[]`. [Helm values](../reference/helm-values.md#addons) has every value of
the block.

### Upgrade the release

Upgrade the release with the file, then wait for the add-on:

```sh
helm upgrade agent-kourier oci://ghcr.io/agentkourier/charts/agent-kourier --version <version> \
  -n agent-kourier -f my-values.yaml
kubectl -n agent-kourier rollout status deployment/agent-kourier-ax
kubectl -n agent-kourier logs deployment/agent-kourier-ax | grep 'agent listening'
```

The add-on listens only after its start-up work: it opens and migrates its store, and fails the turns a restart cut off.
A setting it cannot use stops it at start, with an error that names the variable.

## 3. Declare the backend and the Binding

The AgentBackend names the add-on's A2A endpoint with the `a2a` dialect. The Binding sends the token through
`identity.tokenSecretRef`, as `Authorization: Bearer`, and lets the agent ask questions:

```yaml
apiVersion: agentkourier.dev/v1alpha1
kind: AgentBackend
metadata:
  name: ax
  namespace: agent-kourier
spec:
  dialect: a2a
  url: http://agent-kourier-ax.agent-kourier.svc:8090/a2a # (1)!
---
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata:
  name: ax-chat
  namespace: agent-kourier
spec:
  agent:
    backendRef: {name: ax}
    namespace: ax # (2)!
    name: assistant
  identity:
    userId: agent-kourier-ax
    tokenSecretRef: {name: ax-token} # (3)!
  chat:
    connectionRef: {name: slack}
    channel: C0123456789 # the channel's ID, not its name
    output: live
  interactions:
    askUser: true # (4)!
  session:
    threadTTL: 2160h
```

1. The URL the agent card names too. Agent Kourier fetches the card at this URL plus `/.well-known/agent-card.json`,
   with the same token: unlike the Claude Managed Agents add-on's, this card is behind the caller check.
2. On the `a2a` dialect `agent.namespace` and `agent.name` are labels: the URL selects the agent, and they are not
   sent.
3. The same Secret the add-on reads. A Binding in another namespace reads a Secret in its own namespace, so that
   namespace needs a copy of the token, and the add-on's Secret must hold it.
4. Without it, Agent Kourier cancels the task when the agent asks, and says so in the thread
   ([Let the agent ask questions](agent-questions.md)). The add-on asks no tool approvals, so `toolApprovals` changes
   nothing.

```sh
kubectl apply --dry-run=server -f ax.yaml
kubectl apply -f ax.yaml
kubectl -n agent-kourier get akbinding ax-chat
```

On the ConfigMap route, or to let the chart render them, put the same specs in `config.agentBackends.ax.spec` and
`config.bindings.ax-chat.spec`.

### Or send the token in a header of your own

To keep `identity` for something else, the AgentBackend can carry the token in a header instead. Name the header in the
add-on's values, and put an entry with that name in the AgentBackend's `headers`, whose value comes from the Secret:

```yaml
addons:
  ax:
    auth:
      tokenSecret: {name: ax-token}
      header: X-Ax-Token
```

```yaml
spec:
  dialect: a2a
  url: http://agent-kourier-ax.agent-kourier.svc:8090/a2a
  headers:
    - name: X-Ax-Token
      valueSecretRef: {name: ax-token}
```

The header's value is then the whole token, with no `Bearer`. The header cannot be one that carries the Binding's
identity: `Authorization`, `Proxy-Authorization`, `Cookie`, `X-User-Id`, `X-Agent-Name`, `X-Share-Token` or
`X-Kagent-Insecure-Runtime-Identity`. The Secret is read in the AgentBackend's namespace, and the Binding's `identity`
still needs a `userId` or a `tokenSecretRef`.

## 4. Match the session lifetimes

Agent Kourier gives a thread a new A2A context once it has had no activity for the Binding's `session.threadTTL`
(default `2160h`). The add-on deletes a context's ADK session and its tasks once the context has been inactive for
`sessionIdleTTL` (`SESSION_IDLE_TTL`, the same default) with no turn running, a question the context waits on included.
Keep the two equal:

- Shorter than `threadTTL`, the add-on deletes sessions that a thread can still continue, and the thread's next turn
  starts a new session without the old history.
- Longer than `threadTTL`, sessions that no thread will use again stay until it passes.

## Check it

Mention the bot in the channel. The answer streams into the thread, and replies in the thread continue the same
session. Ask for something the agent cannot answer without more, such as an investigation with no alert text: when it
asks in the thread, your reply in the thread is its answer, on the same task.

If the thread shows nothing, read the two logs:

```sh
kubectl -n agent-kourier logs deployment/agent-kourier | grep 'session turn'
kubectl -n agent-kourier logs deployment/agent-kourier-ax
```

`refused an A2A request` in the add-on's log, with the reason `missing` or `invalid`, means Agent Kourier sent no
token, or one the add-on does not hold: check that the Binding's Secret and the add-on's hold the same value, with no
trailing newline in the Binding's. A turn that fails can be Bedrock refusing the credentials or the model: check the
model's access in the region and, with a NetworkPolicy, the egress above.

## Rotate the token

The add-on accepts every token in its file, so a new one can be added before the old one is removed. Give the add-on a
Secret of its own (not the one the Binding reads), then:

1. Add the new token to the add-on's Secret, on a line of its own.
2. Change the Binding's Secret to the new token ([Rotate the Binding token](rotate-binding-token.md)).
3. Remove the old line from the add-on's Secret.

The kubelet updates the mounted Secret after its own sync delay, and the add-on reads the file again every few seconds.
If a read fails or finds no token, the add-on keeps the tokens it had.

## Turn it off

`addons.ax.replicaCount: 0` stops the add-on and keeps its volume or database. `addons.ax.enabled: false` removes its
objects; the SQLite volume's claim is kept unless `addons.ax.persistence.retainOnUninstall` is `false`.

## Run it inside Google AX

Google AX runs sandboxed Tasks on Substrate. The same agent runs there as a Task, and Agent Kourier reaches it through
AX's router with the `a2a` dialect. Agent Kourier does not call AX's own API: it creates, pauses and resumes no Task,
and that lifecycle stays with you. This route has more parts than the chart's, and is checked by the AX kind tier and
the procedure below, not yet by a production deployment.

What differs from the chart:

| | Chart | Inside Google AX |
|---|---|---|
| Image | `ghcr.io/agentkourier/agent-kourier-ax`, released | `addons/ax/Dockerfile.ax`, which you build: the agent, AX's task runner and git on Debian |
| Listens on | `:8090` | `:80`, with the task runner on `127.0.0.1:8081` (`RUNNER_URL`), to which the agent proxies `/readyz` and `/metadata/` |
| Caller check | The token file | OIDC: an ID token for Agent Kourier's service identity |
| Bedrock key | A Secret, or an IAM role | Injected by Substrate's egress gateway: the Task holds only a placeholder |
| Store | SQLite on a volume, or Postgres | SQLite on the Task's durable workspace, `/workspace/agent-kourier-state/agent.db` |

The files are in [`addons/ax`](https://github.com/agentkourier/agent-kourier/tree/main/addons/ax). AX is pinned
there to the revision in `addons/ax/go.mod`; follow that revision's AX setup.

### 1. Build the image

```sh
make ax-runner-image
```

It builds `agent-kourier-ax-runner:dev`. No release builds or publishes it. Push it to a registry your Substrate workers
pull from, and put its digest in `task.yaml`'s `image`. The Task runs `/usr/local/bin/agent-kourier-ax-agent`; AX starts
the task runner at its fixed path, `/usr/local/bin/ax-task-runner`, which the image replaces with the runner built from
the pinned AX module (on port 80 during the golden-template build, which runs no command, and on 8081 for running
Tasks).

### 2. Provision the atespace and the egress

1. Provision the `agent-kourier-ax` atespace and AX's control plane.
2. Configure Substrate's TLS interception and credential injection, with a credential provider that holds the Bedrock
   API key in its Kubernetes Secret, never in the Task or the image. The Task's `AWS_BEARER_TOKEN_BEDROCK` is a
   placeholder: the SDK sends it, and the gateway replaces the `Authorization` header with the real key.
3. Once the actor exists, apply `egress-policy.json` with `ateapi.Control/CreateActorEgressPolicy`, after you replace the
   provider URI and the issuer's host (`UpdateActorEgressPolicy`, with the fetched UID and version, if a policy is there
   already). It allows Bedrock's runtime host in the region, with the injected key, and the issuer. Destinations it does
   not list stay denied: allow the issuer's key set host too if it is another one, with no credential.

### 3. Merge the template overlay

Merge `template-overlay.yaml` into the ActorTemplate that AX generates, through the Substrate API, before the agent runs
or its golden snapshot is built. Keep the generated storage location, the durable `/workspace` volume and the other
fields, and use the container name of the generated template. The overlay mounts the egress gateway's CA at `/run/ate`
(the Task points `SSL_CERT_FILE` there) and takes DATA snapshots with a COLD_BOOT resume, so that a resumed actor starts
a fresh agent process on its restored SQLite file. AX's default templates do not mount the CA, and applying the Task
alone gives the agent neither its Bedrock key nor a CA it trusts.

### 4. Apply the Task

Substitute the image, the model, and the OIDC settings in `task.yaml`, then:

```sh
ax apply -f task.yaml
```

AX reconciles a Task at once, so provision the template and the egress policy while the actor is paused. `OIDC_ISSUER`,
`OIDC_AUDIENCE` and `OIDC_SUBJECT` are the issuer of Agent Kourier's service identity, the audience its ID token is for,
and its subject. The agent reads the issuer only at the first request that carries a token, so the golden snapshot needs
no egress to it; a request that comes while the issuer cannot be reached is answered 503, and Agent Kourier tries again.

### 5. Put a gateway in front of the router

`gateway.conf` is an nginx fragment that forwards only the A2A endpoint and the card to AX's router, sets the
`ate-target-actor` header to the one actor whatever the request says, and turns off buffering for the stream. Terminate
TLS in front of it, and let only Agent Kourier reach it, with a NetworkPolicy. The agent checks the ID token itself and
refuses another subject; to refuse a caller before the actor wakes up, put an OIDC-verifying front door in front of the
gateway as well.

### 6. Declare the backend and the Binding

`agent-kourier.yaml` holds both. The URL is the gateway's, and a static header names the actor as
`<atespace>/<actor>`:

```yaml
apiVersion: agentkourier.dev/v1alpha1
kind: AgentBackend
metadata:
  name: ax
  namespace: agent-kourier
spec:
  dialect: a2a
  url: https://ax-gateway.example.internal/a2a # (1)!
  headers:
    - name: ate-target-actor
      value: agent-kourier-ax/assistant
```

1. The exact JSON-RPC endpoint, used as written.

The Binding is the one in step 3, with `identity.tokenSecretRef` naming a Secret that holds an ID token for the agent's
audience and subject. Keep that token fresh: Agent Kourier sends what the Secret holds, and follows its changes
([Rotate the Binding token](rotate-binding-token.md)). The token's subject is the caller that owns the agent's tasks,
whatever the thread's people are.

### Keep the state across a pause

The agent's tasks, message IDs and ADK sessions share `/workspace/agent-kourier-state/agent.db`. Include the whole
directory, the `-wal` file with it, in the actor's durable data snapshot. One agent process opens the file at a time: it
holds a lock on `agent.db.lock` beside it, and a second process stops at start.

An agent that ran before the add-on took `STATE_DIR`, which the add-on refuses: set `SQLITE_PATH` to `STATE_DIR` plus
`/agent.db`, as `task.yaml` does, and unset `STATE_DIR`. The first start imports the tasks of the old layout, so a
question that waited across the upgrade can still be answered.

A turn that was running when the actor paused fails at the next start; the model call is not made again. A question that
waited survives. A stream does not survive a pause: recovery rests on the task in the store.

### Check it before a real channel

Run this against an isolated atespace before you bind a real channel:

1. Ask a question in Slack, and check the streamed text and the question the agent posts.
2. Answer in the thread, and check that the same task completes and that the audit names the person who answered.
   Check that someone outside the channel, or a bot, cannot answer, and that an answer after the timeout is refused.
3. Pause the actor while a question waits, resume it by fetching the card through the gateway, and answer. Check that
   the task and context IDs and the ADK session survive.
4. With a completed task, resend the same message ID, and check that the model is not called again. Drop the stream and
   reconnect with `tasks/resubscribe`.
5. Pause the actor during a model call, and check that the task fails once and the context takes a new turn. Cancel a
   running task and check its final state.
6. Check that a missing, expired, wrong-audience or wrong-subject token is refused, that a forged actor header cannot
   route elsewhere, and that egress the policy does not list is denied.

The AX kind tier (`make e2e-ax-up`, `make e2e-ax`) runs the shared Slack scenarios against a real AX and Substrate
install with a deterministic model: streaming, context across restarts, actor suspend and resume, queued replies, and
text questions answered in the thread.

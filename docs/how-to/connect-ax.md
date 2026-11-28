# Connect the AX add-on

The AX add-on accepts A2A turns from Agent Kourier and starts an AX Task for each accepted turn. Supply the Task template, agent image, Workspace if needed, and Gateway. The command runs in AX. Its wrapper reports the exit status and output so Agent Kourier can finish the Slack turn.

## Prepare AX and the image

The launcher is built and tested against AX v0.4.0-rc.2, a pre-release of AX v0.4.0. Keep the AX gRPC service private and use dedicated launcher ownership and a dedicated Gateway for each deployment. The Gateway can inject credentials into any matching HTTPS request from its Tasks. Give its atespace access only to the Secrets it needs.

Copy the result wrapper into the agent image using the AX add-on image for your release as a build stage. For an existing AX image that supplies `/usr/local/bin/ax-launcher`, use this pattern. Substitute your actual image references at build time.

```dockerfile
ARG WRAPPER_IMAGE
ARG AX_AGENT_IMAGE
FROM ${WRAPPER_IMAGE} AS wrapper
FROM ${AX_AGENT_IMAGE}
COPY --from=wrapper /usr/local/bin/agent-kourier-ax-result /usr/local/bin/agent-kourier-ax-result
```

Provide the normal AX runner through the image or the AX runner volume. Keep your existing launcher configuration in the Workspace and Gateway. Set the Task command to the wrapper, `--`, and the agent's command and arguments. AX supplies the current prompt in `AX_GOAL_FILE` and the configured prompt key in `AX_CONFIG_FILE`. Place `AX_OUTPUT_FILE` within the wrapper’s working directory. The wrapper reads a regular file there and falls back to the tail of stdout and stderr if the file is missing or unusable. Use a digest for the resulting agent image.

## Configure credentials and the callback

For this example, install the release as `agent-kourier` in namespace `agent-kourier`. Supply these Kubernetes Secrets in that namespace.

| Secret | Keys |
| --- | --- |
| `slack-main` | `botToken` and `appToken` for the Slack app |
| `ax-a2a-auth` | `token` for Agent Kourier's A2A requests |
| `ax-result-auth` | `token` for the launcher’s accepted callback tokens and `gateway-token` for the single bearer the Gateway sends |
| `ax-results-tls` | TLS certificate and key for the private callback hostname |

Use a dedicated callback bearer distinct from the A2A token. Start with the same bearer in both callback keys. To rotate, add the new bearer to `token` on a second line. Wait for the Secret to propagate and verify that every launcher replica accepts it. Switch `gateway-token` to the new bearer, then remove the old line after all Gateway instances use the new credential. The launcher reads the mounted file every five seconds and retains the previous set if a reload is invalid.

Grant the dedicated AX atespace access to `ax-result-auth` through the Substrate credential provider. Scope the grant to an explicit Secret label. The wrapper sends an Authorization placeholder and the Gateway replaces it outside the sandbox. Configure DNS and ensure the egress Gateway trusts the certificate on the callback's HTTPS origin. AX supplies the trust bundle the wrapper uses for intercepted HTTPS.

Apply this Gateway with `ax apply -f gateway.yaml`. Use the same hostname here and in the Helm values.

```yaml
apiVersion: ax.io/v1alpha1
kind: Gateway
metadata:
  name: ax-agent-results
  atespace: slack-ax
spec:
  egress:
    - https:
        hostnames: [ax-results.example.test]
        effects:
          replaceHeaders:
            - header: Authorization
              prefix: "Bearer "
              credentialUri: "ate-secret://k8s.io/default/agent-kourier/ax-result-auth/gateway-token"
```

Set the launcher values below. The optional Ingress exposes exactly `/ax/results/v1` and `/ax/results/v1/start`. Use the class of your private controller and set controller-specific annotations under `addons.ax.resultCallback.ingress.annotations`. The chart uses private plaintext HTTP for replica forwarding and private plaintext gRPC for the AX control plane. Keep both hops on trusted networks or protect them with a mesh that encrypts pod traffic. If your cluster applies NetworkPolicy, set `addons.ax.networkPolicy.server.to` and its port to allow AX gRPC, and `addons.ax.networkPolicy.callbackFrom` to allow traffic from that ingress controller. The callback Secret belongs to the add-on and stays out of the broker's Secret grants. When enabling the launcher on the files route, list the broker's release-namespace Secrets in `rbac.secretNames` if the chart cannot see them in `config.*`.

```yaml
config:
  source: crds
  chatConnection: agent-kourier/slack-main
rbac:
  secretNames:
    agent-kourier: [slack-main, ax-a2a-auth]
addons:
  ax:
    enabled: true
    server:
      address: ax-server.ax-system.svc:8080
    atespace: slack-ax
    taskTemplate:
      configMap: {name: ax-agent-template, key: task.yaml}
    auth:
      tokenSecret: {name: ax-a2a-auth, key: token}
    resultCallback:
      url: https://ax-results.example.test/ax/results/v1
      tokenSecret: {name: ax-result-auth, key: token}
      ingress:
        enabled: true
        className: internal
        tlsSecretName: ax-results-tls
    history: true
    maxRunning: 1
    turnTimeout: 5m
    keepFailed: 5m
```

## Install the Task template and Binding

Replace the image reference with the digest of your wrapped agent and set the Slack channel ID. Add a `spec.workspace` binding to the Task when your agent requires one. The launcher supplies each Task's name, session ID, prompt, and callback variables.

```yaml
apiVersion: v1
kind: ConfigMap
metadata: {name: ax-agent-template, namespace: agent-kourier}
data:
  task.yaml: |
    apiVersion: ax.io/v1alpha1
    kind: Task
    metadata: {name: agent-template, atespace: slack-ax}
    spec:
      profile:
        image: "registry.example.test/wrapped-agent@sha256:REPLACE_WITH_DIGEST"
      commands: ["/usr/local/bin/agent-kourier-ax-result", "--", "/usr/local/bin/ax-launcher"]
      gateway: {name: ax-agent-results}
      debug: false
---
apiVersion: agentkourier.dev/v1alpha1
kind: ChatConnection
metadata: {name: slack-main, namespace: agent-kourier}
spec:
  platform: slack
  credentialsSecretRef: {name: slack-main}
---
apiVersion: agentkourier.dev/v1alpha1
kind: AgentBackend
metadata: {name: ax, namespace: agent-kourier}
spec:
  dialect: a2a
  url: http://agent-kourier-ax.agent-kourier.svc:8090/a2a
---
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: {name: ax-chat, namespace: agent-kourier}
spec:
  agent:
    backendRef: {name: ax}
    namespace: agent-kourier
    name: ax-agent
  identity:
    tokenSecretRef: {name: ax-a2a-auth}
  chat:
    connectionRef: {name: slack-main}
    channel: C0123456789
    output: live
    progress: steps-and-activity
    triggers:
      - type: mention
```

Invite the bot to the channel and mention it with a short prompt. Watch for sandbox progress and one reply in that thread. Mention it again in the thread to check history. On command failure, take the troubleshooting reference from Slack and find the broker log entry with `failure_ref`. Use its task ID to find the launcher's `AX Task lifecycle` entry with the AX Task name, UID, exit code, and cleanup disposition.

For two add-on replicas, configure a separate Postgres database with `addons.ax.store`. The elected leader processes A2A and callback requests. A standby forwards both. See [AX settings and lifecycle](../reference/ax.md).

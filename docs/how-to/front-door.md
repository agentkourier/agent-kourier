# Put a verifying front door before kagent

For a platform engineer connecting Agent Kourier to kagent 1.0 in OIDC mode: at the end, every call from Agent Kourier
reaches kagent through a proxy that verifies the Binding's token and strips the headers that select a user, and
nothing else can reach kagent's controller port.

kagent's controller port (8083) trusts identity without verifying it. In OIDC mode it decodes the JWT without checking
its signature (a forged `alg: none` token was accepted), and it takes the acting user from `X-User-Id` or `?user_id=`
whenever `X-Agent-Name` is present, even with a valid token. The [security model](../explanation/security.md) has the
background.

## Before you start

- kagent 1.0 in OIDC mode, and an OIDC provider that issues each Binding a token (the kind tiers use Dex).
- oauth2-proxy or agentgateway. The kind tiers use a second oauth2-proxy; a reference configuration is
  [`hack/kind/full/frontdoor.yaml`](https://github.com/agentkourier/agent-kourier/blob/main/hack/kind/full/frontdoor.yaml).
- A CNI that enforces NetworkPolicy. Without it, none of the fences below exist.

## 1. Run the proxy

Configure it to:

1. **Verify the token**: signature by the issuer's keys, issuer, an audience that only Binding tokens carry, and
   expiry.
2. **Forward only `/agents/`** to the kagent controller on 8083, with the `Authorization` header intact.
3. **Strip these request headers** from every request: `X-Agent-Name`, `X-User-Id`, `X-Share-Token` and
   `X-Kagent-Insecure-Runtime-Identity`. kagent lets them choose the acting user or session.

In oauth2-proxy's alpha configuration, a strip is an `injectRequestHeaders` entry with a name and no values:

```yaml
injectRequestHeaders:
  - name: X-Agent-Name
  - name: X-User-Id
  - name: X-Share-Token
  - name: X-Kagent-Insecure-Runtime-Identity
  # ... and the Authorization and X-Forwarded-* entries, which this list replaces
```

The alpha list replaces oauth2-proxy's default injections, so keep its `Authorization` entry, or no token reaches
kagent.

## 2. Fence the controller

Three NetworkPolicies in kagent's namespace (the kind tier's are in
[`hack/kind/full/networkpolicy.yaml`](https://github.com/agentkourier/agent-kourier/blob/main/hack/kind/full/networkpolicy.yaml)):

- **Port 8083** admits only the front door and kagent's own components.
- **The kagent UI** admits only its own oauth2-proxy, because the UI's nginx relays `/mcp` and `/api` to 8083 with
  `Authorization` intact.
- **The front door** admits only Agent Kourier's pods, which carry `app.kubernetes.io/name: agent-kourier`.

!!! danger "The UI's proxy must not accept the Binding's audience"

    If it does, a Binding's token is valid through a second, wider door. Configure the UI's oauth2-proxy without the
    audience your Binding tokens carry.

## 3. Point Agent Kourier at the door

```yaml
config:
  agentBackends:
    kagent:
      spec:
        dialect: kagent-v1
        url: http://kagent-frontdoor.kagent.svc.cluster.local:4180
```

With the chart's `networkPolicy` on, allow the door in `networkPolicy.extraEgress` and nothing else in that namespace:

```yaml
networkPolicy:
  extraEgress:
    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: kagent}}, podSelector: {matchLabels: {app: kagent-frontdoor}}}]
      ports: [{port: 4180}]
```

## Check it

- A turn of a Binding with a valid token succeeds.
- A turn whose token the door refuses fails with HTTP 401 or 403, and the thread says the agent's front door refused
  the Binding's credentials; `agentkourier_credential_rejections_total` counts it.
- A pod labelled `app.kubernetes.io/name: agent-kourier` cannot reach the controller on 8083 directly. The kind tier's
  `make sandbox-heal` uses exactly that probe.

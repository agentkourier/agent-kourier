# Rotate the Binding token

For whoever owns a Binding's identity: at the end, the Binding's bearer token is replaced before it expires, with no
restart and no lost turn.

For the first account and token, start with [Create the Binding identity](create-binding-identity.md).

Agent Kourier reads the token from the Secret named by `identity.tokenSecretRef` on every call, from a watch of the
Secret. The Secret watch and authenticated clients hold credentials in memory; Agent Kourier does not persist a
separate token to disk, logs, audit records or resource status, and never mints one. Rotation happens outside Agent
Kourier: write the new token into the Secret, and subsequent calls use it once the watch observes the update.

## Replace the token by hand

```sh
kubectl -n payments create secret generic payments-agent-token \
  --from-literal=token="$NEW_TOKEN" --dry-run=client -o yaml | kubectl apply --server-side -f -
```

Use the Binding's namespace and Secret name, and the key the reference names (`token` by default). Secrets that Agent
Kourier reads must be type `Opaque`.

A write to the Secret starts a pass of [Binding verification](../reference/metrics.md#binding-verification), within about 30 seconds or when a pass already running ends, which fetches the agent's card with the new token. `agentkourier_binding_ready` is 1 afterwards if the front door takes the token and the channel still reads true; if the front door refuses it, `BackendReachable` reads `False/CredentialsRejected`. A Secret refreshed on a schedule starts a pass each time too.

## Refresh a Dex token on a schedule

When the backend's front door takes Dex ID tokens, the Helm chart can keep the token fresh with an hourly CronJob. It
asks Dex for a token with the password grant and patches it into the one Secret, with a ServiceAccount whose only right
is `patch` on that Secret.

This Job needs a Dex client and account with the password grant enabled, as described in
[Create the Binding identity](create-binding-identity.md#example-issue-the-first-token-with-dex). Its credentials
Secret holds the account password and client secret so it can request new tokens. The Job implements only that
grant; for another authentication flow, arrange an external process to update the Binding's token Secret.

1. Create the Dex credentials Secret in the release namespace, with the keys `clientSecret`, `username` and
   `password`.
2. If the target Secret does not exist yet, create it with a placeholder in the Binding's namespace:

    ```sh
    kubectl -n payments create secret generic payments-agent-token --from-literal=token=placeholder
    ```

    Create the namespace first if needed. The refresh Job patches the Secret and does not create it. With
    `config.source: files` the Secret must also exist before Agent Kourier starts, because a Secret the config names
    and the broker cannot find fails the load. With custom resources the broker starts without it, and the Binding's
    `BackendReachable` condition reads `CredentialsUnreadable` until the Secret is there. A placeholder is not
    a valid credential; wait for the first refresh to succeed before sending messages to the agent. If you already
    stored the first token using the identity guide, keep that Secret and skip this step.
3. Enable the CronJob:

    ```yaml
    tokenRefresh:
      enabled: true
      targetSecret: payments-agent-token # the Binding's identity.tokenSecretRef
      targetKey: token
      targetNamespace: payments          # the Binding's namespace
      dex:
        tokenURL: http://dex.dex.svc.cluster.local:5556/dex/token
        clientId: agent-kourier-binding
        credentialsSecret: agent-kourier-dex
    ```

    With `config.source: files` and the config inline, the render fails unless some Binding in that namespace names that
    Secret and key. With custom resources, the default, or with `config.existingConfigMap`, the chart cannot see the
    Bindings and does not check: compare the three values with the Binding yourself.
    The client ID must match the registered Dex client and the audience accepted by the front door.

4. Run the first refresh by hand:

    ```sh
    kubectl -n agent-kourier create job --from=cronjob/agent-kourier-token-refresh first-token
    kubectl -n agent-kourier wait --for=condition=complete job/first-token --timeout=120s
    ```

In the tested kagent 1.0.0-alpha5 OIDC setup, a session belongs to the identity that created it. Keep the owner claim
the same on every mint: `email` when `userIdClaim: email`, otherwise the claim configured by your backend or its
`sub` fallback.

## When a token expires anyway

The call fails with HTTP 401 at the front door, or with 403 on an A2A call after the card fetch. On a send or a followed
task, Agent Kourier fails the turn, re-reads the Secret, writes an audit entry, and counts
`agentkourier_credential_rejections_total{binding}`. The thread says the agent's front door refused the Binding's
credentials. A lookup before a resend is [retried instead](troubleshoot.md#http-401-at-the-card-fetch) and not counted.
Agent Kourier never falls back to `userId`.

Write a fresh token into the Secret; the next message works. Alert on any increase of
`agentkourier_credential_rejections_total`.

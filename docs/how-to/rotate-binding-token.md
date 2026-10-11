# Rotate the Binding token

For whoever owns a Binding's identity: at the end, the Binding's bearer token is replaced before it expires, with no
restart and no lost turn.

Agent Kourier reads the token from the Secret named by `identity.tokenSecretRef` on every call, from a watch of the
Secret. It never stores the token elsewhere and never mints one. Rotation happens outside Agent Kourier: write the new
token into the Secret, and the next call uses it.

## Replace the token by hand

```sh
kubectl -n payments create secret generic payments-agent-token \
  --from-literal=token="$NEW_TOKEN" --dry-run=client -o yaml | kubectl apply --server-side -f -
```

Use the Binding's namespace and Secret name, and the key the reference names (`token` by default). Secrets that Agent
Kourier reads must be type `Opaque`.

## Refresh a Dex token on a schedule

When the backend's front door takes Dex ID tokens, the Helm chart can keep the token fresh with an hourly CronJob. It
asks Dex for a token with the password grant and patches it into the one Secret, with a ServiceAccount whose only right
is `patch` on that Secret.

1. Create the Dex credentials Secret in the release namespace, with the keys `clientSecret`, `username` and
   `password`.
2. Create the target Secret with a placeholder. It must exist before Agent Kourier starts.
3. Enable the CronJob:

    ```yaml
    tokenRefresh:
      enabled: true
      targetSecret: payments-agent-token # the Binding's identity.tokenSecretRef
      targetKey: token
      targetNamespace: payments          # the Binding's namespace
      dex:
        tokenURL: http://dex.dex.svc.cluster.local:5556/dex/token
        clientId: agent-kourier
        credentialsSecret: agent-kourier-dex
    ```

    With the config inline, the render fails unless some Binding in that namespace names that Secret and key.

4. Run the first refresh by hand:

    ```sh
    kubectl -n agent-kourier create job --from=cronjob/agent-kourier-token-refresh first-token
    ```

The token must carry the same owner claim on every mint (the `userIdClaim`, `email` in the kind tiers, else `sub`): kagent
binds a session to the identity that created it.

## When a token expires anyway

The call fails with HTTP 401 or 403 at the front door. Agent Kourier fails the turn, re-reads the Secret, writes an
audit entry, and counts `agentkourier_credential_rejections_total{binding}`. The thread says the agent's front door
refused the Binding's credentials. It never falls back to `userId`.

Write a fresh token into the Secret; the next message works. Alert on any increase of
`agentkourier_credential_rejections_total`.

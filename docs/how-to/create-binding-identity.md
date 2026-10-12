# Create the Binding identity

A Binding's identity is the account Agent Kourier acts as at the agent backend. Think of `payments-assistant` as
the payments channel's account: people in that channel and its automated alerts all use it to talk to the agent.
You configure that account once on the Binding.

## Why an identity is necessary

Connecting to Slack gives Agent Kourier permission to read and post Slack messages. Calling an agent is a separate
connection. The agent backend needs a caller name, and a backend that verifies credentials also needs proof that
the caller may connect. A Slack bot token does not supply that backend credential.

The Binding supplies it. With `tokenSecretRef`, Agent Kourier sends an issued bearer token that the agent or its
front door verifies. With `userId`, it sends a name to a backend that trusts caller-supplied names. Every Binding
must configure at least one of these, even when the backend does not verify tokens.

The identity also matters after the first request. In the tested kagent 1.0.0-alpha5 OIDC setup, kagent ties a session
to the identity that created it and refuses requests from another owner. A Slack thread can contain an alert,
replies from several people, and answers to an agent's questions. Keeping the backend identity the same lets those
requests continue the same session.

## What a shared Binding identity gives you

- Automated investigations can start without anyone signing in. An alert has no person's backend credentials;
  Agent Kourier calls the agent using the Binding's credentials.
- People can collaborate in one agent session. If one person starts an investigation and another replies, both
  requests use the same backend owner. On kagent, changing owners would prevent the second person from continuing
  that session.
- You manage backend credentials per Binding. Channel members do not each need to obtain and supply backend
  tokens. You can renew the Binding's token in its Secret without restarting Agent Kourier, while keeping the same
  account as the session owner.
- You can give channels separate backend accounts. For example, configure an incident channel's account with
  read-only access and give another channel a different account with different permissions, where the backend
  supports them. A verifying front door authenticates the account; the backend's authorization rules determine
  what that account may do.

For example, an alert starts an investigation as `payments-assistant`. One channel member asks what changed;
another asks the agent to check a deployment. All three turns use `payments-assistant`, so kagent sees the same
session owner throughout. Agent Kourier records the Slack people as attribution in its audit log.

This makes the channel an access boundary. Everyone allowed to use a Binding acts through the same backend
account, so give it only the permissions they are all trusted with. The Binding identity does not grant each
person separate permissions, and it does not automatically configure the agent's tool credentials or Kubernetes
RBAC. Set those permissions in the backend and tools you deploy. See the [security model](../explanation/security.md).

## What you need to create

Agent Kourier does not create accounts or mint tokens. You choose a name for a backend that trusts `X-User-Id`, or
obtain a token from the issuer your backend accepts and store it in a Kubernetes Secret.

## Choose the authentication method

| Backend authentication | Binding configuration | What you create |
|---|---|---|
| The agent or its proxy verifies bearer tokens | `identity.tokenSecretRef` | An account with the accepted issuer, its token, and a Secret holding that token |
| The backend trusts a caller-supplied `X-User-Id` | `identity.userId` | A stable name, configured as the backend requires |

With both fields set, Agent Kourier sends the token only. If it cannot read the token, it never falls back to
`userId`. The two dialects, `a2a` and `kagent-v1`, use the same credential handling.

Use a separate identity for channels with different trust levels. Give it only the permissions everyone in its
channel is trusted with. See the [security model](../explanation/security.md).

## For a backend that trusts a user ID

Choose a stable name for this Binding, for example:

```yaml
identity:
  userId: payments-assistant
```

There is no token to generate and no identity Secret to create. Agent Kourier sends `X-User-Id: payments-assistant`.
Whether that name needs registration or permissions is determined by your backend. This header supplies a name
without proving who sent it; use it only with a backend that deliberately trusts callers on its network.

For kagent 1.0, use OIDC and a [verifying front door](front-door.md), with the token setup below.

## For a backend that verifies a token

1. Ask the platform team which issuer, audience and token type the backend or its front door accepts.
2. Provision a dedicated account with that issuer. The account's credentials obtain tokens; the issued token is
   what Agent Kourier sends to the agent.
3. Obtain a token using the issuer's supported authentication flow. A random string or a self-signed JWT will not
   satisfy a verifier configured for that issuer.
4. Store the token in an `Opaque` Secret in the Binding's namespace, and reference its name and key:

    ```yaml
    identity:
      tokenSecretRef:
        name: payments-agent-token
        key: token
    ```

5. Arrange [rotation before expiry](rotate-binding-token.md). Agent Kourier reads the watched Secret on every call;
   replacing its token needs no broker restart.

For that kagent setup, keep the session-owner claim stable across renewals. With `userIdClaim: email`, use the same
email on every token; when kagent uses `sub`, keep that subject stable. Setting `identity.userId` alongside a token does not
override the token's owner.

## Example: issue the first token with Dex

This example uses the local password connector, as the repository's Dex test deployment and the chart's refresh
Job do. You need an existing Dex installation, a front door that accepts its ID tokens, and permission to change
Dex configuration. The password grant requires account credentials; use your provider's approved flow when this
grant is disabled. Dex describes the setup in its [local connector guide](https://dexidp.io/docs/connectors/local/)
and [OAuth2 configuration](https://dexidp.io/docs/configuration/oauth2/).

Dex documents the password grant as deprecated and less secure. Prefer your provider's supported machine-to-machine
flow when available, with a token type and stable owner claim your backend accepts. The chart's refresh Job supports
only the password grant and needs the account password and client secret in a credentials Secret. For another flow,
use an external token-renewal process to update the Binding's token Secret.

### Register the account and client

Generate a bcrypt hash of the account's password with `htpasswd`, which prompts for the password:

```sh
htpasswd -nB payments-assistant
```

Copy the hash after `payments-assistant:` into the `hash` field below. Generate a separate client secret with your
secret manager. Merge these entries into your existing Dex configuration, replacing both placeholders, and apply
it through your Dex deployment process:

```yaml
enablePasswordDB: true
oauth2:
  passwordConnector: local
  grantTypes: [authorization_code, refresh_token, password]
staticPasswords:
  - email: payments-assistant@example.com
    username: payments-assistant
    userID: payments-assistant-0001
    hash: "<bcrypt hash of the account password>"
staticClients:
  - id: agent-kourier-binding
    name: Agent Kourier Bindings
    secret: "<client secret>"
    redirectURIs: [https://localhost/unused]
```

The account represents this Binding. The client identifies the application requesting tokens; its ID is the
audience the front door must accept. The redirect URI is unused by this password-grant request. Enable the password
grant explicitly by adding `password` to `oauth2.grantTypes` as shown above; preserve any other grants your deployment
uses. The repository's reference deployment pins Dex v2.45.1, whose
[configuration source](https://github.com/dexidp/dex/blob/v2.45.1/cmd/dex/config.go#L151-L154) defaults to all supported
grants, while the current OAuth2 documentation linked above describes narrower defaults. Check the settings for
your deployed version rather than relying on defaults. Keep the existing clients, users and OAuth2 settings when merging.

### Request and store the token

Run this in Bash with `kubectl`, `jq` and curl 8.3 or newer. The
[curl variable options](https://curl.se/docs/manpage.html#--variable) read credentials from the environment rather
than placing their values in the command arguments. Use your Dex token endpoint and the account's email:

```bash
(
  set -euo pipefail
  set +x
  umask 077
  identity_tmp=$(mktemp -d)
  trap 'rm -rf "$identity_tmp"' EXIT

  DEX_TOKEN_URL=https://dex.example.com/dex/token
  DEX_CLIENT_ID=agent-kourier-binding
  export DEX_USERNAME=payments-assistant@example.com
  read -r -s -p 'Dex client secret: ' DEX_CLIENT_SECRET
  printf '\n'
  read -r -s -p 'Binding account password: ' DEX_PASSWORD
  printf '\n'
  export DEX_CLIENT_SECRET DEX_PASSWORD

  curl --fail --silent --show-error --max-time 20 \
    --data-urlencode grant_type=password \
    --data-urlencode "client_id=$DEX_CLIENT_ID" \
    --data-urlencode 'scope=openid email' \
    --variable '%DEX_CLIENT_SECRET' --variable '%DEX_USERNAME' --variable '%DEX_PASSWORD' \
    --expand-data 'client_secret={{DEX_CLIENT_SECRET:url}}' \
    --expand-data 'username={{DEX_USERNAME:url}}' \
    --expand-data 'password={{DEX_PASSWORD:url}}' \
    "$DEX_TOKEN_URL" > "$identity_tmp/response.json"
  jq -e -j '.id_token | select(type == "string" and length > 0)' \
    "$identity_tmp/response.json" > "$identity_tmp/token"

  kubectl -n payments create secret generic payments-agent-token \
    --from-file=token="$identity_tmp/token"
)
```

Create the `payments` namespace first if it does not exist. The command creates an `Opaque` Secret and stores the
`id_token` without a trailing newline; Agent Kourier rejects control characters in credentials. It prints neither
the token nor the response. If the Secret already exists, use the [rotation procedure](rotate-binding-token.md).

Configure the front door for this issuer and the `agent-kourier-binding` audience, and keep the kagent UI's proxy
from accepting that audience. Then add the `tokenSecretRef` above to your [Binding](write-a-binding.md).

For automatic renewal, the chart's [Dex refresh Job](rotate-binding-token.md#refresh-a-dex-token-on-a-schedule)
needs a separate credentials Secret with `clientSecret`, `username` and `password`. Use the same client secret,
account email and account password used here, and set `tokenRefresh.dex.clientId` to `agent-kourier-binding`.
The credentials Secret lives in the Helm release namespace; the issued-token Secret lives in the Binding's
namespace. They hold different credentials and serve different purposes.

## Check it

Mention the bot in the bound channel. A successful agent reply checks that the configured identity can reach the
backend. If the front door rejects the token, check the issuer, audience, expiry and account permissions; see
[troubleshooting](troubleshoot.md#http-401-or-http-403-at-the-card-fetch).

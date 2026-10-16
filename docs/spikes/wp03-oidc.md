# WP-03 spike: identity through Agent Kourier's front door (Q3, Q9)

The spike code (`spike/`) is no longer in the repository; this page is the record of what it measured.

Run 2026-09-30 against the full tier (kagent 1.0.0-alpha5, Dex v2.45.1, oauth2-proxy v7.15.3 as
the front door) with `spike/a2a/run-oidc.sh`, which runs `go run ./spike/a2a -suite oidc`. Every
call goes **through `kagent-frontdoor`** with a real, signed Dex token, except the few marked
"at the controller" (a port-forward to 8083, which bypasses the NetworkPolicy, as in WP-02).
Raw JSON is in `test/fixtures/kagent/oidc-<case>/`; a call's `meta.json` records the identity
claims it used, never a token. Everything below is **measured** unless labelled *source* (read
from the kagent or oauth2-proxy code at the pinned tag) or *inferred* (reasoned, not observed).

**The run asserts the outcomes the spike drove** (not every statement here; the labels say
which are observed, source or inferred): each case counts its checks and fails if any differs
(HTTP status, JSON-RPC code, message, `ErrorInfo` reason, task states, task and context IDs,
response bodies). Final run: 342 checks (identity 102, headerspoof 27, subswap 8, service
148, expiry 14, replay 43), all passing, on a clean `make e2e-full-up`. `go run ./spike/a2a
-suite oidc` needs `-controller`; use `run-oidc.sh`.

## Answers

| # | Question | Answer |
|---|---|---|
| 1 | Q3: which claim owns a session, and is a second real identity refused? | The owner is a string taken from the token: the `email` claim when present, otherwise `sub` (`userIdClaim: email`). A second real identity is refused `-31403` on every method. The same email with a different `sub` is accepted. **But** any holder of a valid token can act as any owner by adding `X-Agent-Name` and `X-User-Id`, because the front door forwards them (finding A). |
| 2 | Service identity | kagent has no notion of one. A service-shaped email is an ordinary owner. A token with no `email` claim passes the front door unchanged and is owned by its `sub`. |
| 3 | Expiry | Refused at the front door with HTTP 401 and the body `{}`. The controller accepts the same expired token. |
| 4 | Replay with a `contextId` while the task runs | Not a live join: `-32004` "resource conflict". After the task ends, the same replay starts a second task (a duplicate run). `ListTasks` by `contextId` finds the first. |
| 5 | Q9: what must the front door accept? | Nothing extra for a missing `email` (measured). Other client-credentials traits are *source* only (below). |

## A. The front door forwards identity-selecting headers (found while doing Q3)

*Source:* kagent's `ProxyAuthenticator` (`go/core/internal/httpserver/auth/proxy_authn.go`) has two
paths. With no `X-Agent-Name` header the owner comes from the bearer JWT. **With** one, the token
is only parsed and the acting user comes from the `user_id` query parameter, then `X-User-Id`,
then `sub`. The chart's UI nginx clears `X-Agent-Name` and `X-User-Id` in trusted-proxy mode
(`oidc-proxy-authentication.md`: "so external callers cannot select the internal agent
authentication path"). The front door is a plain oauth2-proxy, which forwards request headers it
does not know.

*Measured* (`oidc-headerspoof`): B's valid token, against A's session:

| Extra request data | Continue A's session | `GetTask` on A's task |
|---|---|---|
| none | `-31403` denied | `-31403` denied |
| `X-User-Id: <A's email>` alone | `-31403` denied | `-31403` denied |
| `X-Agent-Name: spoof` and `X-User-Id: <A's email>` | **`COMPLETED`** | **`COMPLETED`** |
| `X-Agent-Name: spoof` and `?user_id=<A's email>` | **`COMPLETED`** | **`COMPLETED`** |

So a token that the front door accepts is enough to read, continue, or (by the same path)
cancel any identity's sessions. Everything in section B holds only for callers that do not send
these. Consequences (*inferred*: they depend on every Binding passing the same door): a leaked
Binding token is a key to every Binding's sessions, not just its own (R4, D32), and the session
ownership that spec 8.5 relies on does not isolate Bindings. Not measured: `CancelTask` and
`SubscribeToTask` with the spoof (they take the same authenticator path, *inferred*), and whether
the controller's `X-Agent-Name` path skips other checks.

The last two rows below were measured by the reviewer's curl run, not yet by the spike (the
spike's `oidc-headerspoof` now asserts them, but has not been run on a tier since):

| Extra request data | Result for B against A's session |
|---|---|
| `X-Agent-Name` and `X-User-Id` naming a third identity | `-31403` (the third identity does not own it) |
| `X-Agent-Name` alone | `-31403` (the acting user falls back to the token's `sub`, another owner string) |

*Candidate fix:* strip `X-Agent-Name` and `X-User-Id` at the door. Stripping `X-Agent-Name` alone
should close both spoof rows, since `user_id` is read only on the agent path (*source*).
WP-01d is the fix and owns proving it. What is established about the alpha-config route:

- *Measured:* one attempt on the scratch cluster (a ConfigMap with an alpha config mounted into
  the door, `--alpha-config` added) did not become ready within 120 s. oauth2-proxy v7.15.3 exits
  with usage when `--alpha-config` is combined with `--http-address`, `--upstream` or
  `--pass-authorization-header`, the likely reason the rollout failed (the reviewer's finding; I
  did not read the pod's log).
- *Source:* an alpha `injectRequestHeaders` list replaces the flag-derived one
  (`pkg/apis/options/alpha_options.go:70-76`).
- *Source:* an entry with no values only strips the named request header
  (`pkg/validation/header.go:20-40`, `pkg/header/injector.go:27-38`, `pkg/middleware/headers.go`).

## B. Q3: session owner and a second real identity

Dex now has three static users (`hack/kind/full/dex.yaml`): the Binding identity A
(`binding-test@example.test`), a second person B (`identity-b@example.test`) and a service-shaped
`svc-sre-alerts@example.test`. Each signs in with the password grant; the ID token is the bearer.

**`oidc-identity`.** A creates a session on `test-agent`. B, through the front door, is refused
with JSON-RPC `-31403`, message `permission denied`, `ErrorInfo` reason `UNAUTHORIZED`, HTTP 200,
for `SendStreamingMessage`, `SendMessage`, `GetTask`, `SubscribeToTask`, `CancelTask` and
`ListTasks` with A's `contextId`. A is refused on B's session the same way. On a **running**
task (the slow agent) B is refused before the busy check (`-31403`, not `-32004`) and cannot
cancel or watch it; A's `GetTask` still shows `WORKING` and A's `SubscribeToTask` runs to
`COMPLETED`. `ListTasks` with no filter returns only the caller's own tasks (A's list holds A's
task and not B's, and the reverse). B is a valid identity: it starts its own session normally.

**The owner claim.** Same result as WP-02, now with real tokens: the owner is `email`
(`userIdClaim: email`). *Source:* the claim named by `userIdClaim` (default `sub`), falling back
to `sub` when it is missing or empty.

**`oidc-subswap`: same email, different `sub`.** Dex derives `sub` from the user's `userID`, so
the case renames the user (same email) in Dex's config and restarts Dex between two calls. The
second token has the same `email` and a different `sub`, both really signed. It continues the
first token's session (`COMPLETED`) and reads its task. So `sub` does not matter while `email`
is present, as WP-02 measured with forged tokens.

Consequences: the owner string is an email, so two IdP users with the same email string would
share sessions, and an IdP that lets a user change their email would let them take over a
session by name (*inferred*; the door only admits tokens with the Binding's audience, which
narrows who can try). Do not let a Binding's token change shape (an email today, none later):
the owner string changes and every existing session answers `-31403` (measured, next section).

## C. Q3 and Q9: service identity, and a token with no `email`

Dex cannot mint a service token (no client-credentials grant, Q9). Two stand-ins, both real
signed tokens through the door (`oidc-service`):

- **A service-shaped email** (`svc-sre-alerts@example.test`): an ordinary owner. It owns its
  session; A is refused on it and it is refused on A's, both ways, on every method.
- **A token with no `email` claim**, made by asking Dex for scope `openid` only (the ID token
  keeps `sub`, drops `email`; that this resembles a client-credentials token is *inferred*):
  - the front door **accepts** it with no configuration change (`--email-domain=*`, default
    `oidc-email-claim`): a session is created and continued (`COMPLETED`);
  - kagent owns that session by `sub`. The same person's token *with* an `email` is refused on it
    (`-31403`) and refuses the other way; a no-email token of a different user (different `sub`)
    is refused too;
  - straight at the controller, an unsigned token carrying only that `sub` continues the session:
    the owner string is the `sub` itself.

So kagent treats a service identity as a string, no more. A Binding needs a token whose owner
claim is **stable**: the same shape on every mint. *Source:* the front door's tolerance is
deliberate in oauth2-proxy: `CreateSessionFromToken` says "Allow empty Email in Bearer case" and
`getAuthenticatedSession` validates an email only when it is non-empty
(`providers/oidc.go`, `oauthproxy.go`).

**Q9 input, for a real client-credentials provider.** Measured above: a missing `email` needs
nothing. Not measurable with Dex, *source* only (oauth2-proxy v7.15.3 `providers/provider_data.go`,
`providers/oidc.go`, `pkg/apis/options`), so H-02 must check them against the real provider:

- **The token must be a JWT.** The door verifies it as an ID token, and kagent decodes the payload
  itself; an opaque access token fails at both.
- **Audience.** `aud` must contain the door's client ID (`agent-kourier-binding` here) or an
  `--oidc-extra-audience`. Some providers put the client in `azp` and not `aud`;
  `--oidc-audience-claim` chooses the claim.
- **`email_verified`.** If the token carries `email` and `email_verified: false`, the door refuses
  it unless `--insecure-oidc-allow-unverified-email` is set. A missing `email_verified` passes.
- **Issuer.** It must equal the discovery document's issuer (`--insecure-oidc-skip-issuer-verification`
  disables that check; do not).
- `--oidc-email-claim` is **not** needed to accept a token without `email`. Setting it to `sub`
  would only change what oauth2-proxy calls the email; kagent reads its own claim from the
  forwarded `Authorization` header.

## D. Expiry (`oidc-expiry`)

Dex's ID tokens live 24 hours, so the case patches `expiry.idTokens: 1m` into Dex's ConfigMap,
restarts Dex, mints a token (`exp - iat` is 60 s, asserted), and restores the ConfigMap when it
ends (`run-oidc.sh` re-applies `dex.yaml` on exit as well; the cluster ends with the original
config). Dex is not restarted between the mint and the last call, so the refusal cannot be a
rotated key.

- Fresh token: accepted (`COMPLETED`).
- After 75 s, through the door: `SendMessage` and `GetTask` both get **HTTP 401 with the body
  `{}`** (`--force-json-errors`): no JSON-RPC envelope, no reason. An Agent Kourier client sees a bare 401.
- The door's log names the reason: `oidc: token is expired (Token Expiry: ...)` (saved in the
  fixture `06-frontdoor-log.txt`, two lines, no tokens).
- The same expired token **at the controller** completes a turn: the controller never checks
  `exp` (*source* and measured), so the door is the only expiry check.
- A token minted after that, by the same Dex and door, is accepted.

Issuer checks were not exercised; that the door checks them is *source* (the verifier's issuer
must match discovery). Of WP-01b's "issuer and expiry are inferred", expiry is now measured and
issuer stays inferred.

**Dex restarts and the door.** Dex keeps keys in memory, so a restart rotates them. After the
restarts I checked (observed, not asserted), the door answered 401 to a token signed by the new
keys for about 30 s: its fetch of Dex's keys hit a 30 s `i/o timeout` (its log: `fetching keys oidc: get keys failed ... dial tcp
... i/o timeout`), then recovered by itself. A first run without a wait failed for this reason;
the spike now polls until a fresh token passes the door. This is a test artifact of restarting
Dex, not an Agent Kourier concern, but a pilot IdP that rotates its keys will produce the same short
window of 401s at the door (*inferred*).

## E. Replay with a `contextId` (`oidc-replay`; WP-28 depends on it)

The crash window of a thread reply: the context exists, a task runs, the stream drops, and
Agent Kourier (which has not recorded the task) sends the same message again. The slow agent (eight
one-second ticks) is used, as in WP-02.

| Step | Result |
|---|---|
| m1 sent with no `contextId`, runs to `COMPLETED` | task 1, context C |
| m1 replayed with no `contextId`, task done | one `COMPLETED` snapshot of **task 1** (WP-02's result, reproduced on this agent as the control) |
| m1 replayed **with** C, task done | **a new task** in C runs to `COMPLETED` (full eight ticks): a duplicate turn. `ListTasks` shows two tasks whose history holds m1's `messageId` |
| m2 sent with C, stream dropped at the first tick, m2 replayed **with** C while it runs | **`-32004` "resource conflict"**, reason `UNSUPPORTED_OPERATION`, one event, HTTP 200. Not a live join |
| `ListTasks` `{contextId: C}` while m2 runs | the running task (`WORKING`) with m2's `messageId` in its history, so Agent Kourier can find it without a task ID |
| `SubscribeToTask` on that task | `WORKING` snapshot, deltas, `lastChunk`, `COMPLETED` |
| m2 replayed with C after it completed | **a new task** again; two `COMPLETED` tasks carry m2's `messageId` |

Without a `contextId` kagent deduplicates a replayed `messageId`, joining the task live while it
runs and returning one snapshot after (WP-02). With a `contextId` it does not deduplicate:
`-32004` while the task runs, a second full task after. WP-02's "a rejected `messageId` is not
remembered" (busy case) is the same behaviour seen from the other side.

For WP-28: a crash-recovery resend into an existing context must **not** be a blind replay.
Agent Kourier should first call `ListTasks` with the `contextId` and look for a task whose history holds
its `messageId`: found and running, `SubscribeToTask`; found and done, `GetTask` and render;
absent, send. On `-32004`, treat the context as busy (queue per D21) and repeat the lookup when
the active task ends (*inferred* rule, matching spec 8.2; the lookup finding the task is not
guaranteed, for example after a crash during an input-required pause). Not measured:
whether the model server was called twice for the duplicate task (the second run's ticks show it
executed), `ListTasks` paging past 50 tasks, and a replay with the `taskId` set.

## Not covered

`CancelTask` and `SubscribeToTask` under the header spoof; a real client-credentials token
(Dex cannot issue one); `email_verified: false` and audience variants (Dex cannot produce them);
issuer mismatch; expiry on a streaming call already in flight (that the door checks only at connect is *inferred*);
`userIdClaim` empty (`sub`, *source*) run for real; the effect of a Dex restart on sessions
(they live in kagent's Postgres, keyed by the owner string, and survived here).

## What changed in `hack/kind`

`full/dex.yaml` gained two static users, `identity-b@example.test` and
`svc-sre-alerts@example.test` (same fake password, distinct `userID`s). Nothing else. A clean
`make e2e-full-up` with them passes `verify.sh` unchanged.

## Reproduce

`make e2e-full-up`, then `spike/a2a/run-oidc.sh` (about 15 minutes: the 75 s expiry wait, four Dex restarts
with the door's 30 s stall after each, and the slow agent's eight-second runs). Extra arguments go to the spike, for example
`-case oidc-identity -out <scratch>`; each case wipes its `-out/<case>` directory. It applies
WP-02's `manifests.yaml` for the slow agent, port-forwards the front door and controller, reaches
Dex through `kubectl proxy` (the cases restart Dex, which would break a port-forward), and
re-applies `hack/kind/full/dex.yaml` on exit.

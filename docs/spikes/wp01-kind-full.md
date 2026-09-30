# WP-01 spike: full-tier kind environment on macOS (R15)

Run 2026-09-29 on Docker Desktop for Mac (Docker 29.8.0, aarch64, LinuxKit kernel
7.0.12, 8 CPUs, 7.65 GiB), kind v0.33.0, helm v4.3.0.

## Verdict for R15: works, with caveats

The full tier runs reproducibly on macOS. `make e2e-full-up` builds it from nothing and
proves the acceptance criteria itself (`hack/kind/full/verify.sh`); three clean runs passed
(178 s, 148 s, and 180 s after the review fixes). WP-01b replaced the security checks in
`verify.sh` and added a front door; its clean run took 223 s and a re-run 95 s (below). gVisor is not the problem: Substrate's
atelet downloads the aarch64 `runsc`, and actors run under it, checkpoint, and restore. The
golden snapshot became Ready and an A2A `SendMessage` returned the mock model's answer. No
isolation-weakening workaround was used.

Evidence, an excerpt from the final WP-01 run (its `verify.sh` checks were superseded by
WP-01b's, see the last section):

```
I0930 02:12:02.877670      21 config.go:554] Platform: systrap
== labelled pod: agent card and SendMessage through the controller
ok: card served and mock answer returned
== unlabelled pod: connection to port 8083 must time out
curl exit 28
ok: unlabelled pod cannot reach the controller
proxy: real=200 forged=403; controller: forged=200
full tier up in 180s (context kind-courier-full)
```

The `Platform: systrap` line is from the worker pod's runsc log.

`Platform: systrap` means gVisor needs no KVM here.

Caveats:

- **Memory is measured, headroom is a guess.** The whole environment used 1.9 to 2.1 GiB
  of the 7.65 GiB VM across five samples (measured). That courier, Alertmanager, and fake
  Slack add little is inference, not measurement; the fast tier plus this one on the same
  VM has not been tried.
- **Microvm sandboxes are unavailable** (no `/dev/kvm` in Docker Desktop). Not needed;
  the pilot uses gVisor.
- **Tested on arm64 only.** kagent's own CI runs the same recipe on ubuntu amd64, so Linux
  is expected to work, but that is inference, not measurement.
- **Everything is alpha** (kagent 1.0.0-alpha5, Substrate 0.3.0-alpha1, both unversioned
  APIs). Pins matter.

Findings that change the design, in order of importance:

1. **In OIDC mode the controller does not verify tokens.** `controller.auth.mode:
   trusted-proxy` decodes the bearer JWT without checking signature or expiry
   (`docs/architecture/oidc-proxy-authentication.md`, read from source docs and confirmed
   by measurement). Measured: an unsigned `alg: none` token claiming `mallory@evil.test`
   gets 200 on the controller's port 8083. Spec section 10 says "OIDC mode with per-Binding
   service tokens"; straight to 8083 the token proves nothing and the NetworkPolicy is the
   only control. WP-01 left three sub-findings open; WP-01b closed them (evidence in the last
   section):
   - *No working, verified A2A path.* The UI nginx at alpha5 has no `/agents/` location, so the
     200 the UI's oauth2-proxy returned for `/agents/...` with a real token was the SPA shell,
     not the card (earlier drafts of this note said the proxy routes `/agents/`; that was
     wrong). **Closed:** a dedicated front door forwards `/agents/` to the controller; card,
     `SendMessage`, HITL payloads, and SSE streaming pass through it.
   - *The UI relay was unfenced.* The UI pod had no NetworkPolicy, so any pod could use its
     nginx to reach 8083 with its own headers. **Closed:** a policy admits only the UI's
     oauth2-proxy pods to 8080.
   - *Signature checking was not tested.* A 403 for an unsigned token shows the proxy rejects
     it, not that it checks signatures. **Closed:** tokens forged from a real one (real
     signature with an altered payload; `alg: none` with the real payload) are refused.
2. **Port 8083 is not only the A2A port.** The actors' private TaskStore gRPC lands on the
   same listener, arriving from Substrate's `atenet-egress` pod. A policy admitting only
   courier makes every send fail with `KAGENT_SEND_NOT_ACCEPTED` (measured). The shipped
   policy admits the front door, the kagent UI, and `atenet-egress`. Also:
   kagent's e2e README says TaskStore currently trusts an unsigned identity header in every
   deployment (#1660), so any pod that can reach 8083 is trusted. This sharpens R11.
3. **Dex has no `client_credentials` grant** (v2.45.1 supports authorization code, refresh,
   password, device code, token exchange). The test Binding identity is a static Dex user
   that signs in with the password grant; the ID token is the bearer credential
   (audience is the client id, accepted by oauth2-proxy via `oidc-extra-audience`). H-02
   asks for client credentials, so the pilot provider must support that grant; Dex will not.
4. **Every kagent 1.0 Agent runs as a Substrate actor.** Read from source, not measured:
   `docs/architecture/README.md` and `runtime-and-lifecycle.md` at the alpha5 tag say
   Substrate Actors run the agent processes, and the Agent compiler always emits an
   ActorTemplate. I did not try to run an agent without Substrate, so treat "no
   non-Substrate path" as the documented design.

## Pinned versions

| Component | Pin |
|---|---|
| kind node | `kindest/node:v1.35.0@sha256:4613778f...b269661` (index digest) |
| Feature gates | `ClusterTrustBundle`, `ClusterTrustBundleProjection`, `PodCertificateRequest`, `certificates.k8s.io/v1beta1` |
| Substrate | release `v0.3.0-alpha1` (commit bf51a99), Helm charts `substrate` and `substrate-crds` 0.3.0-alpha1, `ateom-gvisor:v0.3.0-alpha1`; gVisor `nightly/2026-09-02` aarch64, downloaded by atelet |
| `kubectl-ate` | v0.3.0-alpha1, sha256 pinned per platform in `up-full.sh` |
| kagent | chart and images `1.0.0-alpha5` (published 2026-09-27); Go ADK runtime pinned by index digest in `agent.yaml` |
| oauth2-proxy | `v7.15.3` (chart dependency of kagent) |
| Dex | `ghcr.io/dexidp/dex:v2.45.1@sha256:8499afd6...808462` (tag and digest) |
| mockllm | `kagent-dev/mockllm` commit `35792a0` (tag `v0.1.0`), built locally as `courier-e2e/mockllm:v0.1.0`; build image `golang:1.25.14@sha256:69933...dc80`, runtime image `gcr.io/distroless/static-debian12@sha256:d75cdd...43f2` |
| CNI | kindnet `v20251212-v0.29.0-alpha-105-g20ccfc88`, bundled in the node image |
| Front door | `quay.io/oauth2-proxy/oauth2-proxy:v7.15.3@sha256:10a11657...f1f561` (index digest; the chart's tag) |
| Streaming test model | `python:3.13-alpine@sha256:79e7a9b9...120e6f` (tag and digest), serving `slow-agent`'s model from a ConfigMap |
| Probe image | `curlimages/curl:8.11.1@sha256:c1fe1679...9a69` (tag and digest) |
| Host tools | kind v0.33.0 and helm v4.3.0 were used; the scripts check that the tools exist, not their versions |

Substrate is pinned to what kagent 1.0.0-alpha5 pins (its `go.mod` replace and CI), not to
Substrate's newest tag (v0.3.0-alpha3): kagent has not been tested against later ones.

## Measurements

- **Bring-up from nothing:** 178 s, 148 s, and 180 s; 223 s with WP-01b (front door, second
  agent, and a longer verify). Host has the node image and a warm Go builder layer cache;
  ghcr images are pulled fresh each time. Idempotent re-run: 95 s with WP-01b (24 s in
  WP-01, before the review fixes added a mockllm rebuild and restart).
- **Memory:** kind node container 1.90 to 2.14 GiB of 7.65 GiB across five samples. Largest
  working sets: kube-apiserver 381 MiB, atelet 119, kube-controller-manager 95, rustfs 81,
  etcd 62, kagent Postgres 59, Substrate Postgres 52, kagent controller 48.
- **CPU:** node container 22 to 36 percent of one core (settling to about 25 percent idle);
  the VM has 8.

## Network policy

kindnet enforces NetworkPolicy in this node image, so no CNI was installed. Evidence: with
no policy an unlabelled pod reached the controller; with the controller policy
applied the same probe exits 28 (timeout) while an admitted pod still gets the card and the
answer (WP-01 used `controller-admit-courier-only`; WP-01b's policies are described below). Established
connections survive a new policy (conntrack): restart the controller and `atenet-egress`
before believing a negative result. Send `messageId` must be unique per test run; kagent
replays the stored task for a repeated one, which hid a blocked path once.

## Workarounds and deviations

- Substrate is installed from its published Helm charts (as kagent's CI does), not built
  from source with `hack/install-ate-kind.sh` (that path builds every image with ko; the
  published images are multi-arch and it is the version kagent pins). Cluster shape and
  the `proxy_arp` sysctl follow `hack/create-kind-cluster.sh`; its local registry and
  `/dev/kvm` probing are dropped because nothing here needs them.
- CA and JWT pools are created with `kubectl-ate admin make-*-pool`, plus
  `actor-id-ca-certs` and `ate-api-authentication`, exactly as kagent's CI does.
- kagent's OIDC doc says the oauth2-proxy `cookie-secret` is "a random 32-byte key, base64
  encoded". That is 44 bytes and crash-loops the proxy (`cookie_secret must be 16, 24, or 32
  bytes`); use 32 raw characters. Worth an upstream issue.
- mockllm publishes no image; `up-full.sh` builds the pinned tag and `kind load`s it.
- The mock model is reached by Service DNS name; kagent infers the actor's egress allowlist
  from the ModelConfig `baseUrl`, so no EgressPolicy was needed.
- The bundled kagent Postgres has no persistent volume; a Postgres restart loses sessions.

## Not verified

Busy rejection, session ownership across identities, and behavior across controller restarts
(WP-02 measured the first two behind a port-forward; WP-03 repeats ownership through the front
door), and token expiry and issuer checks at the front door. Streaming and the HITL header
are covered by the WP-01b section.

## Reproduce

```
make e2e-full-up   # about 4 minutes; runs hack/kind/full/verify.sh last
make e2e-down      # deletes only the courier-full cluster and the mockllm image
```

Both scripts refuse to touch a `courier-full` cluster whose node lacks the label
`courier.agent-courier.dev/owner=up-full`. They ask the cluster through kind's own kubeconfig
and tell "API server unreachable" (delete it yourself) apart from "not created by this
script"; both messages were exercised against a decoy cluster and a stopped node. One full
tier fits a Docker host: the node is annotated with the creating checkout, and both scripts
warn when another checkout reuses or deletes it. Not removed by `e2e-down`: the host's
`kindest/node` image and the cached `bin/kubectl-ate-*`.

## WP-01b: courier's verifying front door

Run 2026-09-30 on the same host: a clean `make e2e-full-up` (223 s), then a re-run (95 s).
Everything is **measured** unless marked *inferred*.

**What was built.** A second oauth2-proxy (`full/frontdoor.yaml`, image v7.15.3, the chart's
version) in front of `kagent-controller:8083`. The chart's `extraArgs` map cannot repeat
`upstream`, hence a separate instance; agentgateway was not chosen because the image and the
token logic are already in this cluster, and the Binding needs one route. It runs with
`--skip-jwt-bearer-tokens`, `--client-id=courier-binding` as the only audience,
`--force-json-errors` (a JSON 401 instead of a login redirect), `--pass-authorization-header`,
`--flush-interval=100ms`, and `--upstream=http://kagent-controller...:8083/agents/`. The
proxy logged `mapping path "/agents/" => upstream ".../agents/"`, so anything else (`/api/`,
`/mcp`, `/`) is a 404 at the door (`/api/agents` is asserted; the others were seen while building). Three NetworkPolicies (`full/networkpolicy.yaml`):
the controller admits the front door, the UI nginx, and `atenet-egress`; the UI (8080) admits
only the UI's oauth2-proxy; the front door admits only courier pods.

**`verify.sh` from the clean run** (every line is an assertion on a body, a status code, or a
curl exit code; only exit 28, a timeout, counts as "refused"):

```
== courier pod: A2A through the front door, nothing else reachable
ok: card served through the front door
ok: SendMessage answered
ok: A2A-Extensions reaches the controller: HITL payload present
ok: without the header the payload is absent
ok: front door forwards only /agents/
ok: SSE stream complete
ok: SSE events arrive incrementally (5 s apart)
ok: no route to the controller on 8083
ok: no route to the kagent UI relay on 8080
== courier pod: forged, foreign-audience, and missing tokens
ok: forgery A has a different payload
ok: real token accepted at the front door
ok: A: real signature, altered email, refused
ok: B: alg none, real payload, refused
ok: token minted for another client (audience) refused
ok: no token refused
ok: UI door refuses the Binding's token
ok: UI door serves a UI-audience token through the fenced UI   (reworded in WP-01c, see below)
== canary: the controller does not verify JWTs (kagent 1.0.0-alpha5)
ok: controller accepted both forgeries (expected at alpha5)
== courier-labelled pod in another namespace: no route to the controller, the front door, or the UI
ok: control: the pod itself has network (Dex answers)
ok: no route to http://kagent-controller.kagent.svc.cluster.local:8083/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-frontdoor.kagent.svc.cluster.local:4180/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-ui.kagent.svc.cluster.local:8080/mcp
== unlabelled pod: no route to the controller, the front door, or the UI
ok: control: the pod itself has network (Dex answers)
ok: no route to http://kagent-controller.kagent.svc.cluster.local:8083/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-frontdoor.kagent.svc.cluster.local:4180/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-ui.kagent.svc.cluster.local:8080/mcp
```

**What each closed finding rests on.**

- *A2A path.* The card body names `test_agent` and `SendMessage` returns "The answer is 4."
  through the door. `A2A-Extensions` survives the hop: with the header the reply to an
  ask-user prompt carries the `ask_user_request` payload, without it the payload is absent
  (this is why `mockllm.yaml` gained a scripted `ask_user` answer). Streaming: a second agent
  (`slow-agent`) whose model emits a chunk a second for five seconds; through the door the
  first and last SSE events arrived 5 s apart (asserted at least 3 s), so nothing buffers.
  Not exercised: a non-streaming call longer than the door's `--upstream-timeout=10m`, which
  is a choice, not a measurement.
- *Forgery.* A real Dex token was re-encoded two ways: the real header and signature with
  the payload's `email` changed to `mallory@evil.test`, and `{"alg":"none"}` with the real
  payload and no signature. Both get 401 at the door; the real token gets 200. A token Dex
  minted for the other client (audience `kagent-ui`) and a missing token also get 401, so
  the audience is enforced. Signature and audience are measured. **Issuer and expiry are
  not tested** (Dex's ID tokens live 24 h); that oauth2-proxy checks them is *inferred* from
  its OIDC verifier.
- *Canary.* Both forgeries get 200 on the controller's 8083 from a pod the policy admits
  (labelled as the UI nginx, in `kagent`). kagent alpha5 still does not verify; the check
  fails with "kagent now verifies JWTs on 8083; revisit D32/R4" if that changes.
- *UI relay.* **Corrected in WP-01c**; the evidence in this bullet as first written was
  wrong. It said a forged token got 200 from `kagent-ui:8080/api/agents`, `/api/sessions`,
  and the card under `/a2a/`. Those 200s came from the controller's catch-all
  `mux.HandleFunc("/", 200)`, which answers every path it does not know with 200 and an empty
  body (measured: the controller returns 200, empty, for `/api/agents` and `/nonexistent`
  alike, with the real token or a forged one). The controller has no HTTP `/a2a/` route, and
  the 200s prove nothing about identity. The relay evidence rests on `/mcp`, the controller
  route that answers with a body; see "WP-01c" below (a POST `initialize` gets 200 with a
  forged `alg: none` token and 401 with none, through the unfenced relay and directly on
  8083). With the policy, requests to the UI time out. `verify.sh` asserts exit 28 to the UI
  for a courier pod, a courier-labelled pod in `default`, and an unlabelled pod, and the Dex
  control request in each probe shows the pod itself had network.
- *Label spoofing.* A pod labelled `app.kubernetes.io/name=courier` in `default` reaches
  neither 8083 (no policy admits a courier pod there) nor the front door (its policy selects
  the `courier` namespace as well as the label).

**A second door, closed.** The UI's oauth2-proxy accepted the Binding's token because
`kagent-values.yaml` set `oidc-extra-audience: courier-binding`, which would have let courier
reach `/api/`, `/mcp`, and `/a2a/` through the UI. That line is removed; the UI door now
answers 403 to the Binding's token and 200 to a `kagent-ui` token (both asserted). That 200
proves only that the token passed the UI door and the fence and the request reached the
controller, which answers `/api/agents` with its catch-all 200 and an empty body; it is not an
authenticated API call (WP-01c reworded the assertion).

**Residual risk (not fixed here).**
- The trust set is labels: a pod that can be created in `kagent` with the controller-admitted
  labels, or in `courier` with the courier label, is trusted. Namespace RBAC has to keep both
  small. **Requirement:** the pilot cluster's CNI must enforce NetworkPolicy. Both fences and
  the front door's admission rule are only policy objects; on a CNI that ignores them
  (some do, silently) every check in `verify.sh` that expects exit 28 would fail, and in
  production nothing would tell you. kindnet enforces here (measured); the pilot cluster
  (H-01) must be checked the same way, with the unlabelled-pod probe.
- TaskStore trusts an unsigned identity header from actors (kagent #1660); the policy admits
  `atenet-egress` to 8083 because actors need it. R11 stands.
- `kubectl port-forward` reaches 8083 without passing a NetworkPolicy (WP-02 relied on it);
  whoever holds that RBAC can already act as anyone.
- The card advertises `http://kagent-controller.kagent.svc:8083/agents/...` as its interface
  URL, so an SDK client that follows the card lands on the blocked port. Courier must
  replace the URL with the front door's (WP-25).

## WP-01c: front-door proof hardening

Run 2026-09-30, same host. A clean `make e2e-full-up` from nothing (291 s) passed every check.
Everything is **measured** unless marked *inferred*. No security finding: nothing passed
through the front door.

**Path traversal.** With the real token and `curl --path-as-is`, each of
`/agents/../api/agents`, `/agents/..%2Fapi%2Fagents`, and `/agents/kagent%2F..%2F..%2Fapi/agents`
gets **301 with `Location: /api/agents` and an empty body**. That is oauth2-proxy's Go mux
cleaning the path (the `%2F` forms are decoded first), not a pass-through: following the
redirect lands on the door's own 404, because only `/agents/` is mapped. `verify.sh` asserts
status not 200 and no agent name in the body, for each path with and without `-L`, plus a POST
`initialize` to `/agents/../mcp` (301, no MCP answer). The controller answers *every* unknown
path with 200 and an empty body, so the status is the real assertion; the body checks would
catch a future real `/api` or `/mcp` route being reached.

**Forgery A is well-formed.** `forge()` now pads the payload to a multiple of 4 before
`base64 -d` and keeps its stderr. `verify.sh` asserts the real payload and the altered one both
start with `{` and end with `}` (the probe image has no `jq` or python), and that undoing the
email substitution gives back the real payload byte for byte. So the 401 for forgery A is the
signature check, not a malformed token.

**Trusted labels, spoofed.** A pod in `default` labelled
`app.kubernetes.io/component=ui,app.kubernetes.io/name=kagent-frontdoor` (the two selectors the
8083 policy trusts) and one labelled `app.kubernetes.io/name=oauth2-proxy` (the 8080 policy's)
each time out (exit 28) to the controller, the front door, and the UI. Their Dex control
request succeeds, so the pods have network. The policies' `podSelector` entries carry no
`namespaceSelector`, so they match only in `kagent`.

**UI relay, re-measured around `/mcp`.** An unlabelled pod in `default`, with the UI policy
deleted, POSTed an MCP `initialize` to `kagent-ui:8080/mcp`: **200 with a forged `alg: none`
token, 401 with no token**. The same POST straight to `kagent-controller:8083/mcp` from a pod
labelled as the UI nginx gives the same 200 and 401 (now a `verify.sh` canary check). So
the relay forwards a forged identity to a controller route that reads it, and the controller
treats a decoded, unverified token as authentication. `GET /mcp` gives 405 (POST only).
`/api/agents` through the relay was 200 with an empty body, the catch-all discussed above.

**Mutation check.** With `ui-admit-oauth2-proxy-only` deleted from the running cluster,
`verify.sh` fails at `FAIL: no route to the kagent UI relay on 8080` (the courier pod's
request to the UI is no longer dropped); the policy was re-applied and the full run passed.

**Requirement, not inference.** The pilot's CNI must enforce NetworkPolicy (see Residual risk).
Issuer and expiry checks at the door remain *inferred*; `frontdoor.yaml` says so, and WP-03
tests expiry.

**`verify.sh` from the clean run:**

```
== courier pod: A2A through the front door, nothing else reachable
ok: card served through the front door
ok: SendMessage answered
ok: A2A-Extensions reaches the controller: HITL payload present
ok: without the header the payload is absent
ok: front door forwards only /agents/
ok: traversal /agents/../api/agents no redirect: status 301 is not 200
ok: traversal /agents/../api/agents no redirect: no controller /api body
ok: traversal /agents/../api/agents -L: status 404 is not 200
ok: traversal /agents/../api/agents -L: no controller /api body
ok: traversal /agents/..%2Fapi%2Fagents no redirect: status 301 is not 200
ok: traversal /agents/..%2Fapi%2Fagents no redirect: no controller /api body
ok: traversal /agents/..%2Fapi%2Fagents -L: status 404 is not 200
ok: traversal /agents/..%2Fapi%2Fagents -L: no controller /api body
ok: traversal /agents/kagent%2F..%2F..%2Fapi/agents no redirect: status 301 is not 200
ok: traversal /agents/kagent%2F..%2F..%2Fapi/agents no redirect: no controller /api body
ok: traversal /agents/kagent%2F..%2F..%2Fapi/agents -L: status 404 is not 200
ok: traversal /agents/kagent%2F..%2F..%2Fapi/agents -L: no controller /api body
ok: traversal /agents/../mcp: status 301 is not 200
ok: traversal /agents/../mcp: no MCP answer
ok: SSE stream complete
ok: SSE events arrive incrementally (5 s apart)
ok: no route to the controller on 8083
ok: no route to the kagent UI relay on 8080
== courier pod: forged, foreign-audience, and missing tokens
ok: forgery A has a different payload
ok: forgery A: real payload decodes to a JSON object
ok: forgery A: altered payload is a JSON object
ok: forgery A: only the email changed
ok: real token accepted at the front door
ok: A: real signature, altered email, refused
ok: B: alg none, real payload, refused
ok: token minted for another client (audience) refused
ok: no token refused
ok: UI door refuses the Binding's token
ok: UI door passes a UI-audience token through the fence (200 is the controller's catch-all)
== canary: the controller does not verify JWTs (kagent 1.0.0-alpha5)
ok: controller /mcp initialize: forged token accepted
ok: controller /mcp initialize: no token refused
ok: controller accepted both forgeries (expected at alpha5)
== courier-labelled pod in another namespace: no route to the controller, the front door, or the UI
ok: control: the pod itself has network (Dex answers)
ok: no route to http://kagent-controller.kagent.svc.cluster.local:8083/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-frontdoor.kagent.svc.cluster.local:4180/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-ui.kagent.svc.cluster.local:8080/mcp
== unlabelled pod: no route to the controller, the front door, or the UI
ok: control: the pod itself has network (Dex answers)
ok: no route to http://kagent-controller.kagent.svc.cluster.local:8083/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-frontdoor.kagent.svc.cluster.local:4180/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-ui.kagent.svc.cluster.local:8080/mcp
== pod in default labelled as the UI and the front door (8083 trust): no route to the controller, the front door, or the UI
ok: control: the pod itself has network (Dex answers)
ok: no route to http://kagent-controller.kagent.svc.cluster.local:8083/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-frontdoor.kagent.svc.cluster.local:4180/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-ui.kagent.svc.cluster.local:8080/mcp
== pod in default labelled as the UI's oauth2-proxy (8080 trust): no route to the controller, the front door, or the UI
ok: control: the pod itself has network (Dex answers)
ok: no route to http://kagent-controller.kagent.svc.cluster.local:8083/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-frontdoor.kagent.svc.cluster.local:4180/agents/kagent/test-agent/.well-known/agent-card.json
ok: no route to http://kagent-ui.kagent.svc.cluster.local:8080/mcp
```

`up-full.sh` also restarts `deploy/slowllm` after applying it, as it does for mockllm, so an
edited ConfigMap script is picked up.

## WP-01d: the front door strips identity-selecting headers

Run 2026-09-30, same host: `make e2e-down`, then a clean `make e2e-full-up` (258 s) with 83 `ok:`
lines and no `FAIL`. Everything is **measured** unless marked *source* or *inferred*.

**The hole (R4, D32).** *Source* (kagent v1.0.0-alpha5, `proxy_authn.go`): with `X-Agent-Name` set,
the acting user is `?user_id=`, else `X-User-Id`, else the token's `sub`, even for a valid token.
WP-03 measured B's token acting as A that way through the old door. The old door forwarded both.

**The fix.** The door now runs oauth2-proxy in alpha config (`full/frontdoor.yaml`).
`--convert-config-to-alpha` from the pinned image, given the door's old flags, produced the
config; it sits in the ConfigMap `kagent-frontdoor-config`, mounted at `/etc/oauth2-proxy`, and
`up-full.sh` restarts the door so an edit is picked up. Three things differ from the converter's
output:
- **Strip-only `injectRequestHeaders` entries** (name, no values): `X-Agent-Name`, `X-User-Id`,
  `X-Share-Token` (kagent reads it on `/agents/`, `http.go`, and it grants another owner's session)
  and `X-Kagent-Insecure-Runtime-Identity`. *Source* (`pkg/middleware/headers.go`): matching is
  case-insensitive and reads `_` as `-`. The alpha list replaces the flag-derived one, so the
  converter's `Authorization` injection (`id_token`, prefix `Bearer `) and `X-Forwarded-*` entries stay.
- **`clientSecretFile`** (`/etc/oauth2-proxy-secret/client-secret`, from the existing Secret)
  replaces the client secret the converter inlines; no secret is in the ConfigMap (checked with
  `kubectl get configmap -o yaml | grep`).
- **Args:** every converted flag is gone (`--http-address`, `--provider`, `--oidc-issuer-url`,
  `--client-id`, `--upstream`, `--pass-authorization-header`, `--flush-interval`,
  `--upstream-timeout`). `--email-domain`, `--skip-jwt-bearer-tokens`, `--force-json-errors`,
  `--cookie-secure` and `--silence-ping-logging` are core options the converter does not move, and stay.

The door became ready at once; the log shows the same `mapping path "/agents/"` and JWT-skip lines
as before, so the nginx fallback was not needed. WP-03's patched rollout never became ready.
Measured here: the pinned image with `--alpha-config` plus `--http-address` prints its usage text
and exits, which matches that symptom. That it was WP-03's actual cause is *inferred*; I did not
see its pod.

**Two strips are defence in depth, resting on config and source, not on a check.**
- `X-Kagent-Insecure-Runtime-Identity` is read only by kagent's TaskStore authenticator, for the
  gRPC runtime methods (`taskstore/authority.go`, *source*), which the door cannot reach
  (`/agents/` only). There is no observable through the door.
- `X-User-Id` has no observable either, while `X-Agent-Name` is stripped: without it kagent never
  reads `X-User-Id` (`proxy_authn.go`, *source*). The reviewer removed only the `X-User-Id` entry
  and `verify.sh` still passed (83 ok, 0 FAIL, measured). The entry matters only if the
  `X-Agent-Name` strip is ever lost, and the header rows would still pass in that case.

**What `verify.sh` gained** (Dex user B, `identity-b@example.test`, from WP-03):
- the card advertises `"streaming":true` (WP-27e needs it);
- a group where B's valid token, through the door, carries `X-Agent-Name` plus `X-User-Id: <A>` and
  `X-Agent-Name` plus `?user_id=<A>`, each in canonical and lower case, and gets `-31403` on a send to
  A's context and on `GetTask` of A's task; a no-extra-headers control gets `-31403` too; A's own
  send, continue and `GetTask` still complete; a bogus `X-Share-Token` through the door is ignored (200 on the card);
- in the canary, from a pod the 8083 policy admits (the way the existing canary reaches 8083,
  not a port-forward): the same four spoofs and the same control, against the controller. The
  spoofs get `TASK_STATE_COMPLETED` (they work), the control gets `-31403`, and a bogus
  `X-Share-Token` gets 403. That is what makes the door group mean something.

New lines from the clean run (the rest are WP-01c's, unchanged):

```
ok: card advertises streaming (WP-27e refuses agents that do not)
== courier pod: X-Agent-Name, X-User-Id and X-Share-Token are stripped at the front door
ok: A's own send answered
ok: A's session has a task id
ok: A's session has a context id
ok: A continues its own session
ok: A reads its own task
ok: door, no extra headers: B continues A's session: -31403
ok: door, no extra headers: B reads A's task: -31403
ok: door, X-Agent-Name + X-User-Id: B continues A's session: -31403
ok: door, X-Agent-Name + X-User-Id: B reads A's task: -31403
ok: door, x-agent-name + x-user-id (lower case): B continues A's session: -31403
ok: door, x-agent-name + x-user-id (lower case): B reads A's task: -31403
ok: door, X-Agent-Name + ?user_id: B continues A's session: -31403
ok: door, X-Agent-Name + ?user_id: B reads A's task: -31403
ok: door, x-agent-name + ?user_id (lower case): B continues A's session: -31403
ok: door, x-agent-name + ?user_id (lower case): B reads A's task: -31403
ok: X-Share-Token is not forwarded: a bogus one is ignored
== canary: the controller does not verify JWTs (kagent 1.0.0-alpha5)
ok: controller /mcp initialize: forged token accepted
ok: controller /mcp initialize: no token refused
ok: controller accepted both forgeries (expected at alpha5)
ok: controller: A's session has a task id
ok: controller: A's session has a context id
ok: controller, no extra headers: B continues A's session: -31403
ok: controller, no extra headers: B reads A's task: -31403
ok: controller, X-Agent-Name + X-User-Id: B continues A's session: TASK_STATE_COMPLETED
ok: controller, X-Agent-Name + X-User-Id: B reads A's task: TASK_STATE_COMPLETED
ok: controller, x-agent-name + x-user-id (lower case): B continues A's session: TASK_STATE_COMPLETED
ok: controller, x-agent-name + x-user-id (lower case): B reads A's task: TASK_STATE_COMPLETED
ok: controller, X-Agent-Name + ?user_id: B continues A's session: TASK_STATE_COMPLETED
ok: controller, X-Agent-Name + ?user_id: B reads A's task: TASK_STATE_COMPLETED
ok: controller, x-agent-name + ?user_id (lower case): B continues A's session: TASK_STATE_COMPLETED
ok: controller, x-agent-name + ?user_id (lower case): B reads A's task: TASK_STATE_COMPLETED
ok: controller: a bogus X-Share-Token is read (403); the door drops it
```

**Each check can fail** (measured; the door was changed on the running cluster, verify re-run):

| Door | Result |
|---|---|
| The old door (flags only, no strip) | `FAIL: X-Agent-Name + X-User-Id: B continues A's session: -31403` (this run predates the "door," label prefix): B acted as A |
| Strip entries without `X-Share-Token` | all eight spoof checks pass; `FAIL: X-Share-Token is not forwarded: a bogus one is ignored` |
| Strip entries without `X-Agent-Name` and `X-Share-Token` | the two header rows pass (`X-User-Id` alone is stripped, so B falls back to its own `sub`); `FAIL: door, X-Agent-Name + ?user_id: B continues A's session: -31403`. The query rows are their own checks |

The cluster was then restored from the repo and the clean run above followed.

**The agent card** (`test/fixtures/kagent/card/`, fetched through the door with a real token; it
carries no token, so nothing was redacted). `capabilities`, verbatim, identical for `test-agent`
and `slow-agent`:

```json
{"extensions":[{"description":"Human approval and user input","uri":"https://kagent.dev/extensions/hitl/v1"}],"streaming":true,"extendedAgentCard":true}
```

`streaming` is **true** (measured; *source*: `gateway.go` sets `Streaming: true` for every agent).
The card's two `supportedInterfaces` URLs name `kagent-controller.kagent.svc:8083`, not the door.

**Stream, `SubscribeToTask` and `CancelTask` under the spoof** (measured by the reviewer, not by
`verify.sh`). Through the door, with `X-Agent-Name` plus `X-User-Id: <A>`, all five probes (send,
stream, `GetTask`, `SubscribeToTask`, `CancelTask`) got `-31403`, and A's running task stayed
`WORKING`. Straight to 8083 the same spoof gets `SubscribeToTask` a `WORKING` snapshot, and
`CancelTask` **cancelled A's running task**. This is the *source* too: kagent authenticates once
per HTTP request, in `AuthnMiddleware` (`go/core/pkg/auth/auth.go:133`), so every A2A method on
`/agents/` takes the same path.

**Strip entries and what removes them** (mutations; the first is the reviewer's, the second is my
run, the table's third row, which also dropped `X-Share-Token`):
- Only the `X-User-Id` entry removed: 83 ok, 0 FAIL (above).
- Only the `X-Agent-Name` entry removed (`X-User-Id` still stripped): the two header rows pass,
  because `X-User-Id` is gone and B falls back to its own `sub`, and the `?user_id=` row FAILs.
  So `X-Agent-Name` is the entry that matters, and the `?user_id=` rows are what catch its loss.

**Not covered.** The runtime-identity entry and the `X-User-Id` entry (above). WP-03's own
`oidc-headerspoof` case was not re-run here; `verify.sh` is the proof. Its two rows that expected
`COMPLETED` through the door now expect `-31403` (WP-01d).

# WP-01 spike: full-tier kind environment on macOS (R15)

Run 2026-09-29 on Docker Desktop for Mac (Docker 29.8.0, aarch64, LinuxKit kernel
7.0.12, 8 CPUs, 7.65 GiB), kind v0.33.0, helm v4.3.0.

## Verdict for R15: works, with caveats

The full tier runs reproducibly on macOS. `make e2e-full-up` builds it from nothing and
proves the acceptance criteria itself (`hack/kind/full/verify.sh`); three clean runs passed
(178 s, 148 s, and 180 s after the review fixes). gVisor is not the problem: Substrate's
atelet downloads the aarch64 `runsc`, and actors run under it, checkpoint, and restore. The
golden snapshot became Ready and an A2A `SendMessage` returned the mock model's answer. No
isolation-weakening workaround was used.

Evidence, verbatim from the final run:

```
I0930 02:12:02.877670      21 config.go:554] Platform: systrap          # worker pod, runsc
== labelled pod: agent card and SendMessage through the controller
ok: card served and mock answer returned
== unlabelled pod: connection to port 8083 must time out
curl exit 28
ok: unlabelled pod cannot reach the controller
proxy: real=200 forged=403; controller: forged=200
full tier up in 180s (context kind-courier-full)
```

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
   gets 200 on the controller's port 8083, and `verify.sh` keeps that as a canary. The same
   token through oauth2-proxy (port 4180) gets 403; that shows the proxy rejects an unsigned
   token, not that it checks signatures (a token forged from a real one is not yet tested).
   Spec section 10 says "OIDC mode with per-Binding service tokens"; straight to 8083 the
   token proves nothing and the NetworkPolicy is the only control.
   **oauth2-proxy does not yet give courier a working, verified A2A path.** The UI nginx at
   alpha5 has no `/agents/` location (it forwards `/a2a/`, `/mcp`, `/api/` to 8083 with
   `Authorization` intact), so the 200 the proxy returns for `/agents/...` with a real token
   is the SPA shell, not the agent card. Earlier drafts of this note claimed the proxy
   routes `/agents/`; that was wrong. Also, the UI pod carries no NetworkPolicy, so any pod
   can use the UI relay to reach 8083 with its own headers. WP-01b (a courier front door
   that routes A2A, a UI relay fence, and forged tokens derived from the real token) closes
   these.
2. **Port 8083 is not only the A2A port.** The actors' private TaskStore gRPC lands on the
   same listener, arriving from Substrate's `atenet-egress` pod. A policy admitting only
   courier makes every send fail with `KAGENT_SEND_NOT_ACCEPTED` (measured). The shipped
   policy admits courier (namespace `courier`), the kagent UI, and `atenet-egress`. Also:
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
| Probe image | `curlimages/curl:8.11.1@sha256:c1fe1679...9a69` (tag and digest) |
| Host tools | kind v0.33.0 and helm v4.3.0 were used; the scripts check that the tools exist, not their versions |

Substrate is pinned to what kagent 1.0.0-alpha5 pins (its `go.mod` replace and CI), not to
Substrate's newest tag (v0.3.0-alpha3): kagent has not been tested against later ones.

## Measurements

- **Bring-up from nothing:** 178 s, 148 s, and 180 s (host has the node image and a warm Go
  builder layer cache; ghcr images are pulled fresh each time). Idempotent re-run: 24 s
  (measured before the review fixes, which add a mockllm rebuild and restart).
- **Memory:** kind node container 1.90 to 2.14 GiB of 7.65 GiB across five samples. Largest
  working sets: kube-apiserver 381 MiB, atelet 119, kube-controller-manager 95, rustfs 81,
  etcd 62, kagent Postgres 59, Substrate Postgres 52, kagent controller 48.
- **CPU:** node container 22 to 36 percent of one core (settling to about 25 percent idle);
  the VM has 8.

## Network policy

kindnet enforces NetworkPolicy in this node image, so no CNI was installed. Evidence: with
no policy an unlabelled pod reached the controller; with `controller-admit-courier-only`
applied the same probe exits 28 (timeout) while a pod labelled
`app.kubernetes.io/name=courier` still gets the card and the answer. Established
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

Streaming, HITL payloads, busy rejection, session ownership across identities, and behavior
across controller restarts. Those belong to WP-14 and the Phase 0 spike.

## Reproduce

```
make e2e-full-up   # about 3 minutes; runs hack/kind/full/verify.sh last
make e2e-down      # deletes only the courier-full cluster and the mockllm image
```

Both scripts refuse to touch a `courier-full` cluster whose node lacks the label
`courier.agent-courier.dev/owner=up-full`. They ask the cluster through kind's own kubeconfig
and tell "API server unreachable" (delete it yourself) apart from "not created by this
script"; both messages were exercised against a decoy cluster and a stopped node. One full
tier fits a Docker host: the node is annotated with the creating checkout, and both scripts
warn when another checkout reuses or deletes it. Not removed by `e2e-down`: the host's
`kindest/node` image and the cached `bin/kubectl-ate-*`.

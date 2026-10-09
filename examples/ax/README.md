# AX agent with ADK and Bedrock

Agent Kourier's `a2a` dialect connects to an exact JSON-RPC endpoint and fetches its card at
`<endpoint>/.well-known/agent-card.json`. The `kagent-v1` dialect retains its existing
agent-path and HITL-extension behavior. This example runs an ADK agent inside an AX
Task and uses Bedrock Converse/ConverseStream, with the same model settings as the
kagent sandbox. AX lifecycle operations remain with the operator.

The reference is experimental. Local A2A/database recovery and AWS event streams
have automated tests; an actual AX pause/resume deployment still needs the acceptance
procedure below. It has one long-running `ask_user` tool and no mutation tools.
Questions become plain text `input-required` statuses. Thread replies and Submit
answers use Agent Kourier's existing membership checks, bot exclusion, audit and timeout
policy. Structured tool approvals are unsupported.

## Build and test

From the repository root:

```sh
make ax-test
make ax-image
make ax-smoke
```

The example is a separate Go module so ADK, AX and AWS dependencies do not enter the
Agent Kourier binary. AX is pinned to `ac2332829f22360ff97b0ba34d94dd0dd782f17e`, ADK to
v2.5.0 and the A2A SDK to v2.6.0. `ax-smoke` parses the existing mode-600 sandbox
credentials file (`SANDBOX_ENV` overrides its path), performs a real model question,
answer and replay through a loopback A2A server, and prints no credentials. It incurs
Bedrock usage. Mock tests require no credentials.

Validated locally: mock AWS binary event streams, ADK schema encoding, A2A chat,
question/answer, owner isolation, cancellation, message replay and database reopen;
the race detector passes for the reference module. A real sandbox Bedrock/Haiku
question, answer and replay also passed. The Docker image builds from public pinned
Go and Debian bases; the runner is compiled from the pinned AX module.

## Deploy

1. Build and push the image to a registry your Substrate workers can pull. Substitute
   its immutable digest, model settings and OIDC settings in `task.yaml`. Use the
   same `MODEL_NAME` and `MODEL_REGION` as your kagent sandbox. The sample is a Haiku
   inference profile; your account must have access in that region.
2. Provision the `agent-kourier-ax` atespace and AX control plane following the pinned
   [AX setup](https://github.com/google/ax/tree/ac2332829f22360ff97b0ba34d94dd0dd782f17e).
   The Task runs `/usr/local/bin/agent-kourier-ax-agent`; AX itself starts the fixed
   `/usr/local/bin/ax-task-runner` path. This image replaces that runner with the
   pinned Go runner on metadata port 8081 for running Tasks (port 80 during the
   command-free golden-template build). The agent owns port 80 and proxies
   `/metadata/` and `/readyz` to the runner. Workspaces still use AX preparation.
3. Configure Substrate TLS interception and credential injection, plus a registered
   credential provider containing the Bedrock API key. Apply `egress-policy.json`
   through `ateapi.Control/CreateActorEgressPolicy` once the actor exists. Replace the
   provider URI and issuer host first. Use `UpdateActorEgressPolicy` with the fetched
   UID/version if a policy already exists. Unlisted destinations stay denied. Allow
   any separate OIDC JWKS host explicitly, without a credential effect. The real
   Bedrock key belongs in the provider's Kubernetes Secret, never the Task spec or
   image. The SDK sends the nonsecret placeholder and the gateway replaces it.
   [AWS documents this bearer authentication](https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys-use.html).
4. Merge `template-overlay.yaml` into AX's generated Substrate ActorTemplate using
   the Substrate API, **before running the agent or building its golden snapshot**.
   Retain the generated storage location, durable `/workspace` volume and other
   fields. Set the actual container name from the generated template. The fragment
   adds the egress CA projection and selects DATA snapshots with COLD_BOOT resume,
   so a fresh agent process opens the actor's restored SQLite state. AX's default
   templates do not supply this projection. This provisioning step is required;
   applying the Task alone does not enable model credentials or trusted TLS.
5. Apply the Workspace and Task with `ax apply -f task.yaml` after template/egress
   provisioning. AX eagerly reconciles tasks, so prepare template/policy while the
   actor is paused; the agent fails closed until issuer and egress are available.
6. Expose the router through a TLS gateway. `gateway.conf` forwards only the A2A
   endpoint/card, overrides the actor header to one fixed actor, and disables
   buffering for SSE. Restrict access to Agent Kourier with a NetworkPolicy. The reference
   actor verifies the service ID token itself and rejects requests from other
   subjects; for protection before actor wake-up, put the existing OIDC-verifying
   front door in front of this proxy as well.
7. Load `agent-kourier.yaml` into Agent Kourier's configuration directory, substituting gateway,
   channel, ChatConnection and refreshed ID token Secret. The token audience and
   subject must match the agent. Each binding identity owns its A2A tasks; Slack
   attribution text does not select the authenticated owner.

## Persistence and acceptance

ADK sessions, A2A task snapshots and owner/message-ID deduplication share
`/workspace/agent-kourier-state/agent.db` (WAL, FULL synchronous). Include the complete
directory, including WAL files, in the actor's durable data snapshot. Run one agent
process per database. Completed tasks and unanswered questions survive reopening.
Work that was `submitted` or `working` at restart becomes `failed`; model/tool work
is not automatically replayed. A2A transport retries with the same message ID join
or return the persisted task. Cancellation uses the SDK task lifecycle.

Before enabling AX for a real channel, test against an isolated atespace:

1. Ask a question in Slack; check streaming text and the posted question.
2. Answer in the thread; check the same task completes and the audit identifies the
   answerer. Confirm a nonmember/bot cannot answer and a timed-out answer is refused.
3. Pause the actor while a question is pending, resume by calling the card through
   the gateway, and answer. Check the task/context IDs and saved ADK session survive.
4. Repeat with a completed task, resend the same message ID, and confirm no new
   model invocation. Drop the stream and reconnect with `tasks/resubscribe`.
5. Pause during model execution; confirm the reopened task fails once, then accepts
   a fresh turn in the same context. Cancel an active task and check its terminal state.
6. Check missing/expired/wrong-audience/wrong-subject bearer tokens are rejected,
   actor-header spoofing cannot route elsewhere, and unrelated egress is denied.

An SSE stream is not a portable execution snapshot. Recovery is based on persisted
task state; this procedure must verify the chosen AX/Substrate snapshot backend.

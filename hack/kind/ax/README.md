# AX E2E tier

`make e2e-ax-up` creates `kind-courier-ax`, then `make e2e-ax` runs the
shared in-cluster E2E Job. `make e2e-ax-down` removes only this tier.
It does not reuse `courier-full`, which may be running the live kagent sandbox.

After stopping or restarting the entire kind node, Substrate can retain actor routes
whose A2A listeners no longer exist. If authenticated card probes report connection
refused despite ready pods, recreate this test tier with `make e2e-ax-down` followed
by `make e2e-ax-up`. This is separate from the actor suspend/resume and DATA
cold-boot operations covered by S2.

Courier is installed by its Helm chart (`charts/courier`, `ci/ax-values.yaml`: the testhooks image on the fake Slack, the config of
`hack/kind/ax/courier/config` in a ConfigMap that `up-ax.sh` makes, and a database file of its own, `courier-ax.db`). Re-running
`make e2e-ax-up` on a cluster brought up before the chart replaces its hand-made Deployment and keeps its volume.

The tier runs the unmodified AX revision pinned in `examples/ax/go.mod`, Redis,
Substrate, Dex, and the same persistent ADK/A2A server as the Bedrock reference.
A separate test binary supplies deterministic model responses. The production
Bedrock image contains neither that model nor the test control endpoints.

| Scenario | AX coverage |
| --- | --- |
| Smoke and fault injection | Same fake Slack, Alertmanager, sink, Courier deployment and crash hooks |
| S1 | Gated streaming, one final message |
| S2 | Context across Courier restart and AX actor suspend/resume and DATA pause/cold-boot |
| S3 | Plain-text question, outsider refusal, authorized thread answer after Courier restart, one continuation |
| S4 | Queued thread reply, FIFO delivery after active task |
| S5 | Courier crash after posting, outbox adoption without duplication |
| S11 | Explicitly skipped: structured tool-approval metadata is specific to kagent |
| S12 | Failed provider and empty completion, safe user notice and outcome metric |
| S13 | Explicitly skipped: generic A2A does not interpret kagent tool-step metadata |
| S14/S15/S17 | Fast-only alert-trigger fixtures, also skipped by kagent |
| AX deadline | Question expires and cancels without synthesizing an answer |

The fixture provisioner uses public AX/Substrate RPCs. It pre-creates each
Courier-owned template with a durable `/workspace` and `COLD_BOOT` data restore
before AX's reconciliation builds its golden snapshot. Template names follow the
pinned AX image/environment digest algorithm; recheck this when upgrading AX.
The actor's outbound policy admits only Dex. Startup verifies authenticated cards through
Courier's network path and uses the full tier's bounded kind-network recovery if needed. AX and Substrate sources are not
patched.

Gate and task lifecycle controls are ClusterIP-only in namespace `e2e`. The
local image registry publishes only on loopback (`127.0.0.1:5002`) and carries an
ownership label checked by the scripts. The fixture image is pushed there because
Substrate's image cache cannot use kind's containerd side-loaded actor images.

For actual model calls, `make ax-smoke` uses Bedrock/Haiku and the sandbox
credentials file; see [the reference instructions](../../../examples/ax/README.md).
The deterministic tier does not spend model tokens.

Verified on macOS/arm64: the AX E2E Job passed in 130 seconds, including actor DATA
pause/cold-boot, text answer after Courier restart, and crash-after-post adoption.
The AX module race tests and vet, shared harness tests, tagged lint, and shell checks pass.

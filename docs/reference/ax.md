# AX add-on

The AX add-on starts a Task on the private AX control plane for each accepted A2A turn. It polls AX for sandbox progress. The result wrapper's authenticated callback supplies the command result. See [Connect the AX add-on](../how-to/connect-ax.md) for the installation and Task template.

## Turn lifecycle

Repeated A2A message IDs join their existing task. At the running limit the add-on returns A2A busy, which Agent Kourier can retry from its queue. A prompt that exceeds AX's Task limits is refused before the add-on launches a Task. The add-on quotes earlier turns from the authenticated caller's thread and drops the oldest turns first when assembling a prompt. With history disabled it sends only the current message. AX receives a stable session ID for each caller and thread.

In live mode with `chat.progress: steps-and-activity`, observed sandbox transitions appear as `AX sandbox queued.` and `AX sandbox running.`. Fast Tasks may pass through a state between polls. A successful result is persisted with the completed A2A task before subscribers see it. A failed command shows Agent Kourier's standard failure notice with a troubleshooting reference. The launcher records the numeric exit code in its diagnostic log. Failed command output and stderr stay out of Slack and the diagnostic log. Oversized successful output has a visible truncation marker.

The launcher requests deletion after settlement, cancellation, deadline expiry, or interruption. It suspends a failed command's actor for the inspection window and requests deletion at expiry. Cleanup retries can continue after the A2A task leaves history. The launcher checks Task ownership before issuing a cleanup RPC. Use a dedicated launcher name and leave its AX Task names under its control.

After restart or takeover, the launcher fails unfinished A2A turns and cleans up their Tasks. An accepted receipt that was still awaiting A2A settlement is replayable as a receipt. It does not publish the interrupted result. Completed A2A tasks remain replayable under the usual retention settings.

## Caller check

Use a separate token file for A2A callers. The kit permits `AUTH_DISABLED=true` only on a loopback listener. For header and lease rules, see [the shared add-on limits](limits.md#every-add-on).

## Sessions and their lifetime

The shared A2A task and context retention settings govern conversation history. AX launch receipts have their own retention and can outlive that history.

## Settings

--8<-- ".addon-settings.md:settings"

### Launcher settings

| Variable | Default | Helm setting |
| --- | --- | --- |
| `AX_ADDR` | Required | `addons.ax.server.address` |
| `AX_ATESPACE` | `default` | `addons.ax.atespace` |
| `AX_LAUNCHER_ID` | Required. The chart uses the add-on name for the release. | `addons.ax.launcherId` |
| `AX_TASK_TEMPLATE` | Required | `addons.ax.taskTemplate.configMap` |
| `AX_PROMPT_KEY` | `prompt` | `addons.ax.promptKey` |
| `AX_RESULT_URL` | Required HTTPS URL ending in `/ax/results/v1` | `addons.ax.resultCallback.url` |
| `AX_RESULT_TOKEN_FILE` | Required, one or two distinct callback bearers, one per line | `addons.ax.resultCallback.tokenSecret` |
| `AX_HISTORY` | `true` | `addons.ax.history` |
| `AX_MAX_RUNNING` | `10` | `addons.ax.maxRunning` |
| `AX_POLL_INTERVAL` | `2s` | `addons.ax.pollInterval` |
| `AX_TURN_TIMEOUT` | `30m` | `addons.ax.turnTimeout` |
| `AX_KEEP_FAILED` | `24h`, or `0` for deletion after settlement | `addons.ax.keepFailed` |

Restart the launcher to pick up a new Task template. Callback tokens reload from the mounted Secret every five seconds. Use the shared [add-on limits](limits.md#every-add-on) for A2A authentication, retention, and leader election.

## Runner callback protocol

The wrapper reads the AX Task UID from the runner's local metadata endpoint. It posts `result_id`, `task_uid`, and `invocation_id` to `/ax/results/v1/start` before running the command. Only the granted invocation can report. It posts the same three fields plus `exit_code`, `output`, `output_source`, and `truncated` to `/ax/results/v1`. Output sources are `file`, `stdout_stderr`, or `none`. An empty regular output file counts as `file`. Both endpoints require JSON and the callback bearer.

An identical result retry returns `204` while its receipt is retained. A conflicting result or a second invocation's start claim returns `409`. Unknown registrations and mismatches return `404`. Closed or expired registrations return `410`. The wrapper verifies TLS, follows no redirects, and retries the same request body on transport failures, HTTP `429`, and HTTP `5xx` within the deadline. The launcher treats the correlation ID as part of the registration. The Gateway bearer authenticates the request. The wrapper removes its local replay file after the launcher acknowledges the result. An unacknowledged result can still leave a replay file, so keep persistent Workspaces within a single trust boundary.

## Metrics

- `agentkourier_ax_rpc_duration_seconds` measures AX RPC latency by method and gRPC code.
- `agentkourier_ax_callback_total` counts callbacks by route and HTTP response.
- `agentkourier_ax_result_latency_seconds` measures time from turn start to the first committed result.
- `agentkourier_ax_history_trimmed_total` counts earlier turns discarded to fit the prompt.
- `agentkourier_ax_cleanup_pending` counts launches awaiting cleanup on the leader.
- `agentkourier_ax_cleanup_retained` counts suspended Tasks within the failure inspection window.
- `agentkourier_ax_turns_total` records the A2A turn outcome, including deadline expiry.

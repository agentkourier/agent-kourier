# How-to guides

For engineers who know what they want done: each guide is one task, with the steps to finish it on your own cluster.
The guides assume you know Kubernetes and Helm, and link to [explanation](../explanation/index.md) pages for the why.

## Install and connect

- [Install with Helm](install-with-helm.md): the chart, the Secrets and a first answer in a thread.
- [Configure Slack](configure-slack.md): the Slack app, its tokens and the ChatConnection.
- [Create the Binding identity](create-binding-identity.md): choose a user ID or issue and store a bearer token.
- [Write a Binding](write-a-binding.md): your agent in your channel.

## Agents

- [Connect a kagent agent](connect-kagent.md): kagent 1.0 or 0.9.x.
- [Connect a generic A2A agent](connect-a2a-agent.md): any agent that streams A2A over JSON-RPC.
- [Connect an agent on Google AX](connect-ax.md): an A2A agent running in an AX Task.
- [Let the agent ask questions](agent-questions.md): answers from the thread.

## Alerts

- [Investigate Alertmanager alerts](investigate-alertmanager-alerts.md): an investigation in each alert's thread,
  with the `alertmanager` preset.
- [Start investigations from any bot's messages](any-bot-trigger.md): a trigger written in full for another tool's
  bot.
- [Tune triggers with a dry run](tune-triggers.md): replay a channel's history before going live.

## Security

- [Put a verifying front door before kagent](front-door.md): verify the Binding's token and strip identity headers.
- [Rotate the Binding token](rotate-binding-token.md): by hand or on a schedule.

## Operate

- [Run two replicas (high availability)](high-availability.md): Postgres, two replicas on separate nodes, a budget
  for drains, and a failover drill.
- [Export metrics and traces](export-telemetry.md): Prometheus, OpenTelemetry and alerts worth having.
- [Upgrade](upgrade.md): a new build, a rollback, and the move across the rename.
- [Troubleshoot an install](troubleshoot.md): from what you see to the cause and the fix.

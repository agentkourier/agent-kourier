# Roadmap

This is the order of work, without dates. The phases and their exit tests are in
[spec section 12](spec.md#12-phases). The order here differs from the spec in one
place: the first part of hardening (the chart, Postgres, a standby and published
images) came before the pilot, so that the pilot runs on the hardened build. The
spec still lists the pilot first. The
[README](README.md#integrations) lists what each integration supports today.

## Implemented

- Slack over Socket Mode: mentions, thread replies, streamed output and a durable
  reply queue.
- Recovery after a restart, and an outbox that does not repeat a post after a crash.
  A streamed reply cut off as it starts is the one exception.
- Questions from the agent, answered in the thread.
- `botMessage` triggers, with a preset for the alerts Alertmanager posts to Slack:
  one thread per alert, a cooldown, a rate limit and a daily cap.
- Two agent dialects, `kagent-v1` and generic `a2a`, and an experimental Google AX
  reference agent.
- Audit log, Prometheus metrics and optional OpenTelemetry traces.
- Resources loaded from YAML files, usually a mounted ConfigMap.
- A Helm chart.
- A Postgres store beside SQLite.
- Two replicas on Postgres, one leading and one standing by: a Kubernetes
  Lease elects the leader, and an epoch in the database fences a deposed
  leader's writes.
- A release workflow that publishes signed multi-arch images and a signed Helm
  chart to GHCR on a version tag.
- Useful and not-useful buttons on each investigation, one changeable vote per
  person.

## Next: the pilot

- Four weeks with one team, on production alerts.
- A dashboard for the pilot's results.

## Later

These wait for the pilot's verdict or for an upstream release.

- Custom resource definitions and a controller, so `kubectl get bindings` works.
- Slack over the Events API, and direct messages.
- An Alertmanager webhook that Agent Kourier receives directly.
- Tool approvals from Slack, for agents that can change things.
- The move to kagent 1.0 when it is released.
- More chat platforms. The candidates are Mattermost, Microsoft Teams and Discord.
- Open-source readiness: the governance files (contributing, security policy, code
  of conduct) and conformance tests for the `a2a` dialect.

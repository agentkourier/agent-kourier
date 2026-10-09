# Roadmap

This is the order of work, without dates. The phases and their exit tests are in
[spec section 12](spec.md#12-phases). The order here differs from the spec in one
place: the first part of hardening now comes before the pilot, so that the pilot
runs on the hardened build. The spec still lists the pilot first. The
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

## In progress: hardening

- A Helm chart.
- A Postgres store beside SQLite.
- Two replicas, one active and one standby, with a failover test that loses and
  duplicates nothing.
- Container images published to GHCR.

## Next: the pilot

- Four weeks with one team, on production alerts.
- Useful and not-useful buttons on each investigation.
- A dashboard for the pilot's results.

## Later

These wait for the pilot's verdict or for an upstream release.

- Custom resource definitions and a controller, so `kubectl get bindings` works.
- Slack over the Events API, and direct messages.
- An Alertmanager webhook that Agent Kourier receives directly.
- Tool approvals from Slack, for agents that can change things.
- The move to kagent 1.0 when it is released.
- More chat platforms. The candidates are Mattermost, Microsoft Teams and Discord.
- Open-source readiness: a license, a tutorial, reference docs and conformance tests
  for the `a2a` dialect.

# Roadmap

This page describes where Agent Kourier stands and the order of the work ahead, without dates.

Agent Kourier is in an internal-first pilot and has no tagged release yet. The first part of hardening (the
chart, Postgres, a standby and published images) came before the pilot, so that the pilot runs on the hardened build.

## Implemented

- Slack over Socket Mode: mentions, thread replies, streamed output and a durable reply queue.
- Recovery after a restart, and an outbox that does not repeat a post after a crash. A streamed reply cut off as it
  starts is the one exception.
- Questions from the agent, answered in the thread.
- `botMessage` triggers, which start investigations from the alerts a bot posts to the channel: one thread per alert,
  a cooldown, a rate limit and a daily cap. A preset fills them in for Alertmanager; another bot's trigger is written
  in full.
- Two agent dialects, `kagent-v1` and generic `a2a`, reading A2A 1.0 and 0.3 agents, and an experimental Google AX
  reference agent.
- An audit log, Prometheus metrics and optional OpenTelemetry traces.
- Resources loaded from YAML files, usually a mounted ConfigMap.
- A Helm chart.
- A Postgres store beside SQLite.
- Two replicas on Postgres, one leading and one standing by: a Kubernetes Lease elects the leader, and an epoch in
  the database fences a deposed leader's writes.
- A release workflow that publishes signed multi-arch images and a signed Helm chart to GHCR on a version tag.
- Useful and not-useful buttons on each investigation, one changeable vote per person.

## Next: the pilot

- Four weeks with one team, on production alerts.
- A dashboard for the pilot's results.

## Later

These wait for the pilot's verdict or for an upstream release.

- Custom resource definitions and a controller, so `kubectl get bindings` works.
- Slack over the Events API, and direct messages.
- A webhook receiver, so an alert source can post to Agent Kourier directly instead of through Slack.
- Tool approvals from Slack, for agents that can change things.
- The move to kagent 1.0 when it is released.
- More chat platforms. The candidates are Mattermost, Microsoft Teams and Discord.
- Open-source readiness: the governance files (contributing, security policy, code of conduct) and conformance
  tests for the `a2a` dialect.

## What each integration supports today

| Kind | Integration | Status |
|---|---|---|
| Chat platform | Slack over Socket Mode: mentions, thread replies, streamed output, questions | Implemented |
| Chat platform | Slack over the Events API (HTTP) | Planned |
| Chat platform | Slack direct messages | Planned |
| Chat platform | Other platforms, through the `chat.Platform` interface and its conformance suite | Planned |
| Agent backend | kagent 1.0, `kagent-v1` dialect, with structured questions; tested against 1.0.0-alpha5 | Implemented |
| Agent backend | Any A2A 1.0 or 0.3 agent whose card advertises streaming and a JSON-RPC interface, `a2a` dialect | Implemented |
| Agent backend | kagent 0.9.x, `a2a` dialect over A2A 0.3; tested against 0.9.4 | Implemented |
| Agent backend | Google AX, through an A2A agent in an AX Task and the `a2a` dialect | Experimental |
| Agent backend | Tool approvals answered from Slack | Planned |
| Alert source | Alertmanager, through the message it posts to Slack (`botMessage` trigger, `alertmanager` preset) | Implemented |
| Alert source | Any other bot that posts to the channel, with a `botMessage` trigger written in full; no preset, and not exercised in the sandbox or the end-to-end tests | Implemented |
| Alert source | Alertmanager webhook, received by Agent Kourier directly | Planned |

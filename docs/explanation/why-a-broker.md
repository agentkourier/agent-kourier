# Why a broker

This page explains why Agent Kourier is a separate, always-on broker between people, alerts and agents, rather than a
proxy in front of an agent or a feature of one agent platform.

## The gap

Building an agent and connecting it to Slack are separate jobs. A Slack integration has to handle events, track
threads, format replies, and recover conversations after a restart. Putting that handling into each agent repeats the
integration work and gives each agent developer messaging code to maintain alongside the agent's tools and task logic.

Agent Kourier handles the messaging work in a shared broker. Developers connect an agent that exposes a supported
streaming A2A endpoint through a declarative `Binding`, without adding Slack-specific code to the agent. Messages and
alerts reach the agent through A2A, and its answers return to the Slack thread.

When the project started, kagent had no webhook trigger and no way to take in alerts, and
upstream had declined a Slack bot in core. A kagent maintainer stated that events arriving while an agent is asleep need
a third party to act as a broker. The chat half of the problem was partly solved elsewhere; what was missing was the
combination:

1. An alert becomes a Slack thread that is also a live agent session.
2. People continue that session, and answer the agent's questions, in the thread.
3. It works with any A2A agent, not one platform's roster.
4. It is configured declaratively and runs independently of the agents.

## Separate responsibilities

The agent owns its tools and task logic. Its runtime deploys and executes it. Agent Kourier owns the Slack connection,
thread-to-session mapping, reply queue, output rendering, and delivery recovery. Multiple bindings share that handling
while connecting channels to different agents.

A2A is the boundary between the broker and the agent. An agent that already exposes a supported streaming A2A endpoint
needs configuration to connect to Slack. An agent without that endpoint still needs an A2A implementation or adapter;
Agent Kourier removes the need to implement Slack handling in the agent. See
[Connect a generic A2A agent](../how-to/connect-a2a-agent.md) for the endpoint requirements.

## Why always on

Agents may be suspended, scaled to zero, or restarting. Something has to hold the Slack connection, take the alert,
and remember which thread belongs to which agent context while the agent is not there. Agent Kourier holds that state
and wakes the agent by calling it.

That state is also why a restart loses nothing. Sessions, queued replies and pending questions are persisted, and
posts go through a durable outbox, so after a restart Agent Kourier continues each conversation where it stopped. See
[Delivery guarantees](delivery.md).

## Why not a proxy

A proxy forwards a request and forgets it. A conversation in a thread is not one request: it is a sequence of turns,
some of which arrive while the agent is still working, some of which answer the agent's question, and some of which come
from an alert rather than a person. Agent Kourier queues the reply that arrives while a task runs, renders the agent's
question and validates the answer, streams the output into one message, and recovers the task after a crash. None of
that fits in a stateless hop.

The choice of A2A as the one protocol to agents is explained in [Why A2A](why-a2a.md).

Authentication of A2A traffic is a different job, and Agent Kourier leaves it to tools built for it: agentgateway or
oauth2-proxy in front of the agent. See [Security model](security.md).

## Why outside the paging path

Agent Kourier is an additional reader of alerts, never a replacement. It reads the alert your alerting tool already posts
to Slack. It is not an Alertmanager receiver and takes no webhook, so Alertmanager's routes are untouched. An Agent Kourier outage therefore never loses a
page: the alert has already reached people through its own receiver. For the same reason, an alert that a rate limit or
daily cap stops is not posted about one by one; it is counted in one digest line.

## Why protocol first

Agent Kourier speaks public A2A plus one published extension, and needs nothing internal to the agent platform. That
keeps it working with non-kagent agents, including an A2A agent running as a Google AX task, and insulates it from a
platform's internal API changes. Backend-specific behaviour lives in a dialect.

## Why declarative and team-owned

A team commits a namespaced `Binding` next to its agent's manifests, and GitOps deploys both. The platform team owns
the shared `AgentBackend` and `ChatConnection`; an application team wires its own channel to its own agent without
touching platform configuration. Cross-namespace references work only where the owning resource allows the referring
namespace.

## What it gives up

- **Per-person identity at the agent.** Every turn runs as the Binding's service identity. If per-person tool authority
  becomes a requirement, that is a different design. See [Security model](security.md).
- **Shared maintenance.** Agent Kourier is maintained by its own project, not by an agent platform.

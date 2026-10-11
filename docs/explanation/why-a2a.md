# Why A2A

This page explains why Agent Kourier speaks one protocol to agents, A2A, rather than integrating with each agent
platform, and what that choice costs.

## The choice

Agent Kourier talks to every agent through the [Agent2Agent (A2A) protocol](https://a2a-protocol.org): JSON-RPC over
HTTP, version 1.0 or 0.3, with streaming. A kagent agent, an agent running in a Google AX Task and a service you wrote
yourself sit behind the same client. kagent adds one published extension, for structured questions and tool steps,
which the `kagent-v1` dialect reads. Everything else is plain A2A, spoken by the generic `a2a` dialect.

## What the broker needs from an agent

A broker that turns a Slack thread into an agent session needs five things from the other side:

| The broker needs | A2A provides |
|---|---|
| A way to find out what an agent is and how to reach it | The agent card, served at a well-known path |
| One conversation per thread, across many turns | A context ID that groups tasks and messages |
| The answer as it is written, so the thread can show partial output | Streaming task and message events |
| The agent to be able to stop and ask | The `input-required` task state |
| To follow, resume or cancel a turn after a crash or a restart | Tasks with an ID and a lifecycle that can be read back |

Those map onto the broker's own model without translation. A thread's session is an A2A context, a turn is a task, and
a question the agent asks is a task paused in `input-required`, which the person's reply resumes. The first turn needs no
context ID: the agent creates one and Agent Kourier keeps it for the thread. See
[Threads and sessions](threads-and-sessions.md).

## Why not a client for each platform

The alternative is a client per platform: one for kagent's controller API, one for AX's lifecycle API, another for the
next runtime. Three reasons it was not built that way.

- **For AX there was no other way in.** AX has no message model of its own: its API manages the lifecycle of sandboxed
  tasks, not conversations. The only route to a conversation with an agent running there is the A2A endpoint the agent
  itself serves. AX no longer ships an A2A agent, so you build and run one as a Task; the
  [AX guide](../how-to/connect-ax.md) covers what that needs.
- **For kagent, nothing internal is needed.** kagent's A2A gateway creates a session when a message carries no context
  ID, so a client that speaks public A2A and one published extension is enough. That keeps Agent Kourier clear of
  kagent's internal APIs, which were changing quickly through the 1.0 alphas.
- **A new runtime should not need new code.** A team picks its runtime for its own reasons: the model, the tools, the
  language, where it runs. With A2A, a team that changes or adds a runtime keeps the same `Binding` and the same Slack
  channel. The `Binding` names a backend and an agent; nothing else about the wiring depends on what runs behind it.

## Why not MCP

MCP and A2A are built for different ends. MCP connects an agent to tools and data: it is how an agent reads a log or
lists a Deployment. A2A connects a client to an agent: a conversation with a task lifecycle, streaming output and a pause
for input. The practical point is where the agents are reachable: kagent agents and agents hosted on AX expose A2A
endpoints and cards, not MCP servers a chat broker could converse with. The agents can use MCP for their own tools.
Agent Kourier does not call those tools; at most it shows a tool step the agent reports.

## Why not a plain HTTP API to the agent

A single request and response cannot carry what a thread does. The agent's answer arrives over seconds or minutes, in
pieces. The agent may stop and ask a question that a person answers an hour later. The broker may restart in the
middle. A protocol with streaming, task state and a context ID gives the broker something to resume from; a bare HTTP
call gives it nothing to ask about after a restart.

## What it costs

- **A2A does not standardise everything the chat needs.** Structured questions with choices and tool approvals are
  runtime extensions. The `kagent-v1` dialect reads kagent 1.0's structured questions and shows its tool steps as
  cards; it refuses a tool approval request with a notice, because approvals from Slack are not built yet. Every other
  agent, kagent 0.9.x and AX included, asks questions as plain text that a person answers by replying.
- **The agent must stream.** Agent Kourier rejects an agent whose card does not advertise streaming, and one that
  offers only gRPC or REST. Streaming lets the thread show partial output, and one transport means one code path to
  keep correct.
- **The protocol is versioned, and two versions are in use.** A2A 1.0 and 0.3 differ in card shape and methods, and
  kagent 0.9.x still speaks 0.3. The client reads both. A 0.3 agent has no task list, so after a crash a turn may be
  sent twice; see [Delivery guarantees](delivery.md).
- **Verifying the caller is left to the deployment.** Agent Kourier sends the Binding's identity, a service token as a
  bearer token or a user ID header, and the agent or a proxy in front of it has to verify it. kagent's controller port
  trusts identity without verifying it, so it needs a verifying front door. See [Security model](security.md).

## In practice

To point a Binding at an agent that speaks A2A, see [Connect a generic A2A agent](../how-to/connect-a2a-agent.md). For
kagent, whose dialect adds structured questions and tool steps, see [Connect a kagent agent](../how-to/connect-kagent.md).
AX uses the generic dialect: see [Connect an agent on Google AX](../how-to/connect-ax.md).

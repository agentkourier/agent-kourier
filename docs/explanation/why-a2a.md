# Why A2A

This page explains why Agent Kourier speaks one protocol to agents, A2A, rather than integrating with each agent
platform, and what that choice costs.

## The choice

Agent Kourier talks to every agent through the [Agent2Agent (A2A) protocol](https://a2a-protocol.org): JSON-RPC over
HTTP, version 1.0 or 0.3, with streaming. To the broker, a kagent agent, an agent running in a Google AX Task and any
other agent that serves A2A look the same. kagent adds one published extension, for structured questions and tool
steps, which the `kagent-v1` dialect reads. Everything else is plain A2A, spoken by the generic `a2a` dialect.

kagent is the first backend we tested because it is the default way to provision agents, and it is Kubernetes-native by
design. Agent Kourier does not depend on kagent: an AX agent or any other A2A agent connects through the same `Binding`.

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
  runtime extensions, and Agent Kourier reads one of them, on both dialects: see
  [Why approvals use one extension](#why-approvals-use-one-extension). A Binding enables its Slack approval prompts with
  `interactions.toolApprovals`. Tool steps do not need a dialect: every dialect shows them as cards for an agent that
  marks its tool calls the way Google ADK or kagent does. From an agent that does not speak the extension, the `a2a`
  dialect accepts only text-only input-required questions and cancels the task at a pause it cannot use, so the AX
  reference agent cannot use Slack tool approvals through this broker. A pinned kagent 1.0.0-alpha5 runtime check approved `add_numbers` and
  denied `check_status` in one two-tool pause; only `add_numbers` executed. The broker's end-to-end suite verifies this
  with fake Slack and pinned kagent 1.0.0-alpha5.
- **The agent must stream.** Agent Kourier rejects an agent whose card does not advertise streaming, and one that
  offers only gRPC or REST. Streaming lets the thread show partial output, and one transport means one code path to
  keep correct.
- **The protocol is versioned, and two versions are in use.** A2A 1.0 and 0.3 differ in card shape and methods, and
  kagent 0.9.x still speaks 0.3. The client reads both. A 0.3 agent has no task list, so after a crash a turn may be
  sent twice; see [Delivery guarantees](delivery.md).
- **Verifying the caller is left to the deployment.** Agent Kourier sends the Binding's identity, a service token as a
  bearer token or a user ID header, and the agent or a proxy in front of it has to verify it. kagent's controller port
  trusts identity without verifying it, so it needs a verifying front door. See [Security model](security.md).

## Why approvals use one extension

A tool approval needs more than a question. The person approving must see exactly which tool will run and with which
arguments, Agent Kourier must be able to gate the press on `approverGroups` and audit it as an approval, and the agent
must get back a decision for each call, not a sentence to interpret. A2A 1.0 has no such shape. Its `input-required`
state says that the agent wants input and leaves what the input is to the agent
([specification](https://a2a-protocol.org/latest/specification/)), and the A2A project has said that structured
confirmation belongs in an extension rather than in the core protocol (in
[a2aproject/a2a-python#412](https://github.com/a2aproject/a2a-python/issues/412) and
[a2aproject/A2A#2149](https://github.com/a2aproject/A2A/pull/2149)). It has published no such extension yet.

Agent frameworks that serve A2A fill that gap differently. As of October 2026, some put a pending approval on the wire in
a shape of their own, each in a different place (a data part, an artifact, message metadata) and with a different
reply; some send only text; and some complete the task without asking at all. None of those shapes is declared in the
agent card, so nothing tells a client which one to expect, and each can change in a framework's next release.

Agent Kourier therefore reads one approval contract and no framework's own format: kagent's human-in-the-loop
extension, `https://kagent.dev/extensions/hitl/v1`. It is the extension that fits:

- It is declared: the agent lists its URI in the card, and Agent Kourier asks for it, so both sides know it is in use.
- It carries what an approval needs: each tool's ID, name and arguments, and a decision per tool with a reason for a
  denial. It also covers questions with choices and pauses raised by a sub-agent.
- It is not tied to one framework. kagent describes it as framework-independent and uses it in front of more than one:
  its ADK runtime turns ADK's own confirmations into it, and its LangGraph sample speaks it.

An agent built on another framework gets Slack approvals by speaking that extension, usually through a small adapter
in the agent that turns its framework's approval into the extension's request and the reply back into its framework's
decision, as kagent's runtime does for ADK. The shapes are in the
[agent contract](../reference/agent-contract.md#structured-questions-and-tool-approvals). Without it, the agent can
still ask a text question, which a person answers in the thread, but that is not an approval: it shows no tool call, is
not gated by `approverGroups`, and is not audited as a decision.

The cost is a dependency on an extension kagent maintains. Agent Kourier holds to `v1` as the agent contract
describes it. If A2A publishes a standard extension for approvals, Agent Kourier will read that one too.

## In practice

To point a Binding at an agent that speaks A2A, see [Connect a generic A2A agent](../how-to/connect-a2a-agent.md). For
kagent, whose dialect adds structured questions, see [Connect a kagent agent](../how-to/connect-kagent.md).
AX uses the generic dialect: see [Connect an agent on Google AX](../how-to/connect-ax.md).

Plain A2A leaves some choices open, such as where an answer's text goes. The
[agent contract](../reference/agent-contract.md) lists the ones Agent Kourier makes, and what it expects of an agent.

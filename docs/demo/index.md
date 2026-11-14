---
description: >-
  The Agent Kourier demos: in 98 seconds, an alert and a question become agent sessions in Slack threads, the agent
  asks for a decision, and a team adds one Binding; in 3 minutes, three agents in three channels hand a cross-team
  incident on until one person approves the rollback.
---

# Demo

An alert becomes an agent session in its Slack thread, a reply continues the same session, a mention starts a new one,
and the agent asks for a decision and waits for the answer. Then a team adds its agent with one Binding.

<figure class="ak-film">
<video controls preload="none" playsinline poster="../assets/film/agent-kourier-demo-poster-16x9.jpg" aria-label="Agent Kourier demo, 98 seconds, narrated">
<source src="https://media.agentkourier.dev/agent-kourier-demo-16x9.e867a4b092d7.mp4" type="video/mp4">
<track kind="captions" src="../assets/film/agent-kourier-demo.en.vtt" srclang="en" label="English" default>
<a href="https://media.agentkourier.dev/agent-kourier-demo-16x9.e867a4b092d7.mp4">Download the demo (MP4, 98 seconds)</a>.
</video>
<figcaption>The demo, 98 seconds, narrated with captions.</figcaption>
</figure>

The Slack shots are screen recordings of a demo environment with seeded faults and real agent output, sped up in
places; a note under each shot says so. To run a sandbox like it on your own machine, follow
[Your first agent in Slack](../tutorials/first-agent-in-slack.md).

## A cross-team incident

A payments release breaks checkout. Three agents, each in its own Slack channel, investigate, escalate and page,
and the payments agent proposes a rollback that waits for one person to approve it.

<figure class="ak-film">
<video controls preload="none" playsinline poster="../assets/film/agent-kourier-incident-poster-16x9.jpg" aria-label="Agent Kourier cross-team incident demo, 3 minutes, narrated">
<source src="https://media.agentkourier.dev/agent-kourier-incident-16x9.f3c22a406d99.mp4" type="video/mp4">
<track kind="captions" src="../assets/film/agent-kourier-incident.en.vtt" srclang="en" label="English" default>
<a href="https://media.agentkourier.dev/agent-kourier-incident-16x9.f3c22a406d99.mp4">Download the incident demo (MP4, 3 minutes)</a>.
</video>
<figcaption>The cross-team incident, 3 minutes, narrated with captions.</figcaption>
</figure>

What Agent Kourier does in it:

- **One Binding per channel.** checkout's agent answers in `#checkout-alerts`, payments' agent in `#payments-ops`, and
  an incident-commander agent in `#incidents`, which both teams share. See [Write a Binding](../how-to/write-a-binding.md).
- **The alert starts the investigation.** Alertmanager posts the alert to `#checkout-alerts`, and a bot-message
  trigger on that channel's Binding turns the post into an agent session in the alert's thread. See
  [Investigate Alertmanager alerts](../how-to/investigate-alertmanager-alerts.md).
- **Agents hand off through Slack.** The escalation to `#incidents` and the page to `#payments-ops` are posts by a
  pager bot, and each of those channels' Bindings starts its agent from that bot's posts with its own trigger. See
  [Start investigations from any bot's messages](../how-to/any-bot-trigger.md).
- **The rollback waits for a person.** The payments agent's rollback tool requires approval in kagent; the payments
  Binding turns on tool approvals and lists the payments team's Slack user group in `approverGroups`, so a press from
  anyone outside that group is refused. See [Let the agent ask questions](../how-to/agent-questions.md).

The pager bot, the service catalog, the escalation tool's evidence checks and the agents' prompts belong to the demo,
not to Agent Kourier. The Slack shots are screen recordings of the demo on a kind cluster, with a seeded fault and
real agent output, sped up in places; a note under each shot says so. The demo's agents, MCP servers, Bindings and
Slack setup are in the [incident demo repository](https://github.com/agentkourier/kourier-incident-demo), whose README lists
what it needs and how to run it.

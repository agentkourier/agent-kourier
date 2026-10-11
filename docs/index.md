---
description: >-
  Agent Kourier is a Kubernetes-native broker that carries alerts and Slack messages to any A2A agent, wakes it if
  needed, and brings its findings back into the Slack thread where people can keep the session going.
hide:
  - navigation
  - toc
  - footer
---

<div class="ak-hero" markdown>
<div class="ak-hero__lockup">
<img src="assets/brand/agent-kourier-mark.svg" alt="" class="ak-logo--light" width="88" height="88">
<img src="assets/brand/agent-kourier-mark-dark.svg" alt="" class="ak-logo--dark" width="88" height="88">
<h1 class="ak-hero__name">Agent Kourier</h1>
</div>

<p class="ak-hero__pitch">Agent Kourier is a Kubernetes-native broker that carries alerts and Slack messages to any A2A
agent, wakes it if needed, and brings its findings back into the Slack thread where people can keep the session
going.</p>

<div class="ak-hero__actions">
<a class="md-button md-button--primary" href="tutorials/first-agent-in-slack/">Try it in a kind sandbox</a>
<a class="md-button" href="how-to/install-with-helm/">Install with Helm</a>
</div>
</div>

<figure class="ak-film">
<video controls preload="none" playsinline poster="assets/film/agent-kourier-teaser-poster-16x9.jpg" aria-label="Agent Kourier teaser, 20 seconds, narrated">
<source src="assets/film/agent-kourier-teaser-16x9.mp4" type="video/mp4">
<track kind="captions" src="assets/film/agent-kourier-teaser.en.vtt" srclang="en" label="English" default>
<a href="assets/film/agent-kourier-teaser-16x9.mp4">Download the teaser (MP4, 20 seconds)</a>.
</video>
<figcaption>The teaser, 20 seconds, narrated with captions. The Slack footage is the kind sandbox: a real Slack
workspace, a real model, seeded faults.</figcaption>
</figure>

<div class="grid cards" markdown>

-   :material-play-circle-outline:{ .lg } **Try it**

    ---

    Run Agent Kourier in a kind cluster against your own Slack workspace and a read-only kagent agent, and get your
    first answer in a thread.

    [:octicons-arrow-right-24: Your first agent in Slack](tutorials/first-agent-in-slack.md)

-   :material-kubernetes:{ .lg } **Install**

    ---

    Install the Helm chart on a cluster you already run, connect Slack, and bind your first agent to a channel.

    [:octicons-arrow-right-24: Install with Helm](how-to/install-with-helm.md)

-   :material-transit-connection-variant:{ .lg } **How it works**

    ---

    One broker between the chat, the alerts and your agents: a thread is a session, a restart loses no message, and
    [two replicas on Postgres](explanation/high-availability.md) fail over without losing one.

    [:octicons-arrow-right-24: Architecture](explanation/architecture.md)

</div>

## A thread is a session

An alert lands in the channel. Agent Kourier opens a thread under it and hands it to the agent, which investigates and
answers there. People reply in the same thread and the same agent session continues.

<div class="ak-thread" role="figure" aria-label="An illustrated alert thread">
<div class="ak-thread__channel">#payments-alerts</div>
<div class="ak-thread__post ak-thread__post--alert">
<span class="ak-thread__who">Alertmanager</span>
<span><span class="ak-thread__alert-state">FIRING (1)</span> - PaymentsApiErrorRate</span>
</div>
<div class="ak-thread__post ak-thread__post--reply">
<span class="ak-thread__who">Agent Kourier</span>
<span>The 5xx rate rose at 14:02, two minutes after the 14:00 rollout of payments-api. The new pods fail their
readiness probe and restart. The last crash log ends with "pool size must be &gt;= 5".</span>
</div>
<div class="ak-thread__post ak-thread__post--reply">
<span class="ak-thread__who">Engineer</span>
<span>what changed in that rollout?</span>
</div>
<div class="ak-thread__post ak-thread__post--reply">
<span class="ak-thread__who">Agent Kourier</span>
<span>The ConfigMap payments-api-env changed DB_POOL_SIZE from 20 to 2. Nothing else in the Deployment differs from the
previous revision.</span>
</div>
</div>
<p class="ak-thread-label">An illustration of one alert thread, not captured output. The answers come from your agent;
Agent Kourier carries them.</p>

A repeat of the same alert attaches to its thread, and its resolve posts a note there. Agent Kourier reads the alert
your alerting tool already posts to Slack, so it is never in the paging path. Alertmanager has a ready-made preset;
another tool's bot needs a trigger of its own.

<div class="ak-strip">
<div class="ak-strip__group">
<p class="ak-strip__label">Chat</p>
<span class="ak-strip__name">Slack</span>
</div>
<div class="ak-strip__group">
<p class="ak-strip__label">Agents</p>
<span class="ak-strip__name">A2A</span>
<span class="ak-strip__name">kagent</span>
<span class="ak-strip__name">Google AX</span>
</div>
<div class="ak-strip__group">
<p class="ak-strip__label">Alerts</p>
<span class="ak-strip__name">Alertmanager preset</span>
<span class="ak-strip__name">any bot's message</span>
</div>
</div>

Agent Kourier is in an internal-first pilot and has no tagged release yet. The [roadmap](explanation/roadmap.md) says
what is implemented and what comes next.

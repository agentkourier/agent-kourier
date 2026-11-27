# Explanation

For anyone who wants to understand Agent Kourier rather than operate it: these pages explain how it is built, why it is
built that way, and what it deliberately does not do.

<div class="grid cards" markdown>

-   **[Architecture](architecture.md)**

    ---

    What the broker owns, what the agent runtime owns, and the path one conversation takes.

-   **[Why a broker](why-a-broker.md)**

    ---

    Why a separate, always-on process sits between the chat, the alerts and the agents.

-   **[Why A2A](why-a2a.md)**

    ---

    Why one protocol to every agent, what A2A gives the broker, and what it does not.

-   **[Add-ons](add-ons.md)**

    ---

    Optional components released with Agent Kourier, such as the bridge to Claude Managed Agents and the AX agent, and what makes a component an add-on.

-   **[Choose a config route](config-routes.md)**

    ---

    Files or custom resources: how they differ when something is wrong, what is checked where, and who holds a channel.

-   **[Threads and sessions](threads-and-sessions.md)**

    ---

    How a thread becomes an agent session, and what happens to replies, restarts and expiry.

-   **[Delivery guarantees](delivery.md)**

    ---

    What Agent Kourier promises about messages in and posts out, and the windows it accepts.

-   **[High availability and failover](high-availability.md)**

    ---

    Two replicas on Postgres: the Lease, epoch fencing, what a takeover recovers and the duplicates it can leave.

-   **[Security model](security.md)**

    ---

    One identity per Binding, the channel as the access boundary, and why kagent needs a front door.

</div>

Microsoft Teams and webhooks, both experimental and new in 0.6.0, have no page of their own here.
[What each platform supports](../reference/platforms.md) says what Slack, Teams and the `webhook` platform each serve,
and how a Teams connection reads alerts through Microsoft Graph.

[Start agents from webhooks](../how-to/receive-webhooks.md) explains how a webhook connection turns a service's events into sessions while the agent answers on the service.

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

-   **[Threads and sessions](threads-and-sessions.md)**

    ---

    How a thread becomes an agent session, and what happens to replies, restarts and expiry.

-   **[Delivery guarantees](delivery.md)**

    ---

    What Agent Kourier promises about messages in and posts out, and the one window it accepts.

-   **[High availability and failover](high-availability.md)**

    ---

    Two replicas on Postgres: the Lease, epoch fencing, what a takeover recovers and the one duplicate it can leave.

-   **[Security model](security.md)**

    ---

    One identity per Binding, the channel as the access boundary, and why kagent needs a front door.

-   **[Comparison with other projects](comparison.md)**

    ---

    klaus-gateway, kslack, a2a-gateway and the alert investigators, and where Agent Kourier differs.

-   **[Roadmap](roadmap.md)**

    ---

    What is implemented, what is in progress, and what waits for the pilot.

</div>

# Tutorials

For anyone new to Agent Kourier: each tutorial is a guided path you follow step by step, on your own machine, to a
working result you can see in Slack.

Tutorials teach by doing. They make the choices for you and do not stop to explain; the
[explanation](../explanation/index.md) pages do that.

<div class="grid cards" markdown>

-   **Your first agent in Slack**

    ---

    Run Agent Kourier in a kind cluster against your own Slack workspace, ask a read-only agent about a broken
    Deployment, and continue the conversation in the thread.

    [:octicons-arrow-right-24: Start](first-agent-in-slack.md)

-   **Your first alert investigation**

    ---

    Have the sandbox's Alertmanager post an alert to Slack and watch the agent investigate it in the alert's
    thread. Alertmanager is the example; any bot's alerts can start an investigation.

    [:octicons-arrow-right-24: Start](first-alert-investigation.md)

</div>

## Known gaps

- [Your first alert investigation](first-alert-investigation.md) asks you to edit two files in the repository:
  `hack/kind/sandbox/webhook.sh`, for your alert channel's ID, and
  `hack/kind/sandbox/agent-kourier/alert-trial/alert-binding.yaml`, for your webhook's bot ID. The sandbox does not yet
  read them from its secrets file.

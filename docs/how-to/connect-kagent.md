# Connect a kagent agent

For a platform engineer who runs kagent: at the end, an AgentBackend points Agent Kourier at your kagent install and a
Binding's turns reach one of its agents.

=== "kagent 1.0"

    kagent 1.0 uses the `kagent-v1` dialect, which speaks A2A 1.0 and kagent's human-in-the-loop extension: structured
    questions with choices, and tool approvals. It shows tool calls as step cards, as every dialect does. It is tested
    against kagent 1.0.0-alpha5.

    **1. Check the cluster can run it.** kagent 1.0 runs every agent session on Agent Substrate, which needs the
    pod-certificate APIs: the `ClusterTrustBundle`, `ClusterTrustBundleProjection` and `PodCertificateRequest` feature
    gates and `certificates.k8s.io/v1beta1`. They are off by default through Kubernetes 1.36, and managed EKS through 1.36 cannot
    enable them. From 1.37 the APIs are GA, but `v1beta1` is still off by default, and the pinned Substrate needs it; EKS
    1.37 serves the GA `v1` APIs but not `v1beta1`. The project's kind tiers turn them on, so a cluster that runs the kind
    tiers' kagent may still not run it elsewhere. Check:

    ```sh
    kubectl api-resources --api-group=certificates.k8s.io   # lists clustertrustbundles and podcertificaterequests
    kubectl api-versions | grep certificates.k8s.io/v1beta1
    ```

    Without them, use kagent 0.9.x (the other tab) or another agent through the `a2a` dialect.

    **2. Put a verifying front door in front of the controller.** Agent Kourier must never call kagent's controller
    port (8083) directly: in its OIDC (trusted-proxy) mode kagent decodes the token without verifying it. See
    [Put a verifying front door before kagent](front-door.md).

    **3. Declare the backend**, pointing at the front door. The dialect adds the agent's path itself:

    ```yaml
    config:
      agentBackends:
        kagent:
          spec:
            dialect: kagent-v1
            url: http://kagent-frontdoor.kagent.svc.cluster.local:4180
            allowedNamespaces: [payments]
    ```

    **4. Give each Binding its own token.** A Binding's `identity.tokenSecretRef` names a Secret with an OIDC token the
    front door accepts. [Create the Binding identity](create-binding-identity.md) shows how to provision the account
    and issue the first token with Dex. In the tested 1.0.0-alpha5 OIDC setup, kagent binds a session to the identity
    that created it, so every turn of a Binding uses the
    same token owner. A Binding with `tokenSecretRef` must target an OIDC-mode kagent: in insecure mode kagent ignores
    the token and takes the user from `?user_id=` or `X-User-Id`; with neither, it runs every turn as `admin@kagent.dev`.

    **5. Configure interactions.** Set `interactions.askUser: true` when the agent should ask people questions. Set
    `interactions.toolApprovals: true` when a write-capable agent should pause for a Slack decision. Approvals are
    disabled by default. Optionally set `interactions.approverGroups` to Slack user group IDs; without groups, every
    non-bot channel member may decide and the config loader warns. With groups, the decider must belong to one of them,
    belong to the channel, and not be a bot. The Slack app needs the `usergroups:read` bot scope when groups are used;
    reinstall the app after adding that scope. See [Let the agent ask questions](agent-questions.md) for the controls,
    timeout and audit behavior.

    `toolApprovals` and `askUser` are independent. A tool approval on a Binding where `toolApprovals` is false is denied
    through kagent with a fixed reason and a notice in the thread. A prompt that cannot be shown exactly or cannot fit in
    one message is refused and the task is cancelled. A pinned kagent 1.0.0-alpha5 runtime check approved `add_numbers`
    and denied `check_status` in one two-tool pause; only `add_numbers` executed. The broker's end-to-end suite verifies this
    with fake Slack and pinned kagent 1.0.0-alpha5.

=== "kagent 0.9.x"

    kagent 0.9.x speaks A2A 0.3, which the generic `a2a` dialect reads. It runs on a cluster without the
    pod-certificate APIs. It is tested against kagent 0.9.4 on a real cluster. The `a2a` dialect shows its tool calls as
    step cards: kagent 0.9.x sends them in the messages of its status updates, marked `kagent_type`, and Agent Kourier
    reads them there. The tests replay a turn recorded from kagent 0.9.4.

    **1. Find the controller's Service.** Its name depends on the Helm release name. Argo CD names the release after the
    Application, so an Application called `my-kagent` makes the Service
    `my-kagent-controller`.

    ```sh
    kubectl get svc -n <kagent-namespace>
    ```

    **2. Declare the backend** with the agent's A2A endpoint. Keep the trailing slash: a POST without it was answered
    with a 307 in the 0.9.x installs this was written against (kagent's own router issues no redirect, so another hop
    in front of it may be the source), and Agent Kourier follows no redirect.

    ```yaml
    config:
      agentBackends:
        kagent:
          spec:
            dialect: a2a
            url: http://<controller-service>.<namespace>.svc.cluster.local:8083/api/a2a/<agent-namespace>/<agent>/
    ```

    With the `a2a` dialect the URL names one agent, so each agent needs its own AgentBackend.

    **3. Know the limits of this setup.**

    - A 0.3 agent has no task list. After a restart, or after a send whose outcome is unknown, Agent Kourier cannot ask
      whether the agent already has the turn. It sends again, and the agent may run the turn twice. That is harmless for
      a read-only agent and not for one that changes things.
    - The `a2a` dialect shows tool calls but cannot approve or deny them: Slack tool approvals need the `kagent-v1`
      dialect and kagent 1.0, so no approval prompt is posted here, whatever `interactions.toolApprovals` says. The
      agent's own tool list and the role behind its tools are the only fence on a write.
    - A text `input-required` pause is shown as a question, and the reply is sent as text, only when the Binding sets
      `interactions.askUser: true`. Whether kagent 0.9.x ever pauses with a text question has not been tested.
    - Whether kagent 0.9.x pauses for a tool approval has not been tested. kagent 1.0.0-alpha5, reached on the `a2a`
      dialect, pauses for one with text alone, asking for a `FunctionResponse`. A pause like that is handled as the
      question above: its reply is sent as plain text, never as an approval, and what kagent does with that text has
      not been tested.
    - The controller accepted a placeholder token in the first install, so nothing verifies the Binding's identity. Put
      a verifying front door in front of it if that matters.

    !!! danger "Do not install kagent 0.9.x with its defaults"

        Its chart binds the bundled tool server to `cluster-admin` and serves write tools such as delete, apply and
        exec. For an agent that answers alerts, run the tool server with `--read-only`, bind it to a role that cannot
        read Secrets, and list the agent's tools by name. The defaults also run ten bundled agents, about 2.3 GiB of
        memory requests.

## Check it

Mention the bot in a bound channel. The thread gets the agent's answer. If it says "I can't reach the agent right
now, and I'm trying again.", the pod log's `session turn failed` line has the reason: see
[Troubleshoot an install](troubleshoot.md#the-thread-says-i-cant-reach-the-agent-right-now-and-im-trying-again).

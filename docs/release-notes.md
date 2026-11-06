# Release notes

What changed in each release, newest first, and what you must do about it. The steps for an upgrade are in
[Upgrade](how-to/upgrade.md), which has a section for each change that needs one; this page says what changed and points to it.

## 0.2.0

Kubernetes installs now configure Agent Kourier with custom resources. The chart's default is `config.source: crds`, so an install
that names no route gets the `Binding`, `AgentBackend` and `ChatConnection` CustomResourceDefinitions and a broker that reads them.
The ConfigMap route still works. It was built for a quick proof of concept, and it stays for that use and for installs that cannot
add CRDs, but it is no longer the default. **A ConfigMap install that upgrades with its old values file must now name its route**:
read [Breaking changes](#breaking-changes) first.

### The custom resource route

Each team writes its own `Binding` in its own namespace, committed next to the agent's manifests, and sees what became of it with
`kubectl`. The broker's leader writes `Binding.status.conditions` (`Accepted`, `BackendReachable`, `ChannelJoined` and `Ready`), and
`kubectl get akbinding` shows them:

```text
NAMESPACE   NAME          READY   REASON                       CHANNEL   AGE
team-a      team-a-chat   True    OK                           C2        4m
team-a      late          False   Accepted:ChannelConflict     C1        1m
```

- **Short names.** `akbinding`, `akbackend` and `akconnection`, all in the category `agentkourier`.
- **A bad object does not stop the others.** The broker judges each object on its own: it keeps serving the last good generation of an
  object that is edited into an invalid state, and does not serve an invalid new one. [A bad object](explanation/config-routes.md#what-a-bad-object-does) says
  what each case does, and [Choose a config route](explanation/config-routes.md) says which route fits.
- **Secrets are followed one at a time.** The broker watches each Secret the config names, not a whole namespace, so a reload can add a
  Secret in a namespace it was not watching, with no restart, as long as its Role grants `list` and `watch`.
- **Move between the routes.** [Move the config between a ConfigMap and custom resources](how-to/migrate-config-source.md) is a
  two-upgrade path in each direction. [Install with Helm](how-to/install-with-helm.md#install-on-custom-resources) is the path for a new install.
- **Needs** Kubernetes 1.33 or later, and a right to create CustomResourceDefinitions (they are cluster-scoped). The ConfigMap route has no minimum version.

The status is written only on this route. On the ConfigMap route, the leader still probes each Binding and reports the result in
metrics and logs: see [Binding verification](reference/metrics.md#binding-verification) and
[Binding status](reference/binding-status.md).

### Breaking changes

- **The chart's default is `config.source: crds`.** An install that reads a ConfigMap and upgrades with the values it has gets the new
  default. To keep the ConfigMap route, put this in the values file:

    ```yaml
    config:
      source: files
    ```

    With `files`, an unset `crds.install` is false, so the render is the one the previous release made, with no CRD. To move to custom
    resources instead, follow [the migration](how-to/migrate-config-source.md).
- **An unset `config.source` still reads as `files`.** A `helm upgrade --reuse-values` from 0.1.6 hands the chart the values of that
  release, which have no `config.source`, and the chart reads the missing key as `files`. What Helm hands the chart for a new
  chart's defaults under `--reuse-values` is not measured here: the chart was rendered with `config.source=null` to check it.
- **An unset `crds.install` follows the route.** It is true with `crds` and false with `files`. An explicit `true` or `false` always
  wins, so `config.source: files` with `crds.install: true` installs the CRDs before any custom resource exists (step 1 of the
  migration), and `crds.install: false` on `crds` is for a platform that applied the CRDs itself.
- **Kubernetes 1.33 or later is needed for the custom resource route.** The ConfigMap route has no minimum.
- **The chart refuses these values on the default route.** The render fails before anything is applied, so nothing changes, and each
  message says how to keep the ConfigMap route or how to move:

    | Your values | Why it is refused |
    |---|---|
    | `config.existingConfigMap`, and no `config.source` | The broker would read custom resources, mount no ConfigMap and ignore it. |
    | `config.agentBackends`, `config.chatConnections` or `config.bindings`, on a cluster that does not serve the CRDs | A release cannot install the CRDs and the custom resources of them in one apply, so the CRDs must be served first. |
    | A cluster older than Kubernetes 1.33 | The route needs 1.33. With `helm template`, pass `--kube-version`. |
    | No `config.*` resources and no `config.chatConnection` | The broker needs to know which ChatConnection it serves. |

    [Upgrade](how-to/upgrade.md#upgrade-to-the-release-that-makes-custom-resources-the-default) shows each message and what the chart does not
    refuse: an install that already serves the CRDs and upgrades with inline `config.*` has them rendered as custom resources, which is
    step 2 of the migration, done by the upgrade.
- **A config the broker accepted before can be refused now.** This release bounds what a config may hold, on both routes. On the
  ConfigMap route a config over a bound fails at start-up (the broker exits with code 2) and a reload keeps the last good config. On the
  custom resource route a Binding over a bound is not served after the upgrade and says `Accepted=False` with the reason
  `InvalidSpec`. Run `agent-kourier dry-run` of the new release against your config before you upgrade
  ([how](how-to/install-with-helm.md#check-the-values-before-you-install)). What changed:
    - [Triggers, extract entries and headers](how-to/upgrade.md#upgrade-to-the-release-that-bounds-triggers-extract-entries-and-headers) are capped.
    - [A `promptTemplate`](how-to/upgrade.md#upgrade-to-the-release-that-limits-prompt-templates) may no longer loop, define a template or use a variable.
    - [Each regular expression of a trigger, and all those of a Binding](how-to/upgrade.md#upgrade-to-the-release-that-bounds-what-a-triggers-regular-expressions-cost), are held to a size, and a match reads the first 16 KiB of its field.
    - [`interactions.timeout` and `session.threadTTL`](how-to/upgrade.md#upgrade-to-the-release-that-caps-the-pause-timeout-and-the-thread-ttl) have caps. Lowering a `threadTTL` can delete sessions.
    - [Namespaces, chat IDs, URLs, header values and durations](how-to/upgrade.md#upgrade-to-the-release-that-bounds-namespaces-chat-ids-urls-header-values-and-durations) are bounded, and an AgentBackend `url` may have no query or fragment.

    [Limits](reference/limits.md) lists every limit you can run into.
- **The upgrade restarts the pod.** The chart now sets `GOMEMLIMIT` at 80% of `resources.limits.memory`, which changes the pod
  template. With SQLite (`Recreate`) there is a gap while the new pod starts. See
  [the release that sets GOMEMLIMIT](how-to/upgrade.md#upgrade-to-the-release-that-sets-gomemlimit).
- **The broker reads less from an agent.** One event of a stream is read up to 2 MiB (it was 10 MiB), the answer to a call that is not a
  stream up to 4 MiB (it was 16 MiB), and a turn holds 341 KiB of the agent's reply (it was 2 MiB). A turn past a bound ends with the new
  outcome `response_too_large`, and its task is cancelled. See
  [the release that lowers the bounds](how-to/upgrade.md#upgrade-to-the-release-that-lowers-the-bounds-on-what-it-reads).

### Features

- **Binding verification.** The leader probes each Binding on a schedule: it fetches the agent's card on a fresh connection and reads
  the Slack channel, and reports the result as metrics and log lines (and, on custom resources, as status). The scopes it uses are
  `channels:read` and `groups:read`, which the Slack manifest already asks for.
  [A Binding is not ready](how-to/troubleshoot.md#a-binding-is-not-ready) lists each reason and what to do.
- **Working updates show as progress.** The status updates an agent sends in state `working` now title the `Working…` card or show as a
  progress line in `steps-and-activity`, and `activity` counts them. See
  [Working updates](reference/agent-contract.md#working-updates).
- **Agents can notify people.** A Binding declares people under `chat.mentions`, and an agent that writes `@name` for a declared name in
  its final reply notifies that person in a message of its own. `chat.maxMentionsPerTurn` (default 2) caps it. See
  [Write a Binding](how-to/write-a-binding.md).
- **Another bot's mention can start a turn.** On a Binding with a mention trigger, a mention of Agent Kourier by another bot or incoming
  webhook starts a turn in that thread, held to fixed limits per Binding. Set `chat.botMentions: none` to turn it off. See
  [Let another agent start yours](how-to/any-bot-trigger.md#let-another-agent-start-yours).
- **Limits are documented.** [Limits](reference/limits.md) is a page of every cap, bound and timeout you can run into, with what happens
  past it, guarded against the code by a test.

### Fixes

- A reply over a read bound now tells the thread that the turn stopped and cancels the agent's task once, where it was reported as the
  agent refusing to continue. A turn that could not name its task is no longer sent again after every backoff.
- A task that is paused is cancelled once when two controls end the same pause, and a Stop that arrives as a pause reopens ends it.
- A Slack update that Slack refuses as `block_mismatch`, as a recovered turn can meet on a message it did not write, is no longer retried every 2 seconds.
- A vote pressed before the reply's message place is recorded is retried instead of ignored.
- A path such as `@platform/api`, where `platform` is a declared name, no longer notifies that person.
- A 403 at the agent card fetch is no longer treated as a rejected credential: kagent 1.0 answers it for an agent that does not exist, so the
  token is not re-read and the credential-rejection metric is not raised. A 401, and a 403 on an A2A call, stay rejected credentials.
- Working updates are told from protocol noise: an update later on a stream counts as progress whatever it says, so an identical heartbeat
  no longer lets a turn look idle, and the token fragments that kagent 0.9.4 marks `kagent_adk_partial` are not working updates (read in
  kagent-adk at v0.9.4, not measured on the wire).
- The events of one Slack channel are handled in order on a lane of their own, so a slow match no longer delays the acknowledgments of
  other channels. Past 16 events held for a channel, or 256 in all, an event is left unacknowledged and counted in
  `agentkourier_slack_events_shed_total`.
- Dependencies and the container base images are updated, including `slack-go` to v0.34.0.

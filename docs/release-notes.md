# Release notes

What changed in each release, newest first, and what you must do about it. The steps for an upgrade are in
[Upgrade](how-to/upgrade.md), which has a section for each change that needs one; this page says what changed and points to it.

## Version 0.2.1 (unreleased)

The chart now grants the served ChatConnection's Secret by itself, so the install on custom resources needs `config.chatConnection` and no `rbac.secretNames` entry for that Secret
(a Binding's token in the release namespace still needs one), and a team grants its own Secrets in its own namespace. Read [the upgrade section](how-to/upgrade.md#upgrade-to-the-release-that-grants-the-chatconnections-secret) before you upgrade.

- **New value `config.chatConnectionSecret`.** With `config.source: crds` and `config.chatConnection` set, the chart grants `list` and `watch` on one Secret by name, in the
  connection's namespace: the Secret named like the connection, or the one `config.chatConnectionSecret` names. Before, the same install needed an `rbac.secretNames` entry for that Secret, or the pod
  exited with code 3 in a loop. An existing entry keeps working, and a Secret named twice is granted once. The value is ignored with `config.source: files`, when
  `config.chatConnection` is empty, and when `config.chatConnections` renders that connection (its `credentialsSecretRef` is granted instead).
- **One new named grant on upgrade.** An install on `crds` that sets `config.chatConnection` gains `list` and `watch` on one more Secret: the one named like the connection, or the one `config.chatConnectionSecret` names, unless an `rbac.secretNames` entry already names it or
  `config.chatConnections` renders that connection. When the connection reads a Secret of another name, the derived Secret may not exist: the broker never reads it, so the grant covers no Secret until one of that name is created
  in that namespace. Set `config.chatConnectionSecret` to the real name.
- **A Role in the connection's namespace.** With `config.chatConnection: other-ns/name`, the chart now renders the Role and RoleBinding `<fullname>-secrets` in `other-ns`. A Role of that
  name that someone made by hand there conflicts: the upgrade fails with `invalid ownership metadata` until you delete it or rename it.
- **A team grants its own Secrets.** The documentation now leads with the team's own Role and RoleBinding in its namespace, two `kubectl` commands, in
  [Write a Binding](how-to/write-a-binding.md#load-it) and [Let a team add its own Binding](how-to/migrate-config-source.md#let-a-team-add-its-own-binding). `rbac.secretNames` stays for Secrets the
  chart cannot see and the platform owns, such as a Binding's token in the release namespace.
- **The examples name the Slack Secret `slack`,** like the ChatConnection `slack`, so that the default derivation finds it.
- **`CredentialsUnreadable` says which cause it is.** The status message of a Binding whose token or header Secret cannot be read now says whether the Secret does not exist, lacks the key, holds an empty
  value or a control character, is not of type `Opaque`, or was denied to the ServiceAccount (and then what grant it needs), or that the Binding requires a token and has none. It names the Secret and the
  key only when the Secret is in the Binding's namespace: [Binding status](reference/binding-status.md).

## Version 0.2.0

Custom resources are now the recommended way to configure Agent Kourier on Kubernetes, and the chart's default: `Binding`,
`AgentBackend` and `ChatConnection` are CustomResourceDefinitions, and `Binding.status.conditions` shows in `kubectl get`. An install
that names no route (`config.source: crds` is the default) gets the CRDs and a broker that reads them.
The ConfigMap route still works. It was built for a quick proof of concept, and it stays for that use and for installs that cannot
add CRDs, but it is no longer the default. **A ConfigMap install that upgrades with its old values file must now name its route**:
read [Breaking changes](#breaking-changes) first.

The release candidates before it (`v0.2.0-rc.N`) are prereleases on GitHub; what they changed is in this entry.

### The custom resource route

Each team writes its own `Binding` in its own namespace, committed next to the agent's manifests, and sees what became of it with
`kubectl`. The broker's leader writes `Binding.status.conditions` (`Accepted`, `BackendReachable`, `ChannelJoined` and `Ready`), and
`kubectl get akbinding -A` shows them:

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
- **The start line names the route.** The first line of the log is an INFO `config route` with `config_source` (`crds` or `files`) and, with
  `crds`, the `connection` the broker is configured to serve, so a pod's log says which route it reads.
- **A bad ChatConnection name stops the start.** With `crds`, `AGENTKOURIER_CHAT_CONNECTION` must be a namespace that is a DNS-1123 label and a
  name that is a DNS-1123 subdomain. Anything else exits with code 2, with a message that does not show the value. The chart sets it from `config.chatConnection`.
- **Move between the routes.** [Move the config between a ConfigMap and custom resources](how-to/migrate-config-source.md) is a
  two-upgrade path in each direction. [Install with Helm](how-to/install-with-helm.md#install-on-custom-resources) is the path for a new install.
- **Needs** Kubernetes 1.33 or later, and a right to create CustomResourceDefinitions (they are cluster-scoped). The ConfigMap route has no minimum version.
  With `rbac.create`, the chart also makes a ClusterRole, bound to the ServiceAccount: `list` and `watch` on the three kinds in every namespace,
  `patch` on `bindings/status`, and `get` on the three CRDs by name (`charts/agent-kourier/templates/rbac.yaml`). With `rbac.create: false`, the operator creates it.
  The Secret Roles name single Secrets by `resourceNames`, so the Secrets of a team's own Binding, which the chart cannot see in `config.*`, need
  `rbac.secretNames`, `rbac.secretNamespaces` or a Role the team makes; a namespace in `rbac.secretNamespaces` keeps a namespace-wide Role ([Install with Helm](how-to/install-with-helm.md#install-on-custom-resources)).

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

    With `files`, an unset `crds.install` is false, so the render is 0.1.6's plus the `GOMEMLIMIT` variable (below), with no CRD. To move to
    custom resources instead, follow [the migration](how-to/migrate-config-source.md).

    An unset `config.source` still reads as `files`, so the chart keeps the ConfigMap route for the values of a `helm upgrade --reuse-values`
    from 0.1.6, which have no such key. A chart test renders with each new key unset and gets the render of the ConfigMap route
    (`TestUpgradeFromAReleaseThatLacksTheNewValues`). What Helm hands the chart for a new chart's default under `--reuse-values` is Helm's
    behaviour and is unverified here.
- **An unset `crds.install` follows the route.** It is true with `crds` and false with `files`. An explicit `true` or `false` always
  wins, so `config.source: files` with `crds.install: true` installs the CRDs before any custom resource exists (step 1 of the
  migration), and `crds.install: false` on `crds` is for a platform that applied the CRDs itself.
- **Kubernetes 1.33 or later is needed for the custom resource route.** The ConfigMap route has no minimum.
- **The chart refuses these values on the default route.** The render fails before anything is applied, so nothing changes, and each
  message says how to keep the ConfigMap route or how to move:

    The messages that name the default say "the chart's default unless your values set it", since the chart sees the same value whether your values say `config.source: crds` or leave it out.

    | Your values | Why it is refused |
    |---|---|
    | `config.existingConfigMap`, and no `config.source` | The broker would read custom resources, mount no ConfigMap and ignore it. |
    | `config.agentBackends`, `config.chatConnections` or `config.bindings`, on a cluster that does not serve the CRDs | A release cannot install the CRDs and the custom resources of them in one apply, so the CRDs must be served first. |
    | A cluster older than Kubernetes 1.33 | The route needs 1.33. With `helm template`, pass `--kube-version`. |
    | `config.chatConnection` empty, and `config.chatConnections` without exactly one entry | The broker needs to know which ChatConnection it serves. |

    [Upgrade](how-to/upgrade.md#upgrade-to-the-release-that-makes-custom-resources-the-default) shows each message and what the chart does not
    refuse: an install that already serves the CRDs and upgrades with inline `config.*` has them rendered as custom resources, which is
    step 2 of the migration, done by the upgrade.
- **A config the broker accepted before can be refused now.** This release bounds what a config may hold, on both routes. On the
  ConfigMap route a config over a bound fails at start-up (the broker exits with code 2) and a reload keeps the last good config. On the
  custom resource route a Binding over a bound is not served after the upgrade and says `Accepted=False` with the reason
  `InvalidSpec`. Run `agent-kourier dry-run` of the new release against your config before you upgrade
  ([how](how-to/install-with-helm.md#check-the-values-before-you-install); a values file from 0.1.6 needs `config.source: files` first). What changed:
    - [Triggers, extract entries and headers](how-to/upgrade.md#upgrade-to-the-release-that-bounds-triggers-extract-entries-and-headers) are capped.
    - [A `promptTemplate`](how-to/upgrade.md#upgrade-to-the-release-that-limits-prompt-templates) may use only text, comments, `if`, `with`, the view's fields,
      `.Fields.<name>` and a short list of functions. `range`, `define`, `template`, `block`, variables, `print`, `printf`, `println`, `html`, `js`,
      `urlquery` and `call` are refused, and a template that writes more than 262144 bytes starts no turn and is counted as `render_error`, which a dry run cannot catch. A
      `promptTemplate` is capped at 8192 bytes, `spec.agent.name` at 253 and `spec.identity.userId` at 256.
    - [Each regular expression of a trigger, and all those of a Binding](how-to/upgrade.md#upgrade-to-the-release-that-bounds-what-a-triggers-regular-expressions-cost), are held to a size, and a match reads the first 16 KiB of its field.
    - [`interactions.timeout` and `session.threadTTL`](how-to/upgrade.md#upgrade-to-the-release-that-caps-the-pause-timeout-and-the-thread-ttl) have caps. Lowering a `threadTTL` can delete sessions.
    - [Namespaces, chat IDs, URLs, header values and durations](how-to/upgrade.md#upgrade-to-the-release-that-bounds-namespaces-chat-ids-urls-header-values-and-durations) are bounded, and an AgentBackend `url` may have no query or fragment.

    [Limits](reference/limits.md) lists every limit you can run into.
- **The upgrade restarts the pod.** The chart now sets `GOMEMLIMIT` at 80% of `resources.limits.memory`, which changes the pod
  template. With SQLite (`Recreate`) there is a gap while the new pod starts. See
  [the release that sets GOMEMLIMIT](how-to/upgrade.md#upgrade-to-the-release-that-sets-gomemlimit).
- **The broker bounds what it reads from an agent.** Release 0.1.6 had no bound of its own: only the A2A library's 10 MiB on each line of a
  stream, and a reply was shown cut at 12000 UTF-16 units. Now one event of a stream is read up to 2 MiB, the answer to a call that is not a
  stream up to 4 MiB, what a turn holds of the agent's reply is 341 KiB, and the agent card is 1 MiB (`internal/dialect/dialect.go`, `MaxCardBytes`).
  A turn past one of the first three ends with the new outcome `response_too_large`, the thread is told, and, when the agent had named a task,
  the task is cancelled once; a reply that named no task has none to cancel. An agent card over 1 MiB fails every call to that agent, so the Binding takes no turn and its probe
  reports it; a follow of a task that meets it cancels nothing. A message that is sent again, whose outcome is unknown, is not sent, for an agent that can list tasks, when the list cannot be read in
  full: the turn ends with the new outcome `lookup_incomplete`. See
  [the release that bounds what it reads](how-to/upgrade.md#upgrade-to-the-release-that-lowers-the-bounds-on-what-it-reads).
- **Another bot's mention now starts a turn.** `chat.botMentions` defaults to `any`, so every existing Binding with a `mention` trigger takes the
  mentions of Agent Kourier by other bots and incoming webhooks that can post in its channel, held to fixed limits. At 0.1.6 they were ignored. To keep
  that, set `chat.botMentions: none` in the config you upgrade with, in the same upgrade, since 0.1.6 refuses the field. See [the release that lets another bot's mention start a turn](how-to/upgrade.md#upgrade-to-the-release-that-lets-another-bots-mention-start-a-turn).

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
- **Another bot's mention can start a turn,** which is how one agent calls another (see Breaking changes for the default). See
  [Let another agent start yours](how-to/any-bot-trigger.md#let-another-agent-start-yours).
- **Limits are documented.** [Limits](reference/limits.md) is a page of every cap, bound and timeout you can run into, with what happens
  past it, guarded against the code by a test.

### Fixes

- Two controls that end the same pause no longer both ask the agent to cancel its task.
- A Slack update that Slack refuses as `block_mismatch`, as a recovered turn can meet on a message it did not write, is no longer retried every 2 seconds.
- A vote pressed before the reply's message place is recorded is retried instead of ignored.
- A 403 at the agent card fetch is no longer treated as a rejected credential: kagent 1.0 answers it for an agent that does not exist, so the
  token is not re-read and the credential-rejection metric is not raised. A 401, and a 403 on an A2A call, stay rejected credentials.
- The events of one Slack channel are handled in order on a lane of their own, so a slow match no longer delays the acknowledgments of
  other channels. Past 16 events held for a channel, or 256 in all, an event is left unacknowledged and counted in
  `agentkourier_slack_events_shed_total`.
- Dependencies and the container base images are updated, including `slack-go` to v0.34.0.

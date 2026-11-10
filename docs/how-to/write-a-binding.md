# Write a Binding

For an application team that wants its agent in its channel: at the end, a Binding in your namespace connects your
agent to your channel, through the backend the platform team runs and a Slack connection: the platform's, or your own.

## Before you start

- The platform team runs Agent Kourier and an [AgentBackend](../reference/agentbackend.md) for your agent's runtime that, if it is
  in another namespace, lists yours in `allowedNamespaces`.
- A [ChatConnection](../reference/chatconnection.md) for Slack: the platform's, listing your namespace in `allowedNamespaces`,
  or your team's own, in a namespace the platform admits, and listing yours in `allowedNamespaces` if that is another namespace
  ([Let a team bring its own Slack app](team-slack-app.md)).
- That connection's bot is a member of your channel, and you have the channel's ID (it starts with `C`; a private channel created before March 2021 may start with `G`).
- If the backend verifies tokens, a Secret in your namespace holds the Binding's token under the key `token`.
  [Create the Binding identity](create-binding-identity.md) explains the account, how to obtain its token, and how
  to store it. For a backend that trusts `X-User-Id`, that guide explains how to choose a stable name instead.

## Write it

```yaml
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata:
  name: payments-assistant
  namespace: payments
spec:
  agent:
    backendRef: # (1)!
      namespace: agent-kourier
      name: kagent
    namespace: payments # (2)!
    name: payments-agent
  identity:
    tokenSecretRef: # (3)!
      name: payments-agent-token
      key: token
  chat:
    connectionRef:
      namespace: agent-kourier
      name: slack
    channel: C0123456789
    output: live # (4)!
    threadReplies: all # (5)!
    acknowledgment:
      enabled: true # (6)!
```

1. The platform team's AgentBackend. A reference to another namespace works only if that resource lists your
   namespace in `allowedNamespaces`.
2. The agent's namespace and name on the backend, not in your cluster. If the AgentBackend sets `agents`, the namespace must be one it allows (yours, if it sets `sameNamespace: true`), or the
   Binding says `AgentNamespaceNotAllowed` and is not served.
3. The token every turn of this Binding is sent with. Without a front door that verifies it, use `userId` instead,
   which is sent as `X-User-Id`.
4. `live` streams the answer as the agent writes it; `final` posts it once, when the turn ends.
5. `all` makes every person's reply in a thread Agent Kourier owns a turn. `mention` takes only the replies that mention the bot.
6. `acknowledgment` adds a temporary reaction to each accepted message while its turn is processing. It defaults to enabled, with the
   platform's own reaction (Slack: `hourglass_flowing_sand`); set `enabled: false` to omit it, or set `emoji` to another bare Slack reaction name such as `eyes`.

## Load it

The broker reads its resources from custom resources, the chart's default, or, if the platform team chose the quick route, from files, usually a ConfigMap
(see [Choose a config route](../explanation/config-routes.md)). Ask which. With the Helm chart, add the Binding under
`config.bindings`, keyed by its name, with its namespace:

```yaml
config:
  bindings:
    payments-assistant:
      namespace: payments
      spec:
        # the spec above
```

Then `helm upgrade`. On the ConfigMap route, by default (`config.rollOnChange`), the chart rolls the pod when the config changes; with custom resources the chart renders the Binding as an object and the broker reads the change at its next check, every 10 seconds.

If the platform team manages its own ConfigMap (`config.existingConfigMap`), add the Binding's YAML there as a file
ending in `.yaml`, and ask them to list your namespace in `rbac.secretNamespaces` so the broker may read your Secret.

**With custom resources**, the platform team's chart values may render the Binding as above, or you apply it yourself in your own
namespace, where it is a `Binding` object:

```sh
kubectl apply --dry-run=server -f binding.yaml   # the schema and CEL rules of the CRD, with nothing stored
kubectl apply -f binding.yaml
```

It needs two things. The first is always yours; the second is whoever owns the resource it names.

1. **You let the broker read the Secrets that your Binding, or an AgentBackend in your namespace, names.** Skip this only when neither names one (no `tokenSecretRef`, no header with a `valueSecretRef`). A ChatConnection of your own needs its Secret granted too, as [Let a team bring its own Slack app](team-slack-app.md#2-the-team-makes-the-secret-the-grant-and-the-connection) does. In your own namespace, make a Role that names each Secret and a
   RoleBinding to the broker's ServiceAccount. The commands name the ServiceAccount `agent-kourier` in the namespace `agent-kourier`, which is right only for a release named `agent-kourier`; in general it is `serviceAccount.name` when that is set, and otherwise the chart's full name: `fullnameOverride`, or the release name when it contains `agent-kourier`, or `<release>-agent-kourier` (with `serviceAccount.create: false`, it is `serviceAccount.name`, or `default`), so ask the platform team for the real ones, or read it from the Deployment with `kubectl -n <release namespace> get deploy -l app.kubernetes.io/instance=<release> -o jsonpath='{.items[0].spec.template.spec.serviceAccountName}'`:

   ```sh
   kubectl -n payments create role agent-kourier-secrets-payments --verb=list,watch --resource=secrets --resource-name=payments-agent-token
   kubectl -n payments create rolebinding agent-kourier-secrets-payments --role=agent-kourier-secrets-payments --serviceaccount=agent-kourier:agent-kourier
   ```

   The Role gives `list` and `watch` on the Secrets it names, and no other (repeat `--resource-name` for a second one). It needs nothing from the platform team's release or values. It is a grant to the broker, not to your Binding:
   the broker may use those Secrets for any Binding or AgentBackend in your namespace, so name only Secrets you mean it to use. [The how-to](migrate-config-source.md#let-a-team-add-its-own-binding)
   has the same objects as YAML, and how to check them with `kubectl auth can-i`.
2. **Whoever owns the ChatConnection and the AgentBackend lists your namespace** in their `allowedNamespaces`, for each that is in another
   namespace: the platform team for its own, your team for a connection of its own elsewhere. A connection in your own namespace needs no list,
   but the platform team must have admitted that namespace ([Let a team bring its own Slack app](team-slack-app.md#3-the-platform-team-admits-the-namespace)).

Until they exist the status says `CredentialsUnreadable` (the Role) or `NamespaceNotAllowed` (the list). The `CredentialsUnreadable` message names your Secret and says that
Agent Kourier's ServiceAccount needs `list` and `watch` on it, when the API server denied the read:

```sh
kubectl -n payments get akbinding payments-assistant -o jsonpath='{.status.conditions[?(@.type=="BackendReachable")].message}'
```

A `connectionRef` to a connection of another namespace names that namespace.

## Check it

Mention the bot in the channel. A thread opens and the agent answers in it. If nothing happens, see
[Troubleshoot an install](troubleshoot.md#the-mention-gets-no-reply-and-the-log-shows-no-session-turn-started).

With custom resources, read the Binding's status first:

```sh
kubectl -n payments get akbinding payments-assistant
kubectl -n payments wait --for=condition=Ready akbinding/payments-assistant --timeout=120s
```

`READY` is `True` when the broker accepted the Binding and its checks passed. A reason in `REASON` says what to fix: see
[Binding status](../reference/binding-status.md) and [A Binding is not accepted](troubleshoot.md#a-binding-is-not-accepted). Never type
`kubectl get bindings`: that is a core Kubernetes resource, not this one.

The leader also checks the Binding by itself, within about 30 seconds of the reload or when a pass already running ends: `agentkourier_binding_ready{binding="<namespace>/<name>"}` is 1 when the agent's card can be fetched and the bot is in the channel. If it is 0, see [A Binding is not ready](troubleshoot.md#a-binding-is-not-ready).

One channel takes one Binding, on both routes, and what happens to a second one differs: with files the whole config fails to load, and with custom
resources the younger Binding says `ChannelConflict` and the rest carry on. [Who holds a channel](../explanation/config-routes.md#who-holds-a-channel)
says how it is decided, and what an older Binding that you edit onto a younger one's channel does.

## Variations

- **Let the agent ask questions in the thread:** set `interactions.askUser: true`. See
  [Let the agent ask questions](agent-questions.md).
- **Investigate the alerts a bot posts to the channel:** add a `botMessage` trigger. See
  [Investigate Alertmanager alerts](investigate-alertmanager-alerts.md) for a worked trigger, or
  [Start investigations from any bot's messages](any-bot-trigger.md) for another tool's bot.
- **Investigate what people post to the channel, without a mention:** add a `personMessage` trigger. See
  [A person's message](../reference/chat-triggers.md#a-persons-message).
- **Keep threads around for less than 90 days:** set `session.threadTTL`, for example `720h`.
- **Rating buttons on answers:** set `feedback.enabled: true`. Investigations that a `botMessage` trigger started always carry them. A turn that another bot's mention or a `personMessage` trigger started counts as an answer.
- **Choose what a live reply shows while the agent works:** set `chat.progress` to `steps`, `steps-and-activity` (the
  default) or `activity`. The default adds a `Working…` card when 3 seconds pass with nothing new; `activity` shows one
  card (a line in an alert thread) that counts the steps and names no tool; `steps` is the cards and lines alone. See
  [Output](../explanation/threads-and-sessions.md#output).
- **Turn off other bots' mentions:** any bot or webhook in the channel can start a turn by mentioning Agent Kourier. Set
  `chat.botMentions: none` to stop that. See [A bot's mention](../reference/chat-triggers.md#a-bots-mention).
- **Let the agent notify people:** declare them under `chat.mentions`, each with the `name` the agent writes and the
  `user` ID it stands for. See [Let the agent notify people](#let-the-agent-notify-people).
- **Change the processing reaction:** set `chat.acknowledgment.emoji` to a bare Slack reaction name. The reaction is removed when
  the turn ends; a question removes it before the thread waits for an answer, and the answer gets its own reaction.

Every field, with its default, is in the [Binding reference](../reference/binding.md).

## Let the agent notify people

```yaml
spec:
  chat:
    mentions:
      - name: payments-owner
        user: U0123456789
    maxMentionsPerTurn: 2
```

When a turn's reply says `@payments-owner`, Agent Kourier posts one more message in the thread, after the reply, that
mentions that user. Each person named gets a message of their own.

`maxMentionsPerTurn` must be from 1 to 10, and 0 is refused at load. To notify nobody, leave `mentions` out.

- **The agent is told the names.** A session's first prompt ends with a line that lists them. It is not said again: a
  thread that started before you added a name does not get it, and neither does an agent that opens a new session of its own and answers in
  it. A first turn that fails once the agent has started on it still counts as the first, and an agent that did not keep the line is not told again. The line goes out again on any turn that finds no context recorded for the session: for example after the agent answers that it no longer has the task, as Agent Kourier then resets the session, and on every turn of an agent that replies with no context ID. Tell the agent in its own instructions if that matters.
- **Only the text of a reply counts.** A turn that completes or pauses for an answer mentions the people its
  final text names. A question the agent asks through a pause, which Agent Kourier posts as a prompt, is not read for names, so an
  agent that wants someone to answer it has to name them in its reply text. A name in a code span or a fence does not count, though a name in a table does, even where Slack shows the
  table as code. A fence opened after a list marker (`-`, `*`, `+`, or one to nine digits and `.` or `)`, as in `1.`) or a `>` is a fence here and in what Agent Kourier sends to Slack, which shows it as a block at column 0 with the marker dropped, so a name in it does not count either. The block runs to its closing line, which may have the same kind of mark, or none: a line between that has no `>` in a quote, or is indented less than where the item's text starts in a list, is still in the block. Only one mark counts: a fence after `- -`, `> >` or `> -`, or after a list marker with no space after it (`-` and the backticks), is prose, and a name in the lines after it counts. (`>` needs no space: `>` and backticks is a fence.) Outside a block, a mark followed by backticks, and later on the same line by a next run of backticks of the same length, as in ``1. ```make test``` first``, is a list item or a quote with a code span, not a fence: the name in the span does not count, and the rest of the line does. Inside a block opened after the same kind of mark, such a line closes it like any other closing line, and the rest of the line counts; in any other block it is a line of the block, and nothing on it counts. The lines right after a block that opened on a list marker lose the item's content column (where its text starts: the white space before the marker, the marker, and the white space after it), counted in characters (a tab is one), up to the next fence or table of the item, and only while they are indented that far. Past that point the item's lines keep their indent. That is a limit, here and anywhere in a reply: a line indented four or more columns after a blank line, and probably right after a closing fence too (Slack's reading of that case has not been tested), can be code to Slack, and so can a line that starts with a tab, while a name in it still counts. It includes an item's lines after an indented fence, after a second block, or after a table. When Agent Kourier cannot tell what state Slack is in (a stream it did not start, or one that lost a call), it sends fences as they came, not as blocks, so a name it reads as code may show as text. A name that runs straight into more letters, as in `@samさん`, is a different name and does not count, and neither
  does one that ends where a long reply was cut. `_@sam_` and `~~@sam~~` count, and `@sam/@lee` names both people. A `/` after a name otherwise leaves it a name only before white space or the end of the line: `@sam/x`, `cd @sam/..` and `(@sam/)` read as paths, name nobody and are not counted as `undeclared`. Nothing else up to the next white space, `|` or backtick is a name either, so `@acme/x,@sam` names nobody. `@sam@lee` reads as an
  address and names nobody, as does a name straight after a word, `/`, `.`, `=`, `?`, `&`, `#`, `%`, `+` or `\`, and neither is counted as `undeclared`. A name after a `:`
  otherwise counts, so that `Owner:@sam` does. A name after a URL with `://` in it counts only once something ends the URL: white space, `|`, `<`, `>`, a backtick, `)`, `]` or non-ASCII punctuation, such as `—` or `。`. So `https://x.test,@sam` names nobody and is not counted as `undeclared`, while a `)` or `]` in the URL itself can let a name in it count. In a URL with no `://`, such as `mailto:x,@sam`, a name can count after most punctuation. A reply long enough to be cut can also notify someone for a name Slack does not show, as Slack's limit counts the text after escaping. A turn
  that was cut short, failed, was stopped or was rejected mentions nobody, and neither does a name
  past `maxMentionsPerTurn`. Nor does a turn whose final text could not be written, even if a stream had already shown the name.
- **One mention for each person.** A person is mentioned once a turn however often the reply names them, and two names for one
  user are one person. A task that pauses can run on under the same turn, for example when it is answered outside Slack: its later text
  mentions only people the turn has not mentioned yet, and `maxMentionsPerTurn` holds across both. A mention Agent Kourier gave up on posting still counts, and is not tried again.
  Only people still in `mentions` are counted, so changing the list in between can take the turn over the cap, and a name given a new `user` notifies that user.
- **Declare lasting owners, not a rota.** `user` is a fixed ID, so when a person moves on, it goes stale. Slack accepted a mention of an ID that does not
  exist with no error, so nothing tells you. Agent Kourier is not a paging tool, and user groups cannot be declared.
- **Keep the person in the channel.** Agent Kourier has not been tested with a mention of someone who is not a member of it.

A name the Binding does not declare notifies nobody, and `agentkourier_mentions_total` counts it as `undeclared`, which is
how to spot a typo or a name the model made up. [Security](../explanation/security.md#hostile-text) has what hostile text
can do with this, and what an agent is never allowed to do.

To link to a Slack message, the agent writes the message's URL in its reply. Slack shows it as its own reference to that
message, so this needs no setting.

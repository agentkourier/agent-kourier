# Let a team bring its own Slack app

For a platform team that runs Agent Kourier and a team that has a Slack app of its own: at the end, the team's Bindings
answer through the team's app, served by the platform's broker, and the team's tokens stay in the team's namespace.

The team owns a `ChatConnection` in its namespace, as it owns its Bindings. The platform team decides which namespaces may do
this, with a list in the chart's values, and the broker serves the ChatConnections of the namespaces on that list besides its
own, if it has one. A ChatConnection in any other namespace is not served: the broker does not run it and does not read its Secret.
A broker may have no connection of its own and serve only the teams': see
[A broker with no connection of its own](#a-broker-with-no-connection-of-its-own).

## Before you start

- The broker reads custom resources (`config.source: crds`, the chart's default). The admission list of step 3 is
  refused with `config.source: files`, which serves the one ChatConnection of its directory.
- The team has a Slack app with its two tokens, made as in [Configure Slack](configure-slack.md): the same manifest, the same
  scopes. No other ChatConnection may use that app: see [Two connections of one Slack app](#two-connections-of-one-slack-app).
- The broker asks Slack which app the bot of a connection belongs to (`auth.test`, then `bots.info`), for its own connection, if it has one, and for
  each team's. Slack's documentation says `bots.info` needs `users:read` (not verified here), which the manifest in
  [Configure Slack](configure-slack.md#what-the-app-is-allowed-to-do) has. If the call fails for the broker's own connection,
  no team connection starts, and the leader logs `the identity of the own connection is not known yet, so no team connection starts`; a broker with no
  connection of its own asks for no identity first, so this line does not occur there. If it
  fails for a team's, that connection does not start, and the log says `the identity of the ChatConnection's bot is not known yet, so it is not started`.
  Both are asked again at every look, unless Slack says the token is rejected. When the call works, nothing says so on its own: it has no log
  line and no metric, and the app it found is logged nowhere. What shows it is that the connection then starts, which [step 4](#4-check-it) reads.
- The platform team has read [what the broker then holds](../explanation/security.md#a-team-brings-its-own-chat-app).
- The names here are placeholders. The team's namespace is `team-a`, its Secret and its ChatConnection are both called
  `team-a-slack`, and the broker's ServiceAccount is `agent-kourier` in the namespace `agent-kourier`. Ask for the real ones. The ServiceAccount
  is named `agent-kourier` only for a release called `agent-kourier` (or with `fullnameOverride: agent-kourier`). In general it is
  `serviceAccount.name` when that is set, and otherwise the chart's full name: `fullnameOverride`, or the release name when it contains
  `agent-kourier`, or `<release>-agent-kourier`. The Deployment says which it is:
  `kubectl -n <release namespace> get deploy -l app.kubernetes.io/instance=<release> -o jsonpath='{.items[0].spec.template.spec.serviceAccountName}'`.

## 1. The platform team lets the team write a ChatConnection

The `admin` role of a namespace has no right on Agent Kourier's kinds, so the team cannot create a ChatConnection until a Role
gives it that right. This one lets the group `team-a` write ChatConnections in its own namespace. It can be the Role that
[lets the team write its Bindings](migrate-config-source.md#let-a-team-add-its-own-binding), with `chatconnections` in its
resources.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: agent-kourier-team-a-connections, namespace: team-a}
rules:
  - apiGroups: [agentkourier.dev]
    resources: [chatconnections]
    verbs: [get, list, watch, create, update, patch, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: agent-kourier-team-a-connections, namespace: team-a}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: agent-kourier-team-a-connections}
subjects:
  - {kind: Group, name: team-a, apiGroup: rbac.authorization.k8s.io}
```

The right does nothing until step 3: a ChatConnection in a namespace that is not on the list is not served.

## 2. The team makes the Secret, the grant and the connection

**The Secret.** It is in the team's namespace, with the two keys `botToken` and `appToken` (the key names are fixed) and the
type `Opaque`, which `kubectl create secret generic` makes. Agent Kourier reads no other type. The tokens go in through a
file only the team can read, as in [Configure Slack](configure-slack.md), so that they are on no command line:

```sh
kubectl -n team-a create secret generic team-a-slack --from-env-file="$HOME/.config/team-a/slack-secret.env"
```

**The grant.** The broker reads each Secret with its own ServiceAccount, which has no right on the team's namespace yet. The
team gives it `list` and `watch` on the Secrets it needs, with a Role and a RoleBinding in its own namespace. It needs nothing from
the platform's release or values. The Role names two kinds of Secret: the connection's, and the token Secret of each Binding that has a
`tokenSecretRef` (and the Secret behind each `valueSecretRef` of an AgentBackend in the namespace). Here the connection's
is `team-a-slack` and one Binding's token Secret is `team-a-agent-token`; a Role that names only the first lets the connection run and leaves
that Binding's `BackendReachable` condition `False` with `CredentialsUnreadable`:

```sh
kubectl -n team-a create role agent-kourier-secrets-team-a --verb=list,watch --resource=secrets \
  --resource-name=team-a-slack --resource-name=team-a-agent-token
kubectl -n team-a create rolebinding agent-kourier-secrets-team-a --role=agent-kourier-secrets-team-a --serviceaccount=agent-kourier:agent-kourier
kubectl auth can-i list secrets/team-a-slack -n team-a --as system:serviceaccount:agent-kourier:agent-kourier        # yes
kubectl auth can-i list secrets/team-a-agent-token -n team-a --as system:serviceaccount:agent-kourier:agent-kourier   # yes
```

The verbs are `list` and `watch`, not `get`. Give the Role a name the chart would not (`<fullname>-secrets` is the chart's own),
so that the two never meet. The Role is a grant to the broker, not to the ChatConnection: any Binding's `tokenSecretRef` or
AgentBackend's `valueSecretRef` in `team-a` can name this Secret too, and the broker then reads it and sends its value to that
AgentBackend's `url`. Anyone who may write Bindings or AgentBackends in the namespace can therefore have the broker send the app's
tokens to an agent, so grant the Secret only if those writers may hold the tokens
([Security model](../explanation/security.md#what-the-people-who-write-these-objects-can-do)). [Let a team add its own Binding](migrate-config-source.md#let-a-team-add-its-own-binding)
has the same two kinds of object as YAML. The team adds a Binding's token Secret to the Role when it writes the Binding that names it.

If the team cannot create a Role, the platform team grants the Secrets in its values instead, in step 3.

**The connection.**

```yaml
apiVersion: agentkourier.dev/v1alpha1
kind: ChatConnection
metadata: {name: team-a-slack, namespace: team-a}
spec:
  platform: slack
  credentialsSecretRef: {name: team-a-slack}   # a Secret in this namespace
  # allowedNamespaces: [team-b]                # only to let Bindings of other namespaces use it
```

`credentialsSecretRef` names a Secret in the connection's own namespace. `allowedNamespaces` is empty by default, so
only Bindings of `team-a` may use the connection, and they name it without a namespace: `connectionRef: {name: team-a-slack}`.
The [ChatConnection reference](../reference/chatconnection.md) has every field.

The team then writes its Bindings and AgentBackend as in [Write a Binding](write-a-binding.md), and invites the bot of its app
to the channels they name. Create the ChatConnection before its Bindings. A Binding applied while its ChatConnection does not exist
is owned by every broker, which writes `ConnectionNotFound` on it. Once the connection exists in a namespace that is not
admitted, no broker serves it, and nothing rewrites that status until step 3. A Binding applied after its connection has no status
at all until step 3, because the broker reads no Binding of a ChatConnection that it does not serve.

## 3. The platform team admits the namespace

Add the namespace to `config.connectionNamespaces` and upgrade the release:

```yaml
config:
  source: crds
  chatConnection: agent-kourier/slack     # the broker's own connection, as before (leave it empty for a broker with none)
  connectionNamespaces: [team-a]
```

The chart sets `AGENTKOURIER_CONNECTION_NAMESPACES` from the list. The broker reads it at start, so the upgrade restarts
the pods. It adds no rule to the ClusterRole, which already lists and watches ChatConnections in every namespace.

The list is the platform team's consent: the broker holds a chat connection on the tokens in a namespace only if the
namespace is on it. At most 16 connections are served in all, the broker's own included, and nothing changes that
number: see [Limits](../reference/limits.md#run-the-broker). The own connection, if the broker has one, counts first and the others oldest first, so a
connection created after the sixteenth place is taken is left out, and its Bindings say `ConnectionNotServed`, which the leader counts in `agentkourier_config_binding_rejections{reason="ConnectionNotServed"}`.

**If the team's Role does not exist,** the platform team names the Secrets in the chart's values, and the chart renders the Role
and RoleBinding itself in `team-a`, as `<fullname>-secrets`:

```yaml
rbac:
  secretNames:
    team-a: [team-a-slack, team-a-agent-token]
```

`rbac.secretNamespaces: [team-a]` grants every Secret of the namespace instead, which includes the Secrets of the team's
Bindings, so use it only for a namespace that holds nothing but Agent Kourier's. The chart renders one Role of that name
in a namespace, and a Role of the same name that someone made by hand there makes the upgrade fail with `invalid ownership
metadata`; this is why the team's Role above has another name.

A connection whose Secret the broker cannot read yet is not an error, and it never stops the broker or another connection. The
broker looks at the connections it serves every 5 seconds and starts one once its Secret can be read. What the wait is depends on
why it could not be read. A Secret that does not exist yet is picked up at the next look after it is created. A Secret that the API
server denied (the team's Role is not there yet) is asked for again only at the broker's recheck of the config, which comes about
once a minute and no sooner than a minute after the last try, so a Role applied late takes up to about 1 m 10 s, and then up to 5 s
for the look, to be noticed. No setting changes it ([Limits](../reference/limits.md#run-the-broker)).

## 4. Check it

The team reads its Binding. `READY` is `True` once the broker runs the connection, the bot is in the channel and the agent
answers:

```sh
kubectl -n team-a get akbinding
```

The platform team sees the connection in the leader's log (`serving a ChatConnection`, with the connection's name) and in
the metric `agentkourier_chat_connected{connection="team-a/team-a-slack"}`, which is 1 while the connection is up.
[Metrics](../reference/metrics.md#series-by-connection) lists the series that carry the `connection` label.

That line is also the sign that the lookup of whose bot the token is (the one the first section described) worked: the broker starts
a connection only after it has the answer, and logs nothing for the answer itself. So:

- `serving a ChatConnection` is logged: the lookup worked and the connection was started. The metric is 1 once Slack has opened the Socket Mode connection.
- Neither the line nor a series for the connection, and a WARN `the identity of the ChatConnection's bot is not known yet, so it is not started`: the lookup has
  not worked yet, and the WARN says why. A connection that has not started has no `agentkourier_chat_connected` series yet (unless its Secret could not be read, which sets 0:
  [Metrics](../reference/metrics.md#series-by-connection)), so an alert on that metric does not see it;
  its Bindings read `ChannelJoined` `Unknown` with `ChatUnavailable`, and `agentkourier_binding_ready` is 0 for them once their first probe has ended.
- A WARN `the ChatConnection is not served: it uses the same Slack app as the older ChatConnection ...`: the lookup worked, and the app is an older connection's.

## What the team sees in its Binding's status

The broker writes the status of a Binding on a connection it serves, from the connection's own bot. These are the states a
team meets, in the order of this page. [Binding status](../reference/binding-status.md) and
[Troubleshoot an install](troubleshoot.md#a-binding-is-not-ready) have every reason.

| What the team sees | What it means | What to do |
|---|---|---|
| No status, and empty `READY` and `REASON` columns | The namespace is not on the list, or the broker has not read the Binding yet (the first status comes within about 10 seconds). | Ask the platform team to admit the namespace. |
| `Accepted` `False`, `ConnectionNotFound` | The Binding was applied before its ChatConnection existed, and the namespace is still not admitted, so nothing has rewritten the status. | Ask the platform team to admit the namespace. Apply the ChatConnection before its Bindings. |
| `Accepted` `False`, `ConnectionNotServed` | The connection exists and the broker does not serve it: the broker served it before and does not now (its namespace was taken off the list, or it was the broker's own connection and the platform changed that to another or to none, and its namespace is not on the list), it is past the 16, or it is a second connection of a Slack app that an older connection uses. `message` says which. It is written once and not put back. Only a Binding that the connection admits is told. | See below, and [the reason's row](../reference/binding-status.md#accepted). |
| `Accepted` `False`, `ConnectionRejected` | The ChatConnection is invalid. If it had a good generation, that one runs. | Fix the ChatConnection; `kubectl apply --dry-run=server` finds what the schema checks. |
| `ChannelJoined` `Unknown`, `ChatUnavailable` | The connection is not running: it has not started yet, it is waiting for the platform to say whose bot it is, or its Secret cannot be read now. The probe makes no call to Slack. | Wait about two minutes: a Role applied late is noticed up to about 1 m 15 s later. If it stays, check the grant with `kubectl auth can-i` as in step 2, and ask the platform team to read the log. |
| `BackendReachable` `False`, `CredentialsUnreadable` | The Secret named by the Binding's `tokenSecretRef`, or behind a header of its AgentBackend, cannot be read. The `message` names the Secret and says what is wrong; for a Secret the API server denied, it says the ServiceAccount needs `list` and `watch` on it. | Add the Secret to the Role of step 2. It recovers without a restart once the grant exists, at the broker's next recheck of the config, which comes about once a minute. |
| `ChannelJoined` `Unknown`, `ChatCredentialsUnusable` | The Secret, or a key of it, is gone or empty, or the Secret is not of type `Opaque`. | Mend the Secret. The connection starts by itself. |
| `ChannelJoined` `Unknown`, `ChatCredentialsChanged` | The Secret holds other tokens than the connection runs on: it is restarting. The reason clears at the next probe. | Nothing; see [Rotate the tokens](#rotate-the-tokens). |
| `ChannelJoined` `False`, `ChatCredentialsRejected` | Slack rejected a token of the connection. Only this connection is stopped. | See [When Slack rejects a token](#when-slack-rejects-a-token). |
| `ChannelJoined` `False`, `NotInChannel` and the other channel reasons | The connection runs, and the bot cannot use the channel. | Invite the bot (`/invite @<the app's name>`). |

A Binding with `interactions.approverGroups` has `ApproversReadable` too, which reads the same way for the connection's
states.

## Two connections of one Slack app

Slack sends each Socket Mode event of an app to one of the app's open connections, which is why [Configure Slack](configure-slack.md)
says to stop any other process on the same app token. Two ChatConnections of one app in one broker would each miss events, so
the broker serves an app once. It asks Slack which app the bot of each connection belongs to
(`auth.test`, then `bots.info`), and when two connections give the same app ID it serves the older: its own connection first, if it has one,
then the others oldest first (`creationTimestamp`, then namespace and name). The younger one is not run, and its Bindings say
`ConnectionNotServed` with the message `another ChatConnection of the same Slack app is older and is the one this broker
serves, and an app is served once`. The message names no other connection, which may be in a namespace the reader cannot see;
the leader's log names it.

One app installed in two workspaces gives one app ID, so its two connections count as one app: the older is served.
That Slack gives an app's events to one socket was measured in one workspace; across the workspaces of one app it is not
verified here.

An older connection whose token Slack rejected when the broker asked whose bot it is holds no app while it is down, so a younger connection of the same app
can be served meanwhile. When the older one's Secret is fixed, the broker asks again at its next look, the older connection takes the app, and in
that same look the broker stops the younger one, as it stops any running second connection of an app, before it starts the older. The younger
connection's Bindings then say `ConnectionNotServed`. An older connection that was running when Slack rejected its token keeps holding the app.

A running connection whose Secret can no longer be read (it was deleted, a key was removed or emptied, or the broker's cache has not synced) keeps running on the
tokens it started with, and keeps holding its app. If an older connection takes that app in the same look (its Secret now holds that app's tokens, or the
rejection of its token has cleared), the broker stops the younger one and refuses it, as it does any second connection of an app, and its Bindings say
`ConnectionNotServed`. It stays refused for as long as its Secret cannot be read, even once the app is free again, and it is served again at the first look
at which its Secret reads and the app is free. If the Secret holds new tokens, Slack must also have said whose bot they are (see below).

When the broker asks whose bot the tokens are after the ChatConnection or its Secret changed (`auth.test`, then `bots.info`), a failure that may pass
leaves the connection as it was, and not started: either call timing out (each is given 10 seconds), the request failing before an answer, Slack answering
with an HTTP status other than 200 (a 5xx, or a 429), or Slack answering with one of the codes `ratelimited`, `internal_error`, `service_unavailable`,
`request_timeout` or `fatal_error`. The broker asks again at its next look, every 5 seconds,
and there is no other bound: this goes on for as long as the calls fail this way, and ends when Slack answers. Meanwhile a connection keeps holding the app
of the tokens it had (unless an older connection holds it by now), and a connection that was refused stays refused, so a younger connection of the app is
not served in between and then stopped again. The leader logs one WARN, `the identity of the ChatConnection's bot is not known yet, so it is not started`,
naming the connection and the failure (again if the failure changes), and the connection's Bindings read `ChatUnavailable`, or `ConnectionNotServed` for a refused one. If the new tokens
turn out to be another app's, the app it held is free from then on. Every failure not in that list is treated as one Slack gives again for the same tokens (any other
Slack code, such as `missing_scope` for a scope the app lacks; a token that belongs to no bot; an answer with no app), so the broker does not wait on it: the connection holds no app and is
not refused while it fails, as a new connection is, the leader logs the same WARN, and it is asked again at every look until the ChatConnection or the Secret is mended.
A token Slack rejects (`invalid_auth`, `token_revoked`, `token_expired`, `account_inactive`, `not_authed`) is the first case of this section. The broker also keeps the connection's bot among the bots it tells from a person while it
restarts, so a mention by that bot starts no turn on another connection unless a Binding's trigger names the bot, as when it runs.

The ChatConnection that is not run is not deleted, and it is served again if the older one goes away. The platform's own
connection, if the broker has one, always comes first, so a team that copies the platform's tokens gets the second place; a broker with none serves the oldest.

## Rotate the tokens

Write the new tokens into the Secret, and only then revoke the old ones in Slack:

```sh
kubectl -n team-a create secret generic team-a-slack --from-env-file="$HOME/.config/team-a/slack-secret.env" \
  --dry-run=client -o yaml | kubectl apply --server-side -f -
```

The broker restarts a team connection by itself. At its next look, which comes every 5 seconds once the broker has the
changed Secret, it stops the connection (waiting up to 5 seconds for it to end), asks Slack whose bot the new token is, and
starts it again. No restart of the pods is needed, and no other connection is touched; the broker's own connection is the
exception, which takes new tokens only at a restart. Until the connection runs again its Bindings read `ChatUnavailable`, and
the posts waiting for it stay in the outbox and are posted once it runs. Before that, while the old connection still runs on the old
tokens, the Bindings read `ChatCredentialsChanged`. A ChatConnection edit restarts the connection the same way. What the
renderer does with a turn that is in flight while its connection is down is not described here.

If the old tokens are revoked before the Secret holds the new ones, Slack rejects the connection, which stops, as in the next
section. It starts again at the first look after the broker has the new tokens.

## When Slack rejects a token

Slack rejects a token for good when it is invalid, revoked or expired (`invalid_auth`, `not_authed`, `token_revoked`,
`token_expired`, `account_inactive`). For a team connection this stops that connection only:

- the process keeps running, and so does every other connection; the broker's own connection, if it has one, is the exception, since its
  rejected token exits the process (code 1);
- the leader logs one WARN that names the connection and Slack's code, and never a token;
- `agentkourier_chat_connected` is 0 for that connection;
- its Bindings read `ChannelJoined` `False` with `ChatCredentialsRejected`, and the probe makes no call to Slack;
- the posts waiting for it stay in the outbox until it runs, or until the outbox abandons them after 7 days
  ([Limits](../reference/limits.md#what-the-broker-keeps-and-logs));
- it starts again when its Secret or its ChatConnection changes, and not before: the broker does not ask Slack again for the same
  tokens.

Mend it by writing valid tokens into the Secret.

**When the broker notices.** A running connection finds out that Slack rejects its bot token when a call made with the token is answered with a
rejection; every call through the connection's platform reports one, and the connection stops. With no traffic, the call that comes on a schedule is
the probe of a Binding's channel, which runs every 5 minutes, each pass shifted by up to 10% ([Limits](../reference/limits.md#run-the-broker)). In a trial with two Slack apps, a connection
noticed a revoked token at the next of those probes and not before. So a token revoked in Slack can go unnoticed for up to about 5.5 minutes (a post, or any other call the connection makes for a turn, finds it sooner), and a connection with no Binding has no such probe. A token that is
already rejected when a connection is first seen is found by the lookup of whose bot it is, at the look of the broker that first sees the connection (the looks come every 5 seconds).
In that trial Slack reported the revoke as `account_inactive` and not `token_revoked`. That is what was
observed, and it is Slack's behaviour, which this repository has not verified; the broker treats both codes alike.

## A broker with no connection of its own

A platform that runs the broker for its teams does not need a Slack app of its own. Leave `config.chatConnection` empty and list the
namespaces whose ChatConnections the broker serves:

```yaml
config:
  source: crds
  connectionNamespaces: [team-a, team-b]
```

The chart then renders `AGENTKOURIER_CONNECTION_NAMESPACES` and no `AGENTKOURIER_CHAT_CONNECTION`, and every connection the broker serves is a
team connection, as this page has described them. The chart checks the shape when it renders. It refuses `config.chatConnectionSecret` (it names
the Secret of an own connection, and there is none), and it refuses a `config.chatConnections` entry whose namespace is not on the list, because the
chart would render the ChatConnection and the broker would never serve it; the message names the entry. A ChatConnection that `config.chatConnections` renders has
its Secret granted by the chart, from its `credentialsSecretRef`; the teams grant the Secrets of the ones they make, as in step 2. With
`config.chatConnection` and `config.connectionNamespaces` both empty, the chart reads the one `config.chatConnections` entry as the own connection, as it
always did, and with none or several it refuses and names both ways out.

What the broker does without an own connection:

- **It starts with the list alone.** At least one namespace must be on the list: with neither `AGENTKOURIER_CHAT_CONNECTION` nor a list the broker could serve
  nothing until a restart, since it reads the list at start, so it exits with [code 2](../reference/cli.md#exit-codes) and an error that names both variables. A broker
  whose listed namespaces hold no ChatConnection yet starts, is ready, and serves nothing until one appears.
- **A rejected token stops one connection and never the process.** Every connection is a team connection, so none of them exits the process with code 1.
  With every token rejected the pod stays up and `/readyz` still says ready, since readiness never includes Slack; each connection's `agentkourier_chat_connected`
  is 0 and its Bindings read `ChannelJoined` `False` with `ChatCredentialsRejected`. A restart would not mend a rejected token, so the broker does not exit. The pod's state does not say that it serves nobody; a WARN for each connection, the metric and the
  Bindings do: [alert on the metric](../reference/metrics.md#alert-rules-for-a-broker-with-no-connection-of-its-own).
- **Nothing is held.** The hold that keeps an own connection's Bindings while its ChatConnection is deleted is for the own connection alone, so a deleted team
  connection is dropped at the read that settles the deletion, as under [Take it back](#take-it-back), and `agentkourier_config_held_objects` is always 0.
- **All 16 places and every claim of a Slack app go by age.** With no own connection to take the first place, the 16 oldest connections are served
  (`creationTimestamp`, then namespace, then name), and when two connections are of one Slack app the older is served.
- **Rows with no connection stop the leader.** The outbox rows and the button presses that were written before the broker recorded connections, or by an
  older image after a rollback, have an empty connection, and an own connection takes them when the leader starts. With no own connection nobody can,
  so the leader counts them before it posts anything, and if there are any it ends the process with code 2 and the error
  `N outbox rows and M button presses were written before connections were recorded, and this broker has no ChatConnection of its own to give them to: set config.chatConnection (AGENTKOURIER_CHAT_CONNECTION) to the ChatConnection that served them, let the broker start once, then unset it`.
  Do that: set `config.chatConnection` to the connection that served the rows, upgrade, wait until the leader has started (it logs `store: gave the rows written before connections were recorded to this broker's own connection`),
  and unset it again. A store that no own-connection broker ever used has no such row. Only the replica that becomes the leader makes the check, so with Postgres a standby does not.

### Move a broker from its own connection to none

In one upgrade, set `config.chatConnection` to empty, remove `config.chatConnectionSecret` if it is set (the chart refuses it), and put the connection's namespace
on `config.connectionNamespaces`. If `config.chatConnections` renders the connection, keep it there: the chart then still grants its Secret. If you applied it by hand, the
chart no longer grants the Secret by name, because it granted only the own connection's: name it in `rbac.secretNames` or have its namespace grant it, as in step 2. Until
it can be read the connection is not started, and its Bindings read `ChatUnavailable`. The upgrade restarts the pods.

The connection is then a team connection, so it loses what only an own connection has:

- a token that Slack rejects stops that connection and no longer exits the process;
- the hold on its Bindings, when its ChatConnection is deleted;
- the first of the 16 places, which it now has by age like the others;
- the first claim of its Slack app: an older ChatConnection of the same app now wins.

If you leave its namespace off the list, the broker no longer serves it, and its Bindings are told `ConnectionNotServed` once. Rows with no connection are
the leader's to refuse, as above; a broker that has had an own connection has attributed its rows at each start, so you meet that only if an older image wrote more.
To move back, set `config.chatConnection` again.

## Take it back

- **The team stops using its app.** It deletes the ChatConnection. The config drops it at the read that settles the
  deletion, about 10 to 20 seconds after the delete, with no hold, unlike the broker's own connection, and the connection stops at the supervisor's next look, up to 5
  seconds later. The team's Bindings then say `ConnectionNotFound`.
- **The platform stops serving the team.** It takes the namespace off `config.connectionNamespaces`. The broker reads the
  list at start, so the upgrade restarts it; it records the connections it serves in its store, and after the restart it
  writes `ConnectionNotServed` once on the Bindings that the connection admits. Their sessions are kept, and the
  posts waiting for the connection stay in the outbox. An interaction pending on one of their threads cannot be ended through the Binding, which the broker no longer has, so the sweep ends it
  and posts nothing `5m` after the first sweep that finds it both past its timeout and orphaned ([Limits](../reference/limits.md#what-a-pause-may-ask)): the task is cleared from the session record (the restart that took the namespace off the list left no runner parked), and the interaction is audited as `orphaned`.
  The agent's task is not cancelled: it stays input-required at the agent until the agent expires it.
- **A broker with no connection of its own cannot be rolled back to an earlier release in place.** 0.2.0, 0.2.1 and 0.3.0-rc.1 exit with code 2 on
  `config.source: crds` without `AGENTKOURIER_CHAT_CONNECTION`: that is what their source says (`cmd/agent-kourier/main.go` at those tags, read and not run). Set
  `config.chatConnection` first, which makes the broker one with an own connection, and then follow the next bullet.
- **Rolling the release back.** Do not roll back to a release before 0.3.0 while any team connection has an unposted row. The outbox, the
  interactions and the button presses of 0.3.0 record the connection they belong to. 0.2.1 has one chat platform, and read from its
  code (not run) it does not filter by connection in two places that matter:
  - it selects its unposted outbox rows with no regard to a connection and posts them through its one platform, so a post waiting
    for a team's connection would go out through the platform's app;
  - its sweep lists every pending interaction that is past its deadline (`ListDueInteractions`). For one on a team's thread, the
    step that ends the pause (`Session.Cancel`) looks up the thread's Binding, which 0.2.1 does not have, and fails; the sweep never
    gives that step up, so in the usual case nothing is edited or posted, and 0.2.1 logs the failed timeout at every sweep. It goes on to
    edit the prompt and queue the timeout notice, which its one platform posts in the team's thread, only when the thread's session no
    longer records the interaction's task (the session was reset, moved to a newer task, or is gone).

  Button presses are listed without a filter too (`ListPresses`), but a press whose Binding 0.2.1 lacks decides nothing. It is ended, and 0.2.1 still sends the person who pressed an ephemeral
  note that the request is gone, in the team's channel through the platform's app (`authorized`, then `finish`, then `finishWith`,
  which calls `Ephemerals.Ephemeral`). A session whose Binding 0.2.1 does not have is skipped when it takes up its sessions.

  The guard is the unposted rows of every team connection, not whether one is served now. A connection that was served and is not (its namespace was taken off the list, or its ChatConnection was deleted) keeps its unposted rows, up to 7 days
  ([Limits](../reference/limits.md#what-the-broker-keeps-and-logs)), and 0.2.1 would post them through the platform's app. [Metrics](../reference/metrics.md#series-by-connection) says the series of such a connection stays while it has an unposted row.

  The order that leaves nothing behind, all while the team connections are still served:
  1. Let the team's pending interactions end, answered or timed out, while its Bindings still exist. A pending interaction ends at its
     deadline: 30 minutes by default, and a Binding may set up to 7 days (`interactions.timeout`,
     [Limits](../reference/limits.md#write-a-binding-an-agentbackend-or-a-chatconnection)). The broker has no switch that stops new
     ones, so ask the team to start none. Ending it while the Bindings exist cancels a question's task at the agent, or denies an approval's tools; ending it after they are gone leaves the agent's task as it is.
  2. Delete the team's Bindings.
  3. Check again for pending interactions. `agentkourier_interactions_pending` has no `connection` label: if it is 0, none is pending
     on any connection; if not, count the team's in the store (the table `interactions`, with `state = 'pending'` and
     `connection = 'team-a/team-a-slack'`). An interaction left pending after the Bindings are gone is ended by this release's sweep, without a
     word in the thread, `5m` after the first sweep that finds it both past its deadline and without its Binding (a restart or a takeover starts the count again, and
     the sweep is every 10 seconds). Wait for that and check again: the broker you roll back to would meet it otherwise. The agent's task is not cancelled, since the
     Binding that would call the agent is gone; it stays input-required at the agent until the agent expires it.
  4. Check that no button press of another connection waits: no row of the table `interaction_presses` has a `connection` that is neither the broker's own (`agent-kourier/slack` below) nor empty (an empty one is the own connection's, as in step 5). A press is deleted once it is processed.
  5. Wait until `agentkourier_outbox_pending{connection!="agent-kourier/slack"}` is 0 or has no series on the leader, with `agent-kourier/slack` the broker's own connection as `namespace/name`, the value of `AGENTKOURIER_CHAT_CONNECTION` in the pod (`config.chatConnection` may give the name alone for a connection in the release's namespace).
     Read that only while `agentkourier_outbox_pending{connection="agent-kourier/slack"}` is present: the leader shows a 0 for each connection it serves, and a connection it does not serve shows a series only while it has an unposted row. A scrape that fails
     (the metrics endpoint answers an error when the outbox query fails) has no series at all, which is not the same as none being unposted. The store says it directly: no row of the table `outbox` has `message_ref = ''`, an unset `abandoned_at`
     (`abandoned_at IS NULL` on Postgres, `abandoned_at = 0` on SQLite) and a `connection` that is neither `agent-kourier/slack` nor empty. An empty `connection` is a row from before migration 0029 or from an older image, which the leader gives to the own connection, if the broker has one, when it starts.

  If a row stays for a connection that is not served, admit its namespace again so that the connection runs and the row posts, and if the team deleted its ChatConnection have it made again under the same name, and for the same Slack app: the rows post through whatever app the new object's tokens belong to.
  Or wait out the 7 days: the outbox abandons a row older than that, which drops its text and keeps the row.

  Then take the list off the values in the same change as the rollback: the chart of 0.2.1 refuses a `config` key it does not have,
  and this chart refuses a non-empty list with `config.source: files`. A `personMessage` trigger has to go first too: 0.2.1 knows only `mention` and `botMessage`, and refuses a Binding that has a `personMessage` trigger
  ([Upgrade](upgrade.md#roll-back-from-this-release)).

## Related

- [Security model](../explanation/security.md#a-team-brings-its-own-chat-app): what the broker holds, and what the admission list means.
- [Limits](../reference/limits.md#run-the-broker): the 16, and how soon a change is followed.
- [ChatConnection reference](../reference/chatconnection.md)
- [Choose a config route](../explanation/config-routes.md#which-chatconnections-a-broker-serves)

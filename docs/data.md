# What courier stores, and for how long

Courier encrypts nothing itself. At rest it relies on the volume (an encrypted EBS volume for SQLite) or the database (RDS
with a KMS key). What it does control is how long a piece of content is kept, and that an agent's secrets never reach the
store. Both stores (SQLite and Postgres) keep the same promises: the store conformance suite (`internal/store/storetest`)
holds each to them. What courier logs is a separate matter, covered with the telemetry in [observability.md](observability.md).

Agent text that streams to a thread is never stored. Spans, metric labels and the trace context sent to the agent
(`traceparent` and `tracestate`, baggage removed) carry no content.

## Content, and how long it is kept

| Where | What it holds | Kept |
|---|---|---|
| `interactions.request` | The agent's pause: its hint, the questions and choices, a tool's name and arguments, as JSON. Secrets are taken out before it is stored (below). | Until the interaction is settled and the grace has passed; then emptied. |
| `interactions.answer` | The person's answer, free text included. | The same. |
| `interactions` (the rest of the row) | IDs, kind, state, times, the decider's platform ID, the prompt's message ref. | No content. The row goes with its session (below). |
| `reply_queue.text` | A thread reply, or an alert prompt, waiting for its turn. | Cleared when the turn is delivered. A reply that was never delivered is dropped, with its text, after 30 days (below). |
| `interaction_presses.inputs` | What a person typed into a prompt. | Until the press is processed, or the worker gives up (an hour); purged an hour after the interaction's deadline at the latest. |
| `outbox.payload` | The message to post. | Cleared once posted or abandoned; an unrecorded one is purged after 7 days. |
| `sessions`, `reply_queue` (delivered), `interactions`, `interaction_presses`, `outbox` | IDs, keys, refs and times of a thread: no message text is left in them. | Until the session has been expired for 90 days, then all deleted together (below). |
| `feedback` | A person's verdict on a message of an investigation or an answer: the session and turn, the voter's platform ID, the Binding, the agent, the alert name and the verdict, and when. No free text. | For good: kept for the pilot's review (D33). No retention loop deletes a row, and the deletion of a session 90 days after it expired (below) leaves its feedback rows. |
| `sessions.alert_name`, `sessions.agent_name` | The alert name a trigger started the thread for, in the name shape (ASCII letters, digits and `_.-:`, at most 128 bytes) or empty, and the agent of the Binding when the session was made. A vote is stored and counted under them, whatever the Binding says later. | As long as the session row, which is deleted with its thread (below). |
| `audit` | IDs, codes, outcomes and lengths, never message text. | For ever, unless `COURIER_AUDIT_RETENTION` is set (below). |
| `events.payload` | Nothing writes it yet. The Alertmanager receiver comes with its own purge. | n/a |

### Feedback

The useful and not-useful buttons (spec 8.7) store one row for each person and message, in table `feedback` of both stores:

| Column | Holds |
|---|---|
| `connection`, `channel`, `thread_ts` | The session: the thread the rated message is in. |
| `turn_id` | The turn whose output is the rated message. A thread's several answers are rated apart. |
| `user_id` | The platform's ID of who voted (a pseudonym, not a name or an email). |
| `binding`, `agent`, `alert_name` | The Binding, its agent (`namespace/name` on the backend) and the alert name of the thread, as they were at the person's first vote. |
| `kind` | `investigation` (the final message of a thread that a trigger or an alert started) or `answer` (an answer to a person, on a Binding with `feedback.enabled`). |
| `verdict` | `useful` or `not_useful`. A later press of the other button replaces it. |
| `first_at`, `voted_at` | The first vote, and the latest. |

No free text is stored: courier asks for none, and the alert name is kept only in the name shape (an alert's title is
untrusted text; a value outside the shape is stored as empty). A vote counts only from a member of the bound channel and
never from a bot. **Retention: rows are kept.** The pilot's exit decision is computed from them, and a later review reads
them; no retention loop touches the table, the deletion of expired sessions (DP-3) included. A vote on a message whose session
has been deleted is told that the message can no longer be rated. The voter's ID is the only personal datum in a row: remove the
rows of a person with `DELETE FROM feedback WHERE user_id = ...` on the operator's own request.

Each vote is also an audit entry of kind `event` whose subject names the turn, channel and thread and whose outcome is the verdict
(`feedback: verdict=useful changed=false`), and it is counted in `courier_investigation_feedback_total`
([observability.md](observability.md)); the table, not the metric, is the record D33 is computed from.

The pilot's exit decision (D33) counts investigations, not votes: an investigation (the thread a trigger started) is rated if any
person voted on its investigation message, and is useful if more of its votes are useful than not useful, so a tie is not useful.
It is one query, the same in SQLite and Postgres (the stores' tests run this very text against both):

```sql
WITH rated AS (
    SELECT connection, channel, thread_ts,
           SUM(CASE WHEN verdict = 'useful' THEN 1 ELSE 0 END) AS useful,
           COUNT(*) AS votes
    FROM feedback
    WHERE kind = 'investigation'
    GROUP BY connection, channel, thread_ts
)
SELECT COUNT(*) AS rated,
       COALESCE(SUM(CASE WHEN useful * 2 > votes THEN 1 ELSE 0 END), 0) AS useful
FROM rated;
```

### Interactions: request and answer

A settled interaction (answered, timed out or cancelled) keeps its request and answer for a grace after it was decided,
then both are emptied by a sweep that runs every 10 minutes (`interact.Retention`, beside the press purge). The row stays,
so the audit's interaction IDs still resolve.

The grace is **25 hours**, from what reads the two after a decision:

- The repair of ended prompts (`interact.Replacer`) builds its edit from them for 24 hours after the decision
  (`interact.RepairHorizon`), and a press recorded just before then is processed for up to an hour more (the press worker's
  cap). The grace is the sum, and a test ties the default to both.
- A pause that comes again under the ID of a settled interaction (a restart that finds the task paused) needs them to resend
  an answer, so an answered interaction whose answer was **not yet marked delivered** to the session keeps them for
  **7 days** instead: it is the only copy of what the agent has not received. Once delivered it follows the 25 hours.
- Nothing that runs after the grace needs them. The decider pressing again on a prompt that still shows its controls is
  told the question is gone; anyone else is told, as before, who answered or that it is closed (neither reads the blobs).
  The repair has nothing to edit and skips the prompt. The audit, the decider, the timeout sweep and the backstop read the
  row, never the blobs.
- A pause that comes again under the ID of a cleared interaction (a restart that finds the task still paused) is handled
  from the row alone. For a timed-out or cancelled one the task is cancelled again, as before. For an answered one the
  answer cannot be sent: the task is cancelled, the thread is told the answer was not sent before it was cleared, and the
  audit records `answer_cleared` (not an unusable pause).

### Secrets in what is stored

The request is stored through `redact`: the agent's hint and each question's text pass through `redact.Text`, a tool's
name likewise, and a tool's arguments through the redaction that the Slack status line uses (`render.RedactArgs`: values
under a secret name, `name`/`value` pairs, tokens, URL credentials, private keys), so that what is stored is never more than
what is shown. The agent still receives what it always did: the answer is built from request and tool IDs and the chosen
choice, none of which changes.

Accepted limitation: a pause is told to be the same one as a stored pause by comparing both after redaction, so two pauses
under one interaction ID that differ only in a redacted value count as the same. A keyed digest of the unredacted pause
(HMAC, key from a Secret) is planned before tool approvals are rendered (tasks/plans/data-protection.md).

Two things are stored as given, because the agent gets them back as they are: a question's **choices** (an answer picks
one by its index, and the agent receives the stored choice), and the **answer**, until the interaction is cleared.

### Replies that never drained

A reply is delivered, which clears its text, or it waits behind its session's task. A row that stays undelivered for **30
days** with no active task on its session is dropped, as a turn is when its Binding has left: its text is cleared and the row
is marked delivered, so a replayed chat event is not queued again, and it is never sent. A reply whose session records an
active task is never dropped, however old: it waits behind that task by design (a long task, a pause that waits for an
answer, a busy or not-ready session that is waited out without limit). 30 days is far longer than any of those, and a third
of the default `threadTTL`, after which a session is expired and recovery skips it. A drop is logged at WARN, with a count, and counted in `courier_retention_removed_total{what="stranded_replies"}`. The queued
reaction (the hourglass) stays on the dropped reply's message, since nothing takes it off.

### Sessions and what belongs to them

Clearing content leaves the rows, and a thread that nobody answers again would keep its rows for ever. A session is kept after
it expires on purpose: a reply in an expired thread gets the notice that the conversation has expired, and not silence. That is
a courtesy to a person who comes back, and it is not owed for ever. **A session is deleted 90 days after its `threadTTL` has run
out**, that is, when its last activity is older than `threadTTL` plus 90 days, with every row that belongs to it, in one
transaction:

- its **reply queue**: the delivered replies, which stay as tombstones so that a replayed chat event is not queued twice;
- its **interactions**, settled by then, and the **presses** on them;
- its **outbox rows**, posted or abandoned: those made for the thread (a reply, a notice), and those made for one of its replies,
  which have no thread and are found by their source: the queued reaction of a reply, and the removal of it, are made for the
  source `<reply ID>/queued`, so a row whose source starts with a reply's ID and a `/` belongs to that reply's session.

Only **settled rows** are deleted (a delivered reply, a decided interaction, a posted or abandoned outbox row): a row written
after the sweep chose its sessions, such as a notice for a reply in the thread that is not posted yet, is kept even if its
session is not.

What belongs to no session is not touched by this sweep: trigger runs have their own rule (24 hours), as do trigger threads (their
cooldown, or 30 days idle) and the audit log (the retention above). **Kept, with no rule yet (an open item):** the outbox rows of a
digest or a root post, which no thread or reply names, and a notice that is posted into a thread after its session was deleted
(an orphan the sweep never sees, as its session is gone). They hold no content: IDs, refs and times.

**The threadTTL is the Binding's**, read from the config at each sweep. A session whose Binding has left the config has no
threadTTL to go by: it is held to the longest `threadTTL` of any Binding in the config (at least the 90-day default) plus the
90 days, so that a Binding missing for a while, from a config that lost it and is mended, does not have its sessions deleted on
a shorter reckoning than it would have kept them by. Nothing is deleted before a config has loaded.

**Never deleted, whatever its age**, while any of these holds, so that a session is not deleted from under work that still reads it:

- an **active or paused task** on the session (its `task_id` is set; recovery follows it), unless the task is older than the
  horizon, which the sweep ends first (below);
- a **pending interaction**;
- a **reply that was never delivered** (one that is stranded is dropped after 30 days, see above, and then no longer holds the session);
- an **outbox row that was neither posted nor abandoned**.

Whatever held a session back, once it is resolved, the next sweep takes the session.

**A task that a crash left.** A session is deleted only when it holds no task, but a task that was recorded when courier crashed
is not always found again: recovery follows the tasks of sessions that are live, and leaves an expired session exactly as it is,
so a session that expired after the crash, with its `task_id` still set, would be kept for ever. The retention loop ends such a
task. Each tick, before anything is dropped or deleted, it lists the sessions that are **past their horizon** (the same
horizon as the deletion: the Binding's `threadTTL` plus 90 days) and still record a task, oldest first, and for each:

0. leaves it if this process's session manager still runs that task (parked on a question it has not given up on, or streaming
   it): the task is its runner's, and clearing it from under the runner would leave the runner parked after the session is
   deleted, so that a new session under the same key waited behind it. The tick says, at WARN, that it left some. The runner
   ends it (a streaming task, when the runner gives up on it), or the interaction timeout does (a parked one), and a restart
   does when neither can (a Binding that left the config makes the timeout's cancel fail for ever).

1. asks the task's agent to cancel it. This is a courtesy and never stops the sweep: the call is bounded (10 seconds); an agent
   that does not answer is not asked again in that tick, so a Binding whose agent is down costs one timeout and not one for each
   task; an error, or an agent that says it knows no such task, is recorded and goes on; a session whose Binding has left the
   config has no agent to ask, and goes on. A tick that is stopped (the broker is shutting down) before the answer comes ends no
   task, so the next run asks again.
2. clears the task (and the turn recorded for it; the agent's context stays) and cancels the **pending interactions of that task**,
   in one transaction, and only while the session is still past its horizon and still records that task. A session that has been
   active since, or whose task is another, is left whole. The prompt of a cancelled interaction is **not** edited: the purge
   deletes the settled interaction in the same tick, before the repair of ended prompts (every five minutes) lists it, and
   the repair skips a prompt whose interaction is gone. The prompt, nearly 180 days old, keeps its buttons, and a press on
   one is answered that the question is gone.
3. audits it: one entry of kind `event`, by the Binding and, when a cancel was made, the service identity it was made under (as
   a stop's is), subject `task=<id> context=<id>`, outcome `stale_task_ended: cancel=<ok | failed <type of the error> |
   no_binding | skipped> interactions=<n>`: identifiers, the type of an error and a count, never agent text and never the
   thread. The log of the tick carries counts only.

Nothing inside the horizon is touched: a task on a session that is live, or expired for less than the grace, is a task that
recovery may still follow. The session is deleted by the purge that follows in the same tick, or by the next, as any session
whose work has ended. It is bounded as the purge is: 50 sessions to a listing and at most 10 listings (500 tasks) to a tick;
with Postgres each ending checks the leader's epoch first (D37). It counts `stale_tasks` and `stale_task_interactions` in
`courier_retention_removed_total`.

**What a person sees.** A reply in a thread whose session is deleted is a reply in a thread courier never owned, and is ignored
(spec 8.2). An @mention in it starts a new session anchored to that thread, as it does in any thread courier does not own.
Until then, as before, a reply gets the expiry notice.

**How it runs.** The sweep belongs to the store retention loop (hourly, with the replies and the audit purge) and only the
leader runs it. It deletes 100 sessions to a call, in a transaction, and at most 100 calls (10 000 sessions) to a tick: the first
sweep after an upgrade finds every old session, and works them down over several ticks. With Postgres each write checks the
leader's epoch first (D37), and a session that a writer holds locked is left for the next call. It logs a count at INFO and counts in
`courier_retention_removed_total`: `expired_sessions`, and the rows that went with them as `session_replies`,
`session_interactions`, `session_presses` and `session_outbox` (and, for a stale task, as above). Migration 0022 adds the indexes the sweep reads by (a session's
last activity; a thread's replies, interactions and outbox rows; an outbox row's source, in the C collation with Postgres).

Not deleted here: `feedback` rows, which are kept until the pilot's review of them (D33).

### Audit

The audit table is append-only against the application: no path of courier updates or deletes an entry, and a trigger in
both stores refuses it. The single exception is the retention purge.

Set `COURIER_AUDIT_RETENTION` (chart: `auditRetention`) to a duration of at least 24h, such as `8760h` (a year), and entries
older than it are purged, hourly, in batches of 1000 (at most a million entries in one run; a log far past its retention is
worked down over several runs). The default is `keep`: nothing is purged until an operator sets it. The purge writes nothing
to the audit; it logs a count at INFO and counts in `courier_retention_removed_total` (`audit_entries`; settled interactions
that are cleared count as `cleared_interactions`, and deleted sessions as above).

How each store lets the purge through the trigger:

- **Postgres**: `courier_audit_purge(cutoff, batch)` (migration 0020, beside 0006's `courier_audit_purge(cutoff)`) sets
  `courier.audit_retention` for its own transaction, deletes the oldest `batch` entries before the cutoff, and switches the
  setting off before it returns. Only the leader purges (every write checks the epoch).
- **SQLite**: the delete trigger permits a `DELETE` only while the table `audit_purge` holds a cutoff later than the entry
  (migration 0020). The purge inserts that row, deletes a batch and removes the row in one transaction, on the single write
  connection, so no other statement can see it; a rollback or a crash leaves no row.

Threat model, the same in both: the log is protected from a bug in courier, which cannot edit or erase it. It is not
tamper-evident. Whoever owns the database file or the table can drop the trigger, insert the SQLite marker row, or set the
Postgres setting, and nothing in the log would show it.

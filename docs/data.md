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
| `audit` | IDs, codes, outcomes and lengths, never message text. | For ever, unless `COURIER_AUDIT_RETENTION` is set (below). |
| `events.payload` | Nothing writes it yet. The Alertmanager receiver comes with its own purge. | n/a |

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

- an **active or paused task** on the session (its `task_id` is set; recovery follows it);
- a **pending interaction**;
- a **reply that was never delivered** (one that is stranded is dropped after 30 days, see above, and then no longer holds the session);
- an **outbox row that was neither posted nor abandoned**.

Whatever held a session back, once it is resolved, the next sweep takes the session.

**What a person sees.** A reply in a thread whose session is deleted is a reply in a thread courier never owned, and is ignored
(spec 8.2). An @mention in it starts a new session anchored to that thread, as it does in any thread courier does not own.
Until then, as before, a reply gets the expiry notice.

**How it runs.** The sweep belongs to the store retention loop (hourly, with the replies and the audit purge) and only the
leader runs it. It deletes 100 sessions to a call, in a transaction, and at most 100 calls (10 000 sessions) to a tick: the first
sweep after an upgrade finds every old session, and works them down over several ticks. With Postgres each write checks the
leader's epoch first (D37), and a session that a writer holds locked is left for the next call. It logs a count at INFO and counts in
`courier_retention_removed_total`: `expired_sessions`, and the rows that went with them as `session_replies`,
`session_interactions`, `session_presses` and `session_outbox`. Migration 0022 adds the indexes the sweep reads by (a session's
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

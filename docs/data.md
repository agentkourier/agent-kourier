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
| `interactions` (the rest of the row) | IDs, kind, state, times, the decider's platform ID, the prompt's message ref. | For good: no content. |
| `reply_queue.text` | A thread reply, or an alert prompt, waiting for its turn. | Cleared when the turn is delivered. A reply that was never delivered is dropped, with its text, after 30 days (below). |
| `interaction_presses.inputs` | What a person typed into a prompt. | Until the press is processed, or the worker gives up (an hour); purged an hour after the interaction's deadline at the latest. |
| `outbox.payload` | The message to post. | Cleared once posted or abandoned; an unrecorded one is purged after 7 days. |
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

### Audit

The audit table is append-only against the application: no path of courier updates or deletes an entry, and a trigger in
both stores refuses it. The single exception is the retention purge.

Set `COURIER_AUDIT_RETENTION` (chart: `auditRetention`) to a duration of at least 24h, such as `8760h` (a year), and entries
older than it are purged, hourly, in batches of 1000 (at most a million entries in one run; a log far past its retention is
worked down over several runs). The default is `keep`: nothing is purged until an operator sets it. The purge writes nothing
to the audit; it logs a count at INFO and counts in `courier_retention_removed_total` (`audit_entries`; settled interactions
that are cleared count as `cleared_interactions`).

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

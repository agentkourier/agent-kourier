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

The request is stored through `redact`: the agent's hint and each question's text pass through `redact.FreeText`, a tool's
name likewise, and a tool's arguments through the redaction that the Slack status line uses (`render.RedactArgs`: values
under a secret name, `name`/`value` pairs, tokens, URL credentials, private keys), so that what is stored is never more than
what is shown. The agent still receives what it always did: the answer is built from request and tool IDs and the chosen
choice, none of which changes.

**Stored and logged text is normalized.** Redaction returns normalized text, not the original with only the secret spans
replaced: before it matches anything, `internal/redact` deletes the invisible characters (control characters except line
breaks and tab, format characters such as the zero-width space and the bidirectional overrides, private-use and surrogate
code points, invalid UTF-8, the blank fillers U+3164, U+2800, U+115F, U+1160 and U+FFA0, the combining grapheme joiner
U+034F, the Khmer inherent vowels U+17B4 and U+17B5, every variation selector (U+180B to U+180D, U+180F, U+FE00 to U+FE0F,
U+E0100 to U+E01EF), and the reserved code points that Unicode's Default_Ignorable_Code_Point property keeps invisible for
characters yet to come: U+2065, U+FFF0 to U+FFF8, U+E0000 to U+E0FFF) and writes every kind of line break (CR, CRLF, NEL,
U+2028, U+2029) as `\n`. So no such character
can split a secret's name past the redaction, and none is stored, logged or sent to the agent in the alert and thread
blocks. What a reader sees stays, an emoji with its glyph and without its presentation selector. There are two shapes:
**free text** (a pause's hint and questions, an error in a log line, an alert field, and what a thread reply says) keeps its
line breaks and is cut at 4 KiB at a white space; an **identifier** (an audit field, a Binding's name, the
name that the chat platform's user cache keeps) is one line, every run of white space a single space, and is read for tokens
only. A person's display name is not identifier text where it reaches the agent: it is theirs to choose and may say anything,
so the thread block cleans it again as free text and shows it in an element of its own (`<from>`), apart from the platform's
user ID (`<id>`), which is what says who wrote a reply. The alert fields and the text of a thread reply that reach the agent are
the exception to the 4 KiB cut: a field with no white space in it (a Japanese body, a long Grafana link) is read up to 12000
bytes (4000 for a reply), cut at a character and marked, so its first 3000 UTF-16 units (1000 for a reply) reach the agent,
redacted, instead of the whole field being replaced by "… (truncated)" (the last run of a cut
field, back to its last white space, is left out, so that no start of a secret the cut went through is shown; and the text
is read as it came only up to four times that window, so the work is bounded whatever is sent); the footer is an identifier,
so its `name: value` pairs (`courier-key: ns/alertname`) stay and only its tokens are taken out. A token or credential is
matched both in the text as it came and in the normalized text, so that a word or an invisible character that
normalization closes up cannot hide one. The same set of characters is deleted from the text that is shown in
Slack (`chat.Clean`), so display and redaction cannot disagree on it.

Besides a secret's name, free text is redacted by what stands around a value. A space of any width (a no-break space, a thin
or an ideographic space) is white space to the rules, before and after a `=` or `:`, after a `--flag`, at the end of a value
and at the end of a URL's user and password. A value that is itself a key is not the value: in `authorization:\n  credentials: x`
(the Prometheus and Alertmanager `http_config`), `basic_auth:\n  password: x`, `redis:\n  auth:\n    password: x`,
`apiKey:\n  value: x` and `auth: {password: x}` the secret is the value of the nested key. Only what has a key's shape is a
key: a quoted name closed by its quote before the `:` (with white space allowed between them, anywhere), or a name and `: ` with white
space after the colon (and, for a key that begins its own line under the name, white space before it: `password : x`);
`password: Password123` and a line of `=======` under it, `key=x`, `secret:hunter2` and `"Session: x"` are values. A line
under a secret name that begins with a secret pair of another shape (`password : x`, `token = x`, `$env:APIKEY = x`) is
taken whole with the name above it, and so is a compact JSON object on the line under it (`secret:\n  {"db":"x"}`). A `Bearer`
before a secret-named key and its colon (`type: Bearer` and `credentials:` under it, `bearer token: x`) takes no
credential: the key has its own value. Markdown emphasis around a name does not hide it (`**Password:** x`,
`**Password**: x`, `*Token:* x`, `- **API key**: x`). A value does not begin after a blank line unless it is indented under
its name or its line does not read as prose, a plain word first and more words after it (`Password:`, a blank line and
`hunter2x` or `hunter2x (rotated)` is a secret; `Token:`, a blank line and `Please rotate it` is prose). A Kubernetes env entry whose `name:`
is a secret name loses its `value:` when it follows later in the entry, up to eight lines on (comments, blank lines and other
keys among them), or after a comma, as in JSON, also when the JSON is itself in a string; a line that begins another entry
(any line that begins with `-`, `- name:` and `---` among them) or an object (a line that begins with `{` or `}`) ends the
match, so one entry's name does not take the next entry's value, and a name
that is no secret does not hide a later one that is. A YAML block scalar (`|`, `|-`, `>`, `>-` and the other
indicators, with a tag or an anchor before the indicator or without) under a secret name loses all its lines that are indented
more than its key, and a tag or an anchor before a plain or quoted value goes with it (`password: &pw x`, `!!str "x y"`, and
`password: !important note` loses "note" too); a value that follows a trailing continuation mark (`mysql --password \`, PowerShell's backtick, cmd's
caret) is the first word of the next line. A token that has a prefix of its own is redacted with no name before it (and by
identifier text too), in the shape of the real token: GitHub's `ghp_`, `gho_`, `ghu_`, `ghs_` and `ghr_` and their 36 letters
and digits, the fine-grained `github_pat_` with its 22, an underscore and 59, GitLab's `glpat-`, the `sk-` keys of OpenAI and
Anthropic (`sk-proj-`, `sk-svcacct-`, `sk-admin-` and `sk-ant-` from 20 characters with a digit or a capital among them; a bare `sk-`
from 20 characters with a capital, or with a digit and a run of 20 lower-case letters and digits, so a Title-Case slug is taken
too and `sk-learn-tutorial-for-beginners-2024` is not), Google's `AIza` (35 characters), Stripe's
`sk_live_`, `rk_live_` and their `_test_` kind (24 letters and digits), and the `sig=` value of an Azure SAS URL. So "sk-learn",
"AIzawhatever", a pod called `sk-payments-api-7c9d8f6b5-x2k4p`, a namespace called `sk-platform-observability-prod` and
`network_test_connectivityToService` stay. Text that is shown on one line (an audit row's free text, an alert's title and
link, a thread author's name and ID) is read twice, with its lines and then joined, so that a flag, a user or a URL's password that
a line break cuts from its value is taken as it would be on one line.

What the free-text rules do not catch, and what they take that is no secret, is deliberate and pinned by tests
(`TestFreeTextKnownLimits`). The rules read a value as a run up to white space, so a few shapes leave it: a list
(`password:\n- x`) or a mapping of keys that are no secret names (`secret:\n  db: x`) under a secret name, a fenced block after
a name (`password:\n\n` and three backticks: the fence is the value), Terraform's separate `name = "auth.password"` and
`value = "x"` lines, a tag with its value on the next line (`password: !!str\n  x`, left by the one-line entries too), a plain
scalar that goes on over several lines, a short flag (`-p x`) and a name with no separator (`passwd x`). A secret written where
a nested key would be, on the line under a secret name (`token:\n  SECRET : x`, `Password:\n    'SECRET': rotated`), is shown
as the key it looks like by the entries that keep lines (the one-line entries take it). After a blank line a value that
begins with a plain word and goes on in words (`password:\n\nletmein now`) is read as prose and left by the entries that keep
lines (the one-line entries take the word); a one-word paragraph (`Token:\n\nExpired`) is taken in every entry
(`TestAOneWordParagraphAfterABlankLineIsTaken`). A Markdown legend of a secret word (`**Key**: blue means prod`,
`**Session**: 42 minutes`) is taken as a name and its value, an over-redaction (`TestAMarkdownLegendOfASecretWordIsTaken`). A URL's password that a line break cuts (`https://admin:x\npassword=y@host`) is taken only if the line below does not
take its `@` first, because the lines are read before they are joined. And text that reads as a name and a value only once its
lines are joined is taken in the one-line entries: `Token:`, a blank line and `Please rotate it` shows
`Token: [redacted] rotate it`, and `Retry with --token`, a line break and `The deploy` loses `The`; the entries that keep lines
tell the name from the text.

In the alert and thread blocks, "&", "<" and ">" are written `&amp;`, `&lt;` and `&gt;`, and so is every character that a
reader may take for an angle bracket: the full-width and small forms, the single and double guillemets, the mathematical,
CJK, U+2329 and ornament angle brackets, the vertical, curved and dotted forms (U+2991, U+2992), the Canadian syllabics PA and
PO, the arrowhead modifier letters (high and low), the arc brackets, the circled and the nested (double and triple) less-than and
greater-than, much-less and much-greater (and very much), less-than with a dot, precedes and succeeds, and the white triangles. Ordinary text that holds one
reaches the agent escaped as well: French quotation marks (« bonjour ») arrive as `&lt; bonjour &gt;`. The filled triangles
(◀ ▶), which interface text uses as arrows, are left as they are, and so is the Runic letter ᚲ, a letter with no right-hand
partner.

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

### Thread replies in a mention-only Binding

With `chat.threadReplies: mention` (spec 8.2, D38), a reply in a thread that does not mention courier is not a turn. It is
not stored: no reply row, no reaction, no change to the session's last activity. Slack delivers every reply's event, and courier
drops an unmentioned one at intake; two counters, which hold no content, are all that remains of it
(`courier_thread_unmentioned_total` and `courier_intake_total{disposition="unmentioned"}`). Courier reads the text again from
Slack only when someone later mentions courier in the thread, and then only the replies since the session's previous turn.
They are cleaned, redacted and cut (spec 8.2), and go to the agent, and so to its model provider, as part of that turn's text.
Before that they reach neither the agent nor its provider (with `all`, every reply does). Nothing new is stored: the section is built when the turn is
sent and held in memory while that turn is the head of its session's queue, `reply_queue.text` holds only what the person who
mentioned courier wrote, and no reply's content is logged. A typed answer to a question that takes text replies needs no
mention: it is queued pinned to the question and kept as its answer (`interactions.answer`, above).

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

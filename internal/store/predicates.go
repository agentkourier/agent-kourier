package store

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// The policy predicates of the store, written once. They say which rows are pending, which are settled, what holds a
// session back from deletion and when a session is past its horizon; each backend renders them through its Dialect
// and keeps only the shape of its statements. A predicate that names a table takes the alias the statement gives it (a
// Ref), or "" where the statement has no alias.

// PendingOutbox is the outbox rows that are neither posted nor abandoned: those that may still be delivered. They are
// the backlog (courier_outbox_pending), what the sender picks up, and, for the session they belong to, what holds it
// back from deletion.
func PendingOutbox(o Ref) Fixed {
	return func(d Dialect) string {
		return o.col("message_ref") + " = '' AND " + d.IsUnset(o.col("abandoned_at"))
	}
}

// SettledOutbox is the outbox rows that are done with: posted, or given up on. A row that is neither may still be
// delivered, and is not the sweep's to delete (it holds its session back). It is the complement of PendingOutbox.
func SettledOutbox(o Ref) Fixed {
	return func(d Dialect) string {
		return "(" + o.col("message_ref") + " <> '' OR " + d.IsSet(o.col("abandoned_at")) + ")"
	}
}

// UndeliveredReplies is the replies of the reply queue that were never delivered: the backlog
// (courier_reply_queue_depth), and, for the session they belong to, what holds it back from deletion.
func UndeliveredReplies(r Ref) Fixed {
	return func(Dialect) string { return r.col("delivered_at") + " IS NULL" }
}

// DeliveredReplies is the replies that were delivered, the tombstones that are all the queue keeps of them. It is the
// complement of UndeliveredReplies.
func DeliveredReplies(r Ref) Fixed {
	return func(Dialect) string { return r.col("delivered_at") + " IS NOT NULL" }
}

// PendingInteractions is the interactions that await a decision (courier_interactions_pending), and, for the session
// they belong to, what holds it back from deletion.
func PendingInteractions(i Ref) Fixed {
	return func(Dialect) string { return i.col("state") + " = " + quote(InteractionPending) }
}

// DecidedInteractions is the interactions that were decided, by an answer, a timeout or a cancel. It is the complement
// of PendingInteractions.
func DecidedInteractions(i Ref) Fixed {
	return func(Dialect) string { return i.col("state") + " <> " + quote(InteractionPending) }
}

// ClearableInteractions is the decided interactions whose request and answer are no longer wanted, and have not been
// cleared yet (a cleared row has empty blobs, so the test of them keeps a second sweep from counting it again): those
// decided at least grace ago, and an answer that was never delivered only once undeliveredGrace has passed. The
// interactions table is not aliased.
func ClearableInteractions(now time.Time, grace, undeliveredGrace time.Duration) Cond {
	return condFunc(func(b *Binder) string {
		decided := b.d.IsSet("decided_at")
		blobs := "(" + b.d.NonEmpty("request") + " OR " + b.d.NonEmpty("answer") + ")"
		decidedBy := "decided_at <= " + b.Time(now.Add(-grace))
		answered := "(state <> " + quote(InteractionAnswered) + " OR " + b.d.IsSet("delivered_at") + " OR decided_at <= " + b.Time(now.Add(-undeliveredGrace)) + ")"
		return DecidedInteractions("").Text(b.d) + " AND " + decided + " AND " + blobs + " AND " + decidedBy + " AND " + answered
	})
}

// OutboxDerivedFromReplyRow is the outbox rows that were made for a reply, in a statement where the reply is the row r
// of the reply queue (or of a table with its id). Such a row ties to a reply that has no thread of its own: it has the
// reply's ID, a "/" and a suffix as its source (event_id), as the queued reaction of a turn does ("<reply ID>/queued",
// session.queuedID) and the runner's notices ("<reply ID>/<kind>", Manager.notice). Such a row has no thread, so this is the
// only way to find it. Its source is in the range from id + "/" up to, and not including, id + "0", the character after
// "/", which is a range of the outbox_event index (migration 0022): the range holds in the order of the bytes, which the
// dialect asks for (a locale's order ignores punctuation). The range deliberately excludes the bare ID: a row sourced by
// it is a render post made for the thread, which the (channel, thread) delete finds.
func OutboxDerivedFromReplyRow(o, r Ref) Fixed {
	return func(d Dialect) string {
		event, id := d.Bytewise(o.col("event_id")), r.col("id")
		return event + " >= " + id + " || '/' AND " + event + " < " + id + " || '0'"
	}
}

// OutboxDerivedFromReplyID is OutboxDerivedFromReplyRow for the reply whose ID is id, a value of the statement.
func OutboxDerivedFromReplyID(o Ref, id string) Cond {
	return condFunc(func(b *Binder) string {
		event := b.d.Bytewise(o.col("event_id"))
		return event + " >= " + b.bind(ArgText, id+"/") + " AND " + event + " < " + b.bind(ArgText, id+"0")
	})
}

// NothingHoldsBack is the sessions s that may be deleted for what they own, whatever their age: no task active or
// paused, no interaction pending, no reply that was never delivered, no outbox row pending (made for the thread, or
// derived from one of the session's replies). A session that is held back is deleted by the first sweep after what
// held it is resolved.
func NothingHoldsBack(s Ref) Fixed {
	return func(d Dialect) string {
		thread := func(t Ref) string {
			return t.col("connection") + " = " + s.col("connection") + " AND " + t.col("channel") + " = " + s.col("channel") +
				" AND " + t.col("thread_ts") + " = " + s.col("thread_ts")
		}
		return strings.Join([]string{
			s.col("task_id") + " = ''",
			"NOT EXISTS (SELECT 1 FROM interactions i WHERE " + thread("i") + " AND " + PendingInteractions("i").Text(d) + ")",
			"NOT EXISTS (SELECT 1 FROM reply_queue r WHERE " + thread("r") + " AND " + UndeliveredReplies("r").Text(d) + ")",
			"NOT EXISTS (SELECT 1 FROM outbox o WHERE o.channel = " + s.col("channel") + " AND o.thread_ts = " + s.col("thread_ts") +
				" AND " + PendingOutbox("o").Text(d) + ")",
			"NOT EXISTS (SELECT 1 FROM reply_queue r JOIN outbox o ON " + OutboxDerivedFromReplyRow("o", "r").Text(d) +
				" WHERE " + thread("r") + " AND " + PendingOutbox("o").Text(d) + ")",
		}, " AND ")
	}
}

// PastHorizon is the sessions s whose last activity is before their horizon (see SessionHorizons): the one of the
// session's Binding, or Otherwise where it has none. The first part is the latest of all the horizons, which the
// sessions' index answers; the second is the session's own.
func PastHorizon(s Ref, h SessionHorizons) Cond {
	return condFunc(func(b *Binder) string {
		latest := h.Otherwise
		for _, at := range h.ByBinding {
			if at.After(latest) {
				latest = at
			}
		}
		first := b.bind(ArgTime, b.d.TimeArg(latest))
		var when strings.Builder
		for _, binding := range slices.Sorted(maps.Keys(h.ByBinding)) {
			name := b.bind(ArgText, binding)
			at := b.bind(ArgTime, b.d.TimeArg(h.ByBinding[binding]))
			when.WriteString(" WHEN " + name + " THEN " + at)
		}
		horizon := b.bind(ArgTime, b.d.TimeArg(h.Otherwise)) // a CASE needs a WHEN, so with no Binding named it is the one horizon
		if when.Len() > 0 {
			horizon = "CASE " + s.col("binding") + when.String() + " ELSE " + horizon + " END"
		}
		return s.col("last_activity") + " < " + first + " AND " + s.col("last_activity") + " < " + horizon
	})
}

// TriggerThreadOpen is the trigger threads t that are open: not closed.
func TriggerThreadOpen(t Ref) Fixed {
	return func(d Dialect) string { return d.IsUnset(t.col("closed_at")) }
}

// TriggerThreadClosed is the trigger threads t that a close message ended. It is the complement of TriggerThreadOpen.
func TriggerThreadClosed(t Ref) Fixed {
	return func(d Dialect) string { return d.IsSet(t.col("closed_at")) }
}

// TriggerThreadUnstarted is the trigger threads t whose turn has not started: no cooldown runs (MarkTriggerStarted).
func TriggerThreadUnstarted(t Ref) Fixed {
	return func(d Dialect) string { return d.IsUnset(t.col("cooldown_until")) }
}

// TriggerThreadReplaceable is the rule of OpenTriggerThread for replacing the row of a key in an upsert, where the row
// there is the table's own (named trigger_threads) and the row that would replace it is "excluded". A row is replaced
// if its thread is closed and its cooldown, if one runs, ended by the new thread's opening; or if it is an unstarted
// reservation that is another message's, opened before stale.Unstarted; or if it is an open thread whose turn started,
// last seen before stale.Idle. A cutoff that is not in effect (see Binder.Cutoff) replaces none.
func TriggerThreadReplaceable(stale TriggerStale) Cond {
	return condFunc(func(b *Binder) string {
		const row, next = Ref("trigger_threads"), Ref("excluded")
		closedPast := "(" + TriggerThreadClosed(row).Text(b.d) + " AND (" + TriggerThreadUnstarted(row).Text(b.d) +
			" OR " + row.col("cooldown_until") + " <= " + next.col("opened") + "))"
		unstarted := "(" + TriggerThreadUnstarted(row).Text(b.d) + " AND " + row.col("thread") + " <> " + next.col("thread") +
			" AND " + row.col("opened") + " < " + b.Cutoff(stale.Unstarted) + ")"
		idle := "(" + TriggerThreadOpen(row).Text(b.d) + " AND " + b.d.IsSet(row.col("cooldown_until")) +
			" AND " + row.col("last_seen") + " < " + b.Cutoff(stale.Idle) + ")"
		return "(" + closedPast + " OR " + unstarted + " OR " + idle + ")"
	})
}

// TriggerThreadPurgeable is the rows of PurgeTriggerThreads: the closed threads whose cooldown ended before now (one
// that was never set ended), and the open ones last seen before idleBefore, which a fire would take over anyway. An
// idleBefore that is not in effect (see Binder.Cutoff) is before no row.
func TriggerThreadPurgeable(now, idleBefore time.Time) Cond {
	return condFunc(func(b *Binder) string {
		const none = Ref("")
		closedPast := "(" + TriggerThreadClosed(none).Text(b.d) + " AND (" + TriggerThreadUnstarted(none).Text(b.d) +
			" OR cooldown_until < " + b.Time(now) + "))"
		idle := "(" + TriggerThreadOpen(none).Text(b.d) + " AND last_seen < " + b.Cutoff(idleBefore) + ")"
		return "(" + closedPast + " OR " + idle + ")"
	})
}

// quote is the SQL text of the interaction state s.
func quote(s InteractionState) string { return "'" + string(s) + "'" }

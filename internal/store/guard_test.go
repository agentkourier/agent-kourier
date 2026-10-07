package store_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// call is one call of the store with the arguments baked in, for a table of them.
type call func(ctx context.Context, s store.Store) error

// spec is one method of store.Store: a call that is legal, and the calls that are not, each named for the one rule it
// breaks. A refused call must return ErrInvalid and must never reach the adapter.
type spec struct {
	method string
	valid  call
	bad    map[string]call
}

var (
	t0   = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	key  = store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "1.1"}
	epoc = time.Unix(0, 0)

	horizons = store.SessionHorizons{Otherwise: t0, ByBinding: map[string]time.Time{"ns/b": t0}}

	okEvent       = store.Event{ID: "e1", ReceivedAt: t0}
	okOutbox      = store.OutboxEntry{ID: "o1", EventID: "e1", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0}
	okSession     = store.Session{Key: key, Binding: "ns/b", LastActivity: t0}
	okInteraction = store.Interaction{ID: "i1", Key: key, TaskID: "t1", Kind: store.InteractionAskUser, CreatedAt: t0, Deadline: t0.Add(time.Hour)}
	okPress       = store.Press{InteractionID: "i1", User: "U1", Channel: "C1", MessageRef: "1.2", PressedAt: t0}
	okReply       = store.Reply{ID: "r1", Key: key, UserID: "U1", MessageRef: "1.2", EnqueuedAt: t0}
	okRun         = store.TriggerRun{Binding: "ns/b", Trigger: 0, Key: key, At: t0}
	okThread      = store.TriggerThread{Binding: "ns/b", KeyHash: "h", Thread: "1.1", Opened: t0}
	okAudit       = store.AuditEntry{Kind: store.AuditEvent, At: t0, Subject: "e1", Outcome: "accepted"}
	okVote        = store.FeedbackVote{Key: key, Turn: "turn1", User: "U1", Binding: "ns/b", Agent: "a", Kind: store.FeedbackAnswer, Verdict: store.VerdictUseful, At: t0}
	okLimits      = store.PressLimits{Total: 10, PerUser: 3}
	okRunLimits   = store.TriggerRunLimits{DigestWindow: time.Hour}

	// badKeys are the ways a conversation key can be incomplete.
	badKeys = map[string]store.ConversationKey{
		"no connection": {Channel: "C1", Thread: "1.1"},
		"no channel":    {Connection: "slack", Thread: "1.1"},
		"no thread":     {Connection: "slack", Channel: "C1"},
	}
	// badHorizons are the ways a set of horizons can be incomplete.
	badHorizons = map[string]store.SessionHorizons{
		"no default horizon": {ByBinding: map[string]time.Time{"ns/b": t0}},
		"a zero horizon":     {Otherwise: t0, ByBinding: map[string]time.Time{"ns/b": {}}},
	}
	// badLimits are the batch sizes that are refused: none, and more than MaxLimit.
	badLimits = map[string]int{"limit 0": 0, "limit below 0": -1, "limit above MaxLimit": store.MaxLimit + 1}
)

// mut returns v with f applied to a copy.
func mut[T any](v T, f func(*T)) T {
	if f != nil {
		f(&v)
	}
	return v
}

// withKeys returns the calls that each use one incomplete key.
func withKeys(f func(store.ConversationKey) call) map[string]call {
	m := map[string]call{}
	for name, k := range badKeys {
		m[name] = f(k)
	}
	return m
}

// with merges calls into one map.
func with(maps ...map[string]call) map[string]call {
	out := map[string]call{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func appendEvent(f func(*store.Event)) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.AppendEvent(ctx, mut(okEvent, f))
		return err
	}
}

func setEventStatus(status store.EventStatus) call {
	return func(ctx context.Context, s store.Store) error { return s.SetEventStatus(ctx, "e1", status, "") }
}

func claimEvent(now time.Time, lease time.Duration) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.ClaimEvent(ctx, now, lease); return err }
}

func completeEvent(id string, attempt int) call {
	return func(ctx context.Context, s store.Store) error { return s.CompleteEvent(ctx, id, attempt) }
}

func failEvent(id string, attempt int, retryAt time.Time, maxAttempts int) call {
	return func(ctx context.Context, s store.Store) error {
		return s.FailEvent(ctx, id, attempt, retryAt, maxAttempts, "boom")
	}
}

func createOutbox(f func(*store.OutboxEntry)) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.CreateOutbox(ctx, mut(okOutbox, f))
		return err
	}
}

func recordMessageRef(id, ref string) call {
	return func(ctx context.Context, s store.Store) error { return s.RecordMessageRef(ctx, id, ref) }
}

func abandonOutbox(at time.Time) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.AbandonOutbox(ctx, "o1", at); return err }
}

func abandonOutboxBefore(before, at time.Time) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.AbandonOutboxBefore(ctx, before, at)
		return err
	}
}

func upsertSession(f func(*store.Session)) call {
	return func(ctx context.Context, s store.Store) error { return s.UpsertSession(ctx, mut(okSession, f)) }
}

func getSession(k store.ConversationKey, ttl time.Duration, now time.Time) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.GetSession(ctx, k, ttl, now); return err }
}

func touchSession(k store.ConversationKey, at time.Time) call {
	return func(ctx context.Context, s store.Store) error { return s.TouchSession(ctx, k, at) }
}

func setSessionTask(k store.ConversationKey) call {
	return func(ctx context.Context, s store.Store) error {
		return s.SetSessionTask(ctx, k, "ctx1", "t1", store.TaskTurn{})
	}
}

func resetSession(k store.ConversationKey) call {
	return func(ctx context.Context, s store.Store) error { return s.ResetSession(ctx, k) }
}

func purgeExpiredSessions(h store.SessionHorizons, limit int) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.PurgeExpiredSessions(ctx, h, limit)
		return err
	}
}

func listStaleTasks(h store.SessionHorizons, limit int) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.ListStaleTasks(ctx, h, limit); return err }
}

func endStaleTask(h store.SessionHorizons, t store.StaleTask, now time.Time) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.EndStaleTask(ctx, h, t, now); return err }
}

func createInteraction(f func(*store.Interaction)) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.CreateInteraction(ctx, mut(okInteraction, f))
		return err
	}
}

func pendingTextQuestion(k store.ConversationKey) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.PendingTextQuestion(ctx, k); return err }
}

func listDueInteractions(now time.Time) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.ListDueInteractions(ctx, now); return err }
}

func listDecidedInteractions(since time.Time) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.ListDecidedInteractions(ctx, since)
		return err
	}
}

func markInteractionDelivered(id string, at time.Time) call {
	return func(ctx context.Context, s store.Store) error { return s.MarkInteractionDelivered(ctx, id, at) }
}

func submitInteraction(id string, answer []byte, decider string, now time.Time) call {
	return func(ctx context.Context, s store.Store) error {
		return s.SubmitInteraction(ctx, id, answer, decider, now)
	}
}

func resolveInteraction(id string, state store.InteractionState, now time.Time) call {
	return func(ctx context.Context, s store.Store) error { return s.ResolveInteraction(ctx, id, state, now) }
}

func setInteractionMessageRef(id, ref string) call {
	return func(ctx context.Context, s store.Store) error { return s.SetInteractionMessageRef(ctx, id, ref) }
}

func clearSettledInteractions(now time.Time, grace, undelivered time.Duration) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.ClearSettledInteractions(ctx, now, grace, undelivered)
		return err
	}
}

func recordPress(f func(*store.Press), limits store.PressLimits) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.RecordPress(ctx, mut(okPress, f), limits)
		return err
	}
}

func pendingPresses(id string, before time.Time) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.PendingPresses(ctx, id, before)
		return err
	}
}

func purgePresses(now time.Time, grace time.Duration) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.PurgePresses(ctx, now, grace); return err }
}

func enqueueReply(f func(*store.Reply)) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.EnqueueReply(ctx, mut(okReply, f))
		return err
	}
}

func hasReply(id string) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.HasReply(ctx, id); return err }
}

func nextReply(k store.ConversationKey) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.NextReply(ctx, k); return err }
}

func nextReplyAfter(k store.ConversationKey, after int64) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.NextReplyAfter(ctx, k, after); return err }
}

func previousReply(k store.ConversationKey, sequence int64) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.PreviousReply(ctx, k, sequence)
		return err
	}
}

func markReplyDelivered(id string, at time.Time) call {
	return func(ctx context.Context, s store.Store) error { return s.MarkReplyDelivered(ctx, id, at) }
}

func abandonStrandedReplies(now time.Time, age time.Duration) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.AbandonStrandedReplies(ctx, now, age)
		return err
	}
}

func admitTriggerRun(f func(*store.TriggerRun), limits store.TriggerRunLimits) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.AdmitTriggerRun(ctx, mut(okRun, f), limits)
		return err
	}
}

func listTriggerDigests(now time.Time) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.ListTriggerDigests(ctx, now); return err }
}

func markTriggerDigested(binding string, trigger int, start time.Time) call {
	return func(ctx context.Context, s store.Store) error {
		return s.MarkTriggerDigested(ctx, binding, trigger, start)
	}
}

func purgeTriggerRuns(before time.Time) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.PurgeTriggerRuns(ctx, before); return err }
}

func openTriggerThread(f func(*store.TriggerThread)) call {
	return func(ctx context.Context, s store.Store) error {
		_, _, err := s.OpenTriggerThread(ctx, mut(okThread, f), store.TriggerStale{})
		return err
	}
}

func markTriggerStarted(binding, keyHash, thread string, cooldownUntil time.Time) call {
	return func(ctx context.Context, s store.Store) error {
		return s.MarkTriggerStarted(ctx, binding, keyHash, thread, cooldownUntil)
	}
}

func releaseTriggerThread(binding, keyHash, thread string) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.ReleaseTriggerThread(ctx, binding, keyHash, thread)
		return err
	}
}

func getTriggerThread(binding, keyHash string) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.GetTriggerThread(ctx, binding, keyHash)
		return err
	}
}

func reopenTriggerThread(binding, keyHash string, at time.Time) call {
	return func(ctx context.Context, s store.Store) error {
		return s.ReopenTriggerThread(ctx, binding, keyHash, at)
	}
}

func closeTriggerThread(binding, keyHash string, at time.Time) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.CloseTriggerThread(ctx, binding, keyHash, at)
		return err
	}
}

func purgeTriggerThreads(now, idleBefore time.Time) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.PurgeTriggerThreads(ctx, now, idleBefore)
		return err
	}
}

func appendAudit(f func(*store.AuditEntry)) call {
	return func(ctx context.Context, s store.Store) error { return s.AppendAudit(ctx, mut(okAudit, f)) }
}

func listAudit(limit int) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.ListAudit(ctx, time.Time{}, 0, limit)
		return err
	}
}

func purgeAudit(before time.Time, limit int) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.PurgeAudit(ctx, before, limit); return err }
}

func recordVote(f func(*store.FeedbackVote)) call {
	return func(ctx context.Context, s store.Store) error {
		_, err := s.RecordVote(ctx, mut(okVote, f))
		return err
	}
}

func listVotes(k store.ConversationKey) call {
	return func(ctx context.Context, s store.Store) error { _, err := s.ListVotes(ctx, k); return err }
}

// anyOf calls f for each limit rule, so that a limit check is tried on every method that has one.
func limitCalls(f func(limit int) call) map[string]call {
	m := map[string]call{}
	for name, n := range badLimits {
		m[name] = f(n)
	}
	return m
}

func specs() []spec {
	return []spec{
		{"AppendEvent", appendEvent(nil), map[string]call{
			"no ID":         appendEvent(func(e *store.Event) { e.ID = "" }),
			"no ReceivedAt": appendEvent(func(e *store.Event) { e.ReceivedAt = time.Time{} }),
		}},
		{"GetEvent", func(ctx context.Context, s store.Store) error { _, err := s.GetEvent(ctx, "e1"); return err }, nil},
		{"SetEventStatus", setEventStatus(store.EventDone), map[string]call{"undefined status": setEventStatus("bogus")}},
		{"ClaimEvent", claimEvent(t0, time.Minute), map[string]call{
			"no now":        claimEvent(time.Time{}, time.Minute),
			"lease 0":       claimEvent(t0, 0),
			"lease below 0": claimEvent(t0, -time.Minute),
		}},
		{"CompleteEvent", completeEvent("e1", 1), map[string]call{
			"no ID":     completeEvent("", 1),
			"attempt 0": completeEvent("e1", 0),
		}},
		{"FailEvent", failEvent("e1", 1, t0, 3), map[string]call{
			"no ID":         failEvent("", 1, t0, 3),
			"attempt 0":     failEvent("e1", 0, t0, 3),
			"no retryAt":    failEvent("e1", 1, time.Time{}, 3),
			"maxAttempts 0": failEvent("e1", 1, t0, 0),
		}},

		{"CreateOutbox", createOutbox(nil), map[string]call{
			"no ID":          createOutbox(func(o *store.OutboxEntry) { o.ID = "" }),
			"no EventID":     createOutbox(func(o *store.OutboxEntry) { o.EventID = "" }),
			"no Channel":     createOutbox(func(o *store.OutboxEntry) { o.Channel = "" }),
			"no CreatedAt":   createOutbox(func(o *store.OutboxEntry) { o.CreatedAt = time.Time{} }),
			"undefined Role": createOutbox(func(o *store.OutboxEntry) { o.Role = "bogus" }),
		}},
		{"RecordMessageRef", recordMessageRef("o1", "1.2"), map[string]call{
			"no ID":  recordMessageRef("", "1.2"),
			"no ref": recordMessageRef("o1", ""),
		}},
		{"GetOutbox", func(ctx context.Context, s store.Store) error { _, err := s.GetOutbox(ctx, "o1"); return err }, nil},
		{"AbandonOutbox", abandonOutbox(t0), map[string]call{
			"zero time":    abandonOutbox(time.Time{}),
			"epoch":        abandonOutbox(epoc),
			"before epoch": abandonOutbox(epoc.Add(-time.Hour)),
		}},
		{"AbandonOutboxBefore", abandonOutboxBefore(t0, t0), map[string]call{
			"before is zero":  abandonOutboxBefore(time.Time{}, t0),
			"before is epoch": abandonOutboxBefore(epoc, t0),
			"at is zero":      abandonOutboxBefore(t0, time.Time{}),
			"at is epoch":     abandonOutboxBefore(t0, epoc),
		}},
		{"ListUnposted", func(ctx context.Context, s store.Store) error { _, err := s.ListUnposted(ctx); return err }, nil},

		{"UpsertSession", upsertSession(nil), with(
			withKeys(func(k store.ConversationKey) call { return upsertSession(func(s *store.Session) { s.Key = k }) }),
			map[string]call{
				"no Binding":      upsertSession(func(s *store.Session) { s.Binding = "" }),
				"no LastActivity": upsertSession(func(s *store.Session) { s.LastActivity = time.Time{} }),
			})},
		{"GetSession", getSession(key, time.Hour, t0), with(
			withKeys(func(k store.ConversationKey) call { return getSession(k, time.Hour, t0) }),
			map[string]call{
				"ttl 0":  getSession(key, 0, t0),
				"no now": getSession(key, time.Hour, time.Time{}),
			})},
		{"TouchSession", touchSession(key, t0), with(
			withKeys(func(k store.ConversationKey) call { return touchSession(k, t0) }),
			map[string]call{"no time": touchSession(key, time.Time{})})},
		{"SetSessionTask", setSessionTask(key), withKeys(setSessionTask)},
		{"ResetSession", resetSession(key), withKeys(resetSession)},
		{"ListActiveSessions", func(ctx context.Context, s store.Store) error { _, err := s.ListActiveSessions(ctx); return err }, nil},
		{"ListQueuedSessions", func(ctx context.Context, s store.Store) error { _, err := s.ListQueuedSessions(ctx); return err }, nil},
		{"PurgeExpiredSessions", purgeExpiredSessions(horizons, 10), with(
			limitCalls(func(n int) call { return purgeExpiredSessions(horizons, n) }),
			horizonCalls(func(h store.SessionHorizons) call { return purgeExpiredSessions(h, 10) }))},
		{"ListStaleTasks", listStaleTasks(horizons, 10), with(
			limitCalls(func(n int) call { return listStaleTasks(horizons, n) }),
			horizonCalls(func(h store.SessionHorizons) call { return listStaleTasks(h, 10) }))},
		{"EndStaleTask", endStaleTask(horizons, store.StaleTask{Key: key, TaskID: "t1"}, t0), with(
			horizonCalls(func(h store.SessionHorizons) call {
				return endStaleTask(h, store.StaleTask{Key: key, TaskID: "t1"}, t0)
			}),
			withKeys(func(k store.ConversationKey) call {
				return endStaleTask(horizons, store.StaleTask{Key: k, TaskID: "t1"}, t0)
			}),
			map[string]call{
				"no TaskID": endStaleTask(horizons, store.StaleTask{Key: key}, t0),
				"no now":    endStaleTask(horizons, store.StaleTask{Key: key, TaskID: "t1"}, time.Time{}),
			})},

		{"CreateInteraction", createInteraction(nil), with(
			withKeys(func(k store.ConversationKey) call { return createInteraction(func(i *store.Interaction) { i.Key = k }) }),
			map[string]call{
				"no ID":              createInteraction(func(i *store.Interaction) { i.ID = "" }),
				"no TaskID":          createInteraction(func(i *store.Interaction) { i.TaskID = "" }),
				"undefined Kind":     createInteraction(func(i *store.Interaction) { i.Kind = "bogus" }),
				"no CreatedAt":       createInteraction(func(i *store.Interaction) { i.CreatedAt = time.Time{} }),
				"Deadline not after": createInteraction(func(i *store.Interaction) { i.Deadline = i.CreatedAt }),
			})},
		{"GetInteraction", func(ctx context.Context, s store.Store) error { _, err := s.GetInteraction(ctx, "i1"); return err }, nil},
		{"PendingTextQuestion", pendingTextQuestion(key), withKeys(pendingTextQuestion)},
		{"ListDueInteractions", listDueInteractions(t0), map[string]call{"no now": listDueInteractions(time.Time{})}},
		{"ListDecidedInteractions", listDecidedInteractions(t0), map[string]call{"no since": listDecidedInteractions(time.Time{})}},
		{"MarkInteractionDelivered", markInteractionDelivered("i1", t0), map[string]call{
			"no ID":   markInteractionDelivered("", t0),
			"no time": markInteractionDelivered("i1", time.Time{}),
		}},
		{"SubmitInteraction", submitInteraction("i1", []byte("a"), "U1", t0), map[string]call{
			"no ID":      submitInteraction("", []byte("a"), "U1", t0),
			"no answer":  submitInteraction("i1", nil, "U1", t0),
			"no decider": submitInteraction("i1", []byte("a"), "", t0),
			"no now":     submitInteraction("i1", []byte("a"), "U1", time.Time{}),
		}},
		{"ResolveInteraction", resolveInteraction("i1", store.InteractionTimedOut, t0), map[string]call{
			"no ID":                        resolveInteraction("", store.InteractionTimedOut, t0),
			"no now":                       resolveInteraction("i1", store.InteractionTimedOut, time.Time{}),
			"answered is not a resolution": resolveInteraction("i1", store.InteractionAnswered, t0),
			"pending is not a resolution":  resolveInteraction("i1", store.InteractionPending, t0),
		}},
		{"SetInteractionMessageRef", setInteractionMessageRef("i1", "1.2"), map[string]call{
			"no ID":  setInteractionMessageRef("", "1.2"),
			"no ref": setInteractionMessageRef("i1", ""),
		}},
		{"ClearSettledInteractions", clearSettledInteractions(t0, time.Hour, 2*time.Hour), map[string]call{
			"no now":                        clearSettledInteractions(time.Time{}, time.Hour, 2*time.Hour),
			"negative grace":                clearSettledInteractions(t0, -time.Second, 2*time.Hour),
			"undelivered grace below grace": clearSettledInteractions(t0, 2*time.Hour, time.Hour),
		}},

		{"RecordPress", recordPress(nil, okLimits), map[string]call{
			"no InteractionID": recordPress(func(p *store.Press) { p.InteractionID = "" }, okLimits),
			"no User":          recordPress(func(p *store.Press) { p.User = "" }, okLimits),
			"no Channel":       recordPress(func(p *store.Press) { p.Channel = "" }, okLimits),
			"no MessageRef":    recordPress(func(p *store.Press) { p.MessageRef = "" }, okLimits),
			"no PressedAt":     recordPress(func(p *store.Press) { p.PressedAt = time.Time{} }, okLimits),
			"Total limit 0":    recordPress(nil, store.PressLimits{Total: 0, PerUser: 3}),
			"PerUser limit 0":  recordPress(nil, store.PressLimits{Total: 10, PerUser: 0}),
		}},
		{"ListPresses", func(ctx context.Context, s store.Store) error { _, err := s.ListPresses(ctx); return err }, nil},
		{"PendingPresses", pendingPresses("i1", t0), map[string]call{
			"no ID":     pendingPresses("", t0),
			"no before": pendingPresses("i1", time.Time{}),
		}},
		{"DeletePress", func(ctx context.Context, s store.Store) error { return s.DeletePress(ctx, 1) }, nil},
		{"PurgePresses", purgePresses(t0, time.Hour), map[string]call{
			"no now":         purgePresses(time.Time{}, time.Hour),
			"negative grace": purgePresses(t0, -time.Second),
		}},

		{"EnqueueReply", enqueueReply(nil), with(
			withKeys(func(k store.ConversationKey) call { return enqueueReply(func(r *store.Reply) { r.Key = k }) }),
			map[string]call{
				"no ID":         enqueueReply(func(r *store.Reply) { r.ID = "" }),
				"no UserID":     enqueueReply(func(r *store.Reply) { r.UserID = "" }),
				"no MessageRef": enqueueReply(func(r *store.Reply) { r.MessageRef = "" }),
				"no EnqueuedAt": enqueueReply(func(r *store.Reply) { r.EnqueuedAt = time.Time{} }),
			})},
		{"HasReply", hasReply("r1"), map[string]call{"no ID": hasReply("")}},
		{"NextReply", nextReply(key), withKeys(nextReply)},
		{"NextReplyAfter", nextReplyAfter(key, 0), with(
			withKeys(func(k store.ConversationKey) call { return nextReplyAfter(k, 0) }),
			map[string]call{"negative sequence": nextReplyAfter(key, -1)})},
		{"PreviousReply", previousReply(key, 1), with(
			withKeys(func(k store.ConversationKey) call { return previousReply(k, 1) }),
			map[string]call{"sequence 0": previousReply(key, 0)})},
		{"MarkReplyDelivered", markReplyDelivered("r1", t0), map[string]call{
			"no ID":   markReplyDelivered("", t0),
			"no time": markReplyDelivered("r1", time.Time{}),
		}},
		{"AbandonStrandedReplies", abandonStrandedReplies(t0, time.Hour), map[string]call{
			"no now": abandonStrandedReplies(time.Time{}, time.Hour),
			"age 0":  abandonStrandedReplies(t0, 0),
		}},

		{"AdmitTriggerRun", admitTriggerRun(nil, okRunLimits), with(
			withKeys(func(k store.ConversationKey) call {
				return admitTriggerRun(func(r *store.TriggerRun) { r.Key = k }, okRunLimits)
			}),
			map[string]call{
				"no Binding":       admitTriggerRun(func(r *store.TriggerRun) { r.Binding = "" }, okRunLimits),
				"negative trigger": admitTriggerRun(func(r *store.TriggerRun) { r.Trigger = -1 }, okRunLimits),
				"At at the epoch":  admitTriggerRun(func(r *store.TriggerRun) { r.At = epoc }, okRunLimits),
				"no At":            admitTriggerRun(func(r *store.TriggerRun) { r.At = time.Time{} }, okRunLimits),
				"no digest window": admitTriggerRun(nil, store.TriggerRunLimits{}),
			})},
		{"ListTriggerDigests", listTriggerDigests(t0), map[string]call{
			"no now":       listTriggerDigests(time.Time{}),
			"now at epoch": listTriggerDigests(epoc),
		}},
		{"MarkTriggerDigested", markTriggerDigested("ns/b", 0, t0), map[string]call{
			"no Binding":       markTriggerDigested("", 0, t0),
			"negative trigger": markTriggerDigested("ns/b", -1, t0),
			"no start":         markTriggerDigested("ns/b", 0, time.Time{}),
			"start at epoch":   markTriggerDigested("ns/b", 0, epoc),
		}},
		{"PurgeTriggerRuns", purgeTriggerRuns(t0), map[string]call{
			"no time":       purgeTriggerRuns(time.Time{}),
			"time at epoch": purgeTriggerRuns(epoc),
		}},
		{"OpenTriggerThread", openTriggerThread(nil), map[string]call{
			"no Binding":      openTriggerThread(func(t *store.TriggerThread) { t.Binding = "" }),
			"no KeyHash":      openTriggerThread(func(t *store.TriggerThread) { t.KeyHash = "" }),
			"no Thread":       openTriggerThread(func(t *store.TriggerThread) { t.Thread = "" }),
			"no Opened":       openTriggerThread(func(t *store.TriggerThread) { t.Opened = time.Time{} }),
			"Opened at epoch": openTriggerThread(func(t *store.TriggerThread) { t.Opened = epoc }),
		}},
		{"MarkTriggerStarted", markTriggerStarted("ns/b", "h", "1.1", t0), map[string]call{
			"no Binding":        markTriggerStarted("", "h", "1.1", t0),
			"no key hash":       markTriggerStarted("ns/b", "", "1.1", t0),
			"no thread":         markTriggerStarted("ns/b", "h", "", t0),
			"no cooldown":       markTriggerStarted("ns/b", "h", "1.1", time.Time{}),
			"cooldown at epoch": markTriggerStarted("ns/b", "h", "1.1", epoc),
		}},
		{"ReleaseTriggerThread", releaseTriggerThread("ns/b", "h", "1.1"), map[string]call{
			"no Binding":  releaseTriggerThread("", "h", "1.1"),
			"no key hash": releaseTriggerThread("ns/b", "", "1.1"),
			"no thread":   releaseTriggerThread("ns/b", "h", ""),
		}},
		{"GetTriggerThread", getTriggerThread("ns/b", "h"), map[string]call{
			"no Binding":  getTriggerThread("", "h"),
			"no key hash": getTriggerThread("ns/b", ""),
		}},
		{"ReopenTriggerThread", reopenTriggerThread("ns/b", "h", t0), map[string]call{
			"no Binding":    reopenTriggerThread("", "h", t0),
			"no key hash":   reopenTriggerThread("ns/b", "", t0),
			"no time":       reopenTriggerThread("ns/b", "h", time.Time{}),
			"time at epoch": reopenTriggerThread("ns/b", "h", epoc),
		}},
		{"CloseTriggerThread", closeTriggerThread("ns/b", "h", t0), map[string]call{
			"no Binding":    closeTriggerThread("", "h", t0),
			"no key hash":   closeTriggerThread("ns/b", "", t0),
			"no time":       closeTriggerThread("ns/b", "h", time.Time{}),
			"time at epoch": closeTriggerThread("ns/b", "h", epoc),
		}},
		{"PurgeTriggerThreads", purgeTriggerThreads(t0, t0), map[string]call{
			"no now":       purgeTriggerThreads(time.Time{}, t0),
			"now at epoch": purgeTriggerThreads(epoc, t0),
		}},

		{"AppendAudit", appendAudit(nil), map[string]call{
			"undefined Kind":         appendAudit(func(e *store.AuditEntry) { e.Kind = "bogus" }),
			"no At":                  appendAudit(func(e *store.AuditEntry) { e.At = time.Time{} }),
			"no Subject":             appendAudit(func(e *store.AuditEntry) { e.Subject = "" }),
			"no Outcome":             appendAudit(func(e *store.AuditEntry) { e.Outcome = "" }),
			"Outcome over the limit": appendAudit(func(e *store.AuditEntry) { e.Outcome = strings.Repeat("a", store.MaxAuditOutcome+1) }),
		}},
		{"ListAudit", listAudit(10), map[string]call{
			"limit 0":    listAudit(0),
			"limit 1001": listAudit(1001),
		}},
		{"PurgeAudit", purgeAudit(t0, 10), with(
			limitCalls(func(n int) call { return purgeAudit(t0, n) }),
			map[string]call{"no cutoff": purgeAudit(time.Time{}, 10)})},
		{"RecordVote", recordVote(nil), with(
			withKeys(func(k store.ConversationKey) call { return recordVote(func(v *store.FeedbackVote) { v.Key = k }) }),
			map[string]call{
				"no Turn":           recordVote(func(v *store.FeedbackVote) { v.Turn = "" }),
				"no User":           recordVote(func(v *store.FeedbackVote) { v.User = "" }),
				"no Binding":        recordVote(func(v *store.FeedbackVote) { v.Binding = "" }),
				"no Agent":          recordVote(func(v *store.FeedbackVote) { v.Agent = "" }),
				"undefined Kind":    recordVote(func(v *store.FeedbackVote) { v.Kind = "bogus" }),
				"undefined Verdict": recordVote(func(v *store.FeedbackVote) { v.Verdict = "bogus" }),
				"no At":             recordVote(func(v *store.FeedbackVote) { v.At = time.Time{} }),
				"At at the epoch":   recordVote(func(v *store.FeedbackVote) { v.At = epoc }),
			})},
		{"ListVotes", listVotes(key), withKeys(listVotes)},
	}
}

// timeCalls are the calls that check a time with validTime, each with the time put in the argument that is checked.
var timeCalls = map[string]func(time.Time) call{
	"AbandonOutbox":       abandonOutbox,
	"AbandonOutboxBefore": func(at time.Time) call { return abandonOutboxBefore(at, t0) },
	"AdmitTriggerRun":     func(at time.Time) call { return admitTriggerRun(func(r *store.TriggerRun) { r.At = at }, okRunLimits) },
	"ListTriggerDigests":  listTriggerDigests,
	"MarkTriggerDigested": func(at time.Time) call { return markTriggerDigested("ns/b", 0, at) },
	"PurgeTriggerRuns":    purgeTriggerRuns,
	"OpenTriggerThread":   func(at time.Time) call { return openTriggerThread(func(t *store.TriggerThread) { t.Opened = at }) },
	"MarkTriggerStarted":  func(at time.Time) call { return markTriggerStarted("ns/b", "h", "1.1", at) },
	"ReopenTriggerThread": func(at time.Time) call { return reopenTriggerThread("ns/b", "h", at) },
	"CloseTriggerThread":  func(at time.Time) call { return closeTriggerThread("ns/b", "h", at) },
	"PurgeTriggerThreads": func(at time.Time) call { return purgeTriggerThreads(at, t0) },
	"RecordVote":          func(at time.Time) call { return recordVote(func(v *store.FeedbackVote) { v.At = at }) },
}

// TestATimeThatIsSetIsAfterTheEpochAndNoLaterThanBothStoresCanHold: the one rule for the times of rows whose unset reads as
// 0, tried at each edge on every method that checks it: the epoch and a nanosecond past the latest time are refused, and a
// nanosecond past the epoch and the latest time are not.
func TestATimeThatIsSetIsAfterTheEpochAndNoLaterThanBothStoresCanHold(t *testing.T) {
	latest := time.Unix(0, math.MaxInt64)
	for method, at := range timeCalls {
		for what, c := range map[string]struct {
			t  time.Time
			ok bool
		}{
			"the epoch":                {epoc, false},
			"a nanosecond past it":     {epoc.Add(time.Nanosecond), true},
			"the latest time":          {latest, true},
			"a nanosecond past latest": {latest.Add(time.Nanosecond), false},
			"year 2300":                {time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC), false},
		} {
			a := &fakeAdapter{}
			err := at(c.t)(t.Context(), store.Guard(a))
			if c.ok && (err != nil || len(a.calls) != 1) {
				t.Errorf("%s with %s = %v, adapter calls %v; want it legal and passed on", method, what, err, a.calls)
			}
			if !c.ok && (!errors.Is(err, store.ErrInvalid) || len(a.calls) != 0) {
				t.Errorf("%s with %s = %v, adapter calls %v; want ErrInvalid and no call", method, what, err, a.calls)
			}
		}
	}
}

// horizonCalls returns the calls that each use one incomplete set of horizons.
func horizonCalls(f func(store.SessionHorizons) call) map[string]call {
	m := map[string]call{}
	for name, h := range badHorizons {
		m[name] = f(h)
	}
	return m
}

// operations are the methods of store.Store that the guard does not record as operations: Close ends the store and has
// no span, and the claim of a reply's question has never had one.
var untracked = []string{"Close"}

// storeMethods returns the names of the operations of store.Store.
func storeMethods() []string {
	var names []string
	typ := reflect.TypeFor[store.Store]()
	for i := range typ.NumMethod() {
		if n := typ.Method(i).Name; !slices.Contains(untracked, n) {
			names = append(names, n)
		}
	}
	return names
}

// TestEveryOperationHasCases: the tables below cover every operation of store.Store, so that a method added to the
// interface cannot go unchecked and untested.
func TestEveryOperationHasCases(t *testing.T) {
	var got []string
	for _, sp := range specs() {
		got = append(got, sp.method)
	}
	slices.Sort(got)
	want := storeMethods()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the cases cover %v;\nstore.Store has %v", got, want)
	}
}

// TestInvalidArgumentsNeverReachTheAdapter: every argument rule is the guard's. A call that breaks one returns
// ErrInvalid, and the adapter, which assumes its arguments are legal, is not called.
func TestInvalidArgumentsNeverReachTheAdapter(t *testing.T) {
	for _, sp := range specs() {
		for what, bad := range sp.bad {
			t.Run(sp.method+"/"+what, func(t *testing.T) {
				a := &fakeAdapter{}
				err := bad(t.Context(), store.Guard(a))
				if !errors.Is(err, store.ErrInvalid) {
					t.Errorf("%s with %s = %v; want store.ErrInvalid", sp.method, what, err)
				}
				if len(a.calls) != 0 {
					t.Errorf("%s with %s reached the adapter: %v", sp.method, what, a.calls)
				}
			})
		}
	}
}

// TestLegalArgumentsReachTheAdapterOnce: what the guard accepts goes to the adapter, once and by the same name, and
// what the adapter answers comes back as it is.
func TestLegalArgumentsReachTheAdapterOnce(t *testing.T) {
	sentinel := errors.New("the adapter's answer")
	for _, sp := range specs() {
		t.Run(sp.method, func(t *testing.T) {
			a := &fakeAdapter{}
			if err := sp.valid(t.Context(), store.Guard(a)); err != nil {
				t.Fatalf("a legal call = %v", err)
			}
			if !slices.Equal(a.calls, []string{sp.method}) {
				t.Errorf("the adapter was called as %v; want [%s]", a.calls, sp.method)
			}
			a = &fakeAdapter{err: sentinel}
			if err := sp.valid(t.Context(), store.Guard(a)); !errors.Is(err, sentinel) {
				t.Errorf("the adapter's error came back as %v", err)
			}
		})
	}
}

// TestTheLimitOfABatchIsOneRuleForBothStores: a batch is 1 to MaxLimit, whichever store holds the data. The edge on each
// side is tried, and ErrInvalid is not the answer to the largest legal batch.
func TestTheLimitOfABatchIsOneRuleForBothStores(t *testing.T) {
	batches := map[string]func(limit int) call{
		"PurgeAudit":           func(n int) call { return purgeAudit(t0, n) },
		"PurgeExpiredSessions": func(n int) call { return purgeExpiredSessions(horizons, n) },
		"ListStaleTasks":       func(n int) call { return listStaleTasks(horizons, n) },
	}
	for method, f := range batches {
		for n, ok := range map[int]bool{0: false, 1: true, store.MaxLimit: true, store.MaxLimit + 1: false} {
			a := &fakeAdapter{}
			err := f(n)(t.Context(), store.Guard(a))
			if ok != (err == nil) || (!ok && !errors.Is(err, store.ErrInvalid)) {
				t.Errorf("%s with limit %d = %v; want legal: %v", method, n, err, ok)
			}
		}
	}
}

// TestAppendAuditStoresTheRedactedEntry: the adapter is given the entry as Redacted returns it, and the length of the
// outcome is measured on that text, so that text redaction shortens is not refused for its length.
func TestAppendAuditStoresTheRedactedEntry(t *testing.T) {
	a := &fakeAdapter{}
	in := store.AuditEntry{Kind: store.AuditEvent, At: t0, Subject: "Bearer\u00a0FAKE-TOKEN", Outcome: "ok\n" + strings.Repeat("\u200b", store.MaxAuditOutcome)}
	if in.Redacted() == in {
		t.Fatal("the entry needs no redaction, so the test proves nothing")
	}
	if err := store.Guard(a).AppendAudit(t.Context(), in); err != nil {
		t.Fatalf("AppendAudit = %v; an outcome that redaction shortens fits", err)
	}
	if got := a.row.(store.AuditEntry); got != in.Redacted() {
		t.Errorf("the adapter was given %+v; want %+v", got, in.Redacted())
	}
}

// TestClaimReplyInteraction: an empty ID or question is refused before the adapter, and a legal claim passes through,
// error and all.
func TestClaimReplyInteraction(t *testing.T) {
	for what, c := range map[string][2]string{"no ID": {"", "q1"}, "no question": {"r1", ""}} {
		a := &fakeAdapter{}
		err := store.Guard(a).ClaimReplyInteraction(t.Context(), c[0], c[1])
		if !errors.Is(err, store.ErrInvalid) || len(a.calls) != 0 {
			t.Errorf("claim with %s = %v, adapter calls %v; want ErrInvalid and none", what, err, a.calls)
		}
	}
	sentinel := errors.New("the adapter's answer")
	a := &fakeAdapter{err: sentinel}
	if err := store.Guard(a).ClaimReplyInteraction(t.Context(), "r1", "q1"); !errors.Is(err, sentinel) || !slices.Equal(a.calls, []string{"ClaimReplyInteraction"}) {
		t.Errorf("claim = %v, adapter calls %v; want the adapter's error from one call", err, a.calls)
	}
}

// TestCloseGoesToTheAdapter: closing the guarded store closes the adapter.
func TestCloseGoesToTheAdapter(t *testing.T) {
	a := &fakeAdapter{}
	if err := store.Guard(a).Close(); err != nil || !slices.Equal(a.calls, []string{"Close"}) {
		t.Errorf("Close = %v, adapter calls %v; want one Close", err, a.calls)
	}
}

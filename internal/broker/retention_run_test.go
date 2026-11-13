package broker

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// The leader runs the retention loop (internal/retention) with the operator's audit retention and the config's Bindings.

// Run starts the loop with the operator's retention: an audit entry older than it, which the database holds from
// an earlier run, is gone soon after the broker is up, and one inside it stays. With no retention set, the old
// entry stays too, which is what runs until an operator sets one.
func TestRunPurgesAuditEntriesPastTheConfiguredRetention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention time.Duration
		wantLeft  []string
	}{
		{"a retention of 30 days", 30 * 24 * time.Hour, []string{"recent"}},
		{"no retention", 0, []string{"old", "recent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := startConfig(t)
			c.retentionEvery = 10 * time.Millisecond
			c.AuditRetention = tc.retention
			seed, err := sqlite.Open(t.Context(), c.DBPath)
			if err != nil {
				t.Fatal(err)
			}
			for subject, age := range map[string]time.Duration{"old": 60 * 24 * time.Hour, "recent": time.Hour} {
				if err := seed.AppendAudit(t.Context(), store.AuditEntry{At: time.Now().Add(-age), Kind: store.AuditEvent, Subject: subject, Outcome: "accepted"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := seed.Close(); err != nil {
				t.Fatal(err)
			}
			r := start(t, c)
			r.until("/readyz", 200)
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) { // the store's own metric says the loop ran
				if _, body := r.get("/metrics"); strings.Contains(body, `operation="agentkourier.store.AbandonStrandedReplies"`) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the retention loop never ran")
				}
			}
			if tc.retention > 0 { // the abandon ran first in a tick, so give the purge, which follows it, its turn
				for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
					if _, body := r.get("/metrics"); strings.Contains(body, `operation="agentkourier.store.PurgeAudit"`) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the audit purge never ran")
					}
				}
			}
			r.cancel()
			if err := r.wait(); err != nil {
				t.Fatal(err)
			}
			st, err := sqlite.Open(t.Context(), c.DBPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			l, err := st.ListAudit(t.Context(), time.Time{}, 0, 100)
			var left []string
			for _, e := range l {
				if e.Kind == store.AuditEvent {
					left = append(left, e.Subject)
				}
			}
			slices.Sort(left)
			if err != nil || !slices.Equal(left, tc.wantLeft) {
				t.Errorf("audit entries left %v, %v; want %v", left, err, tc.wantLeft)
			}
		})
	}
}

// Run starts the loop with the config's Bindings: a session of a Binding of the example config that has been expired for
// longer than the grace, which the database holds from an earlier run, is gone soon after the broker is up, and one
// that has not stays.
func TestRunDeletesSessionsExpiredForGood(t *testing.T) {
	c := startConfig(t)
	c.retentionEvery = 10 * time.Millisecond
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	old := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "1.1"}
	recent := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "2.2"}
	for k, idle := range map[store.ConversationKey]time.Duration{old: 90*day + 90*day + day, recent: 90*day + 89*day} { // the example's threadTTL is 90 days
		if err := seed.UpsertSession(t.Context(), store.Session{Key: k, Binding: "sre/sre-alerts", LastActivity: time.Now().Add(-idle)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	r := start(t, c)
	r.until("/readyz", 200)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) { // the store's own metric says the sweep ran
		if _, body := r.get("/metrics"); strings.Contains(body, `operation="agentkourier.store.PurgeExpiredSessions"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session purge never ran")
		}
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for k, want := range map[store.ConversationKey]bool{old: false, recent: true} {
		_, err := st.GetSession(t.Context(), k, 100*365*day, time.Now())
		if present := err == nil; present != want {
			t.Errorf("session %s present = %v (%v); want %v", k.Thread, present, err, want)
		}
	}
}

// The broker runs it: a session of the example config's Binding that has been expired for good, with a task that a crash left
// and a question nobody answered, is ended and then deleted soon after the broker is up (the agent cannot be reached from the
// test, which is a failed cancel and no obstacle), the ending is audited and counted, and one inside the horizon keeps its task.
func TestRunEndsATaskThatACrashLeftOnAnExpiredSession(t *testing.T) {
	c := startConfig(t)
	c.retentionEvery = 10 * time.Millisecond
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	stale := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "1.1"}
	live := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: "2.2"}
	for k, idle := range map[store.ConversationKey]time.Duration{stale: 90*day + 90*day + day, live: 90*day + 89*day} { // the example's threadTTL is 90 days
		if err := seed.UpsertSession(t.Context(), store.Session{Key: k, Binding: "sre/sre-alerts", ContextID: "ctx", LastActivity: time.Now().Add(-idle)}); err != nil {
			t.Fatal(err)
		}
		if err := seed.SetSessionTask(t.Context(), k, "", "task-"+k.Thread, store.TaskTurn{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	r := start(t, c)
	r.until("/readyz", 200)
	// The ending, and the deletion that follows it in the same tick: the session of the example's Binding that is inside its
	// horizon is the only other one there, so one session is counted as deleted.
	for _, line := range []string{`agentkourier_retention_removed_total{what="stale_tasks"} 1`, `agentkourier_retention_removed_total{what="expired_sessions"} 1`} {
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, body := r.get("/metrics"); strings.Contains(body, line+"\n") {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("/metrics never held %q; /metrics:\n%s", line, body)
			}
		}
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.GetSession(t.Context(), stale, 100*365*day, time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the stale session = %v; want it deleted by the sweep that follows the ending", err)
	}
	if s, err := st.GetSession(t.Context(), live, 100*365*day, time.Now()); err != nil || s.TaskID != "task-2.2" {
		t.Errorf("the session inside its horizon = task %q, %v; want its task left", s.TaskID, err)
	}
	entries, err := st.ListAudit(t.Context(), time.Time{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var ended []store.AuditEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Outcome, "stale_task_ended:") {
			ended = append(ended, e)
		}
	}
	if len(ended) != 1 || ended[0].Subject != "task=task-1.1 context=ctx" || ended[0].Binding != "sre/sre-alerts" || ended[0].Kind != store.AuditEvent {
		t.Fatalf("the ending in the audit = %+v; want one, of task-1.1, by sre/sre-alerts", ended)
	}
	if ended[0].Identity == "" { // the cancel was made under the Binding's identity, as a stop's is
		t.Error("the ending was audited with no identity; want the Binding's, which the cancel was made under")
	}
	if !strings.HasPrefix(ended[0].Outcome, "stale_task_ended: cancel=failed ") { // the agent was asked, through the Binding, and did not answer
		t.Errorf("the outcome = %q; want a cancel that was made at the agent and failed", ended[0].Outcome)
	}
}

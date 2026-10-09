package broker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

// fakeRetentionStore records what the loop asks for. purged is what each PurgeAudit returns, in turn (the last one
// again after that), and failReplies the number of AbandonStrandedReplies calls that fail.
type fakeRetentionStore struct {
	mu          sync.Mutex
	abandons    []time.Duration
	purges      []auditPurge
	purged      []int
	failReplies int
	failAudit   bool
	dropped     int // what AbandonStrandedReplies reports

	sessionCalls []sessionPurge
	swept        []store.PurgedSessions // what each PurgeExpiredSessions returns, in turn (the last one again after that)
	failSessions bool

	staleQueue []store.StaleTask               // what ListStaleTasks returns, up to its limit, until each is ended
	staleLists []sessionPurge                  // each ListStaleTasks call: its horizons and limit
	endCalls   []store.StaleTask               // each EndStaleTask call, in order
	endAs      map[string]store.EndedStaleTask // what EndStaleTask returns for a task, by ID (default: ended, no interactions)
	endErr     map[string]error                // an error for the EndStaleTask of a task, by ID
	failList   bool
	order      []string // the loop's calls to the store, in order, by method
}

type sessionPurge struct {
	horizons store.SessionHorizons
	limit    int
}

type auditPurge struct {
	before time.Time
	limit  int
}

func (f *fakeRetentionStore) AbandonStrandedReplies(_ context.Context, _ time.Time, age time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "AbandonStrandedReplies")
	f.abandons = append(f.abandons, age)
	if f.failReplies > 0 {
		f.failReplies--
		return 0, errors.New("disk full")
	}
	return f.dropped, nil
}

func (f *fakeRetentionStore) PurgeAudit(_ context.Context, before time.Time, limit int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purges = append(f.purges, auditPurge{before, limit})
	if f.failAudit {
		return 0, errors.New("disk full")
	}
	i := min(len(f.purges), len(f.purged)) - 1
	if i < 0 {
		return 0, nil
	}
	return f.purged[i], nil
}

func (f *fakeRetentionStore) PurgeExpiredSessions(_ context.Context, h store.SessionHorizons, limit int) (store.PurgedSessions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, "PurgeExpiredSessions")
	f.sessionCalls = append(f.sessionCalls, sessionPurge{h, limit})
	if f.failSessions {
		return store.PurgedSessions{}, errors.New("disk full")
	}
	i := min(len(f.sessionCalls), len(f.swept)) - 1
	if i < 0 {
		return store.PurgedSessions{}, nil
	}
	return f.swept[i], nil
}

func (f *fakeRetentionStore) sessions() []sessionPurge {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sessionPurge(nil), f.sessionCalls...)
}

func (f *fakeRetentionStore) calls() (abandons []time.Duration, purges []auditPurge) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.abandons...), append([]auditPurge(nil), f.purges...)
}

// harmless fills what cfg leaves out of the required fields with doubles that do nothing, for the tests that are about
// something else: no config is loaded (so no session is deleted and no stale task listed), no task is held, and nothing is
// cancelled, audited or counted. A test that is about one of them sets it.
func harmless(cfg retentionConfig) retentionConfig {
	if cfg.ThreadTTLs == nil {
		cfg.ThreadTTLs = func() (map[string]time.Duration, bool) { return nil, false }
	}
	if cfg.Canceller == nil {
		cfg.Canceller = &fakeCanceller{}
	}
	if cfg.Audit == nil {
		cfg.Audit = &fakeStaleAudit{}
	}
	if cfg.Holds == nil {
		cfg.Holds = func(store.ConversationKey, string) bool { return false }
	}
	if cfg.OnRemoved == nil {
		cfg.OnRemoved = func(string, int) {}
	}
	return cfg
}

func runRetention(t *testing.T, cfg retentionConfig) (stop func()) {
	t.Helper()
	cfg = harmless(cfg)
	cfg.Interval = time.Millisecond
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return sweepNow }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r, err := newRetention(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	stop = sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Run = %v after its context ended; want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the retention loop did not stop when its context ended")
		}
	})
	t.Cleanup(stop)
	return stop
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// Replies that were never drained are dropped past the age, which is long enough for any turn that waits behind a task
// or a busy session; with the audit retention unset ("keep"), the audit log is never touched.
func TestRetentionAbandonsStrandedRepliesAndKeepsTheAuditLogByDefault(t *testing.T) {
	f := &fakeRetentionStore{}
	stop := runRetention(t, retentionConfig{Store: f})
	waitUntil(t, "three ticks", func() bool { a, _ := f.calls(); return len(a) >= 3 })
	stop()
	abandons, purges := f.calls()
	for _, age := range abandons {
		if age != defaultStrandedReplyAge {
			t.Errorf("abandoned at an age of %v; want %v", age, defaultStrandedReplyAge)
		}
	}
	if len(purges) != 0 {
		t.Errorf("%d audit purges with no retention set; want none: nothing changes until an operator sets one", len(purges))
	}
}

// The purge deletes everything before now minus the retention, a batch at a time while a batch comes back full, and
// stops at the first that is not: a short batch says nothing older is left.
func TestRetentionPurgesAuditInBatchesUntilOneIsShort(t *testing.T) {
	f := &fakeRetentionStore{purged: []int{7, 7, 3, 7}} // the fourth is never asked for
	r, err := newRetention(harmless(retentionConfig{Store: f, AuditRetention: 90 * 24 * time.Hour, AuditBatch: 7,
		Now: func() time.Time { return sweepNow }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	if err != nil {
		t.Fatal(err)
	}
	r.purgeAudit(t.Context())
	_, purges := f.calls()
	if len(purges) != 3 {
		t.Fatalf("%d purge calls; want 3: two full batches, and the short one that ends the tick", len(purges))
	}
	for i, p := range purges {
		if !p.before.Equal(sweepNow.Add(-90*24*time.Hour)) || p.limit != 7 {
			t.Errorf("purge %d = %+v; want the cutoff of the retention and the batch size", i, p)
		}
	}
}

// One tick does not go on for ever: a log far past its retention is worked down over several ticks.
func TestRetentionBoundsTheBatchesOfOneTick(t *testing.T) {
	f := &fakeRetentionStore{purged: []int{5}} // always a full batch
	r, err := newRetention(harmless(retentionConfig{Store: f, AuditRetention: 24 * time.Hour, AuditBatch: 5, MaxAuditBatches: 3,
		Now: func() time.Time { return sweepNow }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	if err != nil {
		t.Fatal(err)
	}
	r.purgeAudit(t.Context())
	if _, purges := f.calls(); len(purges) != 3 {
		t.Errorf("%d purge calls in one tick; want 3, the most it may make", len(purges))
	}
}

// A failure of either is logged and tried again at the next tick, and does not stop the other.
func TestRetentionLogsFailuresAndGoesOn(t *testing.T) {
	f := &fakeRetentionStore{failReplies: 2, failAudit: true}
	logs := &logBuffer{}
	stop := runRetention(t, retentionConfig{Store: f, AuditRetention: 24 * time.Hour, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	waitUntil(t, "four ticks", func() bool { a, p := f.calls(); return len(a) >= 4 && len(p) >= 4 })
	stop()
	if got := logs.String(); !strings.Contains(got, "replies that were never delivered could not be dropped") || !strings.Contains(got, "audit entries past the retention could not be purged") {
		t.Errorf("the log %q; want a warning for each failure", got)
	}
}

func TestNewRetentionRefusesWhatIsNotLegal(t *testing.T) {
	f := &fakeRetentionStore{}
	for name, cfg := range map[string]retentionConfig{
		"no store":               {},
		"a negative retention":   {Store: f, AuditRetention: -time.Hour},
		"a retention of an hour": {Store: f, AuditRetention: time.Hour},
	} {
		if _, err := newRetention(harmless(cfg)); err == nil {
			t.Errorf("newRetention with %s succeeded", name)
		}
	}
	if _, err := newRetention(harmless(retentionConfig{Store: f, AuditRetention: MinAuditRetention})); err != nil {
		t.Errorf("newRetention at the least retention = %v", err)
	}
}

// Against the real store, with the entries it holds: those older than the retention go, those inside it stay, and the
// log is as append-only as before.
func TestRetentionPurgesOnlyAuditEntriesPastTheRetention(t *testing.T) {
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	retention := 30 * 24 * time.Hour
	for _, e := range []struct {
		subject string
		age     time.Duration
	}{{"old-1", retention + 48*time.Hour}, {"old-2", retention + time.Hour}, {"recent", retention - time.Hour}, {"new", time.Hour}} {
		err := st.AppendAudit(t.Context(), store.AuditEntry{At: sweepNow.Add(-e.age), Kind: store.AuditEvent, Subject: e.subject, Outcome: "accepted"})
		if err != nil {
			t.Fatal(err)
		}
	}
	stop := runRetention(t, retentionConfig{Store: st, AuditRetention: retention, AuditBatch: 1})
	waitUntil(t, "the old entries to be purged", func() bool {
		l, _ := st.ListAudit(t.Context(), time.Time{}, 0, 10)
		return len(l) == 2
	})
	stop()
	l, err := st.ListAudit(t.Context(), time.Time{}, 0, 10)
	if err != nil || len(l) != 2 || l[0].Subject != "recent" || l[1].Subject != "new" {
		t.Errorf("audit after the purge = %+v, %v; want the two inside the retention", l, err)
	}
	// Nothing was written to the log by the purge itself.
	if l[1].Sequence != 4 {
		t.Errorf("last sequence %d; want 4: the purge writes no entry of its own", l[1].Sequence)
	}
}

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

// What the loop removes is counted, so that it can be alerted on: a dropped reply is a turn that was never sent, and
// audit rows are gone for good. Nothing is counted when nothing was removed, and nothing when a call failed.
func TestRetentionCountsWhatItRemoves(t *testing.T) {
	f := &fakeRetentionStore{dropped: 2, purged: []int{7, 3}}
	var mu sync.Mutex
	got := map[string]int{}
	r, err := newRetention(harmless(retentionConfig{Store: f, AuditRetention: 48 * time.Hour, AuditBatch: 7, Now: func() time.Time { return sweepNow },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnRemoved: func(what string, n int) { mu.Lock(); got[what] += n; mu.Unlock() }}))
	if err != nil {
		t.Fatal(err)
	}
	r.abandonReplies(t.Context())
	r.purgeAudit(t.Context())
	counted := func() map[string]int { // not held across the passes: the next one counts under the same lock
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(got)
	}
	if got := counted(); got[RemovedStrandedReplies] != 2 || got[RemovedAuditEntries] != 10 || len(got) != 2 {
		t.Errorf("counted %v; want 2 stranded replies and 10 audit entries, and nothing else", got)
	}
	f.dropped, f.failAudit = 0, true
	r.abandonReplies(t.Context())
	r.purgeAudit(t.Context())
	if got := counted(); got[RemovedStrandedReplies] != 2 || got[RemovedAuditEntries] != 10 {
		t.Errorf("counted %v after a pass that removed nothing and one that failed; want no change", got)
	}
}

// ttls is a config of two Bindings, one that keeps its threads 30 days and one 10 days. Before the first load it is not ready.
func ttls() (map[string]time.Duration, bool) {
	return map[string]time.Duration{"ns/long": 30 * 24 * time.Hour, "ns/short": 10 * 24 * time.Hour}, true
}

func newSessionRetention(t *testing.T, f *fakeRetentionStore, mutate func(*retentionConfig)) *retention {
	t.Helper()
	cfg := retentionConfig{Store: f, ThreadTTLs: ttls, SessionBatch: 3, Now: func() time.Time { return sweepNow },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := newRetention(harmless(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A session is past its horizon a grace (90 days) after its threadTTL, which is its Binding's: the store is told, for
// each Binding of the config, the time before which a session of it is deleted. A session whose Binding has left the config
// has no threadTTL to measure by: it is held to the longest of the Bindings', and never to less than the default.
func TestRetentionPurgesSessionsPastTheirBindingsHorizon(t *testing.T) {
	grace := 90 * 24 * time.Hour
	day := 24 * time.Hour
	for _, tc := range []struct {
		name string
		ttls func() (map[string]time.Duration, bool)
		want store.SessionHorizons
	}{
		{"two Bindings", ttls, store.SessionHorizons{
			ByBinding: map[string]time.Time{"ns/long": sweepNow.Add(-30*day - grace), "ns/short": sweepNow.Add(-10*day - grace)},
			Otherwise: sweepNow.Add(-90*day - grace), // the default threadTTL is longer than either
		}},
		{"a Binding with a longer threadTTL than the default", func() (map[string]time.Duration, bool) {
			return map[string]time.Duration{"ns/year": 365 * day}, true
		}, store.SessionHorizons{
			ByBinding: map[string]time.Time{"ns/year": sweepNow.Add(-365*day - grace)},
			Otherwise: sweepNow.Add(-365*day - grace),
		}},
		{"no Binding at all", func() (map[string]time.Duration, bool) { return map[string]time.Duration{}, true }, store.SessionHorizons{
			ByBinding: map[string]time.Time{}, Otherwise: sweepNow.Add(-90*day - grace),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRetentionStore{}
			newSessionRetention(t, f, func(c *retentionConfig) { c.ThreadTTLs = tc.ttls }).purgeSessions(t.Context())
			calls := f.sessions()
			if len(calls) != 1 || calls[0].limit != 3 || !maps.EqualFunc(calls[0].horizons.ByBinding, tc.want.ByBinding, time.Time.Equal) || !calls[0].horizons.Otherwise.Equal(tc.want.Otherwise) {
				t.Errorf("PurgeExpiredSessions calls = %+v; want one, of a batch of 3, with the horizons %+v", calls, tc.want)
			}
		})
	}
}

// Without a config nothing is known of any session's threadTTL, so nothing is deleted.
func TestRetentionLeavesSessionsAloneUntilTheirBindingsAreKnown(t *testing.T) {
	f := &fakeRetentionStore{}
	newSessionRetention(t, f, func(c *retentionConfig) { c.ThreadTTLs = func() (map[string]time.Duration, bool) { return nil, false } }).purgeSessions(t.Context())
	if calls := f.sessions(); len(calls) != 0 {
		t.Errorf("%d purges; want none", len(calls))
	}
}

// One tick does not go on for ever: a batch at a time while a batch comes back full, and at most MaxSessionBatches of
// them; a short batch says nothing more is due.
func TestRetentionBoundsTheSessionBatchesOfOneTick(t *testing.T) {
	full := store.PurgedSessions{Sessions: 3}
	for _, tc := range []struct {
		name      string
		swept     []store.PurgedSessions
		max       int
		wantCalls int
	}{
		{"until a short one", []store.PurgedSessions{full, full, {Sessions: 1}, full}, 10, 3},
		{"never short, so the most it may make", []store.PurgedSessions{full}, 4, 4},
		{"nothing due", nil, 10, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRetentionStore{swept: tc.swept}
			newSessionRetention(t, f, func(c *retentionConfig) { c.MaxSessionBatches = tc.max }).purgeSessions(t.Context())
			if got := len(f.sessions()); got != tc.wantCalls {
				t.Errorf("%d purge calls in one tick; want %d", got, tc.wantCalls)
			}
		})
	}
}

// What is deleted is counted for each kind of row, so that it can be seen and alerted on, and not when nothing was
// deleted or the call failed; a failure after some batches still counts the batches that were done.
func TestRetentionCountsTheSessionsItDeletes(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int{}
	count := func(what string, n int) { mu.Lock(); got[what] += n; mu.Unlock() }
	f := &fakeRetentionStore{swept: []store.PurgedSessions{
		{Sessions: 3, Replies: 10, Interactions: 4, Presses: 1, Outbox: 20}, {Sessions: 1, Replies: 2, Outbox: 5}}}
	newSessionRetention(t, f, func(c *retentionConfig) { c.OnRemoved = count }).purgeSessions(t.Context())
	want := map[string]int{RemovedExpiredSessions: 4, RemovedSessionReplies: 12, RemovedSessionInteractions: 4, RemovedSessionPresses: 1, RemovedSessionOutbox: 25}
	mu.Lock()
	if !maps.Equal(got, want) {
		t.Errorf("counted %v; want %v, and nothing for a kind with none", got, want)
	}
	mu.Unlock()

	clear(got)
	f = &fakeRetentionStore{failSessions: true}
	newSessionRetention(t, f, func(c *retentionConfig) { c.OnRemoved = count }).purgeSessions(t.Context())
	if len(got) != 0 {
		t.Errorf("counted %v after a failed call; want nothing", got)
	}
}

// A failure is logged and tried again at the next tick, and does not stop the rest of the loop.
func TestRetentionLogsASessionPurgeFailureAndGoesOn(t *testing.T) {
	f := &fakeRetentionStore{failSessions: true}
	logs := &logBuffer{}
	stop := runRetention(t, retentionConfig{Store: f, ThreadTTLs: ttls, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	waitUntil(t, "three ticks", func() bool { a, _ := f.calls(); return len(a) >= 3 && len(f.sessions()) >= 3 })
	stop()
	if got := logs.String(); !strings.Contains(got, "expired sessions could not be deleted") {
		t.Errorf("the log %q; want a warning for the failure", got)
	}
}

// Against the real store, through the loop: the sessions past their Binding's horizon go with what belongs to them, the
// one inside it stays, and a session with a task that has not ended stays whatever its age.
func TestRetentionDeletesOnlySessionsPastTheirHorizon(t *testing.T) {
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	day, grace := 24*time.Hour, 90*24*time.Hour
	seed := func(thread, binding, task string, idle time.Duration) store.ConversationKey {
		k := store.ConversationKey{Connection: "slack", Channel: "C1", Thread: thread}
		err := st.UpsertSession(t.Context(), store.Session{Key: k, Binding: binding, TaskID: task, LastActivity: sweepNow.Add(-idle)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = st.EnqueueReply(t.Context(), store.Reply{ID: "r-" + thread, Key: k, UserID: "U1", MessageRef: "1.1", Text: "hi", EnqueuedAt: sweepNow.Add(-idle)})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.MarkReplyDelivered(t.Context(), "r-"+thread, sweepNow.Add(-idle)); err != nil {
			t.Fatal(err)
		}
		return k
	}
	past := seed("1.1", "ns/short", "", 10*day+grace+time.Hour)
	inside := seed("2.2", "ns/short", "", 10*day+grace-time.Hour)
	longer := seed("3.3", "ns/long", "", 10*day+grace+time.Hour) // past the short Binding's horizon, not the long one's
	gone := seed("4.4", "ns/removed", "", 90*day+grace+time.Hour)
	busy := seed("5.5", "ns/short", "task", 1000*day)

	newSessionRetention(t, &fakeRetentionStore{}, func(c *retentionConfig) { c.Store = st }).purgeSessions(t.Context())

	for k, wantSession := range map[store.ConversationKey]bool{past: false, inside: true, longer: true, gone: false, busy: true} {
		_, err := st.GetSession(t.Context(), k, 100*365*day, sweepNow)
		if present := err == nil; present != wantSession {
			t.Errorf("session %s present = %v (%v); want %v", k.Thread, present, err, wantSession)
		}
		if has, err := st.HasReply(t.Context(), "r-"+k.Thread); err != nil || has != wantSession {
			t.Errorf("reply of %s present = %v, %v; want %v with its session", k.Thread, has, err, wantSession)
		}
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

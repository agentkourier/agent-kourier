package broker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
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
}

type auditPurge struct {
	before time.Time
	limit  int
}

func (f *fakeRetentionStore) AbandonStrandedReplies(_ context.Context, _ time.Time, age time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeRetentionStore) calls() (abandons []time.Duration, purges []auditPurge) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.abandons...), append([]auditPurge(nil), f.purges...)
}

func runRetention(t *testing.T, cfg retentionConfig) (stop func()) {
	t.Helper()
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
	r, err := newRetention(retentionConfig{Store: f, AuditRetention: 90 * 24 * time.Hour, AuditBatch: 7,
		Now: func() time.Time { return sweepNow }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
	r, err := newRetention(retentionConfig{Store: f, AuditRetention: 24 * time.Hour, AuditBatch: 5, MaxAuditBatches: 3,
		Now: func() time.Time { return sweepNow }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
		if _, err := newRetention(cfg); err == nil {
			t.Errorf("newRetention with %s succeeded", name)
		}
	}
	if _, err := newRetention(retentionConfig{Store: f, AuditRetention: MinAuditRetention}); err != nil {
		t.Errorf("newRetention at the least retention = %v", err)
	}
}

// Against the real store, with the entries it holds: those older than the retention go, those inside it stay, and the
// log is as append-only as before.
func TestRetentionPurgesOnlyAuditEntriesPastTheRetention(t *testing.T) {
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "courier.db"))
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
				if _, body := r.get("/metrics"); strings.Contains(body, `operation="courier.store.AbandonStrandedReplies"`) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the retention loop never ran")
				}
			}
			if tc.retention > 0 { // the abandon ran first in a tick, so give the purge, which follows it, its turn
				for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
					if _, body := r.get("/metrics"); strings.Contains(body, `operation="courier.store.PurgeAudit"`) {
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
	r, err := newRetention(retentionConfig{Store: f, AuditRetention: 48 * time.Hour, AuditBatch: 7, Now: func() time.Time { return sweepNow },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnRemoved: func(what string, n int) { mu.Lock(); got[what] += n; mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	r.abandonReplies(t.Context())
	r.purgeAudit(t.Context())
	mu.Lock()
	defer mu.Unlock()
	if got[RemovedStrandedReplies] != 2 || got[RemovedAuditEntries] != 10 || len(got) != 2 {
		t.Errorf("counted %v; want 2 stranded replies and 10 audit entries, and nothing else", got)
	}
	f.dropped, f.failAudit = 0, true
	r.abandonReplies(t.Context())
	r.purgeAudit(t.Context())
	if got[RemovedStrandedReplies] != 2 || got[RemovedAuditEntries] != 10 {
		t.Errorf("counted %v after a pass that removed nothing and one that failed; want no change", got)
	}
}

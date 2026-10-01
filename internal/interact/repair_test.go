package interact_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// repairRig is a replaceRig whose clock the test moves, an hour past the deadline of every interaction.
type repairRig struct {
	*replaceRig
	now  *atomic.Int64
	logs *logBuf
}

func newRepairRig(t *testing.T, mod func(*interact.ReplacerConfig)) *repairRig {
	t.Helper()
	now := &atomic.Int64{}
	now.Store(deadline.Add(time.Hour).UnixNano())
	logs := &logBuf{}
	rig := newReplaceRigWith(t, func(c *interact.ReplacerConfig) {
		c.Now, c.Logger = func() time.Time { return time.Unix(0, now.Load()).UTC() }, slog.New(slog.NewTextHandler(logs, nil))
		if mod != nil {
			mod(c)
		}
	})
	return &repairRig{replaceRig: rig, now: now, logs: logs}
}

func (r *repairRig) advance(d time.Duration) { r.now.Add(int64(d)) }

// decidedRows adds one interaction that ended each way, decided within the horizon, and two that a repair
// leaves: one decided long ago, and one still pending.
func (r *repairRig) decidedRows(t *testing.T) {
	t.Helper()
	r.decided(t, "answered", asked, r.answer("UALICE1", [][]string{{"alpha"}, {"x"}}))
	r.decided(t, "timed", asked, r.resolve(store.InteractionTimedOut))
	r.decided(t, "cancelled", asked, r.resolve(store.InteractionCancelled))
	r.decided(t, "old", asked, func(id string) error {
		return r.st.SubmitInteraction(t.Context(), id, []byte(`{"RequestID":"req-1"}`), "UALICE1", d0.Add(-48*time.Hour))
	})
	r.decided(t, "pending", asked, nil)
}

func (r *repairRig) edited() []string {
	var ids []string
	for _, e := range r.up.list() {
		ids = append(ids, e.ts+" "+firstWord(e.content.Text))
	}
	return ids
}

func firstWord(s string) string { return strings.Fields(s + " ")[0] }

// TestRepairReplacesWhatIsDecidedWithinTheHorizonOnce: the prompts of interactions that ended, by an
// answer, a timeout or a cancel, and not one decided before the horizon or one still pending. A second
// pass finds nothing left to do.
func TestRepairReplacesWhatIsDecidedWithinTheHorizonOnce(t *testing.T) {
	r := newRepairRig(t, nil)
	r.decidedRows(t)
	for range 2 {
		if err := r.r.Repair(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got := r.up.list()
	seen := map[string]int{}
	for _, e := range got {
		seen[firstWord(e.content.Text)]++
	}
	if len(got) != 3 || seen["Answered"] != 1 || seen["Timed"] != 1 || seen["Cancelled"] != 1 {
		t.Errorf("edits %v; want one each for the answered, timed out and cancelled prompts, once over two passes", r.edited())
	}
}

// TestRepairFindsWhatTheSweepGaveUpOn: the sweep timed the question out and gave up on the edit after five
// sweeps, so the prompt shows its controls. The repair takes them off.
func TestRepairFindsWhatTheSweepGaveUpOn(t *testing.T) {
	sw := newSweepRig(t)
	sw.pending(t, "ix-1", "task-1", t0.Add(timeout))
	sw.upd.err = errBoom
	s := sw.sweeper(t, nil)
	for range 5 {
		_ = s.Sweep(t.Context())
	}
	if sw.state(t, "ix-1") != store.InteractionTimedOut || !strings.Contains(sw.logs.String(), "keeps its controls") {
		t.Fatal("the sweep did not time the question out and give up on the edit")
	}
	up := &edits{}
	rp, err := interact.NewReplacer(interact.ReplacerConfig{Store: sw.st, Updater: up, Now: func() time.Time { return *sw.clock }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := rp.Repair(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := up.list(); len(got) != 1 || got[0].ts != "100.001" || !strings.Contains(shown(got[0]), "Timed out") {
		t.Errorf("edits %+v; want the swept prompt replaced", got)
	}
}

// TestAFailedEditIsRetriedThenGivenUpOn: a prompt that cannot be edited (its message deleted, say) is tried
// on every pass, and after ten failures it is left, logged at Error, so it is not retried for ever.
func TestAFailedEditIsRetriedThenGivenUpOn(t *testing.T) {
	r := newRepairRig(t, nil)
	r.decided(t, "ix-1", asked, r.resolve(store.InteractionTimedOut))
	r.up.err = errBoom
	for pass := 1; pass <= 12; pass++ {
		_ = r.r.Repair(t.Context())
		want := min(pass, 10)
		if got := len(r.up.list()); got != want {
			t.Fatalf("after pass %d: %d attempts; want %d", pass, got, want)
		}
	}
	if out := r.logs.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "ix-1") {
		t.Errorf("log = %q; want an error naming ix-1 when it was given up on", out)
	}
	r.up.err = nil // given up on for good, even when Slack works
	_ = r.r.Repair(t.Context())
	if got := len(r.up.list()); got != 10 {
		t.Errorf("%d attempts after the give-up; want it left", got)
	}
}

// TestARateLimitIsWaitedOutAndNotCountedAsAFailure: Slack said when to come back. Nothing is tried until
// then, and any number of rate limits do not give the prompt up.
func TestARateLimitIsWaitedOutAndNotCountedAsAFailure(t *testing.T) {
	r := newRepairRig(t, nil)
	r.decided(t, "ix-1", asked, r.resolve(store.InteractionTimedOut))
	r.up.err = fmt.Errorf("slack chat.update: %w", &slackgo.RateLimitedError{RetryAfter: 30 * time.Second})
	_ = r.r.Repair(t.Context())
	for range 3 { // too soon: nothing is tried
		r.advance(10 * time.Second)
		_ = r.r.Repair(t.Context())
	}
	if got := len(r.up.list()); got != 2 { // the first, and the one after 30 seconds
		t.Fatalf("%d attempts; want the first and, once the 30 seconds had passed, one more", got)
	}
	for range 30 { // many more rate limits: still not a failure
		r.advance(31 * time.Second)
		_ = r.r.Repair(t.Context())
	}
	r.up.mu.Lock()
	r.up.err = nil
	r.up.mu.Unlock()
	before := len(r.up.list())
	r.advance(31 * time.Second)
	if err := r.r.Repair(t.Context()); err != nil || len(r.up.list()) != before+1 {
		t.Errorf("after the limit lifted: Repair = %v, %d attempts; want it edited, not given up on", err, len(r.up.list()))
	}
}

// TestRepairIsPacedByTheBatch: after a restart many prompts may be waiting, and a pass edits only a batch.
func TestRepairIsPacedByTheBatch(t *testing.T) {
	r := newRepairRig(t, func(c *interact.ReplacerConfig) { c.Batch = 10 })
	for i := range 25 {
		r.decided(t, fmt.Sprintf("ix-%02d", i), asked, r.resolve(store.InteractionTimedOut))
	}
	for _, want := range []int{10, 20, 25, 25} {
		_ = r.r.Repair(t.Context())
		if got := len(r.up.list()); got != want {
			t.Fatalf("after a pass %d edits; want %d", got, want)
		}
	}
}

// TestARestartRepairsOnceMoreWithTheSameEdit: what the first process replaced is replaced again by the next,
// and it is the same message: an edit is the same however often it is made.
func TestARestartRepairsOnceMoreWithTheSameEdit(t *testing.T) {
	r := newRepairRig(t, nil)
	r.decided(t, "ix-1", asked, r.answer("UALICE1", [][]string{{"beta"}, {"x"}}))
	if err := r.r.Replace(t.Context(), "ix-1"); err != nil { // the decider's, in the first process
		t.Fatal(err)
	}
	_ = r.r.Repair(t.Context()) // done in this process: nothing more
	if len(r.up.list()) != 1 {
		t.Fatalf("%d edits; want the repair to leave what was replaced", len(r.up.list()))
	}
	restarted, err := interact.NewReplacer(interact.ReplacerConfig{Store: r.st, Updater: r.up, Now: func() time.Time { return time.Unix(0, r.now.Load()).UTC() },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	_ = restarted.Repair(t.Context())
	got := r.up.list()
	if len(got) != 2 || shown(got[0]) != shown(got[1]) {
		t.Errorf("%d edits; want the restarted process to replace it once more, identically", len(got))
	}
}

// TestRunDrainsAtStartRelistsAndEnds: Run repairs what is waiting at once, finds what was decided since on
// its next listing, and ends with its context, leaving no goroutine.
func TestRunDrainsAtStartRelistsAndEnds(t *testing.T) {
	r := newRepairRig(t, func(c *interact.ReplacerConfig) { c.Interval, c.ListEvery = 5*time.Millisecond, time.Minute })
	before := goleak.IgnoreCurrent()
	r.decided(t, "first", asked, r.resolve(store.InteractionTimedOut))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.r.Run(ctx) }()
	waitFor(t, "the first prompt", func() bool { return len(r.up.list()) == 1 })
	r.decided(t, "second", asked, r.resolve(store.InteractionCancelled))
	time.Sleep(30 * time.Millisecond) // many ticks, but no listing is due yet
	if len(r.up.list()) != 1 {
		t.Fatal("a prompt decided since was repaired before the next listing was due")
	}
	r.advance(time.Minute)
	waitFor(t, "the second prompt", func() bool { return len(r.up.list()) == 2 })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run returned nil; want the context's error")
	}
	goleak.VerifyNone(t, before)
}

// TestRunRepairsAtOnceAndNotAfterTheFirstTick: what a crash left is repaired when the process starts, not
// an interval later.
func TestRunRepairsAtOnceAndNotAfterTheFirstTick(t *testing.T) {
	r := newRepairRig(t, func(c *interact.ReplacerConfig) { c.Interval = time.Hour })
	r.decided(t, "ix-1", asked, r.resolve(store.InteractionTimedOut))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.r.Run(ctx) }()
	waitFor(t, "the prompt, long before the first tick", func() bool { return len(r.up.list()) == 1 })
	cancel()
	<-done
}

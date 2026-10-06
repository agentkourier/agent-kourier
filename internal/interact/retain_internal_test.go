package interact

import (
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestRetentionRunsEveryTenMinutesWithAnHoursGraceByDefault: configured by nothing, it is the backstop of
// the worker's own cap, which is an hour.
func TestRetentionRunsEveryTenMinutesWithAnHoursGraceByDefault(t *testing.T) {
	rt, err := NewRetention(RetentionConfig{Store: struct{ RetentionStore }{}, OnCleared: func(int) {}})
	if err != nil {
		t.Fatal(err)
	}
	if rt.cfg.Interval != 10*time.Minute || rt.cfg.Grace != time.Hour {
		t.Errorf("Interval %v, Grace %v; want 10 minutes and an hour", rt.cfg.Interval, rt.cfg.Grace)
	}
}

// TestASettledInteractionIsKeptLongerThanAnythingThatReadsIt: the request and answer are read by the repair of
// ended prompts for RepairHorizon after the decision, and by a press that was recorded in that time, which the
// worker keeps for its own cap. The default grace must outlast both, or the repair would find rows it cannot
// build an edit from, and give up on prompts that still show their buttons. An answer not yet delivered waits
// longer still.
func TestASettledInteractionIsKeptLongerThanAnythingThatReadsIt(t *testing.T) {
	rt, err := NewRetention(RetentionConfig{Store: struct{ RetentionStore }{}, OnCleared: func(int) {}})
	if err != nil {
		t.Fatal(err)
	}
	if need := RepairHorizon + defaultGiveUpAfter; rt.cfg.ClearGrace < need {
		t.Errorf("ClearGrace %v; want at least the repair's horizon plus the worker's cap, %v", rt.cfg.ClearGrace, need)
	}
	if rt.cfg.UndeliveredGrace <= rt.cfg.ClearGrace {
		t.Errorf("UndeliveredGrace %v; want it longer than ClearGrace %v", rt.cfg.UndeliveredGrace, rt.cfg.ClearGrace)
	}
	rep, err := NewReplacer(ReplacerConfig{Store: struct{ store.Interactions }{}, Updater: struct{ Editor }{}})
	if err != nil {
		t.Fatal(err)
	}
	bs, err := NewBackstop(BackstopConfig{Store: struct{ store.Interactions }{}, Sessions: struct{ SessionReader }{}, Outbox: struct{ Enqueuer }{}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.cfg.Horizon != RepairHorizon || bs.cfg.Horizon != RepairHorizon {
		t.Errorf("the repair's Horizon %v and the backstop's %v; want both RepairHorizon, which the grace is derived from", rep.cfg.Horizon, bs.cfg.Horizon)
	}
}

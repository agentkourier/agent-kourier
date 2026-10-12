package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func TestClearSessionAcknowledgmentIsFenced(t *testing.T) {
	dsn, _, _ := freshSchema(t)
	a, b := openStandbyAt(t, dsn), openStandbyAt(t, dsn)
	if _, err := a.Lead(t.Context(), "pod-a"); err != nil {
		t.Fatal(err)
	}
	k := oldSession(t, a, "1.1")
	turn := store.TaskTurn{ID: "turn-1", User: "U1", MessageRef: "1700.01", Acknowledgment: "eyes", AcknowledgmentID: "source-1"}
	if err := a.SetSessionTask(t.Context(), k, "ctx-1", "task-1", turn); err != nil {
		t.Fatal(err)
	}

	if err := b.ClearSessionAcknowledgment(t.Context(), k, "task-1", "turn-1"); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("a standby's acknowledgment clear = %v; want store.ErrDeposed", err)
	}
	if _, err := b.Lead(t.Context(), "pod-b"); err != nil {
		t.Fatal(err)
	}
	if err := a.ClearSessionAcknowledgment(t.Context(), k, "task-1", "turn-1"); !errors.Is(err, store.ErrDeposed) {
		t.Errorf("a deposed leader's acknowledgment clear = %v; want store.ErrDeposed", err)
	}
	if got, err := b.GetSession(t.Context(), k, 24*time.Hour, t0); err != nil || got.Turn != turn {
		t.Fatalf("session after deposed clears = %+v, %v; want acknowledgment metadata unchanged", got, err)
	}
	if err := b.ClearSessionAcknowledgment(t.Context(), k, "task-1", "turn-1"); err != nil {
		t.Fatal(err)
	}
	got, err := b.GetSession(t.Context(), k, 24*time.Hour, t0)
	if err != nil {
		t.Fatal(err)
	}
	turn.Acknowledgment, turn.AcknowledgmentID = "", ""
	if got.TaskID != "task-1" || got.ContextID != "ctx-1" || got.Turn != turn {
		t.Errorf("session after current leader clear = %+v; want task/context and non-ack turn fields preserved", got)
	}
}

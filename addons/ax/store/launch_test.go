package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
	kit "github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/store/pgtest"
)

func report(ctx context.Context, st *Store, r result.Report, digest [32]byte) (int, error) {
	status, _, err := st.AcceptResult(ctx, r, digest)
	return status, err
}

func launchFixtures(t *testing.T, s *Store, id string, now time.Time) (Launch, result.Claim) {
	t.Helper()
	ctx := asCaller("agent-kourier")
	if err := createTask(s, id); err != nil {
		t.Fatal(err)
	}
	l := Launch{TaskID: id, Name: "kourier-" + id, ResultID: result.NewID(), Deadline: now.Add(time.Minute).UnixNano()}
	require := func(ok bool, err error) {
		t.Helper()
		if !ok || err != nil {
			t.Fatalf("launch setup: %v, %v", ok, err)
		}
	}
	require(s.Register(ctx, l))
	require(s.Bind(ctx, id, "task-uid", "actor-uid"))
	require(s.Dispatch(ctx, id, true, now.Add(2*time.Minute)))
	return l, result.Claim{ResultID: l.ResultID, TaskUID: "task-uid", InvocationID: result.NewID()}
}

func TestSQLiteLaunchDecisions(t *testing.T) {
	now := time.Now()
	st, err := OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "launch.db"), Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	testLaunchDecisions(t, st, &now)
}

func TestPostgresLaunchDecisions(t *testing.T) {
	dsn := pgtest.DSN(t)
	now := time.Now()
	st := openPostgresWith(t, dsn, Options{Now: func() time.Time { return now }})
	mustLead(t, st, "leader", time.Minute)
	testLaunchDecisions(t, st, &now)
}

func testLaunchDecisions(t *testing.T, s *Store, clock *time.Time) {
	t.Helper()
	ctx := t.Context()
	expect := func(want int, op func() (int, error)) {
		t.Helper()
		got, err := op()
		if err != nil || got != want {
			t.Fatalf("HTTP %d, %v; want %d", got, err, want)
		}
	}
	checkClaim := func(c result.Claim) func() (int, error) {
		return func() (int, error) { return s.Claim(ctx, c) }
	}
	checkReport := func(c result.Claim, body string) func() (int, error) {
		return func() (int, error) {
			return report(ctx, s, result.Report{Claim: c, Output: "x\x00y", OutputSource: "file"}, sha256.Sum256([]byte(body)))
		}
	}
	l, c := launchFixtures(t, s, "receipted", *clock)
	other := c
	other.InvocationID = result.NewID()
	expect(http.StatusNotFound, checkReport(c, "receipt"))
	expect(http.StatusNoContent, checkClaim(c))
	expect(http.StatusNoContent, checkClaim(c))
	expect(http.StatusConflict, checkClaim(other))
	expect(http.StatusNotFound, checkReport(other, "receipt"))
	wrongUID := c
	wrongUID.TaskUID = "actor-uid"
	expect(http.StatusNotFound, checkReport(wrongUID, "receipt"))
	expect(http.StatusNoContent, checkReport(c, "receipt"))
	expect(http.StatusNoContent, checkReport(c, "receipt"))
	expect(http.StatusConflict, checkReport(c, "conflicting receipt"))
	expect(http.StatusGone, checkClaim(c))
	if ok, err := s.RequestCancel(ctx, l.TaskID); ok || err != nil {
		t.Fatalf("cancel beat receipt: %v, %v", ok, err)
	}

	l, c = launchFixtures(t, s, "canceled", *clock)
	expect(http.StatusNoContent, checkClaim(c))
	if ok, err := s.RequestCancel(ctx, l.TaskID); !ok || err != nil {
		t.Fatalf("cancel did not win: %v, %v", ok, err)
	}
	expect(http.StatusGone, checkReport(c, "receipt"))
	expect(http.StatusGone, checkClaim(c))

	l, c = launchFixtures(t, s, "deadline", *clock)
	expect(http.StatusNoContent, checkClaim(c))
	*clock = time.Unix(0, l.Deadline)
	expect(http.StatusGone, checkReport(c, "receipt"))
	expect(http.StatusGone, checkClaim(c))
	if ok, err := s.Decide(ctx, l.TaskID, TimedOut); !ok || err != nil {
		t.Fatalf("timeout did not win: %v, %v", ok, err)
	}
}

func TestPostgresSerializesConcurrentClaims(t *testing.T) {
	dsn := pgtest.DSN(t)
	s := openPostgres(t, dsn)
	mustLead(t, s, "leader", time.Minute)
	_, c := launchFixtures(t, s, "race", time.Now())
	other := c
	other.InvocationID = result.NewID()
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, claim := range []result.Claim{c, other} {
		wg.Go(func() {
			<-start
			code, err := s.Claim(t.Context(), claim)
			if err != nil {
				t.Error(err)
			}
			results <- code
		})
	}
	close(start)
	wg.Wait()
	a, b := <-results, <-results
	if min(a, b) != http.StatusNoContent || max(a, b) != http.StatusConflict {
		t.Fatalf("concurrent claims returned %d, %d", a, b)
	}
}

func TestPostgresReceiptSurvivesInterruptedTakeover(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openPostgres(t, dsn), openPostgres(t, dsn)
	ctx := t.Context()
	first := mustLead(t, a, "leader", time.Minute)
	pending, c := launchFixtures(t, a, "unfinished", time.Now())
	settled, d := launchFixtures(t, a, "settled", time.Now())
	hash := sha256.Sum256([]byte("fixture"))
	for _, claim := range []result.Claim{c, d} {
		if got, err := a.Claim(ctx, claim); got != 204 || err != nil {
			t.Fatalf("start: HTTP %d, %v", got, err)
		}
		if got, err := report(ctx, a, result.Report{Claim: claim, Output: "success", OutputSource: "file"}, hash); got != 204 || err != nil {
			t.Fatalf("report: HTTP %d, %v", got, err)
		}
	}
	owner := asCaller("agent-kourier")
	stored, err := a.Get(owner, a2a.TaskID(settled.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	task := *stored.Task
	task.Status.State = a2a.TaskStateCompleted
	task.Artifacts = []*a2a.Artifact{{ID: "result", Parts: []*a2a.Part{a2a.NewTextPart("success")}}}
	if _, err := a.Update(owner, &taskstore.UpdateRequest{Task: &task, PrevTask: stored.Task, PrevVersion: stored.Version}); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	second := mustLead(t, b, "standby", time.Minute)
	if second.Epoch <= first.Epoch {
		t.Fatal("lease epoch did not move")
	}
	if err := b.Recover(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SettleInterrupted(ctx, "interrupted"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		l        Launch
		decision string
		state    a2a.TaskState
	}{
		{pending, Interrupted, a2a.TaskStateFailed}, {settled, Succeeded, a2a.TaskStateCompleted},
	} {
		recovered, err := b.Launch(ctx, tc.l.TaskID)
		if err != nil || recovered.Decision != tc.decision || recovered.CleanupAt == 0 || recovered.Output != "" {
			t.Fatalf("recovered %s: decision=%q, cleanup=%d, err=%v", tc.l.TaskID, recovered.Decision, recovered.CleanupAt, err)
		}
		stored, err := b.Get(owner, a2a.TaskID(tc.l.TaskID))
		if err != nil || stored.Task.Status.State != tc.state || (len(stored.Task.Artifacts) != 0) != (tc.state == a2a.TaskStateCompleted) {
			t.Fatalf("after takeover %s: %+v, %v", tc.l.TaskID, stored, err)
		}
	}
	if got, err := report(ctx, b, result.Report{Claim: c}, hash); got != 204 || err != nil {
		t.Fatalf("receipt replay after interruption got %d, %v", got, err)
	}
	if _, err := report(ctx, a, result.Report{Claim: c}, hash); !errors.Is(err, kit.ErrDeposed) {
		t.Fatalf("old leader could write: %v", err)
	}
}

func TestUpgradeSettlesLegacyQuestionsAndKeepsHistory(t *testing.T) {
	s := openSQLite(t, filepath.Join(t.TempDir(), "legacy.db"))
	ctx := asCaller("agent-kourier")
	states := []a2a.TaskState{a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateInputRequired, a2a.TaskStateCompleted}
	for i, state := range states {
		id := a2a.TaskID(fmt.Sprint(i))
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("prompt"))
		msg.ID = "message-" + string(id)
		task := &a2a.Task{ID: id, ContextID: "thread", Status: a2a.TaskStatus{State: state}, History: []*a2a.Message{msg}}
		if _, err := s.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SettlePaused(ctx, "AX upgraded"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleInterrupted(ctx, "AX restarted"); err != nil {
		t.Fatal(err)
	}
	for i, state := range states {
		id := a2a.TaskID(fmt.Sprint(i))
		task, err := s.MessageTask(ctx, "message-"+string(id))
		want := a2a.TaskStateFailed
		if state == a2a.TaskStateCompleted {
			want = state
		}
		if err != nil || task == nil || task.Status.State != want {
			t.Fatalf("replay %s: %+v, %v, want %s", state, task, err, want)
		}
	}
}

func TestRecoverClosesCallbacksForAnAlreadyTerminalTask(t *testing.T) {
	s := openSQLite(t, filepath.Join(t.TempDir(), "terminal.db"))
	ctx := t.Context()
	l, claim := launchFixtures(t, s, "terminal-without-decision", time.Now())
	owner := asCaller("agent-kourier")
	stored, err := s.Get(owner, a2a.TaskID(l.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	task := *stored.Task
	task.Status.State = a2a.TaskStateFailed
	if _, err := s.Update(owner, &taskstore.UpdateRequest{Task: &task, PrevTask: stored.Task, PrevVersion: stored.Version}); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Launch(ctx, l.TaskID)
	if err != nil || recovered.Decision != Failed || recovered.CleanupAt == 0 {
		t.Fatalf("recovery = %+v, %v", recovered, err)
	}
	if code, err := s.Claim(ctx, claim); code != 410 || err != nil {
		t.Fatalf("late start = %d, %v", code, err)
	}
}

package interact_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

func TestTimeoutAuditFailureCannotSendApprovalDenial(t *testing.T) {
	r := onlyApproval(t)
	r.aud.err = errBoom
	sw := r.sweeper(t, nil)
	if err := sw.Sweep(t.Context()); err == nil {
		t.Error("timeout audit failure must be retried")
	}
	if got := len(r.sess.sent()); got != 0 {
		t.Errorf("denials sent without audit: %d; want 0", got)
	}
	if got := r.state(t, "ix-1"); got != store.InteractionPending {
		t.Errorf("state %s; want pending", got)
	}
	if got := len(r.hooked()); got != 0 {
		t.Errorf("outcome hooks %d; want 0", got)
	}
}

func TestTimeoutAuditRetryDoesNotDuplicateDenialOrOutcome(t *testing.T) {
	r := onlyApproval(t)
	r.aud.err = errBoom
	sw := r.sweeper(t, nil)
	if err := sw.Sweep(t.Context()); err == nil {
		t.Fatal("timeout audit failure must be retried")
	}
	r.aud.err = nil
	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := sw.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(r.aud.got); got != 2 {
		t.Errorf("timeout audits %d; want the failed attempt and one successful retry", got)
	}
	if got := len(r.sess.sent()); got != 1 {
		t.Errorf("denials sent %d; want one after the successful audit", got)
	}
	if got := r.state(t, "ix-1"); got != store.InteractionTimedOut {
		t.Errorf("state %s; want timed_out", got)
	}
	if got := len(r.outcomes()); got != 1 {
		t.Errorf("approval outcomes %d; want one", got)
	}
	if got := len(r.hooked()); got != 1 {
		t.Errorf("timeout hooks %d; want one", got)
	}
}

func TestTimeoutAuditUsesBindingIdentity(t *testing.T) {
	r := onlyApproval(t)
	b := r.look.bindings["ns/b"]
	b.Identity = "agent-service"
	r.look.bindings["ns/b"] = b
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(r.aud.act) != 1 || r.aud.act[0].Binding != "ns/b" || r.aud.act[0].Identity != "agent-service" {
		t.Errorf("audit actors %+v; want the Binding name and identity", r.aud.act)
	}
}

func TestTimeoutAuditRestartFailureStillPrecedesDenial(t *testing.T) {
	r := onlyApproval(t)
	// The first process writes its audit and takes the denial, then dies before the row commit.
	r.st.fail = true
	first := r.sweeper(t, nil)
	if err := first.Sweep(t.Context()); err == nil {
		t.Fatal("a pre-commit failure must be retried after restart")
	}
	if got := len(r.aud.got); got != 1 || len(r.sess.sent()) != 1 || r.state(t, "ix-1") != store.InteractionPending {
		t.Fatalf("after first process: audits %d, denials %d, state %s; want one audit, one denial and pending", len(r.aud.got), len(r.sess.sent()), r.state(t, "ix-1"))
	}

	// A replacement process cannot rely on the in-memory marker. Its failed audit must not send another denial.
	r.aud.err = errBoom
	second := r.sweeper(t, nil)
	if err := second.Sweep(t.Context()); err == nil {
		t.Fatal("the replacement audit failure must be retried")
	}
	if got := len(r.sess.sent()); got != 1 {
		t.Errorf("denials after replacement audit failure %d; want 1", got)
	}
	if got := r.state(t, "ix-1"); got != store.InteractionPending {
		t.Errorf("state %s; want pending", got)
	}

	r.aud.err = nil
	if err := second.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(r.sess.sent()); got != 2 {
		t.Errorf("denials after replacement recovery %d; want 2", got)
	}
	if got := r.state(t, "ix-1"); got != store.InteractionTimedOut {
		t.Errorf("state %s; want timed_out", got)
	}
	if got := len(r.outcomes()); got != 1 {
		t.Errorf("approval outcomes %d; want one", got)
	}
}

func TestTimeoutCannotOverwriteFirstCompleteAnswer(t *testing.T) {
	r := onlyApproval(t)
	r.st.conflict = true
	r.st.win = func(ctx context.Context, id string) {
		answer, err := json.Marshal(dialect.Answer{RequestID: "req-task-1", Decisions: []dialect.Decision{{ToolID: "t1", Approved: true}, {ToolID: "t2", Approved: false}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.st.SubmitInteraction(ctx, id, answer, "UALICE1", t0.Add(timeout-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.sweeper(t, nil).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	row, err := r.st.GetInteraction(t.Context(), "ix-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.InteractionAnswered || row.Decider != "UALICE1" {
		t.Errorf("row %+v; want the person's first-complete answer preserved", row)
	}
	if got := len(r.outcomes()); got != 0 {
		t.Errorf("approval outcomes %d; want no timeout outcome", got)
	}
	if got := len(r.hooked()); got != 0 {
		t.Errorf("timeout hooks %d; want none", got)
	}
	if len(r.aud.got) != 2 || r.aud.got[0].Result != audit.DecisionTimedOut || r.aud.got[1].Result != audit.DecisionSuperseded {
		t.Errorf("audit results %+v; want timed_out followed by superseded compensation", r.aud.got)
	}
	if r.aud.act[1].Binding != "ns/b" || r.aud.act[1].Identity != "" || r.aud.act[1].User != "" {
		t.Errorf("compensating audit actor %+v; want the timeout Binding with no person", r.aud.act[1])
	}
}

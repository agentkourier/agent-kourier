package trigger_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry/telemetrytest"
)

// A message's text is the user's, and an error from the session manager can quote it: neither
// may reach a span attribute, a status or a metric label, whatever the disposition.
func TestIntakeTelemetryExcludesMessageAndErrorText(t *testing.T) {
	const prompt, failure = "private-prompt-canary", "private-error-canary"
	for _, tc := range []struct {
		name        string
		answer      func(session.Inbound) (session.Result, error)
		wantErr     bool
		disposition string
	}{
		{"accepted", nil, false, "accepted"},
		{"unmentioned", func(session.Inbound) (session.Result, error) {
			return resultOf(session.Unmentioned), nil
		}, false, "unmentioned"},
		{"transient error", func(session.Inbound) (session.Result, error) { return session.Result{}, errors.New(failure) }, true, "error"},
		{"invalid", func(session.Inbound) (session.Result, error) {
			return session.Result{}, fmt.Errorf("%s: %w", failure, store.ErrInvalid)
		}, false, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := telemetrytest.New(t)
			in, sub := newIntake(t)
			sub.fn = tc.answer
			m := inbound("C1", "5.5", "", true)
			m.Text = prompt + " " + failure
			if err := in.Message(h.Ctx, m); (err != nil) != tc.wantErr {
				t.Fatalf("Message = %v, want error %v", err, tc.wantErr)
			}
			h.RequireNone(t, prompt, failure)
			if got := h.Counter(t, "agentkourier_intake_total", map[string]string{"source": "message", "disposition": tc.disposition, "connection": "agent-kourier-system/slack-main"}); got != 1 {
				t.Fatalf("agentkourier_intake_total{disposition=%s} = %v, want 1: %s", tc.disposition, got, h.Exported(t))
			}
		})
	}
}

// Every Intake counts under the connection it serves: two connections' messages and presses are two series each, and a press that fails
// is counted under its error class, on its own connection only.
func TestIntakeTotalIsCountedByConnection(t *testing.T) {
	h := telemetrytest.New(t)
	conns := []v1alpha1.ObjectRef{{Namespace: "agent-kourier-system", Name: "slack-main"}, {Namespace: "team-a", Name: "slack"}}
	for i, ref := range conns {
		in, _ := newIntakeOn(t, ref, nil, "", "")
		// A message outside a thread, in an unbound channel, that mentions nobody: ignored. The second connection has one more.
		for j := range i + 1 {
			if err := in.Message(h.Ctx, inbound("C9", fmt.Sprintf("5.%d", j), "", false)); err != nil {
				t.Fatal(err)
			}
		}
		if err := in.Interaction(h.Ctx, chat.Interaction{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		conn   string
		source string
		disp   string
		want   float64
	}{
		{"agent-kourier-system/slack-main", "message", "ignored", 1},
		{"team-a/slack", "message", "ignored", 2},
		{"agent-kourier-system/slack-main", "interaction", "success", 1},
		{"team-a/slack", "interaction", "success", 1},
	} {
		labels := map[string]string{"source": tc.source, "disposition": tc.disp, "connection": tc.conn}
		if got := h.Counter(t, "agentkourier_intake_total", labels); got != tc.want {
			t.Errorf("agentkourier_intake_total%v = %v; want %v: %s", labels, got, tc.want, h.Exported(t))
		}
	}
}

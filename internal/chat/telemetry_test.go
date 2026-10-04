package chat_test

import (
	"errors"
	"fmt"
	"testing"

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
			if got := h.Counter(t, "courier_intake_total", map[string]string{"source": "message", "disposition": tc.disposition}); got != 1 {
				t.Fatalf("courier_intake_total{disposition=%s} = %v, want 1: %s", tc.disposition, got, h.Exported(t))
			}
		})
	}
}

package trigger_test

import (
	"errors"
	"maps"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// intakeErrors reads agentkourier_intake_total{source="message",disposition="error"} of the connection of the test's Intake: the
// count of the events whose handling failed, which the platform delivers again.
func (h *triggered) intakeErrors(t *testing.T) float64 {
	t.Helper()
	return h.intakeCount(t, "error")
}

// intakeCount reads agentkourier_intake_total{source="message"} of the connection of the test's Intake and the disposition.
func (h *triggered) intakeCount(t *testing.T, disposition string) float64 {
	t.Helper()
	families, err := h.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"source": "message", "disposition": disposition, "connection": v1alpha1.NamespacedName(h.cfg.Connection.Namespace, h.cfg.Connection.Name)}
	for _, f := range families {
		if f.GetName() != "agentkourier_intake_total" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if maps.Equal(labels, want) {
				return m.Counter.GetValue()
			}
		}
	}
	return 0
}

// A decision is counted once it is taken. An attempt whose start failed counts no decision, only the intake's error; its
// redelivery decides again and is the start, counted once, whichever kind of trigger it is and whether or not it threads by
// key, and a webhook event's too. A delivery after that start is a dedup.
func TestAFailedStartIsCountedOnceItsRedeliveryStarts(t *testing.T) {
	tests := []struct {
		name    string
		trigger []v1alpha1.Trigger
		msg     chat.Inbound
		intake  func(*testing.T, ...v1alpha1.Trigger) *triggered // newLimited if nil
	}{
		{"a bot message whose trigger threads by key", []v1alpha1.Trigger{trialTrigger()}, botInbound("C1", "100.1", kindFiring), nil},
		{"a bot message whose trigger threads by nothing", []v1alpha1.Trigger{kindOnly()}, botInbound("C1", "100.1", kindFiring), nil},
		{"a person's message", []v1alpha1.Trigger{personTrigger(nil)}, personPost("C1", "9.9", "", "checkout is down"), nil},
		{"a bot's mention", nil, botMentionOf("5.5", "4.4"), nil},
		{"a webhook event", []v1alpha1.Trigger{webhookTrigger(nil)}, prEvent("e1", "d1"), newWebhook},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intake := tt.intake
			if intake == nil {
				intake = newLimited
			}
			h := intake(t, tt.trigger...)
			h.hookErr = errors.New("the queue is down")
			if err := h.in.Message(h.ctx, tt.msg); !errors.Is(err, h.hookErr) {
				t.Fatalf("Message = %v; want the hook's error, so that the platform sends it again", err)
			}
			if d := h.decisions(t); len(d) != 0 {
				t.Errorf("decisions after the failed attempt = %v; want none: nothing was decided that stands", d)
			}
			if got := h.intakeErrors(t); got != 1 {
				t.Errorf("intake errors = %v; want the failed attempt counted there", got)
			}
			h.hookErr = nil
			if err := h.in.Message(h.ctx, tt.msg); err != nil {
				t.Fatalf("the redelivery: %v", err)
			}
			if d := h.decisions(t); !maps.Equal(d, map[string]float64{"started": 1}) {
				t.Errorf("decisions after the redelivery = %v; want the one start, and no dedup: the redelivery started the turn", d)
			}
			if err := h.in.Message(h.ctx, tt.msg); err != nil {
				t.Fatalf("the delivery after the start: %v", err)
			}
			if d := h.decisions(t); !maps.Equal(d, map[string]float64{"started": 1, "dedup": 1}) {
				t.Errorf("decisions after a delivery of a started message = %v; want one started and one dedup", d)
			}
			if got := len(h.got()); got != 2 {
				t.Errorf("the start hook was asked %d times; want twice: the failed attempt and its redelivery", got)
			}
		})
	}
}

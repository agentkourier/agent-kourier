package broker

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

func mustMetrics(t *testing.T, stale func() bool) *Metrics {
	t.Helper()
	m, err := NewMetrics(stale)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// samples is every series of the registry as "name{label=value,...} value".
func samples(t *testing.T, m *Metrics) []string {
	t.Helper()
	fams, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range fams {
		for _, s := range f.GetMetric() {
			var ls []string
			for _, l := range s.GetLabel() {
				ls = append(ls, l.GetName()+"="+l.GetValue())
			}
			v := s.GetCounter().GetValue()
			if f.GetType().String() == "GAUGE" {
				v = s.GetGauge().GetValue()
			}
			out = append(out, f.GetName()+"{"+strings.Join(ls, ",")+"} "+map[float64]string{0: "0", 1: "1", 2: "2"}[v])
		}
	}
	return out
}

// Each hook the other packages offer lands on its own series, and no other.
func TestEachHookIncrementsItsMetric(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Metrics)
		want string
	}{
		{"audit write failure", func(m *Metrics) { m.AuditFailure(store.AuditAgentCall) }, `courier_audit_write_failures_total{kind=agent_call} 1`},
		{"credential rejection", func(m *Metrics) { m.CredentialRejected("sre/sre-alerts") }, `courier_credential_rejections_total{binding=sre/sre-alerts} 1`},
		{"config reload failure", func(m *Metrics) { m.ReloadError(errors.New("bad yaml")) }, `courier_config_reload_errors_total{} 1`},
		{"outbox abandon", func(m *Metrics) { m.OutboxAbandoned("row-1", "channel_gone") }, `courier_outbox_abandoned_total{reason=channel_gone} 1`},
		{"turn outcome", func(m *Metrics) { m.TurnOutcome("sre/sre-alerts", session.TurnFailed) }, `courier_turn_outcomes_total{binding=sre/sre-alerts,outcome=failed} 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMetrics(t, nil)
			tc.call(m)
			var hit []string
			for _, s := range samples(t, m) {
				if strings.HasSuffix(s, " 1") && !strings.HasPrefix(s, "courier_config_stale") {
					hit = append(hit, s)
				}
			}
			if !slices.Equal(hit, []string{tc.want}) {
				t.Errorf("series at 1 = %v; want only %q", hit, tc.want)
			}
		})
	}
}

// The stale gauge is read at each scrape, so it follows the config.
func TestStaleGaugeFollowsTheConfig(t *testing.T) {
	stale := false
	m := mustMetrics(t, func() bool { return stale })
	if !slices.Contains(samples(t, m), "courier_config_stale{} 0") {
		t.Errorf("samples = %v; want stale 0", samples(t, m))
	}
	stale = true
	if !slices.Contains(samples(t, m), "courier_config_stale{} 1") {
		t.Errorf("samples = %v; want stale 1", samples(t, m))
	}
}

// The chat gauge is 1 or 0 per ChatConnection, and its label is the connection's name alone.
func TestChatConnectedGaugeFollowsTheConnection(t *testing.T) {
	const series = "courier_chat_connected{connection=courier-system/slack-main} "
	m := mustMetrics(t, nil)
	if got := samples(t, m); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "courier_chat_connected") }) {
		t.Fatalf("samples = %v; a connection nobody reported has no series", got)
	}
	for _, step := range []struct {
		up   bool
		want string
	}{{false, "0"}, {true, "1"}, {false, "0"}} {
		m.ChatConnected("courier-system/slack-main", step.up)
		if got := samples(t, m); !slices.Contains(got, series+step.want) {
			t.Errorf("after ChatConnected(%v) samples = %v; want %q", step.up, got, series+step.want)
		}
	}
}

// Labels are bounded: Binding names, audit kinds and a fixed set of abandon reasons, never
// anything a user or an agent can choose. A reason or a kind outside the sets is one series.
func TestMetricLabelsAreBounded(t *testing.T) {
	m := mustMetrics(t, nil)
	m.AuditFailure("not-a-kind")
	m.AuditFailure(store.AuditEvent)
	for _, r := range []string{"invalid", "lookup_timeout", "expired", "channel_gone", "U12345 posted in C99999", "made-up"} {
		m.OutboxAbandoned("id", r)
	}
	m.CredentialRejected("sre/sre-alerts")
	m.TurnOutcome("sre/sre-alerts", "an error text an agent chose")
	fams, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"kind": true, "binding": true, "reason": true, "outcome": true}
	for _, f := range fams {
		for _, s := range f.GetMetric() {
			for _, l := range s.GetLabel() {
				if !allowed[l.GetName()] {
					t.Errorf("%s has label %q; the allowed labels are kind, binding and reason", f.GetName(), l.GetName())
				}
			}
		}
	}
	got := samples(t, m)
	for _, bad := range []string{"not-a-kind", "U12345", "made-up", "an error text"} {
		if strings.Contains(strings.Join(got, "\n"), bad) {
			t.Errorf("series %v carry %q; an unknown value must become %q", got, bad, "other")
		}
	}
	if !slices.Contains(got, "courier_audit_write_failures_total{kind=other} 1") || !slices.Contains(got, "courier_outbox_abandoned_total{reason=other} 2") {
		t.Errorf("samples = %v; want the unknown kind and the two unknown reasons counted as other", got)
	}
}

// Every outcome the session manager can report is its own label, and one it cannot is "other".
func TestEveryTurnOutcomeHasItsOwnLabel(t *testing.T) {
	for _, o := range session.TurnOutcomes() {
		m := mustMetrics(t, nil)
		m.TurnOutcome("ns/b", o)
		if got := samples(t, m); !slices.Contains(got, "courier_turn_outcomes_total{binding=ns/b,outcome="+string(o)+"} 1") {
			t.Errorf("outcome %q: samples = %v; want its own label", o, got)
		}
	}
}

// Every reason the outbox gives, and every neutral reason of a permanent error (chat.Reason), is its own label: a
// reason added there and missing here would otherwise be counted as "other", and hide. The platform's own code is
// not one: it is in the log, and a metric that took it would grow with the platform's vocabulary.
func TestEveryAbandonReasonHasItsOwnLabel(t *testing.T) {
	reasons := []string{outbox.AbandonInvalid, outbox.AbandonLookupTimeout, outbox.AbandonExpired,
		string(chat.ReasonChannelGone), string(chat.ReasonTooLong), string(chat.ReasonInvalidContent), string(chat.ReasonOther)}
	for _, r := range reasons {
		m := mustMetrics(t, nil)
		m.OutboxAbandoned("id", r)
		if got := samples(t, m); !slices.Contains(got, "courier_outbox_abandoned_total{reason="+r+"} 1") {
			t.Errorf("reason %q: samples = %v; want its own label", r, got)
		}
	}
	for _, code := range []string{"channel_not_found", "is_archived", "not_in_channel", "msg_too_long", "invalid_blocks", "invalid_blocks_format"} {
		m := mustMetrics(t, nil)
		m.OutboxAbandoned("id", code)
		if got := samples(t, m); !slices.Contains(got, "courier_outbox_abandoned_total{reason=other} 1") || strings.Contains(strings.Join(got, "\n"), code) {
			t.Errorf("code %q: samples = %v; want it counted as other, and never as a label", code, got)
		}
	}
}

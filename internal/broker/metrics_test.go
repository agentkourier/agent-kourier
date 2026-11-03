package broker

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/bindingcheck"
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
		{"audit write failure", func(m *Metrics) { m.AuditFailure(store.AuditAgentCall) }, `agentkourier_audit_write_failures_total{kind=agent_call} 1`},
		{"credential rejection", func(m *Metrics) { m.CredentialRejected("sre/sre-alerts") }, `agentkourier_credential_rejections_total{binding=sre/sre-alerts} 1`},
		{"config reload failure", func(m *Metrics) { m.ReloadError(errors.New("bad yaml")) }, `agentkourier_config_reload_errors_total{} 1`},
		{"outbox abandon", func(m *Metrics) { m.OutboxAbandoned("row-1", "channel_gone") }, `agentkourier_outbox_abandoned_total{reason=channel_gone} 1`},
		{"retention removal", func(m *Metrics) { m.RetentionRemoved(RemovedStrandedReplies, 1) }, `agentkourier_retention_removed_total{what=stranded_replies} 1`},
		{"turn outcome", func(m *Metrics) { m.TurnOutcome("sre/sre-alerts", session.TurnFailed) }, `agentkourier_turn_outcomes_total{binding=sre/sre-alerts,outcome=failed} 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMetrics(t, nil)
			tc.call(m)
			var hit []string
			for _, s := range samples(t, m) {
				if strings.HasSuffix(s, " 1") && !strings.HasPrefix(s, "agentkourier_config_stale") {
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
	if !slices.Contains(samples(t, m), "agentkourier_config_stale{} 0") {
		t.Errorf("samples = %v; want stale 0", samples(t, m))
	}
	stale = true
	if !slices.Contains(samples(t, m), "agentkourier_config_stale{} 1") {
		t.Errorf("samples = %v; want stale 1", samples(t, m))
	}
}

// The chat gauge is 1 or 0 per ChatConnection, and its label is the connection's name alone.
func TestChatConnectedGaugeFollowsTheConnection(t *testing.T) {
	const series = "agentkourier_chat_connected{connection=agent-kourier-system/slack-main} "
	m := mustMetrics(t, nil)
	if got := samples(t, m); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "agentkourier_chat_connected") }) {
		t.Fatalf("samples = %v; a connection nobody reported has no series", got)
	}
	for _, step := range []struct {
		up   bool
		want string
	}{{false, "0"}, {true, "1"}, {false, "0"}} {
		m.ChatConnected("agent-kourier-system/slack-main", step.up)
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
	if !slices.Contains(got, "agentkourier_audit_write_failures_total{kind=other} 1") || !slices.Contains(got, "agentkourier_outbox_abandoned_total{reason=other} 2") {
		t.Errorf("samples = %v; want the unknown kind and the two unknown reasons counted as other", got)
	}
}

// Every outcome the session manager can report is its own label, and one it cannot is "other".
func TestEveryTurnOutcomeHasItsOwnLabel(t *testing.T) {
	for _, o := range session.TurnOutcomes() {
		m := mustMetrics(t, nil)
		m.TurnOutcome("ns/b", o)
		if got := samples(t, m); !slices.Contains(got, "agentkourier_turn_outcomes_total{binding=ns/b,outcome="+string(o)+"} 1") {
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
		if got := samples(t, m); !slices.Contains(got, "agentkourier_outbox_abandoned_total{reason="+r+"} 1") {
			t.Errorf("reason %q: samples = %v; want its own label", r, got)
		}
	}
	for _, code := range []string{"channel_not_found", "is_archived", "not_in_channel", "msg_too_long", "invalid_blocks", "invalid_blocks_format"} {
		m := mustMetrics(t, nil)
		m.OutboxAbandoned("id", code)
		if got := samples(t, m); !slices.Contains(got, "agentkourier_outbox_abandoned_total{reason=other} 1") || strings.Contains(strings.Join(got, "\n"), code) {
			t.Errorf("code %q: samples = %v; want it counted as other, and never as a label", code, got)
		}
	}
}

// transition is a change of one condition of one Binding to status, for the metric hooks.
func transition(binding string, t bindingcheck.ConditionType, status bindingcheck.Status) bindingcheck.Transition {
	return bindingcheck.Transition{Binding: binding, New: bindingcheck.Condition{Type: t, Status: status, Reason: bindingcheck.ReasonOK}}
}

// seriesOf is the samples of the registry whose name starts with prefix.
func seriesOf(t *testing.T, m *Metrics, prefix string) []string {
	t.Helper()
	var out []string
	for _, s := range samples(t, m) {
		if strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

// A condition's status is three series, one at 1 and the others at 0, and Ready also sets the 0 or 1 gauge: 1 for True, and 0 for False and for
// Unknown, which is not a yes.
func TestBindingConditionSetsTheStatusSeriesAndReadyIsOneOnlyForTrue(t *testing.T) {
	const b = "sre/sre-alerts"
	for _, tc := range []struct {
		status bindingcheck.Status
		ready  string
	}{{bindingcheck.True, "1"}, {bindingcheck.False, "0"}, {bindingcheck.Unknown, "0"}} {
		t.Run(string(tc.status), func(t *testing.T) {
			m := mustMetrics(t, nil)
			m.BindingCondition(transition(b, bindingcheck.Ready, tc.status))
			got := seriesOf(t, m, "agentkourier_binding_")
			want := []string{"agentkourier_binding_ready{binding=" + b + "} " + tc.ready}
			for _, s := range []string{"true", "false", "unknown"} {
				v := "0"
				if s == strings.ToLower(string(tc.status)) {
					v = "1"
				}
				want = append(want, "agentkourier_binding_condition{binding="+b+",condition=Ready,status="+s+"} "+v)
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("series = %v; want %v", got, want)
			}
		})
	}
}

// A condition that is not Ready leaves the ready gauge alone, and a later transition replaces what an earlier one set.
func TestBindingConditionOfAnotherTypeLeavesReadyAloneAndALaterTransitionReplaces(t *testing.T) {
	const b = "sre/sre-alerts"
	m := mustMetrics(t, nil)
	m.BindingCondition(transition(b, bindingcheck.BackendReachable, bindingcheck.False))
	if got := seriesOf(t, m, "agentkourier_binding_ready"); len(got) != 0 {
		t.Errorf("a BackendReachable transition set %v; Ready alone sets the ready gauge", got)
	}
	m.BindingCondition(transition(b, bindingcheck.BackendReachable, bindingcheck.True))
	got := seriesOf(t, m, "agentkourier_binding_condition")
	for _, want := range []string{
		"agentkourier_binding_condition{binding=sre/sre-alerts,condition=BackendReachable,status=true} 1",
		"agentkourier_binding_condition{binding=sre/sre-alerts,condition=BackendReachable,status=false} 0",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("series = %v; want %q", got, want)
		}
	}
}

// Pruning a Binding deletes all of its series, the ready gauge and every condition's, and no other Binding's.
func TestBindingsPrunedDeletesTheSeriesOfThoseBindingsOnly(t *testing.T) {
	m := mustMetrics(t, nil)
	for _, b := range []string{"a/one", "a/two"} {
		for _, c := range []bindingcheck.ConditionType{bindingcheck.BackendReachable, bindingcheck.ChannelJoined, bindingcheck.ApproversReadable, bindingcheck.Ready} {
			m.BindingCondition(transition(b, c, bindingcheck.False))
		}
	}
	m.BindingsPruned([]string{"a/one"})
	got := strings.Join(seriesOf(t, m, "agentkourier_binding_"), "\n")
	if strings.Contains(got, "a/one") {
		t.Errorf("series after the prune:\n%s\nwant none of a/one's", got)
	}
	if !strings.Contains(got, "agentkourier_binding_ready{binding=a/two} 0") || strings.Count(got, "a/two") != 13 {
		t.Errorf("series after the prune:\n%s\nwant all 13 of a/two's (4 conditions of 3 statuses, and the ready gauge)", got)
	}
}

// A probe's duration lands in the histogram of its condition, and a label outside the closed set is one series, "other".
func TestBindingProbedObservesTheDurationByCondition(t *testing.T) {
	m := mustMetrics(t, nil)
	m.BindingProbed(bindingcheck.BackendReachable, 50*time.Millisecond)
	m.BindingProbed(bindingcheck.ChannelJoined, time.Second)
	m.BindingProbed("a condition an adapter made up", time.Second)
	fams, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for _, f := range fams {
		if f.GetName() != "agentkourier_binding_probe_duration_seconds" {
			continue
		}
		for _, s := range f.GetMetric() {
			counts[s.GetLabel()[0].GetValue()] = s.GetHistogram().GetSampleCount()
		}
	}
	if want := map[string]uint64{"BackendReachable": 1, "ChannelJoined": 1, "other": 1}; !maps.Equal(counts, want) {
		t.Errorf("observations by condition = %v; want %v", counts, want)
	}
}

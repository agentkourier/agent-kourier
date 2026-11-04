package broker

import (
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/config"
)

// gauges returns the series of one metric as "label value" lines, sorted.
func gauges(t *testing.T, m *Metrics, name string) []string {
	t.Helper()
	var out []string
	for _, s := range samples(t, m) {
		if strings.HasPrefix(s, name+"{") {
			out = append(out, strings.TrimPrefix(s, name))
		}
	}
	slices.Sort(out)
	return out
}

// The report of a config source that fails one object at a time is two gauges, each set whole at every report: a kind or a
// reason that has none reads 0, so that a series that cleared does not stay at its last value.
func TestConfigReportSetsTheRejectionGauges(t *testing.T) {
	m := mustMetrics(t, nil)
	m.ConfigReport(config.Report{
		Invalid: []config.Invalid{{Kind: config.KindAgentBackend}, {Kind: config.KindBinding}, {Kind: config.KindBinding}},
		Rejections: []config.Rejection{
			{Reason: config.ReasonBackendRejected}, {Reason: config.ReasonBackendRejected}, {Reason: config.ReasonChannelConflict}, {Reason: "Surprise"},
		},
	})
	wantInvalid := []string{"{kind=AgentBackend} 1", "{kind=Binding} 2", "{kind=ChatConnection} 0"}
	if got := gauges(t, m, "agentkourier_config_invalid_objects"); !slices.Equal(got, wantInvalid) {
		t.Errorf("invalid objects = %v; want %v", got, wantInvalid)
	}
	got := gauges(t, m, "agentkourier_config_binding_rejections")
	for _, want := range []string{"{reason=BackendRejected} 2", "{reason=ChannelConflict} 1", "{reason=InvalidSpec} 0", "{reason=other} 1", "{reason=NamespaceNotAllowed} 0"} {
		if !slices.Contains(got, want) {
			t.Errorf("binding rejections = %v; want %q among them", got, want)
		}
	}

	m.ConfigReport(config.Report{})
	for _, s := range append(gauges(t, m, "agentkourier_config_invalid_objects"), gauges(t, m, "agentkourier_config_binding_rejections")...) {
		if !strings.HasSuffix(s, " 0") {
			t.Errorf("%s after an empty report; want 0", s)
		}
	}
}

// With no report, as with files, the gauges read 0 once and are never nonzero.
func TestConfigGaugesStartAtZero(t *testing.T) {
	m := mustMetrics(t, nil)
	if got := gauges(t, m, "agentkourier_config_invalid_objects"); len(got) != 3 {
		t.Errorf("invalid objects series = %v; want one per kind", got)
	}
}

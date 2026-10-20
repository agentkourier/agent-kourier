package broker

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// interactions sets the interactions of sre/oncall in a resolved snapshot, which the test owns.
func interactions(t *testing.T, f *fixture, in v1alpha1.Interactions) {
	t.Helper()
	snap := snapshot(t, f.url, v1alpha1.DialectKagentV1, time.Hour)
	rb, ok := snap.Binding("sre", "oncall")
	if !ok {
		t.Fatal("the fixture has no sre/oncall")
	}
	rb.Binding.Spec.Interactions = in
	f.snaps.p.Store(snap)
}

// The Binding's chat output mode reaches the session, which tells the sink of each turn: dropped here,
// every Binding streams, whatever its spec says (S5). Every field of session.Backend is set from the spec.
func TestBackendCarriesTheBindingsOutputMode(t *testing.T) {
	f := newFixture(t)
	for _, mode := range []v1alpha1.OutputMode{v1alpha1.OutputFinal, v1alpha1.OutputLive} {
		snap := snapshot(t, f.url, v1alpha1.DialectKagentV1, time.Hour)
		rb, _ := snap.Binding("sre", "oncall")
		rb.Binding.Spec.Chat.Output = mode
		f.snaps.p.Store(snap)

		be, err := f.b.Backend("sre/oncall")

		if err != nil || be.Output != mode {
			t.Errorf("Backend.Output = %q, %v; want %q from spec.chat.output", be.Output, err, mode)
		}
	}
}

// The Binding's chat.progress reaches the session through Backend, and a reload changes it for the next turn.
func TestBackendCarriesTheBindingsProgress(t *testing.T) {
	f := newFixture(t)
	for _, mode := range []v1alpha1.ProgressMode{v1alpha1.ProgressActivity, v1alpha1.ProgressSteps, v1alpha1.ProgressStepsAndActivity} {
		snap := snapshot(t, f.url, v1alpha1.DialectKagentV1, time.Hour)
		rb, _ := snap.Binding("sre", "oncall")
		rb.Binding.Spec.Chat.Progress = mode
		f.snaps.p.Store(snap)

		be, err := f.b.Backend("sre/oncall")

		if err != nil || be.Progress != mode {
			t.Errorf("Backend.Progress = %q, %v; want %q from spec.chat.progress", be.Progress, err, mode)
		}
	}
}

func TestBindingCarriesWhatTheInteractionsNeed(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		ask       bool
		approvals bool
		groups    []string
	}{
		{ask: false},
		{ask: true},
		{ask: true, approvals: true, groups: []string{"S0123456789", "S9876543210"}},
	} {
		interactions(t, f, v1alpha1.Interactions{AskUser: tc.ask, ToolApprovals: tc.approvals, ApproverGroups: tc.groups, Timeout: metav1.Duration{Duration: 7 * time.Minute}})
		got, err := f.b.Binding("sre/oncall")
		if err != nil {
			t.Fatal(err)
		}
		if got.AskUser != tc.ask || got.ToolApprovals != tc.approvals || !slices.Equal(got.ApproverGroups, tc.groups) || got.Timeout != 7*time.Minute {
			t.Errorf("Binding = %+v, want AskUser %t, ToolApprovals %t, groups %v and the 7m timeout of the spec", got, tc.ask, tc.approvals, tc.groups)
		}
	}

	got, err := f.b.Binding("sre/oncall")
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity != "secret:sre/creds" {
		t.Errorf("Identity = %q, want the token Secret, as the audit rows of Backend name it", got.Identity)
	}
	if got.Dialect == nil || got.Dialect != f.b.kits[v1alpha1.DialectKagentV1].dialect {
		t.Errorf("Dialect = %v, want the dialect the agent pool of that backend connects through", got.Dialect)
	}
	if want := (dialect.Platform{ID: "slack", Name: "Slack"}); got.Platform != want {
		t.Errorf("Platform = %+v, want %+v: the platform of the Binding's ChatConnection", got.Platform, want)
	}
}

// A Binding that left the config is the sentinel the session manager already uses, so that a caller can
// tell it from a config that has not loaded; and a reload takes effect on the next call.
func TestBindingNotFoundIsTheSentinelAndReloadsApply(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"sre/nope", "nope/oncall", "oncall", "", "sre/oncall/extra"} {
		if _, err := f.b.Binding(name); !errors.Is(err, session.ErrUnknownBinding) {
			t.Errorf("Binding(%q) = %v, want an error wrapping ErrUnknownBinding", name, err)
		}
	}
	interactions(t, f, v1alpha1.Interactions{Timeout: metav1.Duration{Duration: 9 * time.Minute}})
	if got, err := f.b.Binding("sre/oncall"); err != nil || got.Timeout != 9*time.Minute {
		t.Errorf("after a reload Binding = %+v, %v; want the new 9m timeout", got, err)
	}

	f.snaps.p.Store(nil)
	if _, err := f.b.Binding("sre/oncall"); err == nil || errors.Is(err, session.ErrUnknownBinding) {
		t.Errorf("Binding before the first load = %v, want an error that is not ErrUnknownBinding", err)
	}
}

func TestBindingRefusesADialectThatIsNotBuilt(t *testing.T) {
	f := newFixture(t)
	snap := snapshot(t, f.url, v1alpha1.DialectKagentV1, time.Hour)
	rb, _ := snap.Binding("sre", "oncall")
	rb.Backend.Spec.Dialect = "unknown"
	f.snaps.p.Store(snap)

	_, err := f.b.Binding("sre/oncall")

	if err == nil || errors.Is(err, session.ErrUnknownBinding) || !strings.Contains(err.Error(), `"unknown"`) {
		t.Errorf("Binding of an a2a Binding = %v, want an error naming the dialect that is not the sentinel", err)
	}
}

// feedback.enabled of the Binding reaches the session as Backend.Feedback, which makes the final message of an answer carry the
// buttons (spec 8.7).
func TestBackendCarriesTheBindingsFeedbackSetting(t *testing.T) {
	f := newFixture(t)
	for _, enabled := range []bool{true, false} {
		snap := snapshot(t, f.url, v1alpha1.DialectKagentV1, time.Hour)
		rb, _ := snap.Binding("sre", "oncall")
		rb.Binding.Spec.Feedback.Enabled = enabled
		f.snaps.p.Store(snap)

		be, err := f.b.Backend("sre/oncall")

		if err != nil || be.Feedback != enabled {
			t.Errorf("Backend.Feedback = %t, %v; want %t from spec.feedback.enabled", be.Feedback, err, enabled)
		}
	}
}

package broker

import (
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
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

func TestBindingCarriesWhatTheInteractionsNeed(t *testing.T) {
	f := newFixture(t)
	// Each flag on its own, so that a swap of the two is seen.
	for _, flags := range []struct{ ask, tools bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		interactions(t, f, v1alpha1.Interactions{AskUser: flags.ask, ToolApprovals: flags.tools, Timeout: metav1.Duration{Duration: 7 * time.Minute}})
		got, err := f.b.Binding("sre/oncall")
		if err != nil {
			t.Fatal(err)
		}
		if got.AskUser != flags.ask || got.ToolApprovals != flags.tools || got.Timeout != 7*time.Minute {
			t.Errorf("Binding = %+v, want AskUser %t, ToolApprovals %t and the 7m timeout of the spec", got, flags.ask, flags.tools)
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
	rb.Backend.Spec.Dialect = v1alpha1.DialectA2A
	f.snaps.p.Store(snap)

	_, err := f.b.Binding("sre/oncall")

	if err == nil || errors.Is(err, session.ErrUnknownBinding) || !strings.Contains(err.Error(), `"a2a"`) {
		t.Errorf("Binding of an a2a Binding = %v, want an error naming the dialect that is not the sentinel", err)
	}
}

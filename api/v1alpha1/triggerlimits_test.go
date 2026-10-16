package v1alpha1

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func i32(n int32) *int32 { return &n }

func rate(maxRuns int32, per time.Duration, maxConcurrent int32) *RateLimit {
	return &RateLimit{MaxRuns: maxRuns, Per: metav1.Duration{Duration: per}, MaxConcurrent: maxConcurrent}
}

// A botMessage trigger has no default limits: it loads only with a rate limit (all three fields) and a daily cap,
// and the message names the field that is missing and where the example is.
func TestABotMessageTriggerMustSetItsLimits(t *testing.T) {
	const at = "spec.chat.triggers[1].limits"
	const hint = "see the example in how-to/investigate-alertmanager-alerts (https://agentkourier.dev/how-to/investigate-alertmanager-alerts/)"
	runMutations[Binding](t, "binding.yaml", []mutation[Binding]{
		{"both set", func(*Binding) {}, ""},
		{"no rate limit", func(o *Binding) { bot(o).Limits.RateLimit = nil },
			at + ".rateLimit: required: set maxRuns, per and maxConcurrent; " + hint},
		{"no daily cap", func(o *Binding) { bot(o).Limits.MaxRunsPerDay = nil },
			at + ".maxRunsPerDay: required: set the cap for a day, or 0 for no cap; " + hint},
		{"an explicit daily cap of 0 is the written choice of no cap", func(o *Binding) { bot(o).Limits.MaxRunsPerDay = i32(0) }, ""},
	})

	b := load[Binding](t, "binding.yaml")
	bot(b).Limits = TriggerLimits{}
	want := []string{
		at + ".rateLimit: required: set maxRuns, per and maxConcurrent; " + hint,
		at + ".maxRunsPerDay: required: set the cap for a day, or 0 for no cap; " + hint,
	}
	if got := problemsOf(b.Validate()); !reflect.DeepEqual(got, want) {
		t.Errorf("a trigger with no limits: problems = %q, want %q", got, want)
	}
}

func TestLimitsAreReadFromYAML(t *testing.T) {
	var tr Trigger
	const doc = "type: botMessage\nlimits:\n  rateLimit: {maxRuns: 2, per: 30s, maxConcurrent: 1}\n  maxRunsPerDay: 0\n"
	if err := yaml.UnmarshalStrict([]byte(doc), &tr); err != nil {
		t.Fatal(err)
	}
	want := TriggerLimits{RateLimit: rate(2, 30*time.Second, 1), MaxRunsPerDay: i32(0)}
	if !reflect.DeepEqual(tr.Limits, want) {
		t.Errorf("limits = %+v, want %+v", tr.Limits, want)
	}
	if out, err := yaml.Marshal(Trigger{Type: TriggerMention}); err != nil || string(out) != "type: mention\n" {
		t.Errorf("a trigger with no limits encodes as %q, %v; want the field left out", out, err)
	}
}

func TestDigestWindow(t *testing.T) {
	for _, tt := range []struct {
		name string
		l    TriggerLimits
		want time.Duration
	}{
		{"the rate window", TriggerLimits{RateLimit: rate(5, 10*time.Minute, 2)}, 10 * time.Minute},
		{"a daily cap only", TriggerLimits{MaxRunsPerDay: i32(5)}, DefaultDigestWindow},
		{"none", TriggerLimits{}, DefaultDigestWindow},
	} {
		if got := tt.l.DigestWindow(); got != tt.want {
			t.Errorf("%s: DigestWindow() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestTriggerLimitsValidate(t *testing.T) {
	const at = "spec.chat.triggers[1].limits"
	runMutations[Binding](t, "binding.yaml", []mutation[Binding]{
		{"the fixture's limits are valid", func(*Binding) {}, ""},
		{"a different cap and rate", func(o *Binding) {
			bot(o).Limits = TriggerLimits{RateLimit: rate(1, time.Hour, 1), MaxRunsPerDay: i32(3)}
		}, ""},
		{"maxRuns zero", func(o *Binding) { bot(o).Limits.RateLimit.MaxRuns = 0 }, at + ".rateLimit.maxRuns: must be positive"},
		{"maxRuns negative", func(o *Binding) { bot(o).Limits.RateLimit.MaxRuns = -1 }, at + ".rateLimit.maxRuns: must be positive"},
		{"maxConcurrent zero", func(o *Binding) { bot(o).Limits.RateLimit.MaxConcurrent = 0 }, at + ".rateLimit.maxConcurrent: must be positive"},
		{"per under the floor", func(o *Binding) { bot(o).Limits.RateLimit.Per.Duration = 9 * time.Second },
			at + ".rateLimit.per: 9s is out of range, must be between 10s and 24h0m0s"},
		{"per at the floor", func(o *Binding) { bot(o).Limits.RateLimit.Per.Duration = MinRateWindow }, ""},
		{"per at the ceiling", func(o *Binding) { bot(o).Limits.RateLimit.Per.Duration = MaxLimitWindow }, ""},
		{"per over the ceiling", func(o *Binding) { bot(o).Limits.RateLimit.Per.Duration = 25 * time.Hour },
			at + ".rateLimit.per: 25h0m0s is out of range, must be between 10s and 24h0m0s"},
		{"per negative", func(o *Binding) { bot(o).Limits.RateLimit.Per.Duration = -time.Minute },
			at + ".rateLimit.per: -1m0s is out of range, must be between 10s and 24h0m0s"},
		{"maxRunsPerDay negative", func(o *Binding) { *bot(o).Limits.MaxRunsPerDay = -1 },
			at + ".maxRunsPerDay: must not be negative; 0 means no cap"},
		{"maxRunsPerDay zero is no cap", func(o *Binding) { *bot(o).Limits.MaxRunsPerDay = 0 }, ""},
		{"a mention trigger with limits", func(o *Binding) { o.Spec.Chat.Triggers[0].Limits = TriggerLimits{MaxRunsPerDay: i32(1)} },
			"spec.chat.triggers[0].limits: not allowed with type: mention"},
	})
}

// A rate limit set in part is reported field by field: nothing fills in what it leaves out.
func TestRateLimitSetInPartIsReportedPerField(t *testing.T) {
	b := load[Binding](t, "binding.yaml")
	bot(b).Limits.RateLimit = rate(3, 0, 0)
	want := []string{
		"spec.chat.triggers[1].limits.rateLimit.maxConcurrent: must be positive",
		"spec.chat.triggers[1].limits.rateLimit.per: 0s is out of range, must be between 10s and 24h0m0s",
	}
	if got := problemsOf(b.Validate()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Validate() problems = %q, want %q", got, want)
	}
}

// The spec 7 example's limits are the ones it writes: nothing is merged into them.
func TestTheSpecExampleKeepsItsOwnLimits(t *testing.T) {
	b := load[Binding](t, "binding.yaml")
	want := TriggerLimits{RateLimit: rate(5, 10*time.Minute, 2), MaxRunsPerDay: i32(20)}
	if got := bot(b).Limits; !reflect.DeepEqual(got, want) {
		t.Errorf("limits = %+v (rate %+v), want %+v", got, got.RateLimit, want)
	}
}

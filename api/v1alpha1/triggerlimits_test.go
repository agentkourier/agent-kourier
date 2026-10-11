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

// The alertmanager preset limits a trigger to five starts in 10 minutes, two at a time, and 40 a day, and a Binding
// that sets nothing gets them.
func TestAlertmanagerPresetHasLimits(t *testing.T) {
	want := TriggerLimits{RateLimit: rate(5, 10*time.Minute, 2), MaxRunsPerDay: i32(40)}
	if got := alertmanagerTrigger().Limits; !reflect.DeepEqual(got, want) {
		t.Errorf("preset limits = %+v, want %+v", got, want)
	}
	b := load[Binding](t, "binding-minimal.yaml")
	b.Spec.Chat.Triggers = []Trigger{{Type: TriggerBotMessage, From: &BotSource{BotID: "B0EXAMPLE01"}, Preset: PresetAlertmanager}}
	b.Default()
	if err := b.Validate(); err != nil {
		t.Fatalf("a bare alertmanager trigger is invalid: %v", err)
	}
	if got := b.Spec.Chat.Triggers[0].Limits; !reflect.DeepEqual(got, want) {
		t.Errorf("limits after Default = %+v, want the preset's %+v", got, want)
	}
}

func TestPresetLimitsMergeFieldByField(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  TriggerLimits
		want TriggerLimits
	}{
		{"nothing set", TriggerLimits{}, TriggerLimits{RateLimit: rate(5, 10*time.Minute, 2), MaxRunsPerDay: i32(40)}},
		{"maxRuns only", TriggerLimits{RateLimit: rate(3, 0, 0)}, TriggerLimits{RateLimit: rate(3, 10*time.Minute, 2), MaxRunsPerDay: i32(40)}},
		{"per only", TriggerLimits{RateLimit: rate(0, time.Hour, 0)}, TriggerLimits{RateLimit: rate(5, time.Hour, 2), MaxRunsPerDay: i32(40)}},
		{"maxConcurrent only", TriggerLimits{RateLimit: rate(0, 0, 1)}, TriggerLimits{RateLimit: rate(5, 10*time.Minute, 1), MaxRunsPerDay: i32(40)}},
		{"all of rateLimit", TriggerLimits{RateLimit: rate(9, time.Minute, 4)}, TriggerLimits{RateLimit: rate(9, time.Minute, 4), MaxRunsPerDay: i32(40)}},
		{"the daily cap only", TriggerLimits{MaxRunsPerDay: i32(7)}, TriggerLimits{RateLimit: rate(5, 10*time.Minute, 2), MaxRunsPerDay: i32(7)}},
		{"a daily cap of 0 is no cap, not unset", TriggerLimits{MaxRunsPerDay: i32(0)}, TriggerLimits{RateLimit: rate(5, 10*time.Minute, 2), MaxRunsPerDay: i32(0)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := Trigger{Type: TriggerBotMessage, From: &BotSource{BotID: "B0EXAMPLE01"}, Preset: PresetAlertmanager, Limits: tt.set}
			tr.applyPreset()
			if !reflect.DeepEqual(tr.Limits, tt.want) {
				t.Errorf("limits = %+v (rate %+v), want %+v (rate %+v)", tr.Limits, tr.Limits.RateLimit, tt.want, tt.want.RateLimit)
			}
			once := tr
			tr.applyPreset()
			if !reflect.DeepEqual(once.Limits, tr.Limits) {
				t.Errorf("applying the preset twice changed the limits: %+v then %+v", once.Limits, tr.Limits)
			}
		})
	}
}

// A trigger shares nothing with the preset or with the trigger the Binding wrote, so changing one trigger's
// limits at runtime (a test, a reload) cannot change another's.
func TestPresetLimitsAreCopies(t *testing.T) {
	a, b := alertmanagerTrigger(), alertmanagerTrigger()
	a.Limits.RateLimit.MaxRuns = 99
	*a.Limits.MaxRunsPerDay = 99
	if got := b.Limits; got.RateLimit.MaxRuns != 5 || *got.MaxRunsPerDay != 40 {
		t.Errorf("two triggers share their limits: %+v", got)
	}

	set := TriggerLimits{RateLimit: rate(3, 0, 0), MaxRunsPerDay: i32(7)}
	tr := Trigger{Type: TriggerBotMessage, From: &BotSource{BotID: "B0EXAMPLE01"}, Preset: PresetAlertmanager, Limits: set}
	tr.applyPreset()
	tr.Limits.RateLimit.MaxRuns, *tr.Limits.MaxRunsPerDay = 99, 99
	if set.RateLimit.MaxRuns != 3 || *set.MaxRunsPerDay != 7 {
		t.Errorf("the defaulted trigger shares its limits with what the Binding wrote: %+v", set)
	}
}

// A trigger with no preset takes no default: it is unlimited unless the Binding limits it.
func TestNoPresetNoLimits(t *testing.T) {
	tr := Trigger{Type: TriggerBotMessage, From: &BotSource{BotID: "B0EXAMPLE01"}}
	tr.applyPreset()
	if !tr.Limits.IsZero() {
		t.Errorf("a trigger with no preset got limits %+v", tr.Limits)
	}
	tr.Limits = TriggerLimits{MaxRunsPerDay: i32(3)}
	tr.applyPreset()
	if tr.Limits.RateLimit != nil || *tr.Limits.MaxRunsPerDay != 3 {
		t.Errorf("limits = %+v; want only the daily cap the Binding set", tr.Limits)
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
		{"a Binding that overrides one limit", func(o *Binding) { bot(o).Limits = TriggerLimits{MaxRunsPerDay: i32(3)} }, ""},
		{"no limits at all, without a preset", func(o *Binding) { bot(o).Preset, bot(o).Limits = "", TriggerLimits{} }, ""},
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

// Without a preset nothing fills in what a rate limit leaves out, so it is reported field by field.
func TestRateLimitSetInPartWithoutAPresetIsReportedPerField(t *testing.T) {
	b := load[Binding](t, "binding.yaml")
	bot(b).Preset, bot(b).Limits = "", TriggerLimits{RateLimit: rate(3, 0, 0)}
	want := []string{
		"spec.chat.triggers[1].limits.rateLimit.maxConcurrent: must be positive",
		"spec.chat.triggers[1].limits.rateLimit.per: 0s is out of range, must be between 10s and 24h0m0s",
	}
	if got := problemsOf(b.Validate()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Validate() problems = %q, want %q", got, want)
	}
}

// The spec 7 example overrides the daily cap and nothing else: the rate limit it gets is the preset's.
func TestTheSpecExampleOverridesOnlyTheDailyCap(t *testing.T) {
	b := load[Binding](t, "binding.yaml")
	want := TriggerLimits{RateLimit: rate(5, 10*time.Minute, 2), MaxRunsPerDay: i32(20)}
	if got := bot(b).Limits; !reflect.DeepEqual(got, want) {
		t.Errorf("limits = %+v (rate %+v), want %+v", got, got.RateLimit, want)
	}
}

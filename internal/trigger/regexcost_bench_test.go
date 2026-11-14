package trigger_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/trigger"
)

// BenchmarkRegexCost measures what the limits on a trigger's regexes allow, on the text the documentation names: fields of
// 16 KiB, which is all a regex reads. It is a benchmark, so `make test` does not run it; to measure your own hardware:
//
//	go test ./internal/trigger -run '^$' -bench BenchmarkRegexCost -benchtime 3x
//
// Each case is one Classify of a message whose six fields are the text, by the intake's own matcher:
//   - one_regex_miss: one regex of about 2000 instructions that finds no match in the text, the cost of a regex that fails.
//   - one_regex_hit: the same regex, finding its match at the last byte.
//   - binding_at_budget: a Binding at its budget of 5000 instructions: two match regexes of about 2000, each finding its match at
//     the last byte, and an exclude of about 1000 that finds none, so every one of them runs over the whole field.
//
// Each is run with the dot (`.{0,n}`) and with a Unicode class (`\pL{0,n}`), which costs more. The cost of a search is the
// instructions of its program times the length of the text, so the Binding takes about as long as its instructions say, and
// the time rises when the CPU is shared: read the load average beside the figures, and run them more than once.
func BenchmarkRegexCost(b *testing.B) {
	const size = 16 << 10
	miss := strings.Repeat("a", size)
	hit := strings.Repeat("a", size-1) + "Z"
	for _, kind := range []struct {
		name      string
		big, half string // the regexes of about 2000 and about 1000 instructions
	}{
		{"dot", `.{0,997}Z`, `.{0,497}Z`},
		{"unicode", `\pL{0,990}Z`, `\pL{0,495}Z`},
	} {
		run := func(name string, text string, match map[v1alpha1.MessageField]string, exclude map[v1alpha1.MessageField]string) {
			binding := benchBinding(b, match, exclude)
			replay, err := trigger.NewReplay(binding)
			if err != nil {
				b.Fatal(err)
			}
			m := botInbound("C1", "1.1", chat.BotMessage{ID: webhookBot, Title: text, Link: text, Fallback: text, Footer: text, Body: text})
			m.Text = text
			b.Run(kind.name+"/"+name, func(b *testing.B) {
				for range b.N {
					replay.Classify(chat.HistoryMessage{Message: m})
				}
			})
		}
		one := map[v1alpha1.MessageField]string{v1alpha1.FieldText: kind.big}
		run("one_regex_miss", miss, one, nil)
		run("one_regex_hit", hit, one, nil)
		run("binding_at_budget", hit,
			map[v1alpha1.MessageField]string{v1alpha1.FieldText: kind.big, v1alpha1.FieldTitle: kind.big},
			map[v1alpha1.MessageField]string{v1alpha1.FieldBody: strings.Replace(kind.half, "Z", "Y", 1)})
	}
}

// benchBinding is a Binding with one botMessage trigger of the sender webhookBot, checked by Validate, so that a benchmark
// cannot measure a Binding that the limits would refuse. The exclude regex ends in a byte that the text lacks, so it
// does not match and the trigger takes the message after reading every field it names.
func benchBinding(b *testing.B, match, exclude map[v1alpha1.MessageField]string) *v1alpha1.Binding {
	b.Helper()
	zero := int32(0)
	binding := &v1alpha1.Binding{ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "bench"}}
	binding.Spec.Agent = v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Name: "backend"}, Namespace: "kagent", Name: "agent"}
	binding.Spec.Identity.UserID = "agent-kourier"
	binding.Spec.Chat.ConnectionRef = v1alpha1.ObjectRef{Namespace: "agent-kourier", Name: "slack"}
	binding.Spec.Chat.Channel = "C1"
	binding.Spec.Chat.Triggers = []v1alpha1.Trigger{{
		Type: v1alpha1.TriggerBotMessage, From: &v1alpha1.BotSource{ID: webhookBot}, Match: match, Exclude: exclude, PromptTemplate: "An alert.",
		Limits: v1alpha1.TriggerLimits{RateLimit: &v1alpha1.RateLimit{MaxRuns: 1, Per: metav1.Duration{Duration: 10 * time.Minute}, MaxConcurrent: 1}, MaxRunsPerDay: &zero},
	}}
	binding.Default()
	if err := binding.Validate(); err != nil {
		b.Fatalf("the Binding of the benchmark is refused: %v", err)
	}
	return binding
}

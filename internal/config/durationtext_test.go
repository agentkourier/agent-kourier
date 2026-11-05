package config

import (
	"context"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// The text of a duration is capped at 64 characters before it becomes a Duration, on the file route and on the custom resource
// route alike, and the refusal shows no value. A duration of 64 characters loads.
func TestDurationTextIsCappedOnBothRoutes(t *testing.T) {
	long := strings.Repeat("0h", 33) + "1h"  // 68 characters, and 1h
	atCap := strings.Repeat("0h", 31) + "1h" // 64 characters
	for _, tc := range []struct{ name, field, path string }{
		{"timeout", "  interactions: { timeout: %q }\n", "spec.interactions.timeout"},
		{"threadTTL", "  session: { threadTTL: %q }\n", "spec.session.threadTTL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := func(text string) string { return bindingDoc + strings.Replace(tc.field, "%q", `"`+text+`"`, 1) }
			want := tc.path + ": is 68 characters, longer than the 64 character limit"

			// The file route.
			if _, err := Load(context.Background(), write(t, map[string]string{"b.yaml": doc(atCap)})); err != nil {
				t.Errorf("Load of a duration of 64 characters: %v", err)
			}
			_, err := Load(context.Background(), write(t, map[string]string{"b.yaml": doc(long)}))
			if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), long) {
				t.Errorf("Load of a duration of 68 characters = %v; want %q and no value", err, want)
			}

			// The custom resource route.
			if obj, _, errs := DecodeObject[v1alpha1.Binding](asJSON(t, doc(atCap))); obj == nil {
				t.Errorf("DecodeObject of a duration of 64 characters: %v", errs)
			}
			obj, _, errs := DecodeObject[v1alpha1.Binding](asJSON(t, doc(long)))
			var msgs []string
			for _, e := range errs {
				msgs = append(msgs, e.Error())
			}
			if obj != nil || !strings.Contains(strings.Join(msgs, "\n"), want) || strings.Contains(strings.Join(msgs, "\n"), long) {
				t.Errorf("DecodeObject of a duration of 68 characters = %v, %q; want no object and %q with no value", obj, msgs, want)
			}
		})
	}
}

// A trigger's durations are read at their own index, on both routes.
func TestDurationTextOfATriggerIsCapped(t *testing.T) {
	long := strings.Repeat("0h", 33) + "2h"
	trigger := `  chat:
    connectionRef: { name: slack-main }
    channel: C0123456789
    triggers:
      - { type: mention }
      - type: botMessage
        from: { id: B0123456789 }
        extract: { key: { field: title, regex: "(?P<key>.+)" } }
        thread: { by: key, cooldown: "` + long + `" }
        limits: { rateLimit: { maxRuns: 1, per: "` + long + `", maxConcurrent: 1 }, maxRunsPerDay: 0 }
        promptTemplate: "x"
`
	doc := strings.Replace(bindingDoc, "  chat: { connectionRef: { name: slack-main }, channel: C0123456789 }\n", trigger, 1)
	_, _, errs := DecodeObject[v1alpha1.Binding](asJSON(t, doc))
	var msgs []string
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	for _, want := range []string{
		"spec.chat.triggers[1].thread.cooldown: is 68 characters, longer than the 64 character limit",
		"spec.chat.triggers[1].limits.rateLimit.per: is 68 characters, longer than the 64 character limit",
	} {
		if !strings.Contains(strings.Join(msgs, "\n"), want) {
			t.Errorf("DecodeObject problems = %q; want %q", msgs, want)
		}
	}
}

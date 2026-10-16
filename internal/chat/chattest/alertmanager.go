package chattest

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// AlertmanagerTrigger returns a botMessage trigger for Alertmanager's Slack receiver posted by botID, written in
// full: the trigger the Alertmanager how-to documents. It matches a title that reads "FIRING (n) - <alertname>" or
// "RESOLVED - <alertname>", optionally behind a bracketed tag such as "[kind] ", and a fallback that starts
// "[FIRING:n]" or "[RESOLVED]". It extracts the state, the thread key (the group label values of the fallback,
// without the count) and the alertname. The thread has a 4h cooldown and closes on RESOLVED; the limits are 5
// starts in 10 minutes, 2 at once and 40 a day. Each call builds a new trigger, so a test may change its maps.
func AlertmanagerTrigger(botID string) v1alpha1.Trigger {
	const (
		tag   = `(?:\[[^\]]{1,32}\] )?` // an optional title prefix, at most 32 bytes
		alert = `(?:FIRING \(\d+\)|RESOLVED)`
	)
	maxPerDay := int32(40)
	return v1alpha1.Trigger{
		Type: v1alpha1.TriggerBotMessage,
		From: &v1alpha1.BotSource{BotID: botID},
		Match: map[v1alpha1.MessageField]string{
			v1alpha1.FieldTitle:    `^` + tag + alert + ` - `,
			v1alpha1.FieldFallback: `^\[(?:FIRING(?::\d+)?|RESOLVED)\] `,
		},
		Extract: map[string]v1alpha1.Extract{
			"state":     {Field: v1alpha1.FieldTitle, Regex: `^` + tag + `(?P<state>FIRING|RESOLVED)`},
			"key":       {Field: v1alpha1.FieldFallback, Regex: `^\[[A-Z]+(?::\d+)?\] (?P<key>[^(|]*[^(|\s])`},
			"alertname": {Field: v1alpha1.FieldTitle, Regex: `^` + tag + alert + ` - (?P<alertname>.+)$`},
		},
		Thread: v1alpha1.TriggerThread{
			By:        v1alpha1.ThreadByKey,
			Cooldown:  metav1.Duration{Duration: 4 * time.Hour},
			CloseWhen: map[string]string{"state": "RESOLVED"},
		},
		Limits: v1alpha1.TriggerLimits{
			RateLimit:     &v1alpha1.RateLimit{MaxRuns: 5, Per: metav1.Duration{Duration: 10 * time.Minute}, MaxConcurrent: 2},
			MaxRunsPerDay: &maxPerDay,
		},
		PromptTemplate: `An alert is {{ .State }} in channel {{ .Channel }}: {{ .Fields.alertname }}.
Investigate it with your read-only tools. Report the likely cause, the evidence you
checked, and the next steps a person should take.
`,
	}
}

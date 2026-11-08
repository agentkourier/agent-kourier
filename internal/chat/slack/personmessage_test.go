package slack_test

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// person is a personMessage trigger with the limits every such trigger must set.
func person() v1alpha1.Trigger {
	return v1alpha1.Trigger{Type: v1alpha1.TriggerPersonMessage, PromptTemplate: "A person wrote.",
		Exclude: map[v1alpha1.MessageField]string{v1alpha1.FieldText: `^thanks\b`},
		Limits: v1alpha1.TriggerLimits{
			RateLimit:     &v1alpha1.RateLimit{MaxRuns: 1000, Per: metav1.Duration{Duration: time.Hour}, MaxConcurrent: 1000},
			MaxRunsPerDay: new(int32(0)),
		}}
}

// A person's top-level message goes through the adapter and the trigger table: it matches a personMessage trigger, anchored to
// itself, with the text the trigger reads. What the adapter drops, the table never sees: Agent Kourier's own post, a bot's
// post that no trigger names, an edit, and a message with no text. An exclude turns a
// message away. A message that mentions Agent Kourier arrives as two events, and is one start.
func TestFromAPersonToAMatchedPersonMessageTrigger(t *testing.T) {
	bi := startBotIntake(t, person())
	edit := map[string]any{"user": "U1", "text": "edited", "ts": "1.1"}
	events := []map[string]any{
		message(map[string]any{"ts": "2.1", "event_ts": "2.1", "text": "checkout is down"}),
		message(map[string]any{"ts": "2.2", "event_ts": "2.2", "text": "thanks, fixed"}),
		message(map[string]any{"ts": "2.3", "event_ts": "2.3", "text": "mine", "user": "UFAKEBOT"}),
		message(map[string]any{"ts": "2.4", "event_ts": "2.4", "text": "theirs", "user": "UOTHER", "bot_id": "BOTHER"}),
		message(map[string]any{"ts": "2.5", "event_ts": "2.5", "text": "", "subtype": "message_changed", "user": "", "message": edit, "previous_message": edit}),
		message(map[string]any{"ts": "2.6", "event_ts": "2.6", "text": ""}),
		message(map[string]any{"ts": "2.7", "event_ts": "2.7", "text": "<@UFAKEBOT> checkout is down"}),
		message(map[string]any{"ts": "2.7", "event_ts": "2.7", "text": "<@UFAKEBOT> checkout is down", "type": "app_mention", "channel_type": nil}),
	}
	var ids []string
	for _, ev := range events {
		ids = append(ids, send(t, bi.fake, ev))
	}
	waitAck(t, bi.fake, ids...)

	var anchors []string
	for _, m := range bi.got() {
		anchors = append(anchors, m.Message.At.ID)
		if m.Binding != "sre/sre-alerts" || m.Index != 0 || m.Mention || m.Message.Bot != nil || m.Message.User != "U1" || m.Message.At.Thread != "" {
			t.Errorf("match = %+v; want trigger 0 of sre/sre-alerts on a top-level message of U1 with no bot and no thread", m)
		}
	}
	if want := []string{"2.1", "2.7"}; !slices.Equal(anchors, want) {
		t.Errorf("turns started for %v; want %v: the message, and the mention once for its two events", anchors, want)
	}
	if n := bi.unmatched(t); n != 1 {
		t.Errorf("agentkourier_trigger_unmatched_total = %v; want 1, the one the exclude turned away", n)
	}
}

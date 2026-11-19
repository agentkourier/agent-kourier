package chat_test

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// Value reads a field by its name: a present field's value, and nothing for one that is Invalid, absent or not the event's.
func TestWebhookEventValueReadsAFieldByName(t *testing.T) {
	ev := &chat.WebhookEvent{Fields: []chat.WebhookField{
		{Name: "issue", Value: "OPS-7", ID: true, Present: true},
		{Name: "sha", ID: true, Invalid: true},
		{Name: "body"},
	}}
	for name, want := range map[string]string{"issue": "OPS-7", "sha": "", "body": "", "missing": ""} {
		if got := ev.Value(name); got != want {
			t.Errorf("Value(%q) = %q, want %q", name, got, want)
		}
	}
}

package slack_test

import (
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// RefAt gives back the Ref that Post returned, from what a caller recorded of it (the channel, the thread and the
// message's ID): so the outbox and the interactions, which record the ID alone, need write no platform field.
func TestRefAtIsThePostedRefFromWhatWasRecorded(t *testing.T) {
	w := newWorld(t)
	posted, err := w.p.Post(t.Context(), w.target, chat.Say(chat.Message{Fallback: "hi"}), "tag-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.p.RefAt(posted.Channel, posted.Thread, posted.ID); got != posted {
		t.Errorf("RefAt = %+v, want the posted %+v", got, posted)
	}
}

package outbox_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// A post that says what it says in the neutral model is stored as Slack's form of it, until the outbox stores the
// neutral model itself: the payload is what slack.Render makes, escaped and laid out, and a message that cannot be
// shown is invalid, which no retry mends.
func TestAPostInTheNeutralModelIsStoredAsSlacksFormOfIt(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	ob := newOutbox(t, st, &poster{})
	out := chat.Say(chat.Message{Fallback: "<!channel> hi", Parts: []chat.Part{chat.Body{Markdown: "*b* [x](y)"}}})

	if _, err := ob.Enqueue(t.Context(), outbox.Post{Source: "ev1", Role: store.OutboxNotice, Channel: "C1", Out: out}); err != nil {
		t.Fatal(err)
	}

	row, err := st.GetOutbox(t.Context(), "ev1/notice")
	if err != nil {
		t.Fatal(err)
	}
	want, err := slack.Render(out)
	if err != nil {
		t.Fatal(err)
	}
	var got slack.Content
	if err := json.Unmarshal([]byte(row.Payload), &got); err != nil || got.Text != want.Text || string(got.Blocks) != string(want.Blocks) || got.Text != "&lt;!channel&gt; hi" {
		t.Errorf("payload %q, %v; want Slack's rendering %+v", row.Payload, err, want)
	}
}

func TestAPostInTheNeutralModelThatCannotBeShownIsInvalid(t *testing.T) {
	ob := newOutbox(t, openStore(t, filepath.Join(t.TempDir(), "c.db")), &poster{})
	for name, out := range map[string]chat.Outgoing{
		"a message with nothing in it": chat.Say(chat.Message{}),
		"a version that is not known":  {V: 7, Message: &chat.Message{Fallback: "x"}},
	} {
		_, err := ob.Enqueue(t.Context(), outbox.Post{Source: "ev1", Role: store.OutboxNotice, Channel: "C1", Out: out})
		if !errors.Is(err, store.ErrInvalid) || !errors.Is(err, chat.ErrInvalid) {
			t.Errorf("%s: Enqueue = %v, want an error wrapping store.ErrInvalid and chat.ErrInvalid", name, err)
		}
	}
}

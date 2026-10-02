package outbox_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// A post is stored as the message in the neutral model, versioned, exactly as it was given: the outbox renders
// nothing, so what is stored holds no platform's markup, and what the platform is handed at delivery is what was
// stored.
func TestAPostIsStoredAsTheVersionedNeutralMessage(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	ob := newOutbox(t, st, &poster{})
	out := chat.Say(chat.Message{Fallback: "<!channel> hi", Parts: []chat.Part{chat.Body{Markdown: "*b* [x](y)"}, chat.Mention{User: "U1", Text: "said"}}})

	if _, err := ob.Enqueue(t.Context(), outbox.Post{Source: "ev1", Role: store.OutboxNotice, Channel: "C1", Out: out}); err != nil {
		t.Fatal(err)
	}

	row, err := st.GetOutbox(t.Context(), "ev1/notice")
	if err != nil {
		t.Fatal(err)
	}
	var got chat.Outgoing
	if err := json.Unmarshal([]byte(row.Payload), &got); err != nil || got.V != chat.OutgoingVersion || !reflect.DeepEqual(got, out) {
		t.Errorf("payload %s, %v; want the message as it was given, at version %d", row.Payload, err, chat.OutgoingVersion)
	}
	if strings.Contains(row.Payload, `"blocks"`) || strings.Contains(row.Payload, "&lt;") {
		t.Errorf("payload %s holds a platform's form of the message", row.Payload)
	}
}

// What the platform is handed at delivery is the stored message, and the tag is the row's ID.
func TestThePlatformIsHandedTheStoredMessage(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	p := &poster{}
	ob := newOutbox(t, st, p)
	out := chat.Ask(chat.Prompt{ID: "ix-1", Hint: "h", Questions: []chat.Question{{Text: "q", Choices: []string{"a", "b"}}}})
	ref, err := ob.Send(t.Context(), outbox.Post{Source: "ix-1", Role: store.OutboxReply, Channel: "C1", ThreadTS: "1.1", Out: out})
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sent) != 1 || !reflect.DeepEqual(p.sent[0], out) || p.tags[0] != "ix-1/reply" || p.targets[0] != (chat.Target{Channel: "C1", Thread: "1.1"}) {
		t.Errorf("posted %+v with tags %v to %v; want the stored message, tagged with the row's ID, to the row's place", p.sent, p.tags, p.targets)
	}
	if ref.Channel != "C1" || ref.Thread != "1.1" || ref.ID == "" {
		t.Errorf("Send returned %+v; want where the message landed", ref)
	}
}

// A message that cannot be shown, or a prompt that does not fit, is invalid where it is enqueued, and no retry mends it.
func TestAMessageThatCannotBeShownIsInvalid(t *testing.T) {
	p := &poster{fitsErr: chat.ErrTooLarge}
	ob := newOutbox(t, openStore(t, filepath.Join(t.TempDir(), "c.db")), p)
	for name, out := range map[string]chat.Outgoing{
		"a message with nothing in it": chat.Say(chat.Message{}),
		"a version that is not known":  {V: 7, Message: &chat.Message{Fallback: "x"}},
		"nothing at all":               {},
		"a prompt that does not fit":   chat.Ask(chat.Prompt{ID: "ix-1", Questions: []chat.Question{{Text: "q"}}}),
	} {
		for verb, call := range map[string]func() error{
			"Enqueue": func() error {
				_, err := ob.Enqueue(t.Context(), outbox.Post{Source: "ev1", Role: store.OutboxNotice, Channel: "C1", Out: out})
				return err
			},
			"Send": func() error {
				_, err := ob.Send(t.Context(), outbox.Post{Source: "ev1", Role: store.OutboxNotice, Channel: "C1", Out: out})
				return err
			},
		} {
			if err := call(); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %s = %v, want an error wrapping store.ErrInvalid", name, verb, err)
			}
		}
	}
	if posts, _ := p.counts(); posts != 0 {
		t.Errorf("%d posts of messages that cannot be shown", posts)
	}
}

// A row stored by the version before this one holds Slack's payload (text and blocks, no version). It is not migrated:
// it is abandoned as invalid, once, with a line in the log and without its text, and the platform is not called.
func TestALegacySlackPayloadIsAbandonedAsInvalid(t *testing.T) {
	for name, payload := range map[string]string{
		"slack content":         `{"text":"the old words","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"the old words"}}]}`,
		"slack text only":       `{"text":"the old words"}`,
		"another version":       `{"v":7,"message":{"fallback":"the old words"}}`,
		"a part not known":      `{"v":1,"message":{"parts":[{"kind":"hologram","text":"the old words"}]}}`,
		"not JSON":              `the old words`,
		"a message with no one": `{"v":1}`,
	} {
		st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
		if _, err := st.CreateOutbox(t.Context(), store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		p := &poster{}
		ob, hooks, logs := abandoning(t, st, p, nil)

		for range 2 { // a second drain finds nothing to do and says nothing more
			if err := ob.Reconcile(t.Context()); err != nil {
				t.Fatalf("%s: Reconcile = %v; an abandoned row is no drain failure", name, err)
			}
		}

		row, _ := st.GetOutbox(t.Context(), "ev1/reply")
		if posts, finds := p.counts(); posts != 0 || finds != 0 || row.AbandonedAt.IsZero() || row.Payload != "" || row.MessageRef != "" {
			t.Errorf("%s: %d posts, %d finds, row %+v; want it abandoned with no payload and the platform never called", name, posts, finds, row)
		}
		if got := hooks.list(); len(got) != 1 || got[0] != "ev1/reply "+outbox.AbandonInvalid {
			t.Errorf("%s: OnAbandon calls = %q; want one, as invalid", name, got)
		}
		if out := logs.String(); warnings(logs) != 1 || strings.Contains(out, "the old words") || !strings.Contains(out, "ev1/reply") {
			t.Errorf("%s: log = %q; want one warning that names the row and not its text", name, out)
		}
		if _, err := ob.Send(t.Context(), reply("ev1")); !errors.Is(err, outbox.ErrAbandoned) {
			t.Errorf("%s: Send of the same ID = %v; want ErrAbandoned: the words are posted anew only under a new source ID", name, err)
		}
	}
}

// The log line of an abandoned legacy payload says what it was, so that the owner who reads it knows the loss was the
// upgrade and not a fault.
func TestTheLogSaysThatALegacyPayloadWasNotMigrated(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	if _, err := st.CreateOutbox(t.Context(), store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1", CreatedAt: t0, Payload: `{"text":"hi"}`}); err != nil {
		t.Fatal(err)
	}
	ob, _, logs := abandoning(t, st, &poster{}, nil)
	if err := ob.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if out := logs.String(); !strings.Contains(out, "legacy") || !strings.Contains(out, "not migrated") {
		t.Errorf("log = %q; want it to say the payload was a legacy one and not migrated", out)
	}
}

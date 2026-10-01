package slack_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// newPoster returns a fake Slack and a Client over it that pages two messages at a time.
func newPoster(t *testing.T, opts ...fakeslack.Option) (*fakeslack.Server, *slack.Client) {
	t.Helper()
	fake := newFake(t, opts...)
	cfg := clientConfig(fake)
	cfg.PageLimit = 2
	return fake, slack.New(cfg)
}

func TestPostTagsTheMessage(t *testing.T) {
	fake, c := newPoster(t)
	blocks := json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"*bold*"}}]`)
	root, err := c.Post(t.Context(), "C1", "", slack.Content{Text: "root"}, "ev1/root")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := c.Post(t.Context(), "C1", root, slack.Content{Text: "reply", Blocks: blocks}, "ev1/reply")
	if err != nil {
		t.Fatal(err)
	}
	msgs := fake.Messages("C1")
	if len(msgs) != 2 || msgs[0].TS != root || msgs[1].TS != reply {
		t.Fatalf("stored %+v; want the root %s, then the reply %s", msgs, root, reply)
	}
	for i, id := range []string{"ev1/root", "ev1/reply"} {
		md := msgs[i].Metadata
		if md == nil || md.EventType != slack.MetadataEventType || md.EventPayload["outbox_id"] != id {
			t.Errorf("message %d metadata = %+v; want %s naming %s", i, md, slack.MetadataEventType, id)
		}
	}
	if msgs[1].ThreadTS != root || msgs[1].Text != "reply" || !strings.Contains(string(msgs[1].Blocks), "*bold*") {
		t.Errorf("reply = %+v; want it in the thread with its text and blocks", msgs[1])
	}
}

func TestPostRefusals(t *testing.T) {
	_, c := newPoster(t)
	tests := []struct {
		name, thread, id, blocks, want string
	}{
		{"no outbox ID: the post could not be found again", "", "", "", "outbox ID"},
		{"blocks that are not blocks", "", "ev1", "{", "blocks"},
		{"a thread that does not exist", "9.9", "ev1", "", "slack chat.postMessage: thread_not_found"},
	}
	for _, tt := range tests {
		_, err := c.Post(t.Context(), "C1", tt.thread, slack.Content{Text: "hi", Blocks: json.RawMessage(tt.blocks)}, tt.id)
		if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), botToken) {
			t.Errorf("%s: Post = %v; want an error with %q and no token", tt.name, err, tt.want)
		}
	}
}

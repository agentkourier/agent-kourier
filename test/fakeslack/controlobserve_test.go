package fakeslack_test

import (
	"net/url"
	"reflect"
	"testing"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// TestControlShowsEphemeralsAndNothingElseDoes: an ephemeral is for one user alone, so no read path
// of Slack, nor any other of the control surface, shows it. /_control/ephemerals is the one, and
// it is for e2e tests, which have no user to look through.
func TestControlShowsEphemeralsAndNothingElseDoes(t *testing.T) {
	fake, api := newFake(t)
	root := post(t, api, text("root"))
	if _, err := api.PostEphemeral("C1", "U1", text("answer every question"), slack.MsgOptionTS(root)); err != nil {
		t.Fatal(err)
	}
	var got []fakeslack.Ephemeral
	q := "?" + url.Values{"channel": {"C1"}}.Encode()
	if s := control(t, fake, "GET", "/_control/ephemerals"+q, nil, &got); s != 200 || len(got) != 1 ||
		got[0].Recipient != "U1" || got[0].Text != "answer every question" || got[0].ThreadTS != root {
		t.Fatalf("ephemerals = %d, %+v; want one for U1, in the thread", s, got)
	}
	var raw []map[string]any // the key is the contract, not only the Go field that round-trips it
	if s := control(t, fake, "GET", "/_control/ephemerals"+q, nil, &raw); s != 200 || len(raw) != 1 || raw[0]["recipient"] != "U1" {
		t.Fatalf("ephemerals as JSON = %d, %v; want a recipient key", s, raw)
	}
	for _, path := range []string{"/_control/messages" + q, "/_control/thread?" + url.Values{"channel": {"C1"}, "ts": {root}}.Encode()} {
		var msgs []fakeslack.MessageView
		if s := control(t, fake, "GET", path, nil, &msgs); s != 200 || len(msgs) != 1 || msgs[0].Text != "root" {
			t.Errorf("%s = %d, %+v; want only the root, with the ephemeral not in it", path, s, msgs)
		}
	}
	var none []fakeslack.Ephemeral
	if s := control(t, fake, "GET", "/_control/ephemerals?channel=C2", nil, &none); s != 200 || none == nil || len(none) != 0 {
		t.Errorf("ephemerals of another channel = %d, %#v; want an empty list, not null", s, none)
	}
	if s := control(t, fake, "GET", "/_control/ephemerals", nil, new(map[string]string)); s != 400 {
		t.Errorf("ephemerals with no channel = %d; want 400", s)
	}
}

func TestControlShowsReactions(t *testing.T) {
	fake, api := newFake(t)
	root := post(t, api, text("root"))
	path := "/_control/reactions?" + url.Values{"channel": {"C1"}, "ts": {root}}.Encode()
	var got []fakeslack.Reaction
	if s := control(t, fake, "GET", path, nil, &got); s != 200 || got == nil || len(got) != 0 {
		t.Fatalf("reactions of a message with none = %d, %#v; want an empty list, not null", s, got)
	}
	if err := api.AddReaction("hourglass", slack.ItemRef{Channel: "C1", Timestamp: root}); err != nil {
		t.Fatal(err)
	}
	want := []fakeslack.Reaction{{Name: "hourglass", Users: []string{"UFAKEBOT"}}}
	if s := control(t, fake, "GET", path, nil, &got); s != 200 || !reflect.DeepEqual(got, want) {
		t.Fatalf("reactions = %d, %+v; want %+v", s, got, want)
	}
	for name, p := range map[string]struct {
		path string
		want int
	}{
		"an unknown message": {"/_control/reactions?channel=C1&ts=1.1", 404},
		"no channel":         {"/_control/reactions?ts=" + root, 400},
		"no ts":              {"/_control/reactions?channel=C1", 400},
	} {
		if s := control(t, fake, "GET", p.path, nil, new(map[string]string)); s != p.want {
			t.Errorf("%s: status %d; want %d", name, s, p.want)
		}
	}
}

package fakeslack_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

func TestReactionsAddIsKeptOnTheMessage(t *testing.T) {
	fake, api := newFake(t)
	root := post(t, api, text("root"))
	if err := api.AddReaction("hourglass", slack.ItemRef{Channel: "C1", Timestamp: root}); err != nil {
		t.Fatalf("AddReaction = %v", err)
	}
	if err := api.AddReaction("eyes", slack.ItemRef{Channel: "C1", Timestamp: root}); err != nil {
		t.Fatalf("AddReaction = %v", err)
	}
	want := []fakeslack.Reaction{{Name: "hourglass", Users: []string{"UFAKEBOT"}}, {Name: "eyes", Users: []string{"UFAKEBOT"}}}
	got := fake.Reactions("C1", root)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Reactions = %+v; want %+v", got, want)
	}
	got[0].Users[0] = "changed" // a copy: the store does not change
	if again := fake.Reactions("C1", root); !reflect.DeepEqual(again, want) {
		t.Fatalf("Reactions after a change to a copy = %+v; want %+v", again, want)
	}
	if got := fake.Messages("C1")[0].Reactions; len(got) != 2 {
		t.Fatalf("the message holds %+v; want both reactions", got)
	}
	if n := len(fake.Reactions("C1", "1.1")); n != 0 {
		t.Fatalf("Reactions of an unknown message = %d; want none", n)
	}
}

func TestReactionsAddRefusesWhatSlackWould(t *testing.T) {
	_, api := newFake(t)
	root := post(t, api, text("root"))
	if err := api.AddReaction("hourglass", slack.ItemRef{Channel: "C1", Timestamp: root}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, reaction string
		item           slack.ItemRef
		want           string
	}{
		{"again", "hourglass", slack.ItemRef{Channel: "C1", Timestamp: root}, "already_reacted"},
		{"no message", "hourglass", slack.ItemRef{Channel: "C1", Timestamp: "1.1"}, "message_not_found"},
		{"no channel", "hourglass", slack.ItemRef{Timestamp: root}, "no_item_specified"},
		{"no timestamp", "hourglass", slack.ItemRef{Channel: "C1"}, "no_item_specified"},
		{"no name", "", slack.ItemRef{Channel: "C1", Timestamp: root}, "invalid_name"},
		{"a name with a colon", ":eyes:", slack.ItemRef{Channel: "C1", Timestamp: root}, "invalid_name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := api.AddReaction(tc.reaction, tc.item); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AddReaction = %v; want %s", err, tc.want)
			}
		})
	}
}

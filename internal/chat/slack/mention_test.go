package slack_test

import (
	"encoding/json"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// elementsOf is the elements of a context block.
func elementsOf(t *testing.T, b map[string]any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(b["elements"])
	if err != nil {
		t.Fatal(err)
	}
	var els []map[string]any
	if err := json.Unmarshal(raw, &els); err != nil {
		t.Fatal(err)
	}
	return els
}

// A Mention of a user ID of Slack's shape is a mention in an element of its own that holds nothing else, and the
// words are plain text beside it: nothing in them is markup.
func TestAMentionOfAValidUserIsOneMentionAndTheWordsArePlain(t *testing.T) {
	w := newWorld(t)
	blocks := w.post(t, chat.Say(chat.Message{Fallback: "x", Parts: []chat.Part{chat.Mention{User: "UALICE1", Text: "answered *first* <!here> & co"}}}))
	if len(blocks) != 1 || blocks[0]["type"] != "context" {
		t.Fatalf("blocks %v; want one context line", types(blocks))
	}
	els := elementsOf(t, blocks[0])
	if len(els) != 2 || els[0]["type"] != "mrkdwn" || els[0]["text"] != "<@UALICE1>" || els[1]["type"] != "plain_text" || els[1]["text"] != "answered *first* &lt;!here&gt; &amp; co" {
		t.Errorf("elements %v; want the mention alone in a mrkdwn element and the words in a plain_text one, with the entities", els)
	}
}

// A user that has not the shape of an ID is "someone", and what it held is shown nowhere: no mention, no token.
func TestAMentionOfAnythingElseIsSomeone(t *testing.T) {
	w := newWorld(t)
	for _, user := range []string{"", "alice", "<!channel>", "UALICE1>", "UALICE1\n<!here>", "u12345", "U1", "UALICE1 <@UBOB22>"} {
		blocks := w.post(t, chat.Say(chat.Message{Fallback: "x", Parts: []chat.Part{chat.Mention{User: user, Text: "answered first"}}}))
		els := elementsOf(t, blocks[0])
		if len(els) != 2 || els[0]["type"] != "plain_text" || els[0]["text"] != "someone" || els[1]["text"] != "answered first" {
			t.Errorf("user %q: elements %v; want %q and the words, as plain text", user, els, "someone")
		}
	}
}

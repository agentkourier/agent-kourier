package fakeslack_test

import (
	"encoding/json"
	"net/url"
	"testing"
)

// The blocks of the block_mismatch tests. A markdown block is text, as Slack stores it as rich_text.
const (
	markdownBlock = `{"type":"markdown","text":"body"}`
	richBlock     = `{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"body"}]}]}`
	contextBlock  = `{"type":"context","elements":[{"type":"plain_text","text":"Working"}]}`
	sectionBlock  = `{"type":"section","text":{"type":"plain_text","text":"s"}}`
	cardBlock     = `{"type":"task_card","task_id":"s1","title":"t","status":"complete"}`
)

// slackError is the error code of a response of the fake, "" for ok.
func slackError(t *testing.T, body string) string {
	t.Helper()
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return r.Error
}

// TestUpdateRefusesToDropText holds the fake to what Slack answered on 2026-11-03 in a sandbox workspace: an update whose
// blocks hold no text is block_mismatch on a message that holds only text, and every other update of the table passes.
func TestUpdateRefusesToDropText(t *testing.T) {
	const onlyText, mixed = "[" + markdownBlock + "]", "[" + markdownBlock + "," + contextBlock + "]"
	for _, tc := range []struct{ name, held, update, want string }{
		{"a card for text", onlyText, "[" + cardBlock + "]", "block_mismatch"},
		{"a context for text", onlyText, "[" + contextBlock + "]", "block_mismatch"},
		{"a section for text", onlyText, "[" + sectionBlock + "]", "block_mismatch"},
		{"a card for rich text", "[" + richBlock + "]", "[" + cardBlock + "]", "block_mismatch"},
		{"text for text", onlyText, "[" + markdownBlock + "]", ""},
		{"rich text for text", onlyText, "[" + richBlock + "]", ""},
		{"a card and text for text", onlyText, "[" + cardBlock + "," + markdownBlock + "]", ""},
		{"no blocks for text", onlyText, "", ""},
		{"empty blocks for text", onlyText, "[]", ""},
		{"a card for text beside a line", mixed, "[" + cardBlock + "]", ""},
		{"a card for a line", "[" + contextBlock + "]", "[" + cardBlock + "]", ""},
		{"a card for a section", "[" + sectionBlock + "]", "[" + cardBlock + "]", ""},
		{"a card for a post with no blocks", "", "[" + cardBlock + "]", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, _ := newFake(t)
			post := url.Values{"token": {testToken}, "channel": {"C1"}, "text": {"held"}}
			if tc.held != "" {
				post.Set("blocks", tc.held)
			}
			var posted struct {
				TS string `json:"ts"`
			}
			if err := json.Unmarshal([]byte(rawBody(t, fake, "chat.postMessage", post)), &posted); err != nil {
				t.Fatal(err)
			}
			update := url.Values{"token": {testToken}, "channel": {"C1"}, "ts": {posted.TS}, "text": {"new"}}
			if tc.update != "" {
				update.Set("blocks", tc.update)
			}
			if got := slackError(t, rawBody(t, fake, "chat.update", update)); got != tc.want {
				t.Errorf("chat.update = %q; want %q", got, tc.want)
			}
		})
	}
}

// A stopped stream that holds only text is protected as a post is, and one that holds a card or a stop's blocks is not.
func TestUpdateRefusesToDropAStreamsText(t *testing.T) {
	for _, tc := range []struct{ name, chunks, stop, want string }{
		{"text", `[{"type":"markdown_text","text":"body"}]`, "", "block_mismatch"},
		{"text and a card", `[{"type":"markdown_text","text":"body"},{"type":"task_update","id":"s1","title":"t","status":"complete"}]`, "", ""},
		{"a card", `[{"type":"task_update","id":"s1","title":"t","status":"complete"}]`, "", ""},
		{"text and a footer", `[{"type":"markdown_text","text":"body"}]`, "[" + contextBlock + "]", ""},
		{"nothing", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, client := newFake(t)
			_, root, err := client.PostMessage("C1", text("root"))
			if err != nil {
				t.Fatal(err)
			}
			start := url.Values{"token": {testToken}, "channel": {"C1"}, "thread_ts": {root}, "recipient_user_id": {"U1"}, "recipient_team_id": {"T1"}}
			if tc.chunks != "" {
				start.Set("chunks", tc.chunks)
			}
			var started struct {
				TS string `json:"ts"`
			}
			if err := json.Unmarshal([]byte(rawBody(t, fake, "chat.startStream", start)), &started); err != nil {
				t.Fatal(err)
			}
			stop := url.Values{"token": {testToken}, "channel": {"C1"}, "ts": {started.TS}}
			if tc.stop != "" {
				stop.Set("blocks", tc.stop)
			}
			if got := slackError(t, rawBody(t, fake, "chat.stopStream", stop)); got != "" {
				t.Fatalf("chat.stopStream = %q", got)
			}
			update := url.Values{"token": {testToken}, "channel": {"C1"}, "ts": {started.TS}, "text": {"new"}, "blocks": {"[" + cardBlock + "]"}}
			if got := slackError(t, rawBody(t, fake, "chat.update", update)); got != tc.want {
				t.Errorf("chat.update = %q; want %q", got, tc.want)
			}
		})
	}
}

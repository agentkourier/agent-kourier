package chattest

import (
	"encoding/json"
	"slices"
	"testing"
)

// The reader of a Slack message is pinned on its own: each field is read by the reader of its dialect, and what a person
// would act on is found wherever the message holds it.
func TestShownSlackMessageFindsWhatSlackWouldActOn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		msg       SlackMessage
		displayed []string // what each Shown displays, in order; nil to skip the check
		want      []Kind
	}{
		{"a broadcast in the top-level text", SlackMessage{Text: "<!channel> look"}, []string{"!channel look"}, []Kind{KindBroadcast}},
		{"a user mention in a plain_text object", SlackMessage{Blocks: json.RawMessage(
			`[{"type":"header","text":{"type":"plain_text","text":"<@U0123456789>"}}]`)}, nil, []Kind{KindMention}},
		{"a link in a markdown block", SlackMessage{Blocks: json.RawMessage(
			`[{"type":"markdown","text":"[bank](https://evil.test)"}]`)}, []string{"bank"}, []Kind{KindLink}},
		{"a channel link in a mrkdwn section", SlackMessage{Blocks: json.RawMessage(
			`[{"type":"section","text":{"type":"mrkdwn","text":"<#C0123456789>"}}]`)}, nil, []Kind{KindChannelLink}},
		{"a date in a context block's mrkdwn element", SlackMessage{Blocks: json.RawMessage(
			`[{"type":"context","elements":[{"type":"mrkdwn","text":"<!date^1700000000^{date}|then>"}]}]`)}, []string{"then"}, []Kind{KindDate}},
		{"an image in a stream's markdown_text", SlackMessage{MarkdownText: "![alt](https://evil.test/x.png)"}, []string{"alt"}, []Kind{KindImage}},
		{"a bare URL in a markdown block is an autolink", SlackMessage{Blocks: json.RawMessage(
			`[{"type":"markdown","text":"see https://evil.test"}]`)}, []string{"see https://evil.test"}, []Kind{KindAutolink}},
		{"a user element in a rich_text block", SlackMessage{Blocks: json.RawMessage(
			`[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"hi "},{"type":"user","user_id":"U1"}]}]}]`)},
			[]string{"hi "}, []Kind{KindMention}},
		{"a task_card's title is the characters it holds", SlackMessage{Blocks: json.RawMessage(
			`[{"type":"task_card","task_id":"t1","title":"<!channel> [x](https://evil.test) *b*","status":"complete"}]`)},
			[]string{"<!channel> [x](https://evil.test) *b*"}, nil},
		{"a task_update chunk's title with the adapter's escapes undone", SlackMessage{Chunks: json.RawMessage(
			`[{"type":"task_update","id":"t1","title":"\\[x](y) &amp; z","status":"in_progress"}]`)}, []string{"[x](y) & z"}, nil},
		{"a markdown_text chunk is CommonMark", SlackMessage{Chunks: json.RawMessage(
			`[{"type":"markdown_text","text":"[x](https://evil.test)"}]`)}, []string{"x"}, []Kind{KindLink}},
		{"escaped text is no construct", SlackMessage{Text: "&lt;!channel&gt;", Blocks: json.RawMessage(
			`[{"type":"header","text":{"type":"plain_text","text":"&lt;@U1&gt;"}}]`)}, []string{"<!channel>", "<@U1>"}, nil},
		{"blocks that are not JSON show nothing", SlackMessage{Blocks: json.RawMessage(`[{"type":`)}, []string{}, nil},
	} {
		got := ShownSlackMessage(tc.msg)
		var found []Kind
		var shown []string
		for _, s := range got {
			found = append(found, kinds(s)...)
			shown = append(shown, s.Displayed)
		}
		if !slices.Equal(found, tc.want) {
			t.Errorf("%s: found %v, want %v (shown %+v)", tc.name, found, tc.want, got)
		}
		if tc.displayed != nil && !slices.Equal(shown, tc.displayed) {
			t.Errorf("%s: displayed %q, want %q", tc.name, shown, tc.displayed)
		}
	}
}

// A message is read in a fixed order: the top-level text, a stream's markdown_text, its chunks, then the blocks.
func TestShownSlackMessageReadsTheFieldsInOrder(t *testing.T) {
	got := ShownSlackMessage(SlackMessage{
		Text:         "one",
		MarkdownText: "two",
		Chunks:       json.RawMessage(`[{"type":"markdown_text","text":"three"}]`),
		Blocks:       json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"four"}}]`),
	})
	var shown []string
	for _, s := range got {
		shown = append(shown, s.Displayed)
	}
	if want := []string{"one", "two", "three", "four"}; !slices.Equal(shown, want) {
		t.Errorf("displayed %q, want %q", shown, want)
	}
}

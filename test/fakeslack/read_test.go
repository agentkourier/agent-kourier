package fakeslack_test

import (
	"fmt"
	"testing"

	"github.com/slack-go/slack"
)

func post(t *testing.T, api *slack.Client, opts ...slack.MsgOption) string {
	t.Helper()
	_, ts, err := api.PostMessage("C1", opts...)
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	return ts
}

func history(t *testing.T, api *slack.Client, p slack.GetConversationHistoryParameters) *slack.GetConversationHistoryResponse {
	t.Helper()
	p.ChannelID = "C1"
	resp, err := api.GetConversationHistory(&p)
	if err != nil {
		t.Fatalf("GetConversationHistory: %v", err)
	}
	return resp
}

func tsList(msgs []slack.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Timestamp
	}
	return out
}

func wantTS(t *testing.T, what string, msgs []slack.Message, want ...string) {
	t.Helper()
	if got := tsList(msgs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s = %v; want %v", what, got, want)
	}
}

func TestUpdateMessage(t *testing.T) {
	fake, api := newFake(t)
	blocks := slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", "b", false, false), nil, nil)
	meta := slack.SlackMetadata{EventType: "courier_message", EventPayload: map[string]any{"outbox_id": "ob-1"}}
	ts := post(t, api, text("old"), slack.MsgOptionBlocks(blocks), slack.MsgOptionMetadata(meta))

	channel, gotTS, gotText, err := api.UpdateMessage("C1", ts, text("new"))
	if err != nil || channel != "C1" || gotTS != ts || gotText != "new" {
		t.Fatalf("UpdateMessage = %q, %q, %q, %v", channel, gotTS, gotText, err)
	}
	m := fake.Messages("C1")[0]
	if m.Text != "new" || m.Edited == nil || m.Edited.TS == "" {
		t.Fatalf("after update = %+v; want new text and an edited marker", m)
	}
	// UNCONFIRMED(WP-04): fields the update omits are kept.
	if len(m.Blocks) == 0 || m.Metadata == nil || m.Metadata.EventPayload["outbox_id"] != "ob-1" {
		t.Fatalf("update dropped blocks or metadata: %+v", m)
	}

	meta.EventPayload = map[string]any{"outbox_id": "ob-2"}
	if _, _, _, err := api.UpdateMessage("C1", ts, text("newer"), slack.MsgOptionMetadata(meta)); err != nil {
		t.Fatalf("update with metadata: %v", err)
	}
	if m = fake.Messages("C1")[0]; m.Metadata.EventPayload["outbox_id"] != "ob-2" || m.Text != "newer" {
		t.Fatalf("after metadata update = %+v", m)
	}
	// UNCONFIRMED(WP-04): blocks=[] with text clears the blocks, as courier's
	// removal of interaction controls (spec 8.4) needs.
	if _, _, _, err := api.UpdateMessage("C1", ts, text("done"), slack.MsgOptionBlocks()); err != nil {
		t.Fatalf("clearing blocks: %v", err)
	}
	if m = fake.Messages("C1")[0]; len(m.Blocks) != 0 || m.Text != "done" {
		t.Fatalf("after clearing blocks = %+v", m)
	}

	for name, tc := range map[string]struct {
		channel, ts string
		opts        []slack.MsgOption
		want        string
	}{
		"unknown ts":      {"C1", "1.000001", []slack.MsgOption{text("x")}, "message_not_found"},
		"no content":      {"C1", ts, nil, "no_text"},
		"metadata only":   {"C1", ts, []slack.MsgOption{slack.MsgOptionMetadata(meta)}, "no_text"},
		"empty blocks":    {"C1", ts, []slack.MsgOption{slack.MsgOptionBlocks()}, "no_text"},
		"missing ts":      {"C1", "", []slack.MsgOption{text("x")}, "invalid_arguments"},
		"missing channel": {"", ts, []slack.MsgOption{text("x")}, "channel_not_found"},
		"other channel":   {"C2", ts, []slack.MsgOption{text("x")}, "message_not_found"},
	} {
		if _, _, _, err := api.UpdateMessage(tc.channel, tc.ts, tc.opts...); err == nil || err.Error() != tc.want {
			t.Errorf("%s: UpdateMessage = %v; want %s", name, err, tc.want)
		}
	}
}

func TestHistoryIsNewestFirstAndFoldsThreads(t *testing.T) {
	_, api := newFake(t)
	a := post(t, api, text("a"))
	b := post(t, api, text("b"))
	reply := post(t, api, text("reply"), slack.MsgOptionTS(b))
	c := post(t, api, text("c"))

	resp := history(t, api, slack.GetConversationHistoryParameters{})
	wantTS(t, "history", resp.Messages, c, b, a) // the reply lives in the thread
	if resp.HasMore || resp.ResponseMetaData.NextCursor != "" {
		t.Fatalf("has_more = %v, cursor %q; want a single page", resp.HasMore, resp.ResponseMetaData.NextCursor)
	}
	if got := resp.Messages[1]; got.ReplyCount != 1 || got.LatestReply != reply || got.ThreadTimestamp != b {
		t.Fatalf("thread parent = %+v; want reply_count 1, latest_reply %s", got, reply)
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	_, api := newFake(t)
	meta := slack.SlackMetadata{EventType: "courier_message", EventPayload: map[string]any{"outbox_id": "ob-7"}}
	parent := post(t, api, text("root"), slack.MsgOptionMetadata(meta))
	reply := post(t, api, text("reply"), slack.MsgOptionTS(parent), slack.MsgOptionMetadata(meta))

	// Without include_all_metadata Slack omits metadata from reads.
	if got := history(t, api, slack.GetConversationHistoryParameters{}).Messages[0]; got.Metadata.EventType != "" {
		t.Fatalf("history without include_all_metadata returned metadata: %+v", got.Metadata)
	}
	got := history(t, api, slack.GetConversationHistoryParameters{IncludeAllMetadata: true}).Messages[0]
	if got.Metadata.EventType != "courier_message" || got.Metadata.EventPayload["outbox_id"] != "ob-7" {
		t.Fatalf("history metadata = %+v", got.Metadata)
	}
	msgs, _, _, err := api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: "C1", Timestamp: parent, IncludeAllMetadata: true})
	if err != nil {
		t.Fatalf("GetConversationReplies: %v", err)
	}
	wantTS(t, "replies", msgs, parent, reply)
	for _, m := range msgs {
		if m.Metadata.EventPayload["outbox_id"] != "ob-7" {
			t.Errorf("reply metadata for %s = %+v", m.Timestamp, m.Metadata)
		}
	}
}

func TestHistoryPagination(t *testing.T) {
	_, api := newFake(t)
	var ts []string
	for i := range 5 {
		ts = append(ts, post(t, api, text(fmt.Sprint(i))))
	}

	var seen []string
	cursor := ""
	for page := 1; page <= 3; page++ {
		resp := history(t, api, slack.GetConversationHistoryParameters{Limit: 2, Cursor: cursor})
		seen = append(seen, tsList(resp.Messages)...)
		cursor = resp.ResponseMetaData.NextCursor
		if resp.HasMore != (cursor != "") || resp.HasMore != (page < 3) {
			t.Fatalf("page %d: has_more = %v, cursor %q", page, resp.HasMore, cursor)
		}
		if page == 1 {
			// A new message arrives between pages. Cursors must anchor on the last
			// message returned, not an offset, or later pages would repeat one.
			post(t, api, text("late"))
		}
	}
	if want := []string{ts[4], ts[3], ts[2], ts[1], ts[0]}; fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("pages = %v; want %v", seen, want)
	}

	for want, p := range map[string]slack.GetConversationHistoryParameters{
		"invalid_cursor":    {ChannelID: "C1", Cursor: "bogus"},
		"invalid_limit":     {ChannelID: "C1", Limit: -1},
		"channel_not_found": {},
	} {
		if _, err := api.GetConversationHistory(&p); err == nil || err.Error() != want {
			t.Errorf("params %+v: err = %v; want %s", p, err, want)
		}
	}
}

func TestRepliesParentFirstAndPaginated(t *testing.T) {
	_, api := newFake(t)
	parent := post(t, api, text("root"))
	r1 := post(t, api, text("r1"), slack.MsgOptionTS(parent))
	r2 := post(t, api, text("r2"), slack.MsgOptionTS(parent))
	r3 := post(t, api, text("r3"), slack.MsgOptionTS(parent))
	replies := func(ts, cursor string, limit int) ([]slack.Message, bool, string) {
		msgs, more, next, err := api.GetConversationReplies(&slack.GetConversationRepliesParameters{
			ChannelID: "C1", Timestamp: ts, Cursor: cursor, Limit: limit})
		if err != nil {
			t.Fatalf("GetConversationReplies(%s): %v", ts, err)
		}
		return msgs, more, next
	}

	msgs, more, next := replies(parent, "", 2)
	wantTS(t, "page 1", msgs, parent, r1)
	if !more || next == "" {
		t.Fatalf("page 1: has_more = %v, cursor %q", more, next)
	}
	msgs, more, next = replies(parent, next, 2)
	wantTS(t, "page 2", msgs, r2, r3)
	if more || next != "" {
		t.Fatalf("page 2: has_more = %v, cursor %q", more, next)
	}
	msgs, _, _ = replies(r2, "", 0) // a reply's ts returns just that message
	wantTS(t, "reply ts", msgs, r2)

	if _, _, _, err := api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: "C1", Timestamp: "1.000001"}); err == nil || err.Error() != "thread_not_found" {
		t.Fatalf("replies for an unknown ts = %v; want thread_not_found", err)
	}
}

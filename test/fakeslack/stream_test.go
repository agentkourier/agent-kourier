package fakeslack_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// recipients are the options chat.startStream needs in a channel.
func recipients() []slack.MsgOption {
	return []slack.MsgOption{slack.MsgOptionRecipientUserID("U1"), slack.MsgOptionRecipientTeamID("T1")}
}

func startIn(api *slack.Client, channel, thread string, opts ...slack.MsgOption) (string, error) {
	_, ts, err := api.StartStream(channel, append([]slack.MsgOption{slack.MsgOptionTS(thread)}, opts...)...)
	return ts, err
}

func TestStreamLifecycle(t *testing.T) {
	fake, api := newFake(t)
	parent := post(t, api, text("question"))
	meta := slack.SlackMetadata{EventType: "courier_message", EventPayload: map[string]any{"outbox_id": "ob-9"}}
	section := slack.NewSectionBlock(mrkdwn("done"), nil, nil)

	ts, err := startIn(api, "C1", parent, append(recipients(), slack.MsgOptionChunks(slack.NewMarkdownTextChunk("Hel")))...)
	if err != nil || ts == "" {
		t.Fatalf("StartStream = %q, %v", ts, err)
	}
	stream := func() fakeslack.Message {
		thread := fake.Thread("C1", parent)
		if len(thread) != 2 || thread[1].TS != ts || thread[1].ThreadTS != parent {
			t.Fatalf("thread = %+v; want the parent then the streamed message", thread)
		}
		return thread[1]
	}
	if m := stream(); m.Text != "Hel" || !m.Streaming {
		t.Fatalf("after start = %+v", m)
	}

	if _, _, err := api.AppendStream("C1", ts, slack.MsgOptionChunks(slack.NewMarkdownTextChunk("lo "))); err != nil {
		t.Fatalf("AppendStream text: %v", err)
	}
	// Markdown chunks append to the text, and the stream is in chunks mode from its start; every chunk is kept for
	// assertions.
	if _, _, err := api.AppendStream("C1", ts, slack.MsgOptionChunks(
		slack.NewMarkdownTextChunk("wor"), slack.NewTaskUpdateChunk("t1", "Tool call"))); err != nil {
		t.Fatalf("AppendStream chunks: %v", err)
	}
	m := stream()
	if m.Text != "Hello wor" || !m.Streaming || len(m.Chunks) != 4 {
		t.Fatalf("mid-stream = %+v", m)
	}
	var task struct{ Type, ID, Title string }
	if err := json.Unmarshal(m.Chunks[3], &task); err != nil || task.Type != "task_update" || task.Title != "Tool call" {
		t.Fatalf("chunk 3 = %s, %v; want the task_update", m.Chunks[3], err)
	}

	if _, _, err := api.StopStream("C1", ts, slack.MsgOptionChunks(slack.NewMarkdownTextChunk("ld")), slack.MsgOptionBlocks(section),
		slack.MsgOptionMetadata(meta)); err != nil {
		t.Fatalf("StopStream: %v", err)
	}
	if m := stream(); m.Text != "Hello world" || m.Streaming || len(m.Blocks) == 0 {
		t.Fatalf("after stop = %+v", m)
	}
	// The streamed text stays as the message's first block, and the stop's block follows it.
	if got, want := blockSummary(t, stream().Blocks), []string{"markdown:Hello world", "section:done"}; !slices.Equal(got, want) {
		t.Fatalf("blocks after stop = %v; want %v: the stop's blocks are appended under the streamed text", got, want)
	}

	// A finished stream is an ordinary message: reads return it, metadata included.
	msgs, _, _, err := api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: "C1", Timestamp: parent, IncludeAllMetadata: true})
	if err != nil || len(msgs) != 2 || msgs[1].Text != "Hello world" || msgs[1].Metadata.EventPayload["outbox_id"] != "ob-9" {
		t.Fatalf("replies after stop = %+v, %v", msgs, err)
	}
	for name, err := range map[string]error{
		"append after stop": func() error {
			_, _, err := api.AppendStream("C1", ts, slack.MsgOptionChunks(slack.NewMarkdownTextChunk("x")))
			return err
		}(),
		"stop after stop": func() error { _, _, err := api.StopStream("C1", ts); return err }(),
	} {
		if err == nil || err.Error() != "message_not_in_streaming_state" {
			t.Errorf("%s = %v; want message_not_in_streaming_state", name, err)
		}
	}
	if _, _, _, err := api.UpdateMessage("C1", ts, text("edit")); err != nil {
		t.Errorf("updating a finished stream = %v; want success", err)
	}
}

// blockSummary lists blocks as "type:text": the text of a markdown block, or of a section.
func blockSummary(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var blocks []struct {
		Type string `json:"type"`
		Text any    `json:"text"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &blocks); err != nil {
			t.Fatalf("blocks %s: %v", raw, err)
		}
	}
	var out []string
	for _, b := range blocks {
		text, _ := b.Text.(string)
		if o, ok := b.Text.(map[string]any); ok {
			text, _ = o["text"].(string)
		}
		out = append(out, b.Type+":"+text)
	}
	return out
}

// A stop that sends no blocks leaves the streamed text as the message's one block; one that sends the text
// again, as a courier does that takes the stop to replace the stream, shows it twice. A stream that carried no
// text has no block of its own.
func TestStopStreamKeepsTheStreamedTextAsABlock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start string
		stop  []slack.MsgOption
		want  []string
	}{
		{"no stop blocks", "Hello", nil, []string{"markdown:Hello"}},
		{"the text sent again", "Hello", []slack.MsgOption{slack.MsgOptionBlocks(slack.NewMarkdownBlock("", "Hello"))}, []string{"markdown:Hello", "markdown:Hello"}},
		{"empty stop blocks", "Hello", []slack.MsgOption{slack.MsgOptionBlocks()}, []string{"markdown:Hello"}},
		{"no text", "", []slack.MsgOption{slack.MsgOptionBlocks(slack.NewSectionBlock(mrkdwn("only"), nil, nil))}, []string{"section:only"}},
		{"no text, no blocks", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, api := newFake(t)
			parent := post(t, api, text("question"))
			start := recipients()
			if tc.start != "" {
				start = append(start, slack.MsgOptionMarkdownText(tc.start))
			}
			ts, err := startIn(api, "C1", parent, start...)
			if err != nil {
				t.Fatal(err)
			}
			if err := stopOf(api, "C1", ts, tc.stop...); err != nil {
				t.Fatal(err)
			}
			th := fake.Thread("C1", parent)
			if got := blockSummary(t, th[len(th)-1].Blocks); !slices.Equal(got, tc.want) {
				t.Errorf("blocks = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestStreamErrors(t *testing.T) {
	_, api, live := newStream(t)
	parent := post(t, api, text("question"))
	plainTS := post(t, api, text("not a stream"))
	user, team := slack.MsgOptionRecipientUserID("U1"), slack.MsgOptionRecipientTeamID("T1")
	meta := slack.MsgOptionMetadata(slack.SlackMetadata{EventPayload: map[string]any{"k": "v"}}) // no event_type

	start := func(channel string, opts ...slack.MsgOption) error {
		_, err := startIn(api, channel, parent, opts...)
		return err
	}
	appendTo := func(ts string, opts ...slack.MsgOption) error {
		_, _, err := api.AppendStream("C1", ts, opts...)
		return err
	}
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"no channel":            {start("", user, team), "channel_not_found"},
		"channel, no user":      {start("C1", team), "missing_recipient_user_id"},
		"channel, no team":      {start("C1", user), "missing_recipient_team_id"},
		"append unknown ts":     {appendTo("1.000001", slack.MsgOptionMarkdownText("x")), "message_not_found"},
		"append non-stream":     {appendTo(plainTS, slack.MsgOptionMarkdownText("x")), "message_not_in_streaming_state"},
		"append without ts":     {appendTo("", slack.MsgOptionMarkdownText("x")), "invalid_arguments"},
		"append nothing":        {appendTo(live), "invalid_arguments"},
		"append unknown chunk":  {appendTo(live, slack.MsgOptionChunks(unknownChunk{})), "invalid_chunks"},
		"stop unknown chunk":    {stopOf(api, "C1", live, slack.MsgOptionChunks(unknownChunk{})), "invalid_chunks"},
		"blocks chunk, no key":  {appendTo(live, slack.MsgOptionChunks(rawChunk(`{"type":"blocks"}`))), "invalid_chunks"},
		"blocks chunk, empty":   {appendTo(live, slack.MsgOptionChunks(rawChunk(`{"type":"blocks","blocks":[]}`))), "invalid_chunks"},
		"stop bad metadata":     {stopOf(api, "C1", live, meta), "invalid_metadata_format"},
		"stop in other channel": {stopOf(api, "C2", live), "message_not_found"},
		"update while running":  {updateErr(api, live), "cant_update_message"},
	} {
		if tc.err == nil || tc.err.Error() != tc.want {
			t.Errorf("%s = %v; want %s", name, tc.err, tc.want)
		}
	}
	if _, err := startIn(api, "C1", "1.000001", user, team); err == nil || err.Error() != "thread_not_found" {
		t.Errorf("start in an unknown thread = %v; want thread_not_found", err)
	}
	if _, _, err := api.StartStream("C1", user, team); err == nil || err.Error() != "invalid_arguments" {
		t.Errorf("start without thread_ts = %v; want invalid_arguments", err)
	}
	// Direct-message channels (D...) need no recipient.
	_, dmRoot, err := api.PostMessage("D1", text("dm root"))
	if err != nil {
		t.Fatalf("post in DM: %v", err)
	}
	if _, err := startIn(api, "D1", dmRoot); err != nil {
		t.Errorf("StartStream in a DM without a recipient = %v; want success", err)
	}
}

// unknownChunk is a chunk type slack-go does not define.
type unknownChunk struct{}

func (unknownChunk) ChunkType() slack.StreamChunkType { return "confetti" }

func (unknownChunk) MarshalJSON() ([]byte, error) { return []byte(`{"type":"confetti"}`), nil }

// rawChunk is a blocks chunk sent as the given JSON.
type rawChunk string

func (rawChunk) ChunkType() slack.StreamChunkType { return slack.StreamChunkBlocks }

func (c rawChunk) MarshalJSON() ([]byte, error) { return []byte(c), nil }

func stopOf(api *slack.Client, channel, ts string, opts ...slack.MsgOption) error {
	_, _, err := api.StopStream(channel, ts, opts...)
	return err
}

func updateErr(api *slack.Client, ts string) error {
	_, _, _, err := api.UpdateMessage("C1", ts, text("edit"))
	return err
}

// Streams, appends, and reads run together under -race, and each stream ends
// with exactly the text appended to it.
func TestStreamsAreSafeUnderConcurrency(t *testing.T) {
	fake, api := newFake(t)
	const streams, appends = 8, 20
	var wg sync.WaitGroup
	for i := range streams {
		parent := post(t, api, text(fmt.Sprint("q", i)))
		wg.Go(func() {
			ts, err := startIn(api, "C1", parent, recipients()...)
			if err != nil {
				t.Errorf("stream %d start: %v", i, err)
				return
			}
			for range appends {
				if _, _, err := api.AppendStream("C1", ts, slack.MsgOptionMarkdownText("x")); err != nil {
					t.Errorf("stream %d append: %v", i, err)
				}
				if _, _, _, err := api.GetConversationReplies(&slack.GetConversationRepliesParameters{
					ChannelID: "C1", Timestamp: parent}); err != nil {
					t.Errorf("stream %d read: %v", i, err)
				}
				fake.Messages("C1")
			}
			if _, _, err := api.StopStream("C1", ts); err != nil {
				t.Errorf("stream %d stop: %v", i, err)
			}
		})
	}
	wg.Wait()

	done := 0
	for _, m := range fake.Messages("C1") {
		if m.ThreadTS != "" && m.ThreadTS != m.TS {
			if m.Streaming || len(m.Text) != appends {
				t.Errorf("stream %s = %d characters, streaming %v; want %d, done", m.TS, len(m.Text), m.Streaming, appends)
			}
			done++
		}
	}
	if done != streams {
		t.Fatalf("found %d finished streams; want %d", done, streams)
	}
}

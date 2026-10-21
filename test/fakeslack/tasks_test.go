package fakeslack_test

import (
	"slices"
	"testing"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

func chunks(cs ...slack.StreamChunk) slack.MsgOption { return slack.MsgOptionChunks(cs...) }

func task(id, title string, status slack.TaskCardStatus) slack.TaskUpdateChunk {
	c := slack.NewTaskUpdateChunk(id, title)
	c.Status = status
	return c
}

// TestTasksMergeEachTaskUpdateAsSlackDoes: a task_update with an ID the stream has seen is the same task, and a task
// keeps the place of its first chunk. Its title and status replace the card's; its details and output are appended to
// the card's; and a chunk that leaves them out keeps them (Slack, observed 2026-10-18).
func TestTasksMergeEachTaskUpdateAsSlackDoes(t *testing.T) {
	fake, api := newFake(t)
	parent := post(t, api, text("question"))
	ts, err := startIn(api, "C1", parent, append(recipients(), chunks(slack.NewMarkdownTextChunk("Hel")))...)
	if err != nil {
		t.Fatal(err)
	}
	begun := task("s1", "search", slack.TaskCardStatusInProgress)
	begun.Details = "q=up"
	done := task("s1", "searched", slack.TaskCardStatusComplete)
	done.Output = "found"
	for _, c := range []slack.StreamChunk{
		begun,
		task("s2", "fetch", slack.TaskCardStatusInProgress),
		done,
		task("s2", "fetch", slack.TaskCardStatusError),
	} {
		if _, _, err := api.AppendStream("C1", ts, chunks(c)); err != nil {
			t.Fatal(err)
		}
	}
	want := []fakeslack.Task{
		{ID: "s1", Title: "searched", Status: "complete", Details: "q=up", Output: "found"},
		{ID: "s2", Title: "fetch", Status: "error"},
	}
	if got := fake.Thread("C1", parent)[1].Tasks(); !slices.Equal(got, want) {
		t.Fatalf("Tasks = %+v; want %+v", got, want)
	}
	again := task("s1", "searched", slack.TaskCardStatusComplete)
	again.Details, again.Output = "q=up", "found"
	if _, _, err := api.AppendStream("C1", ts, chunks(again)); err != nil {
		t.Fatal(err)
	}
	if got := fake.Thread("C1", parent)[1].Tasks()[0]; got.Details != "q=upq=up" || got.Output != "foundfound" {
		t.Fatalf("Tasks()[0] = %+v; want the details and the output that were sent again appended, as Slack shows them", got)
	}
}

// TestAStopLeavesOnlyCompleteCards: once the stream stops, every card that is not complete is an error, as Slack shows it
// (probed 2026-10-21): only complete survives a stop. While the stream runs, the cards are as they were sent.
func TestAStopLeavesOnlyCompleteCards(t *testing.T) {
	fake, api := newFake(t)
	parent := post(t, api, text("question"))
	ts, err := startIn(api, "C1", parent, append(recipients(), chunks(slack.NewMarkdownTextChunk("Hel")))...)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := api.AppendStream("C1", ts, chunks(task("s1", "search", slack.TaskCardStatusInProgress), task("s2", "fetch", slack.TaskCardStatusComplete),
		task("s3", "wait", slack.TaskCardStatusPending))); err != nil {
		t.Fatal(err)
	}
	running := []fakeslack.Task{{ID: "s1", Title: "search", Status: "in_progress"}, {ID: "s2", Title: "fetch", Status: "complete"}, {ID: "s3", Title: "wait", Status: "pending"}}
	if got := fake.Thread("C1", parent)[1].Tasks(); !slices.Equal(got, running) {
		t.Fatalf("Tasks while streaming = %+v; want %+v", got, running)
	}
	if _, _, err := api.StopStream("C1", ts); err != nil {
		t.Fatal(err)
	}
	stopped := []fakeslack.Task{{ID: "s1", Title: "search", Status: "error"}, {ID: "s2", Title: "fetch", Status: "complete"}, {ID: "s3", Title: "wait", Status: "error"}}
	if got := fake.Thread("C1", parent)[1].Tasks(); !slices.Equal(got, stopped) {
		t.Fatalf("Tasks after the stop = %+v; want %+v", got, stopped)
	}
}

func TestAMessageThatWasNeverStreamedHasNoTasks(t *testing.T) {
	fake, api := newFake(t)
	parent := post(t, api, text("question"))
	if got := fake.Thread("C1", parent)[0].Tasks(); len(got) != 0 {
		t.Fatalf("Tasks = %+v; want none", got)
	}
}

// TestAStreamIsInOneModeFromItsFirstContent: a request is in markdown_text mode or in chunks mode, never both, and the
// mode of the stream is the mode of its first content, which every later append and the stop must keep.
func TestAStreamIsInOneModeFromItsFirstContent(t *testing.T) {
	md := slack.MsgOptionMarkdownText("x")
	ch := chunks(slack.NewMarkdownTextChunk("x"))
	both := []slack.MsgOption{md, ch}
	for _, tc := range []struct {
		name  string
		start []slack.MsgOption
		then  []slack.MsgOption
		stop  bool
		want  string
	}{
		{"start in markdown_text, append in chunks", []slack.MsgOption{md}, []slack.MsgOption{ch}, false, "streaming_mode_mismatch"},
		{"start in chunks, append in markdown_text", []slack.MsgOption{ch}, []slack.MsgOption{md}, false, "streaming_mode_mismatch"},
		{"start in chunks, stop in markdown_text", []slack.MsgOption{ch}, []slack.MsgOption{md}, true, "streaming_mode_mismatch"},
		{"start in markdown_text, stop in chunks", []slack.MsgOption{md}, []slack.MsgOption{ch}, true, "streaming_mode_mismatch"},
		{"start in chunks, append in chunks", []slack.MsgOption{ch}, []slack.MsgOption{ch}, false, ""},
		{"start in markdown_text, append in markdown_text", []slack.MsgOption{md}, []slack.MsgOption{md}, false, ""},
		{"start with no content, append in markdown_text", nil, []slack.MsgOption{md}, false, ""},
		{"start with no content, append in chunks", nil, []slack.MsgOption{ch}, false, ""},
		{"start in chunks, a stop with no content", []slack.MsgOption{ch}, nil, true, ""},
		{"both in the start", both, nil, false, "cannot_provide_both_markdown_text_and_chunks"},
		{"both in an append", []slack.MsgOption{ch}, both, false, "cannot_provide_both_markdown_text_and_chunks"},
		{"both in the stop", []slack.MsgOption{ch}, both, true, "cannot_provide_both_markdown_text_and_chunks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, api := newFake(t)
			parent := post(t, api, text("question"))
			ts, err := startIn(api, "C1", parent, append(recipients(), tc.start...)...)
			if tc.name == "both in the start" {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("start = %v; want %s", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.stop {
				_, _, err = api.StopStream("C1", ts, tc.then...)
			} else {
				if tc.then == nil {
					t.Fatal("a case with nothing to append")
				}
				_, _, err = api.AppendStream("C1", ts, tc.then...)
			}
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("got %v; want success", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("got %v; want %s", err, tc.want)
			}
		})
	}
}

// TestTheModeOfAStreamIsKeptAfterAFirstAppendSetsIt: a stream that was started with no content is in the mode of
// its first append, and the next one in the other mode is refused.
func TestTheModeOfAStreamIsKeptAfterAFirstAppendSetsIt(t *testing.T) {
	_, api := newFake(t)
	parent := post(t, api, text("question"))
	ts, err := startIn(api, "C1", parent, recipients()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := api.AppendStream("C1", ts, slack.MsgOptionMarkdownText("a")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := api.AppendStream("C1", ts, chunks(slack.NewMarkdownTextChunk("b"))); err == nil || err.Error() != "streaming_mode_mismatch" {
		t.Fatalf("append in the other mode = %v; want streaming_mode_mismatch", err)
	}
}

// RefuseChunkMode makes the requests in chunks mode fail with the code given, and leaves markdown_text mode alone,
// until it is lifted.
func TestRefuseChunkModeRefusesOnlyRequestsInChunksMode(t *testing.T) {
	fake, api := newFake(t)
	parent := post(t, api, text("question"))
	fake.RefuseChunkMode("invalid_chunks")
	if _, err := startIn(api, "C1", parent, append(recipients(), chunks(slack.NewMarkdownTextChunk("x")))...); err == nil || err.Error() != "invalid_chunks" {
		t.Fatalf("start in chunks mode = %v; want invalid_chunks", err)
	}
	ts, err := startIn(api, "C1", parent, append(recipients(), slack.MsgOptionMarkdownText("x"))...)
	if err != nil {
		t.Fatalf("start in markdown_text mode = %v; want success", err)
	}
	if _, _, err := api.AppendStream("C1", ts, slack.MsgOptionMarkdownText("y")); err != nil {
		t.Fatalf("append in markdown_text mode = %v; want success", err)
	}
	fake.RefuseChunkMode("")
	if _, err := startIn(api, "C1", parent, append(recipients(), chunks(slack.NewMarkdownTextChunk("x")))...); err != nil {
		t.Fatalf("start in chunks mode after the lift = %v; want success", err)
	}
}

// card is a task_card block, with its details as one rich_text section.
func card(id, title string, status slack.TaskCardStatus, details string) *slack.TaskCardBlock {
	b := slack.NewTaskCardBlock(id, title).WithStatus(status)
	if details != "" {
		b = b.WithDetails(slack.NewRichTextBlock("", slack.NewRichTextSection(slack.NewRichTextSectionTextElement(details, nil))))
	}
	return b
}

// TestAnUpdateWithBlocksReplacesTheCardsOfAStream: a chat.update with blocks replaces the content of a stopped stream, and
// its cards with it, and the task_card blocks it carries are the cards the message shows (Slack, probed 2026-10-21 in a
// sandbox workspace: an update whose blocks had no task_card dropped the stream's cards, and one with task_card blocks
// showed them).
func TestAnUpdateWithBlocksReplacesTheCardsOfAStream(t *testing.T) {
	fake, api := newFake(t)
	parent := post(t, api, text("question"))
	ts, err := startIn(api, "C1", parent, append(recipients(), chunks(slack.NewMarkdownTextChunk("Hel"), task("s1", "search", slack.TaskCardStatusComplete)))...)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := api.StopStream("C1", ts); err != nil {
		t.Fatal(err)
	}
	if got := fake.Thread("C1", parent)[1].Tasks(); len(got) != 1 {
		t.Fatalf("Tasks after the stop = %+v; want the streamed card", got)
	}
	if _, _, _, err := api.UpdateMessage("C1", ts, text("Hello"), slack.MsgOptionBlocks(slack.NewMarkdownBlock("", "Hello"))); err != nil {
		t.Fatal(err)
	}
	if m := fake.Thread("C1", parent)[1]; len(m.Tasks()) != 0 || len(m.Chunks) != 0 {
		t.Fatalf("after an update with no task_card: tasks %+v, %d chunks; want none: Slack drops the stream's cards", m.Tasks(), len(m.Chunks))
	}
	if _, _, _, err := api.UpdateMessage("C1", ts, text("Hello"), slack.MsgOptionBlocks(
		card("s1", "search", slack.TaskCardStatusComplete, `q="up"`), card("s2", "fetch", slack.TaskCardStatusError, ""),
		slack.NewMarkdownBlock("", "Hello"))); err != nil {
		t.Fatalf("an update with task_card blocks = %v; want success", err)
	}
	want := []fakeslack.Task{{ID: "s1", Title: "search", Status: "complete", Details: `q="up"`}, {ID: "s2", Title: "fetch", Status: "error"}}
	if got := fake.Thread("C1", parent)[1].Tasks(); !slices.Equal(got, want) {
		t.Fatalf("Tasks = %+v; want the cards of the update %+v", got, want)
	}
}

// A post may carry task_card blocks too; one with no task_id or title, or with a status Slack does not have, is refused.
func TestATaskCardBlockIsValidOnAPostAndNeedsItsFields(t *testing.T) {
	_, api := newFake(t)
	parent := post(t, api, text("question"))
	if _, _, err := api.PostMessage("C1", slack.MsgOptionTS(parent), text("x"), slack.MsgOptionBlocks(card("s1", "search", slack.TaskCardStatusInProgress, ""))); err != nil {
		t.Fatalf("a post with a task_card = %v; want success", err)
	}
	for name, b := range map[string]*slack.TaskCardBlock{
		"no task_id": card("", "search", slack.TaskCardStatusComplete, ""),
		"no title":   card("s1", "", slack.TaskCardStatusComplete, ""),
		"bad status": card("s1", "search", slack.TaskCardStatus("done"), ""),
	} {
		if _, _, err := api.PostMessage("C1", slack.MsgOptionTS(parent), text("x"), slack.MsgOptionBlocks(b)); err == nil || err.Error() != "invalid_blocks" {
			t.Errorf("%s: %v; want invalid_blocks", name, err)
		}
	}
}

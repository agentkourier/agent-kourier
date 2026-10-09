package e2e

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

const fakeMention = "<@UFAKEBOT> investigate"

// fixture is a fake Slack behind the control client, and the bot's own API client (for the posts
// Agent Kourier would make).
type fixture struct {
	sl   *Slack
	fake *fakeslack.Server
	bot  *slack.Client
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	bot := slack.New("xoxb-fake-test-token", slack.OptionAPIURL(fake.URL()+"/"), slack.OptionAppLevelToken("xapp-fake-test-token"))
	return fixture{NewSlack(fake.URL()), fake, bot}
}

func (f fixture) connect(t *testing.T, ack bool) { t.Helper(); connectSocket(t, f.bot, ack) }

// connectSocket runs a Socket Mode client for bot, as Agent Kourier does, and returns once the fake has
// sent this client its hello. The fake registers a connection before it says hello and sends each
// envelope to its newest connection only, so after hello this client is the one that gets the next
// event; counting connections would not tell it from an older one. It acks every envelope if ack is
// set, and stops with the test.
func connectSocket(t *testing.T, bot *slack.Client, ack bool) {
	t.Helper()
	smc := socketmode.New(bot)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = smc.RunContext(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	hello := make(chan struct{})
	go func() { // slack-go never closes Events, so this ends with the test's context
		var once sync.Once
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-smc.Events:
				if ev.Type == socketmode.EventTypeHello {
					once.Do(func() { close(hello) })
				}
				if ack && ev.Request != nil {
					_ = smc.Ack(*ev.Request)
				}
			}
		}
	}()
	select {
	case <-hello:
	case <-time.After(10 * time.Second):
		t.Fatal("the Socket Mode client got no hello from the fake")
	}
}

func (f fixture) post(t *testing.T, opts ...slack.MsgOption) string {
	t.Helper()
	_, ts, err := f.bot.PostMessage("C1", opts...)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestSlackActsAsUsersAndReadsTheirAcks(t *testing.T) {
	f, ctx := newFixture(t), t.Context()
	if n, err := f.sl.Connections(ctx); err != nil || n != 0 {
		t.Fatalf("Connections before connecting = %d, %v", n, err)
	}
	f.connect(t, true)
	root := f.post(t, slack.MsgOptionText("investigating", false))
	ask := f.post(t, slack.MsgOptionTS(root), slack.MsgOptionText("a question", false), slack.MsgOptionBlocks(
		slack.NewInputBlock("q1", slack.NewTextBlockObject("plain_text", "Which?", false, false), nil, slack.NewPlainTextInputBlockElement(nil, "answer_1")),
		slack.NewActionBlock("buttons",
			slack.NewButtonBlockElement("approve", "yes", slack.NewTextBlockObject("plain_text", "Approve", false, false)),
			slack.NewButtonBlockElement("submit", "go", slack.NewTextBlockObject("plain_text", "Submit", false, false)))))

	mention, err := f.sl.Mention(ctx, "C1", "U1", fakeMention)
	if err != nil || mention.Channel != "C1" || mention.TS == "" || len(mention.Envelopes) != 2 {
		t.Fatalf("Mention = %+v, %v; want two envelopes", mention, err)
	}
	reply, err := f.sl.Reply(ctx, "C1", root, "U2", "more")
	if err != nil || reply.TS == "" {
		t.Fatalf("Reply = %+v, %v", reply, err)
	}
	dm, err := f.sl.DM(ctx, "U1", "hi")
	if err != nil || dm.Channel != fakeslack.DMChannel("U1") {
		t.Fatalf("DM = %+v, %v", dm, err)
	}
	click, err := f.sl.Click(ctx, "U1", ask, "approve", "yes")
	if err != nil || click == "" {
		t.Fatalf("Click = %q, %v", click, err)
	}
	answer := fakeslack.StateValues{"q1": {"answer_1": {"type": "plain_text_input", "value": "prod"}}}
	submit, err := f.sl.Submit(ctx, "U1", ask, "submit", answer)
	if err != nil || submit == "" {
		t.Fatalf("Submit = %q, %v", submit, err)
	}
	for _, id := range append([]string{click, submit, reply.Envelopes[0], dm.Envelopes[0]}, mention.Envelopes...) {
		if _, err := f.sl.AckWait(ctx, 5*time.Second, id); err != nil {
			t.Errorf("AckWait(%s) = %v", id, err)
		}
	}

	msgs, err := f.sl.Messages(ctx, "C1")
	if err != nil || len(msgs) != 4 || msgs[2].User != "U1" || msgs[2].Text != fakeMention || msgs[3].User != "U2" {
		t.Fatalf("Messages = %+v, %v; want the bot's two posts, the mention and the reply", msgs, err)
	}
	thread, err := f.sl.Thread(ctx, "C1", root)
	if err != nil || len(thread) != 3 || thread[0].TS != root || thread[2].Text != "more" {
		t.Fatalf("Thread = %+v, %v; want the root, the question and the reply", thread, err)
	}
}

// Webhook posts as an incoming webhook does, which is how Alertmanager's alerts reach a channel: a bot's message with
// its content in an attachment and no user. The fake delivers the event to the connected client, which acks it, and
// stores the message with its attachment.
func TestSlackPostsThroughTheIncomingWebhook(t *testing.T) {
	f, ctx := newFixture(t), t.Context()
	if _, err := f.sl.Webhook(ctx, fakeslack.WebhookPost{Channel: "C1", BotID: "B0ALERT01", Attachments: []fakeslack.Attachment{{Title: "FIRING (1) - Disk"}}}); err == nil {
		t.Fatal("Webhook with no client connected succeeded; want the fake's refusal")
	}
	f.connect(t, true)
	m, err := f.sl.Webhook(ctx, fakeslack.WebhookPost{Channel: "C1", BotID: "B0ALERT01", Attachments: []fakeslack.Attachment{{Title: "FIRING (1) - Disk", TitleLink: "http://am/#/alerts?receiver=a"}}})
	if err != nil || m.Channel != "C1" || m.TS == "" || len(m.Envelopes) != 1 {
		t.Fatalf("Webhook = %+v, %v; want a message with one envelope", m, err)
	}
	if _, err := f.sl.AckWait(ctx, 5*time.Second, m.Envelopes[0]); err != nil {
		t.Errorf("AckWait = %v", err)
	}
	msgs, err := f.sl.Messages(ctx, "C1")
	if err != nil || len(msgs) != 1 || msgs[0].BotID != "B0ALERT01" || msgs[0].User != "" || len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].Title != "FIRING (1) - Disk" {
		t.Fatalf("Messages = %+v, %v; want the bot's message with its attachment", msgs, err)
	}
}

func TestSlackSetsUpUsersAndMembers(t *testing.T) {
	f, ctx := newFixture(t), t.Context()
	if err := f.sl.AddUser(ctx, fakeslack.User{ID: "U1", Name: "ada", Email: "ada@example.test"}); err != nil {
		t.Fatal(err)
	}
	if err := f.sl.SetMembers(ctx, "C1", "U1", "U2"); err != nil {
		t.Fatal(err)
	}
	if u, err := f.bot.GetUserInfo("U1"); err != nil || u.Profile.Email != "ada@example.test" {
		t.Errorf("users.info = %+v, %v", u, err)
	}
	if m, _, err := f.bot.GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: "C1"}); err != nil || len(m) != 2 {
		t.Errorf("conversations.members = %v, %v", m, err)
	}
}

func TestSlackShowsAStreamingMessage(t *testing.T) {
	f, ctx := newFixture(t), t.Context()
	root := f.post(t, slack.MsgOptionText("question", false))
	_, ts, err := f.bot.StartStream("C1", slack.MsgOptionTS(root), slack.MsgOptionRecipientUserID("U1"),
		slack.MsgOptionRecipientTeamID("T1"), slack.MsgOptionChunks(slack.NewMarkdownTextChunk("Hel")))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.bot.AppendStream("C1", ts, slack.MsgOptionChunks(slack.NewTaskUpdateChunk("t1", "Tool call"))); err != nil {
		t.Fatal(err)
	}
	thread, err := f.sl.Thread(ctx, "C1", root)
	if err != nil || len(thread) != 2 || !thread[1].Streaming || len(thread[1].Chunks) != 2 || thread[1].Text != "Hel" {
		t.Fatalf("Thread = %+v, %v; want a streaming reply with a text chunk and a task chunk", thread, err)
	}
	if _, _, err := f.bot.StopStream("C1", ts); err != nil {
		t.Fatal(err)
	}
	if thread, err = f.sl.Thread(ctx, "C1", root); err != nil || thread[1].Streaming {
		t.Fatalf("after the stop: %+v, %v; want Streaming false", thread, err)
	}
}

func TestSlackErrorsSurface(t *testing.T) {
	f, ctx := newFixture(t), t.Context()
	status := func(err error) int {
		var se *StatusError
		if !errors.As(err, &se) {
			return 0
		}
		return se.Status
	}
	if _, err := f.sl.Mention(ctx, "C1", "U1", fakeMention); status(err) != http.StatusConflict {
		t.Errorf("Mention with no client connected = %v, want a 409", err)
	}
	if _, err := f.sl.Mention(ctx, "C1", "U1", "no mention"); status(err) != http.StatusBadRequest {
		t.Errorf("Mention that does not mention the bot = %v, want a 400", err)
	}
	if _, err := f.sl.Thread(ctx, "C1", "1.1"); status(err) != http.StatusNotFound {
		t.Errorf("Thread of nothing = %v, want a 404", err)
	}
	if _, err := f.sl.Click(ctx, "U1", "1.1", "a", "v"); status(err) != http.StatusNotFound {
		t.Errorf("Click on nothing = %v, want a 404", err)
	}
	if err := f.sl.AddUser(ctx, fakeslack.User{}); status(err) != http.StatusBadRequest {
		t.Errorf("AddUser with no id = %v, want a 400", err)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusInternalServerError) }))
	defer down.Close()
	if _, err := NewSlack(down.URL).Connections(ctx); status(err) != http.StatusInternalServerError {
		t.Errorf("Connections of a failing fake = %v, want a 500", err)
	}
	if _, _, err := f.sl.acked(ctx, "env-nope"); status(err) != http.StatusNotFound {
		t.Errorf("Acked of an unknown envelope = %v, want a 404", err)
	}
	if _, err := f.sl.AckWait(ctx, 100*time.Millisecond, "env-nope"); !errors.Is(err, ErrTimeout) || status(err) != http.StatusNotFound {
		t.Errorf("AckWait of an unknown envelope = %v, want a timeout wrapping the 404", err)
	}
}

func TestSlackAckWaitTimesOutOnAnEnvelopeNobodyAcks(t *testing.T) {
	f, ctx := newFixture(t), t.Context()
	f.connect(t, false)
	mention, err := f.sl.Mention(ctx, "C1", "U1", fakeMention)
	if err != nil {
		t.Fatal(err)
	}
	if acked, _, err := f.sl.acked(ctx, mention.Envelopes[0]); err != nil || acked {
		t.Fatalf("Acked = %v, %v, want false", acked, err)
	}
	if _, err := f.sl.AckWait(ctx, 100*time.Millisecond, mention.Envelopes[0]); !errors.Is(err, ErrTimeout) {
		t.Fatalf("AckWait = %v, want ErrTimeout", err)
	}
}

func TestSlackFailedDeliveryStillReturnsTheStoredMessage(t *testing.T) {
	// A control call whose events could not be delivered has already stored the message, and the
	// fake says so in the error body, so a caller that retries does not post it twice.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"no client","message":{"channel":"C1","ts":"1.2","envelopes":["e1"]}}`))
	}))
	defer srv.Close()
	got, err := NewSlack(srv.URL).Mention(t.Context(), "C1", "U1", fakeMention)
	var se *StatusError
	if !errors.As(err, &se) || got.TS != "1.2" || got.Channel != "C1" || len(got.Envelopes) != 1 {
		t.Fatalf("Mention = %+v, %v; want the 409 and the stored message", got, err)
	}
}

func TestConnectSocketWaitsForItsOwnConnection(t *testing.T) {
	// The fake sends each envelope to its newest connection only. If connectSocket returned while an
	// older client already counted as connected, the mention would go to that client, which never
	// acks, and AckWait would time out or pass against the wrong client.
	f, ctx := newFixture(t), t.Context()
	f.connect(t, false)
	f.connect(t, true)
	mention, err := f.sl.Mention(ctx, "C1", "U1", fakeMention)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range mention.Envelopes {
		if _, err := f.sl.AckWait(ctx, 2*time.Second, id); err != nil {
			t.Fatalf("the acking client never got the event: %v", err)
		}
	}
}

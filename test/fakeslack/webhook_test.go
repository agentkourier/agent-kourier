package fakeslack_test

import (
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// alertPost is what Alertmanager's Slack receiver posts through an incoming webhook: no text, and one
// legacy attachment with its title, link, fallback, colour and the fields it asks Slack to render as mrkdwn.
func alertPost() fakeslack.WebhookPost {
	return fakeslack.WebhookPost{Channel: "C1", BotID: "B0EXAMPLE01", Username: "Alertmanager", Attachments: []fakeslack.Attachment{{
		Color:      "danger",
		Fallback:   "[FIRING:1] KubeAPIErrorBudgetBurn kube-system (critical) | http://am/#/alerts?receiver=slack-critical",
		Title:      "FIRING (1) - KubeAPIErrorBudgetBurn",
		TitleLink:  "http://am/#/alerts?receiver=slack-critical",
		Text:       "*KubeAPIErrorBudgetBurn* - critical",
		Footer:     "agent-kourier-key: KubeAPIErrorBudgetBurn/kube-system",
		MarkdownIn: []string{"text", "fields"},
	}}}
}

// An incoming webhook's post reaches the client as a bot_message event: the bot's ID and a name, no user and
// no app_id, empty text and the attachments, as the owner's channel shows it.
func TestWebhookDeliversABotMessageEvent(t *testing.T) {
	fake, smc := connected(t)
	got, err := fake.Webhook(alertPost())
	if err != nil || got.Channel != "C1" || got.TS == "" || len(got.Envelopes) != 1 {
		t.Fatalf("Webhook = %+v, %v; want one envelope", got, err)
	}
	env, _, inner := nextInner(t, smc)
	m, ok := inner.(*slackevents.MessageEvent)
	if !ok || env != got.Envelopes[0] || m.SubType != "bot_message" || m.BotID != "B0EXAMPLE01" || m.Username != "Alertmanager" ||
		m.User != "" || m.Text != "" || m.TimeStamp != got.TS || m.Channel != "C1" || m.ChannelType != "channel" || m.ThreadTimeStamp != "" {
		t.Fatalf("event = %#v; want a top-level bot_message from the webhook's bot with no user and no text", inner)
	}
	if len(m.Message.Attachments) != 1 {
		t.Fatalf("attachments = %+v; want one", m.Message.Attachments)
	}
	want := alertPost().Attachments[0]
	a := m.Message.Attachments[0]
	if a.ID != 1 || a.Color != want.Color || a.Fallback != want.Fallback || a.Title != want.Title || a.TitleLink != want.TitleLink ||
		a.Text != want.Text || a.Footer != want.Footer || !slices.Equal(a.MarkdownIn, want.MarkdownIn) {
		t.Fatalf("attachment = %+v; want %+v with the ID Slack gives", a, want)
	}
	waitAcked(t, fake, env)
}

// The shape on the wire has no user and no app_id (an incoming webhook has neither), where an app's own post
// has both.
func TestWebhookEventShapeOnTheWire(t *testing.T) {
	leakCheck(t)
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	ws := dial(t, fake)
	if _, _, err := ws.ReadMessage(); err != nil { // hello
		t.Fatalf("read hello: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := fake.Webhook(alertPost()); err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	_, raw, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	var env struct {
		Payload struct {
			Event map[string]any `json:"event"`
		}
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	ev := env.Payload.Event
	_, hasUser := ev["user"]
	_, hasApp := ev["app_id"]
	text, hasText := ev["text"]
	if hasUser || hasApp || !hasText || text != "" || ev["type"] != "message" || ev["subtype"] != "bot_message" || ev["bot_id"] != "B0EXAMPLE01" {
		t.Fatalf("event = %v; want type message, subtype bot_message, bot_id, empty text and neither user nor app_id", ev)
	}
}

// The post is stored like any message, so conversations.history and the control surface show it in the shape of
// the owner's channel too.
func TestWebhookPostIsStoredAndRead(t *testing.T) {
	fake, _ := connected(t)
	api := slack.New(testToken, slack.OptionAPIURL(fake.URL()+"/"))
	if _, err := fake.Webhook(alertPost()); err != nil {
		t.Fatal(err)
	}
	msgs := fake.Messages("C1")
	if len(msgs) != 1 || msgs[0].Subtype != "bot_message" || msgs[0].BotID != "B0EXAMPLE01" || msgs[0].AppID != "" || msgs[0].User != "" ||
		len(msgs[0].Attachments) != 1 {
		t.Fatalf("stored = %+v; want one bot_message with its attachment", msgs)
	}
	h := history(t, api, slack.GetConversationHistoryParameters{})
	if len(h.Messages) != 1 || h.Messages[0].SubType != "bot_message" || h.Messages[0].BotID != "B0EXAMPLE01" ||
		len(h.Messages[0].Attachments) != 1 || h.Messages[0].Attachments[0].Title != "FIRING (1) - KubeAPIErrorBudgetBurn" {
		t.Fatalf("history = %+v; want the webhook post", h.Messages)
	}
	raw := rawBody(t, fake, "conversations.history", url.Values{"token": {testToken}, "channel": {"C1"}})
	if strings.Contains(raw, `"user"`) || strings.Contains(raw, `"app_id"`) {
		t.Fatalf("history = %s; want a message with neither user nor app_id", raw)
	}
	// The caller's attachments are copied: changing them later changes nothing stored.
	p := alertPost()
	p.Channel = "C2"
	if _, err := fake.Webhook(p); err != nil {
		t.Fatal(err)
	}
	p.Attachments[0].Title = "changed"
	p.Attachments[0].MarkdownIn[0] = "changed"
	stored := fake.Messages("C2")[0].Attachments[0]
	if stored.Title == "changed" || stored.MarkdownIn[0] == "changed" {
		t.Fatalf("stored attachment %+v shares memory with the caller's", stored)
	}
	// And what a test reads back is a copy too.
	fake.Messages("C2")[0].Attachments[0].MarkdownIn[0] = "changed"
	if fake.Messages("C2")[0].Attachments[0].MarkdownIn[0] == "changed" {
		t.Fatal("Messages returned the store's own attachments")
	}
}

// With no client to deliver the event to, nothing happens, as for a user's message.
func TestWebhookWithNoClientIsRefused(t *testing.T) {
	fake, _ := newFake(t)
	if _, err := fake.Webhook(alertPost()); !errors.Is(err, fakeslack.ErrNoConnection) {
		t.Fatalf("Webhook with no client = %v; want ErrNoConnection", err)
	}
	if n := len(fake.Messages("C1")); n != 0 {
		t.Fatalf("a post nobody could receive was stored: %d messages", n)
	}
}

// The webhook may post as the fake's own bot, which is how a test shows Agent Kourier ignoring itself.
func TestWebhookMayPostAsTheFakesOwnBot(t *testing.T) {
	fake, smc := connected(t)
	p := alertPost()
	p.BotID = "BFAKEBOT"
	if _, err := fake.Webhook(p); err != nil {
		t.Fatal(err)
	}
	if _, _, inner := nextInner(t, smc); inner.(*slackevents.MessageEvent).BotID != "BFAKEBOT" {
		t.Fatalf("event = %#v; want the fake's own bot ID", inner)
	}
}

func TestWebhookRefusesWhatNoWebhookCouldPost(t *testing.T) {
	fake, _ := connected(t)
	for name, mutate := range map[string]func(*fakeslack.WebhookPost){
		"no channel":                 func(p *fakeslack.WebhookPost) { p.Channel = "" },
		"no bot ID":                  func(p *fakeslack.WebhookPost) { p.BotID = "" },
		"no text and no attachments": func(p *fakeslack.WebhookPost) { p.Attachments = nil },
		"an empty attachment":        func(p *fakeslack.WebhookPost) { p.Attachments = []fakeslack.Attachment{{Color: "good"}} },
	} {
		p := alertPost()
		mutate(&p)
		if _, err := fake.Webhook(p); !errors.Is(err, fakeslack.ErrInvalid) {
			t.Errorf("%s: Webhook = %v; want ErrInvalid", name, err)
		}
	}
	if n := len(fake.Messages("C1")); n != 0 {
		t.Fatalf("a refused post was stored: %d messages", n)
	}
}

// The control surface wraps the Go call one for one.
func TestControlPostsAsAWebhook(t *testing.T) {
	fake, smc := connected(t)
	var got fakeslack.UserMessage
	if s := control(t, fake, "POST", "/_control/webhook", alertPost(), &got); s != 200 || got.Channel != "C1" || len(got.Envelopes) != 1 {
		t.Fatalf("webhook = %d, %+v; want one envelope", s, got)
	}
	env, _, inner := nextInner(t, smc)
	if m := inner.(*slackevents.MessageEvent); env != got.Envelopes[0] || m.SubType != "bot_message" || m.Message.Attachments[0].TitleLink != "http://am/#/alerts?receiver=slack-critical" {
		t.Fatalf("event = %#v (envelope %s); want the webhook's bot_message", inner, env)
	}
	var errBody map[string]string
	bad := alertPost()
	bad.BotID = ""
	if s := control(t, fake, "POST", "/_control/webhook", bad, &errBody); s != 400 {
		t.Fatalf("webhook without a bot ID = %d, %v; want 400", s, errBody)
	}
	if s := control(t, fake, "POST", "/_control/webhook", `{"channel":"C1","bot_id":"B1","text":"x","nope":1}`, &errBody); s != 400 {
		t.Fatalf("webhook with an unknown field = %d; want 400", s)
	}
}

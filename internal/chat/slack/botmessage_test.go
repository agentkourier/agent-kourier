package slack_test

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	slackgo "github.com/slack-go/slack"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// webhookBot is the bot of the incoming webhook in the owner's alert channel, which posts Alertmanager's alerts, Grafana's
// and the heartbeat.
const webhookBot = "B0123456789"

// alert is what Alertmanager's Slack receiver posts through the webhook for a kind alert: no text, one legacy
// attachment, whose text Slack is asked to render as mrkdwn.
func alert() fakeslack.WebhookPost {
	const link = "http://alertmanager.kind.local/#/alerts?receiver=slack-critical-kind"
	return fakeslack.WebhookPost{Channel: "C1", BotID: webhookBot, Username: "Alertmanager", Attachments: []fakeslack.Attachment{{
		Color:      "danger",
		Title:      "[kind] FIRING (1) - KubePodCrashLooping",
		TitleLink:  link,
		Fallback:   "[FIRING:1] KubePodCrashLooping shop (checkout-7d9f8b6c5-x2x4k critical) | " + link,
		Text:       "*KubePodCrashLooping* - critical\nPod is crash looping.\n",
		Footer:     "agent-kourier-key: KubePodCrashLooping/shop",
		MarkdownIn: []string{"text", "fields"},
	}}}
}

// botSink is a Sink that takes the messages of the bots it is told of, as the neutral Intake does, and records
// the questions it was asked.
type botSink struct {
	*sink
	mu    sync.Mutex
	takes func(channel, botID string) bool
	asked [][2]string
}

func (b *botSink) AcceptsBot(channel, botID string) bool {
	b.mu.Lock()
	b.asked = append(b.asked, [2]string{channel, botID})
	b.mu.Unlock()
	return b.takes(channel, botID)
}

func (b *botSink) questions() [][2]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.asked)
}

// startBots runs a Client against a fake Slack, with a sink that takes the bot allowed in channel, and logs to
// the buffer it returns.
func startBots(t *testing.T, allowedChannel, allowedBot string) (*fakeslack.Server, *botSink, *syncBuffer) {
	t.Helper()
	fake := newFake(t)
	logs := &syncBuffer{}
	cfg := clientConfig(fake)
	cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sk := &botSink{sink: &sink{}, takes: func(channel, bot string) bool { return channel == allowedChannel && bot == allowedBot }}
	run(t, slack.New(cfg), sk)
	waitConnected(t, fake)
	return fake, sk, logs
}

// An incoming webhook's post reaches the sink as a bot's message: no user, no text of its own, the bot's ID, and
// the first attachment flattened into the fields a trigger reads, as raw as Slack delivered them.
func TestAWebhookPostIsForwardedWithItsAttachmentFlattened(t *testing.T) {
	fake, sk, _ := startBots(t, "C1", webhookBot)
	got, err := fake.Webhook(alert())
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, fake, got.Envelopes...)
	a := alert().Attachments[0]
	want := []slack.Message{{Channel: "C1", TS: got.TS, ChannelType: "channel", Bot: slack.BotPost{
		ID: webhookBot, Title: a.Title, TitleLink: a.TitleLink, Fallback: a.Fallback, Footer: a.Footer, Text: a.Text}}}
	if msgs := sk.messages(); !slices.Equal(msgs, want) {
		t.Fatalf("messages =\n%+v\nwant\n%+v", msgs, want)
	}
	if q := sk.questions(); !slices.Equal(q, [][2]string{{"C1", webhookBot}}) {
		t.Errorf("the sink was asked %v; want whether it takes %s in C1", q, webhookBot)
	}
}

// Of several attachments only the first is read, so that the title, link and fallback a trigger matches are one
// attachment's. mrkdwn_in changes how Slack renders text and not the text: whatever it names, and whatever markup
// and escapes the text holds, the fields are what was delivered.
func TestOnlyTheFirstAttachmentIsReadAndItsTextIsRaw(t *testing.T) {
	fake, sk, _ := startBots(t, "C1", webhookBot)
	p := alert()
	p.Attachments[0].Text = "*bold* _it_ `code` <http://x.example|link> <!channel> &amp; &lt;b&gt;"
	p.Attachments[0].MarkdownIn = []string{"text", "pretext", "fields"}
	p.Attachments = append(p.Attachments, fakeslack.Attachment{Title: "second title", TitleLink: "http://second", Fallback: "second fallback", Text: "second text", Footer: "second footer"})
	got, err := fake.Webhook(p)
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, fake, got.Envelopes...)
	msgs := sk.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v; want one", msgs)
	}
	if b := msgs[0].Bot; b.Text != p.Attachments[0].Text || b.Title != p.Attachments[0].Title || b.Fallback != p.Attachments[0].Fallback ||
		b.TitleLink != p.Attachments[0].TitleLink || b.Footer != p.Attachments[0].Footer {
		t.Fatalf("bot post = %+v; want the first attachment, text unrendered and unescaped", b)
	}
}

// A message with text and no attachment, as a plain webhook posts, is a bot's message with only its text.
func TestAWebhookPostWithTextAndNoAttachment(t *testing.T) {
	fake, sk, _ := startBots(t, "C1", webhookBot)
	got, err := fake.Webhook(fakeslack.WebhookPost{Channel: "C1", BotID: webhookBot, Text: "deploy finished"})
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, fake, got.Envelopes...)
	want := []slack.Message{{Channel: "C1", TS: got.TS, ChannelType: "channel", Text: "deploy finished", Bot: slack.BotPost{ID: webhookBot}}}
	if msgs := sk.messages(); !slices.Equal(msgs, want) {
		t.Fatalf("messages = %+v; want %+v", msgs, want)
	}
}

// An app's own post (a bot user, a bot_id and an app_id, no subtype) is a bot's message too, with its text. A
// mention of Agent Kourier in a bot's text starts nothing.
func TestAnAppPostIsABotMessageAndNeverAMention(t *testing.T) {
	fake, sk, _ := startBots(t, "C1", "BOTHERAPP1")
	app := message(map[string]any{"user": "UOTHERAPP", "bot_id": "BOTHERAPP1", "app_id": "AOTHER", "text": botMark + " deploy done"})
	waitAck(t, fake, send(t, fake, app))
	want := []slack.Message{{Channel: "C1", TS: "1.1", User: "UOTHERAPP", Text: botMark + " deploy done", ChannelType: "channel", Bot: slack.BotPost{ID: "BOTHERAPP1"}}}
	if msgs := sk.messages(); !slices.Equal(msgs, want) {
		t.Fatalf("messages = %+v; want %+v, with Mentioned false", msgs, want)
	}
}

// What is not a bot's new message is dropped, and acknowledged, though the bot is one the sink takes: edits,
// deletions, a bot's other subtypes, an app_mention by a bot, a message with nothing in it, a direct message.
func TestABotsEditsDeletionsAndOtherSubtypesAreIgnored(t *testing.T) {
	fake, sk, _ := startBots(t, "C1", webhookBot)
	post := map[string]any{"subtype": "bot_message", "bot_id": webhookBot, "username": "Alertmanager", "user": "", "text": "",
		"attachments": []map[string]any{{"id": 1, "title": "FIRING (1) - X", "fallback": "[FIRING:1] X | l"}}}
	botEdit := message(map[string]any{"subtype": "message_changed", "user": "", "text": "", "hidden": true, "ts": "2.2", "message": map[string]any{
		"type": "message", "subtype": "bot_message", "bot_id": webhookBot, "text": "", "ts": "1.1", "edited": map[string]any{"user": "", "ts": "2.2"},
		"attachments": []map[string]any{{"id": 1, "title": "FIRING (1) - X", "fallback": "[FIRING:1] X | l"}}}})
	tests := []struct {
		name string
		ev   map[string]any
	}{
		{"an edit of a webhook's post", botEdit},
		{"a deletion of a webhook's post", message(map[string]any{"subtype": "message_deleted", "user": "", "text": "", "deleted_ts": "1.1", "previous_message": post})},
		{"a bot's file share", message(map[string]any{"subtype": "file_share", "user": "", "bot_id": webhookBot, "text": "x"})},
		{"a bot's thread broadcast", message(map[string]any{"subtype": "thread_broadcast", "thread_ts": "0.5", "user": "", "bot_id": webhookBot, "text": "x"})},
		{"a bot's channel join", message(map[string]any{"subtype": "channel_join", "user": "", "bot_id": webhookBot, "text": "x"})},
		{"a bot_message with nothing in it", message(map[string]any{"subtype": "bot_message", "bot_id": webhookBot, "user": "", "text": ""})},
		{"an app_mention by a bot", message(map[string]any{"type": "app_mention", "bot_id": webhookBot, "text": botMark + " x"})},
		{"a bot's direct message", message(map[string]any{"subtype": "bot_message", "bot_id": webhookBot, "channel": "D1", "channel_type": "im", "text": "x"})},
	}
	for _, tt := range tests {
		waitAck(t, fake, send(t, fake, tt.ev))
		if msgs := sk.messages(); len(msgs) != 0 {
			t.Fatalf("%s: the sink took %+v; want nothing", tt.name, msgs)
		}
	}
	// And the same bot's new message right after is taken: the sink was not shut by the edits.
	got, err := fake.Webhook(alert())
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, fake, got.Envelopes...)
	if len(sk.messages()) != 1 {
		t.Fatalf("the bot's new post was not forwarded after its edits were ignored: %+v", sk.messages())
	}
}

// A bot's reply in a thread is forwarded like its other posts, with its thread: the adapter is dumb about it, and the
// neutral Intake, which every platform shares, is what ignores it.
func TestABotsThreadReplyIsForwardedWithItsThread(t *testing.T) {
	fake, sk, _ := startBots(t, "C1", webhookBot)
	reply := message(map[string]any{"subtype": "bot_message", "bot_id": webhookBot, "user": "", "text": "x", "thread_ts": "0.5"})
	waitAck(t, fake, send(t, fake, reply))
	if msgs := sk.messages(); len(msgs) != 1 || msgs[0].ThreadTS != "0.5" {
		t.Fatalf("messages = %+v; want the reply with its thread", msgs)
	}
}

// Only a bot the sink says it takes in the channel is forwarded: another bot, the same bot in another channel,
// and any bot at all for a sink that cannot say.
func TestOnlyABotTheSinkTakesIsForwarded(t *testing.T) {
	fake, sk, _ := startBots(t, "C1", webhookBot)
	other := alert()
	other.BotID = "B0OTHERBOT1"
	elsewhere := alert()
	elsewhere.Channel = "C2"
	for name, p := range map[string]fakeslack.WebhookPost{"another bot": other, "the bot in another channel": elsewhere} {
		got, err := fake.Webhook(p)
		if err != nil {
			t.Fatal(err)
		}
		waitAck(t, fake, got.Envelopes...)
		if msgs := sk.messages(); len(msgs) != 0 {
			t.Fatalf("%s: the sink took %+v; want nothing", name, msgs)
		}
	}
	if q := sk.questions(); len(q) != 2 {
		t.Errorf("the sink was asked %v; want one question for each", q)
	}

	// A sink that has no way to say gets no bot's message, but still gets a person's.
	plain := &sink{}
	fake2 := start(t, plain)
	for _, ev := range []map[string]any{
		message(map[string]any{"user": "UOTHER", "bot_id": "BOTHER"}),
		message(map[string]any{"subtype": "bot_message", "bot_id": webhookBot, "user": "", "text": "x"}),
		message(map[string]any{"text": "from a person"}),
	} {
		waitAck(t, fake2, send(t, fake2, ev))
	}
	if msgs := plain.messages(); len(msgs) != 1 || msgs[0].Text != "from a person" {
		t.Fatalf("a sink without BotFilter took %+v; want only the person's message", msgs)
	}
}

// Agent Kourier's own messages are never forwarded, by user ID or by bot ID, even for a sink that takes every bot, as
// when a trigger names Agent Kourier's own bot ID by mistake. That mistake is logged once, at WARN, and the log says
// nothing of the message.
func TestAgentKouriersOwnMessagesAreNeverForwardedEvenIfATriggerNamesThem(t *testing.T) {
	fake := newFake(t)
	logs := &syncBuffer{}
	cfg := clientConfig(fake)
	cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sk := &botSink{sink: &sink{}, takes: func(string, string) bool { return true }}
	run(t, slack.New(cfg), sk)
	waitConnected(t, fake)

	// A webhook post that carries Agent Kourier's bot ID.
	own := alert()
	own.BotID = "BFAKEBOT"
	own.Attachments[0].Title = "SECRET-OWN-TITLE"
	var envelopes []string
	for range 2 {
		got, err := fake.Webhook(own)
		if err != nil {
			t.Fatal(err)
		}
		envelopes = append(envelopes, got.Envelopes...)
	}
	// An app's post of Agent Kourier's user ID with another bot ID, and one with Agent Kourier's bot ID and another user.
	envelopes = append(envelopes,
		send(t, fake, message(map[string]any{"ts": "5.5", "event_ts": "5.5", "user": "UFAKEBOT", "bot_id": "BOTHERAPP1", "text": "x"})),
		send(t, fake, message(map[string]any{"ts": "5.6", "event_ts": "5.6", "user": "UOTHER", "bot_id": "BFAKEBOT", "text": "x"})))
	// And Agent Kourier's own real posts, which the fake sends back as events, an edit among them.
	api := slackgo.New(botToken, slackgo.OptionAPIURL(fake.URL()+"/"))
	_, ts, err := api.PostMessage("C1", slackgo.MsgOptionText("an answer", false))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := api.UpdateMessage("C1", ts, slackgo.MsgOptionText("an answer, edited", false)); err != nil {
		t.Fatal(err)
	}
	waitAck(t, fake, slices.Concat(envelopes, fake.OwnEventEnvelopes("C1", ts))...)

	if msgs := sk.messages(); len(msgs) != 0 {
		t.Fatalf("agent-kourier's own messages reached the sink: %+v", msgs)
	}
	if n := logs.count("WARN", "names Agent Kourier's own bot ID"); n != 1 {
		t.Errorf("%d warnings that a trigger names Agent Kourier's own bot ID; want exactly one for all of its posts\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "SECRET-OWN-TITLE") {
		t.Errorf("the log holds the message: %s", logs.String())
	}
}

// With no trigger naming Agent Kourier's own bot ID there is nothing to warn of.
func TestAgentKouriersOwnPostsWarnOfNothingWhenNoTriggerNamesThem(t *testing.T) {
	fake, sk, logs := startBots(t, "C1", webhookBot)
	own := alert()
	own.BotID = "BFAKEBOT"
	got, err := fake.Webhook(own)
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, fake, got.Envelopes...)
	if len(sk.messages()) != 0 || logs.count("WARN") != 0 {
		t.Fatalf("messages %+v, log %s; want no message and no warning", sk.messages(), logs.String())
	}
}

// botIntake is the adapter with the neutral Intake as its sink, over a config that binds C1 to a Binding with the
// given triggers: what a bot's message becomes when it goes through the adapter and the trigger table.
type botIntake struct {
	fake    *fakeslack.Server
	reg     *prometheus.Registry
	logs    *syncBuffer
	mu      sync.Mutex
	matches []chat.TriggerMatch
}

func (b *botIntake) got() []chat.TriggerMatch {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.matches)
}

func (b *botIntake) unmatched(t *testing.T) float64 {
	t.Helper()
	families, err := b.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "agentkourier_trigger_unmatched_total" {
			for _, m := range f.Metric {
				return m.Counter.GetValue()
			}
		}
	}
	return 0
}

func startBotIntake(t *testing.T, triggers ...v1alpha1.Trigger) *botIntake {
	t.Helper()
	meta := func(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	allow := []string{"sre"}
	b := v1alpha1.Binding{ObjectMeta: meta("sre", "sre-alerts"), Spec: v1alpha1.BindingSpec{
		Agent:    v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "kagent"}, Namespace: "kagent", Name: "sre-agent"},
		Identity: v1alpha1.Identity{UserID: "agent-kourier-sre"},
		Chat:     v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "slack-main"}, Channel: "C1", Triggers: triggers}}}
	b.Default()
	if err := b.Validate(); err != nil {
		t.Fatalf("the test's triggers are invalid: %v", err)
	}
	snap, err := config.Resolve(t.Context(), &config.Set{
		AgentBackends:   []v1alpha1.AgentBackend{{ObjectMeta: meta("agent-kourier-system", "kagent"), Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, AllowedNamespaces: allow}}},
		ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: meta("agent-kourier-system", "slack-main"), Spec: v1alpha1.ChatConnectionSpec{AllowedNamespaces: allow}}},
		Bindings:        []v1alpha1.Binding{b},
	}, allKeys{})
	if err != nil {
		t.Fatal(err)
	}
	bi := &botIntake{reg: prometheus.NewRegistry(), logs: &syncBuffer{}}
	obs, err := telemetry.New(bi.reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(bi.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sub := &submitter{}
	intake := chat.NewIntake(chat.IntakeConfig{
		Connection: v1alpha1.ObjectRef{Namespace: "agent-kourier-system", Name: "slack-main"},
		Snapshot:   func() *config.Snapshot { return snap }, Submitter: sub, Logger: log,
		OnStart: func(_ context.Context, m chat.TriggerMatch) error {
			bi.mu.Lock()
			defer bi.mu.Unlock()
			bi.matches = append(bi.matches, m)
			return nil
		}})
	bi.fake = newFake(t)
	cfg := clientConfig(bi.fake)
	cfg.Logger = log
	platform := slack.NewPlatform(slack.New(cfg), intakeConn)
	ctx, cancel := context.WithCancel(telemetry.WithObserver(t.Context(), obs))
	done := make(chan error, 1)
	go func() { done <- platform.Run(ctx, intake) }()
	t.Cleanup(func() {
		cancel()
		<-done
		if got := sub.submitted(); len(got) != 0 {
			t.Errorf("a bot's message started a session: %+v (the Intake hands starts on, and CT-4 starts the session)", got)
		}
	})
	waitConnected(t, bi.fake)
	return bi
}

// loose matches any alert-shaped fallback, and excludes the Watchdog heartbeat by its title: the heartbeat's fallback
// passes the match, so that it is the exclude alone that turns it away.
func loose() v1alpha1.Trigger {
	return v1alpha1.Trigger{Type: v1alpha1.TriggerBotMessage, From: &v1alpha1.BotSource{BotID: webhookBot}, PromptTemplate: "An alert.",
		Match:   map[v1alpha1.MessageField]string{v1alpha1.FieldFallback: `^\[(?:FIRING|RESOLVED)`},
		Exclude: map[v1alpha1.MessageField]string{v1alpha1.FieldTitle: `^Prometheus heartbeat$`},
		Extract: map[string]v1alpha1.Extract{"state": {Field: v1alpha1.FieldFallback, Regex: `^\[(?P<state>FIRING|RESOLVED)`}}}
}

// An allowlisted bot's alert goes through the adapter and the trigger table: the trigger matches, with what it
// extracted and the message flattened. The heartbeat, which the same bot posts to the same channel, passes the
// bot's name and is ignored by the exclude; another bot's alert, and Agent Kourier's own message, never get as far as
// the table.
func TestFromTheWebhookToAMatchedTrigger(t *testing.T) {
	bi := startBotIntake(t, loose())

	alertPost, err := bi.fake.Webhook(alert())
	if err != nil {
		t.Fatal(err)
	}
	beat := alert()
	beat.Attachments[0].Title = "Prometheus heartbeat"
	beat.Attachments[0].Fallback = "[FIRING:1] Watchdog  (none prometheus) | http://alertmanager/#/alerts?receiver=slack-heartbeat"
	beatPost, err := bi.fake.Webhook(beat)
	if err != nil {
		t.Fatal(err)
	}
	stranger := alert()
	stranger.BotID = "B0OTHERBOT1"
	strangerPost, err := bi.fake.Webhook(stranger)
	if err != nil {
		t.Fatal(err)
	}
	own := alert()
	own.BotID = "BFAKEBOT"
	ownPost, err := bi.fake.Webhook(own)
	if err != nil {
		t.Fatal(err)
	}
	waitAck(t, bi.fake, slices.Concat(alertPost.Envelopes, beatPost.Envelopes, strangerPost.Envelopes, ownPost.Envelopes)...)

	matches := bi.got()
	if len(matches) != 1 {
		t.Fatalf("matched %d messages %+v; want only the alert", len(matches), matches)
	}
	m := matches[0]
	a := alert().Attachments[0]
	if m.Binding != "sre/sre-alerts" || m.Index != 0 || m.Fields["state"] != "FIRING" || m.Message.At.ID != alertPost.TS || m.Message.Bot == nil ||
		*m.Message.Bot != (chat.BotMessage{BotID: webhookBot, Title: a.Title, TitleLink: a.TitleLink, Fallback: a.Fallback, Footer: a.Footer, AttachmentText: a.Text}) {
		t.Fatalf("match = %+v; want the alert of the webhook bot, flattened", m)
	}
	// The heartbeat reached the table, where the exclude counted it as unmatched; the other two were stopped at
	// the adapter and never counted.
	if n := bi.unmatched(t); n != 1 {
		t.Errorf("agentkourier_trigger_unmatched_total = %v; want 1, the heartbeat alone", n)
	}
}

// A bot's edit of its alert, and a reply of the bot in the alert's thread, start nothing.
func TestAnEditAndAThreadReplyOfTheWebhookBotMatchNothing(t *testing.T) {
	bi := startBotIntake(t, loose())
	first, err := bi.fake.Webhook(alert())
	if err != nil {
		t.Fatal(err)
	}
	attach := []map[string]any{{"id": 1, "title": "[kind] FIRING (1) - X", "fallback": "[FIRING:1] X | l"}}
	edit := message(map[string]any{"subtype": "message_changed", "user": "", "text": "", "hidden": true, "ts": "9.9", "event_ts": "9.9", "message": map[string]any{
		"type": "message", "subtype": "bot_message", "bot_id": webhookBot, "text": "", "ts": first.TS, "attachments": attach}})
	reply := message(map[string]any{"subtype": "bot_message", "bot_id": webhookBot, "user": "", "text": "", "ts": "9.10", "event_ts": "9.10", "thread_ts": first.TS, "attachments": attach})
	waitAck(t, bi.fake, send(t, bi.fake, edit), send(t, bi.fake, reply))
	if matches := bi.got(); len(matches) != 1 || matches[0].Message.At.ID != first.TS {
		t.Fatalf("matches = %+v; want only the first post", matches)
	}
}

// A trigger that names Agent Kourier's own bot ID matches nothing, and warns once.
func TestATriggerNamingAgentKouriersOwnBotMatchesNothing(t *testing.T) {
	mistake := loose()
	mistake.From = &v1alpha1.BotSource{BotID: "BFAKEBOT"}
	bi := startBotIntake(t, mistake)
	own := alert()
	own.BotID = "BFAKEBOT"
	var envelopes []string
	for range 2 {
		got, err := bi.fake.Webhook(own)
		if err != nil {
			t.Fatal(err)
		}
		envelopes = append(envelopes, got.Envelopes...)
	}
	waitAck(t, bi.fake, envelopes...)
	if matches := bi.got(); len(matches) != 0 {
		t.Fatalf("matched Agent Kourier's own message: %+v", matches)
	}
	if n := bi.logs.count("WARN", "names Agent Kourier's own bot ID"); n != 1 {
		t.Errorf("%d warnings; want one\n%s", n, bi.logs.String())
	}
	if n := bi.unmatched(t); n != 0 {
		t.Errorf("agent-kourier's own messages were counted as unmatched: %v", n)
	}
}

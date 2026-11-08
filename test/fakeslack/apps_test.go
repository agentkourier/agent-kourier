package fakeslack_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// Two apps in one workspace, and a third in another, with obviously fake tokens.
var (
	appA = fakeslack.App{
		AppID: "AAPPA", BotUser: "UBOTA", BotID: "BBOTA", TeamID: "TTEAMA", TeamName: "team-a",
		BotToken: "xoxb-app-a-token", AppToken: "xapp-app-a-token",
	}
	appB = fakeslack.App{
		AppID: "AAPPB", BotUser: "UBOTB", BotID: "BBOTB", TeamID: "TTEAMA", TeamName: "team-a",
		BotToken: "xoxb-app-b-token", AppToken: "xapp-app-b-token",
	}
)

// twoApps starts a fake that hosts appA and appB beside its default app.
func twoApps(t *testing.T, opts ...fakeslack.Option) *fakeslack.Server {
	t.Helper()
	leakCheck(t)
	fake := fakeslack.New(append([]fakeslack.Option{fakeslack.WithApp(appA), fakeslack.WithApp(appB)}, opts...)...)
	t.Cleanup(fake.Close)
	return fake
}

// webAs returns a Web API client that authenticates as the app.
func webAs(fake *fakeslack.Server, a fakeslack.App) *slack.Client {
	return slack.New(a.BotToken, slack.OptionAPIURL(fake.URL()+"/"), slack.OptionAppLevelToken(a.AppToken))
}

// socketAs runs a Socket Mode client as the app and waits for its hello.
func socketAs(t *testing.T, fake *fakeslack.Server, a fakeslack.App) *socketmode.Client {
	t.Helper()
	smc := runClientWith(t, webAs(fake, a))
	waitEvent(t, smc, socketmode.EventTypeHello)
	return smc
}

// runClientWith runs a Socket Mode client on api until the test ends.
func runClientWith(t *testing.T, api *slack.Client) *socketmode.Client {
	t.Helper()
	smc := socketmode.New(api)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = smc.RunContext(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return smc
}

// payload is the part of an events_api payload these tests read.
type payload struct {
	APIAppID       string `json:"api_app_id"`
	TeamID         string `json:"team_id"`
	Authorizations []struct {
		TeamID string `json:"team_id"`
		UserID string `json:"user_id"`
	} `json:"authorizations"`
	Event struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		User    string `json:"user"`
		BotID   string `json:"bot_id"`
		AppID   string `json:"app_id"`
		Text    string `json:"text"`
		Channel string `json:"channel"`
	} `json:"event"`
}

// nextPayload waits for an Events API envelope, acks it, and returns its envelope_id and payload.
func nextPayload(t *testing.T, smc *socketmode.Client) (string, payload) {
	t.Helper()
	ev := waitEvent(t, smc, socketmode.EventTypeEventsAPI)
	if err := smc.Ack(*ev.Request); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	var p payload
	if err := json.Unmarshal(ev.Request.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return ev.Request.EnvelopeID, p
}

// quiet fails the test if the client is sent an Events API or interactive envelope within a short while. A
// negative check cannot be slow-machine flaky: an envelope that is late is still not one the test saw.
func quiet(t *testing.T, name string, smc *socketmode.Client) {
	t.Helper()
	timeout := time.After(150 * time.Millisecond)
	for {
		select {
		case ev := <-smc.Events:
			if ev.Type == socketmode.EventTypeEventsAPI || ev.Type == socketmode.EventTypeInteractive || ev.Type == socketmode.EventTypeSlashCommand {
				t.Fatalf("%s was sent a %q envelope, want none", name, ev.Type)
			}
		case <-timeout:
			return
		}
	}
}

func TestAppsAuthenticateByTheirOwnTokens(t *testing.T) {
	fake := twoApps(t)
	for _, tc := range []struct {
		name, token, user, bot, team, teamName string
	}{
		{"app a", appA.BotToken, "UBOTA", "BBOTA", "TTEAMA", "team-a"},
		{"app b, in a's workspace", appB.BotToken, "UBOTB", "BBOTB", "TTEAMA", "team-a"},
		{"a token no app owns is the default app's", "xoxb-nobody-token", "UFAKEBOT", "BFAKEBOT", "TFAKE", "fakeworkspace"},
	} {
		got, err := slack.New(tc.token, slack.OptionAPIURL(fake.URL()+"/")).AuthTest()
		if err != nil || got.UserID != tc.user || got.BotID != tc.bot || got.TeamID != tc.team || got.Team != tc.teamName {
			t.Errorf("%s: auth.test = %+v, %v; want user %s, bot %s, team %s (%s)", tc.name, got, err, tc.user, tc.bot, tc.team, tc.teamName)
		}
	}

	// Each token works as the type it is: an app token is no Web API token and a bot token opens no socket.
	if _, err := slack.New(appA.AppToken, slack.OptionAPIURL(fake.URL()+"/")).AuthTest(); err == nil || err.Error() != "invalid_auth" {
		t.Errorf("auth.test with an app token = %v, want invalid_auth", err)
	}
	if _, _, err := slack.New(appA.BotToken, slack.OptionAPIURL(fake.URL()+"/"), slack.OptionAppLevelToken(appA.BotToken)).
		StartSocketModeContext(t.Context()); err == nil || err.Error() != "invalid_auth" {
		t.Errorf("apps.connections.open with a bot token = %v, want invalid_auth", err)
	}
}

func TestWithAppIgnoresAnAppThatFailsValidation(t *testing.T) {
	bad := []fakeslack.App{
		{BotUser: "U1", BotID: "B1", BotToken: "xoxb-x"},
		{AppID: "A1", BotID: "B1", BotToken: "xoxb-x"},
		{AppID: "A1", BotUser: "U1", BotToken: "xoxb-x"},
		{AppID: "A1", BotUser: "U1", BotID: "B1", BotToken: "tok"},
		{AppID: "A1", BotUser: "U1", BotID: "B1", AppToken: "xoxb-x"},
	}
	for i, a := range bad {
		if a.Validate() == nil {
			t.Errorf("case %d: Validate() = nil, want an error", i)
		}
		fake := fakeslack.NewHandler(fakeslack.WithApp(a))
		if n := len(fake.Apps()); n != 1 {
			t.Errorf("case %d: %d apps hosted, want only the default app", i, n)
		}
		fake.Close()
	}
	if err := appA.Validate(); err != nil {
		t.Errorf("Validate on a good app = %v", err)
	}
}

func TestEachAppPostsAsItsOwnBot(t *testing.T) {
	fake := twoApps(t, fakeslack.WithoutOwnPostEvents())
	a, b := webAs(fake, appA), webAs(fake, appB)
	_, tsA, err := a.PostMessage("C1", text("from a"))
	if err != nil {
		t.Fatalf("a posts: %v", err)
	}
	_, tsB, err := b.PostMessage("C1", text("from b"))
	if err != nil {
		t.Fatalf("b posts: %v", err)
	}
	for ts, want := range map[string]fakeslack.App{tsA: appA, tsB: appB} {
		i := slices.IndexFunc(fake.Messages("C1"), func(m fakeslack.Message) bool { return m.TS == ts })
		m := fake.Messages("C1")[i]
		if m.User != want.BotUser || m.BotID != want.BotID || m.AppID != want.AppID {
			t.Errorf("message %s = user %s, bot %s, app %s; want %s, %s, %s", ts, m.User, m.BotID, m.AppID, want.BotUser, want.BotID, want.AppID)
		}
	}

	// Reactions are the calling bot's: both bots may add one name, and each removes only its own.
	if err := a.AddReaction("eyes", slack.ItemRef{Channel: "C1", Timestamp: tsA}); err != nil {
		t.Fatalf("a reacts: %v", err)
	}
	if err := b.AddReaction("eyes", slack.ItemRef{Channel: "C1", Timestamp: tsA}); err != nil {
		t.Fatalf("b reacts to the same name: %v", err)
	}
	if err := b.AddReaction("eyes", slack.ItemRef{Channel: "C1", Timestamp: tsA}); err == nil || err.Error() != "already_reacted" {
		t.Errorf("b reacts twice = %v, want already_reacted", err)
	}
	if got := fake.Reactions("C1", tsA); len(got) != 1 || !slices.Equal(got[0].Users, []string{"UBOTA", "UBOTB"}) {
		t.Fatalf("reactions = %+v, want eyes by both bots", got)
	}
	if err := b.RemoveReaction("eyes", slack.ItemRef{Channel: "C1", Timestamp: tsA}); err != nil {
		t.Fatalf("b removes its own: %v", err)
	}
	if err := b.RemoveReaction("eyes", slack.ItemRef{Channel: "C1", Timestamp: tsA}); err == nil || err.Error() != "no_reaction" {
		t.Errorf("b removes again = %v, want no_reaction", err)
	}
	if got := fake.Reactions("C1", tsA); len(got) != 1 || !slices.Equal(got[0].Users, []string{"UBOTA"}) {
		t.Errorf("reactions = %+v, want eyes by a alone", got)
	}

	// A message is changed by the app that posted it.
	if _, _, _, err := b.UpdateMessage("C1", tsA, text("b edits a")); err == nil || err.Error() != "cant_update_message" {
		t.Errorf("b updates a's message = %v, want cant_update_message", err)
	}
	if _, _, _, err := a.UpdateMessage("C1", tsA, text("a edits a")); err != nil {
		t.Errorf("a updates its own message: %v", err)
	}
	if m := fake.Messages("C1")[0]; m.Edited == nil || m.Edited.User != "UBOTA" {
		t.Errorf("edited = %+v, want by a's bot", m.Edited)
	}

	// An ephemeral and a stream carry the posting app, and only that app continues the stream.
	if _, err := b.PostEphemeral("C1", "U1", text("psst")); err != nil {
		t.Fatalf("b ephemeral: %v", err)
	}
	if eph := fake.Ephemerals("C1"); len(eph) != 1 || eph[0].AppID != "AAPPB" || eph[0].User != "UBOTB" {
		t.Errorf("ephemerals = %+v, want b's", eph)
	}
	root := post(t, a, text("root"))
	streamTS, err := startIn(a, "C1", root, append(recipients(), slack.MsgOptionChunks(slack.NewMarkdownTextChunk("start")))...)
	if err != nil {
		t.Fatalf("a starts a stream: %v", err)
	}
	more := slack.MsgOptionChunks(slack.NewMarkdownTextChunk("more"))
	if _, _, err := b.AppendStream("C1", streamTS, more); err == nil || err.Error() != "message_not_in_streaming_state" {
		t.Errorf("b appends to a's stream = %v, want message_not_in_streaming_state", err)
	}
	if _, _, err := a.AppendStream("C1", streamTS, more); err != nil {
		t.Errorf("a appends to its own stream: %v", err)
	}
}

func TestChannelMembershipIsPerApp(t *testing.T) {
	fake := twoApps(t)
	fake.SetMembers("C1", "U1", "UBOTA")
	member := func(a fakeslack.App) bool {
		t.Helper()
		ch, err := webAs(fake, a).GetConversationInfo(&slack.GetConversationInfoInput{ChannelID: "C1"})
		if err != nil {
			t.Fatalf("conversations.info as %s: %v", a.AppID, err)
		}
		return ch.IsMember
	}
	if !member(appA) || member(appB) {
		t.Fatalf("is_member for a and b = %v, %v; want true, false", member(appA), member(appB))
	}
	if err := fake.Join("C1", "AAPPB"); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if err := fake.Join("C1", "AAPPB"); err != nil {
		t.Fatalf("Join twice: %v", err)
	}
	if !member(appA) || !member(appB) {
		t.Errorf("after b joined, is_member for a and b = %v, %v; want true, true", member(appA), member(appB))
	}
	members, _, err := webAs(fake, appB).GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: "C1"})
	if err != nil || !slices.Equal(members, []string{"U1", "UBOTA", "UBOTB"}) {
		t.Errorf("conversations.members = %v, %v; want the two bots once each beside U1", members, err)
	}
	if err := fake.Join("C1", "ANOBODY"); !errors.Is(err, fakeslack.ErrNotFound) {
		t.Errorf("Join of an app the Server does not host = %v, want ErrNotFound", err)
	}
	// The default app's bot joins through the empty app of the control surface; its token is any other.
	if got, err := slack.New("xoxb-nobody-token", slack.OptionAPIURL(fake.URL()+"/")).GetConversationInfo(&slack.GetConversationInfoInput{ChannelID: "C1"}); err != nil || got.IsMember {
		t.Errorf("default app is_member = %v, %v; want false", got.IsMember, err)
	}
}

func TestAMessageReachesEveryAppInTheChannelOverItsOwnSockets(t *testing.T) {
	fake := twoApps(t, fakeslack.WithoutOwnPostEvents())
	smcA, smcB := socketAs(t, fake, appA), socketAs(t, fake, appB)
	fake.SetMembers("C1", "U1", "UBOTA", "UBOTB")
	fake.SetMembers("C2", "U1", "UBOTA")

	got, err := fake.Mention("C1", "U1", "<@UBOTA> look at this")
	if err != nil {
		t.Fatalf("Mention: %v", err)
	}
	if len(got.AppEnvelopes["AAPPA"]) != 2 || len(got.AppEnvelopes["AAPPB"]) != 1 || len(got.Envelopes) != 3 || len(got.AppEnvelopes) != 2 {
		t.Fatalf("envelopes = %v, by app %v; want a: mention and message, b: message", got.Envelopes, got.AppEnvelopes)
	}

	envA1, mention := nextPayload(t, smcA)
	envA2, message := nextPayload(t, smcA)
	envB, other := nextPayload(t, smcB)
	if mention.Event.Type != "app_mention" || message.Event.Type != "message" || other.Event.Type != "message" {
		t.Fatalf("events = %q, %q for a and %q for b; want app_mention and message, then message", mention.Event.Type, message.Event.Type, other.Event.Type)
	}
	if envA1 != got.AppEnvelopes["AAPPA"][0] || envA2 != got.AppEnvelopes["AAPPA"][1] || envB != got.AppEnvelopes["AAPPB"][0] {
		t.Errorf("envelopes seen = %q, %q, %q; want the ones returned", envA1, envA2, envB)
	}
	// Each envelope is made for the app it goes to.
	for name, tc := range map[string]struct {
		p    payload
		user string
		app  string
	}{"a": {message, "UBOTA", "AAPPA"}, "b": {other, "UBOTB", "AAPPB"}} {
		if tc.p.APIAppID != tc.app || tc.p.TeamID != "TTEAMA" || len(tc.p.Authorizations) != 1 ||
			tc.p.Authorizations[0].UserID != tc.user || tc.p.Authorizations[0].TeamID != "TTEAMA" {
			t.Errorf("%s's envelope = %+v; want api_app_id %s and the authorization of %s", name, tc.p, tc.app, tc.user)
		}
	}
	waitAcked(t, fake, got.Envelopes...)

	// C2 has a's bot alone, and the mention of b's bot in it goes to nobody.
	if _, err := fake.Mention("C2", "U1", "<@UBOTA> only you"); err != nil {
		t.Fatalf("Mention in C2: %v", err)
	}
	if _, err := fake.Mention("C2", "U1", "<@UBOTB> not you"); err != nil {
		t.Fatalf("Mention of b in C2: %v", err)
	}
	for range 3 { // a's mention and message, then the message of the second text
		nextPayload(t, smcA)
	}
	quiet(t, "b", smcB)

	// A text that mentions no bot is refused, and the error names the bots.
	if _, err := fake.Mention("C1", "U1", "<@USOMEONE> hello"); !errors.Is(err, fakeslack.ErrInvalid) || !strings.Contains(err.Error(), "<@UBOTB>") {
		t.Errorf("Mention of no bot = %v, want ErrInvalid naming the bots", err)
	}
}

func TestOnePostIsSeenByAnotherAppAsABotMessage(t *testing.T) {
	fake := twoApps(t)
	smcA, smcB := socketAs(t, fake, appA), socketAs(t, fake, appB)
	fake.SetMembers("C1", "U1", "UBOTA", "UBOTB")
	fake.SetMembers("C2", "U1", "UBOTA")

	_, ts, err := webAs(fake, appA).PostMessage("C1", text("a speaks"))
	if err != nil {
		t.Fatalf("a posts: %v", err)
	}
	// The poster hears itself, as the fake's one app always did, and b hears a.
	_, own := nextPayload(t, smcA)
	_, heard := nextPayload(t, smcB)
	for name, p := range map[string]payload{"a": own, "b": heard} {
		if p.Event.Type != "message" || p.Event.Subtype != "" || p.Event.User != "UBOTA" || p.Event.BotID != "BBOTA" ||
			p.Event.AppID != "AAPPA" || p.Event.Text != "a speaks" || p.Event.Channel != "C1" {
			t.Errorf("%s heard %+v; want a's bot message", name, p.Event)
		}
	}
	if own.APIAppID != "AAPPA" || heard.APIAppID != "AAPPB" || heard.Authorizations[0].UserID != "UBOTB" {
		t.Errorf("envelopes made for api_app_id %q and %q (authorized %v); want a's and b's own", own.APIAppID, heard.APIAppID, heard.Authorizations)
	}
	ownIDs, heardIDs := fake.OwnEventEnvelopes("C1", ts), fake.EventEnvelopes("AAPPB", "C1", ts)
	if len(ownIDs) != 1 || len(heardIDs) != 1 || ownIDs[0] == heardIDs[0] || len(fake.EventEnvelopes("AAPPA", "C1", ts)) != 1 {
		t.Errorf("OwnEventEnvelopes = %v, EventEnvelopes for b = %v; want one envelope each, apart", ownIDs, heardIDs)
	}
	waitAcked(t, fake, append(ownIDs, heardIDs...)...)

	// An update reaches both too, and a post in a channel b is not in reaches a alone.
	if _, _, _, err := webAs(fake, appA).UpdateMessage("C1", ts, text("a corrects")); err != nil {
		t.Fatalf("a updates: %v", err)
	}
	nextPayload(t, smcA)
	if _, changed := nextPayload(t, smcB); changed.Event.Subtype != "message_changed" {
		t.Errorf("b heard %+v of the update, want message_changed", changed.Event)
	}
	if _, _, err := webAs(fake, appA).PostMessage("C2", text("private")); err != nil {
		t.Fatalf("a posts in C2: %v", err)
	}
	nextPayload(t, smcA)
	quiet(t, "b", smcB)
}

func TestWithoutOwnPostEventsSilencesTheOtherAppsToo(t *testing.T) {
	fake := twoApps(t, fakeslack.WithoutOwnPostEvents())
	smcB := socketAs(t, fake, appB)
	fake.SetMembers("C1", "UBOTA", "UBOTB")
	if _, _, err := webAs(fake, appA).PostMessage("C1", text("quiet")); err != nil {
		t.Fatalf("a posts: %v", err)
	}
	quiet(t, "b", smcB)
}

func TestAPressGoesOnlyToTheAppThatPostedTheMessage(t *testing.T) {
	fake := twoApps(t, fakeslack.WithoutOwnPostEvents())
	smcA, smcB := socketAs(t, fake, appA), socketAs(t, fake, appB)
	fake.SetMembers("C1", "U1", "UBOTA", "UBOTB")
	ts := post(t, webAs(fake, appB), text("approve?"), slack.MsgOptionBlocks(
		slack.NewActionBlock("buttons", slack.NewButtonBlockElement("approve", "yes", plain("Approve")))))

	id, err := fake.Click("U1", ts, "approve", "yes")
	if err != nil {
		t.Fatalf("Click: %v", err)
	}
	gotID, cb := nextInteraction(t, smcB)
	if gotID != id || cb.APIAppID != "AAPPB" || cb.Team.ID != "TTEAMA" || cb.User.ID != "U1" || cb.ActionCallback.BlockActions[0].ActionID != "approve" {
		t.Errorf("b got envelope %q, payload %+v; want the press, made for b", gotID, cb)
	}
	waitAcked(t, fake, id)
	quiet(t, "a", smcA)
}

func TestAPressNeedsTheAppThatPostedItToBeConnected(t *testing.T) {
	fake := twoApps(t, fakeslack.WithoutOwnPostEvents())
	smcA := socketAs(t, fake, appA)
	fake.SetMembers("C1", "U1", "UBOTA", "UBOTB")
	ts := post(t, webAs(fake, appB), text("approve?"), slack.MsgOptionBlocks(
		slack.NewActionBlock("buttons", slack.NewButtonBlockElement("approve", "yes", plain("Approve")))))
	if _, err := fake.Click("U1", ts, "approve", "yes"); !errors.Is(err, fakeslack.ErrNoConnection) {
		t.Errorf("Click on b's message while only a is connected = %v, want ErrNoConnection", err)
	}
	quiet(t, "a", smcA)
}

func TestADirectMessageGoesToTheAppItIsWith(t *testing.T) {
	fake := twoApps(t, fakeslack.WithoutOwnPostEvents())
	smcA, smcB := socketAs(t, fake, appA), socketAs(t, fake, appB)

	got, err := fake.DMApp("AAPPB", "U1", "hello b")
	if err != nil {
		t.Fatalf("DMApp: %v", err)
	}
	if want := fake.DMChannelFor("AAPPB", "U1"); got.Channel != want || want == fakeslack.DMChannel("U1") {
		t.Errorf("channel = %q, want %q, apart from the default app's %q", got.Channel, want, fakeslack.DMChannel("U1"))
	}
	if _, p := nextPayload(t, smcB); p.Event.Text != "hello b" || p.Event.Channel != got.Channel {
		t.Errorf("b got %+v, want the DM", p.Event)
	}
	// A reply in that channel stays with b.
	if _, err := fake.Reply(got.Channel, got.TS, "U1", "and again"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if _, p := nextPayload(t, smcB); p.Event.Text != "and again" {
		t.Errorf("b got %+v, want the reply", p.Event)
	}
	quiet(t, "a", smcA)

	if _, err := fake.DMApp("ANOBODY", "U1", "hi"); !errors.Is(err, fakeslack.ErrNotFound) {
		t.Errorf("DMApp to an app the Server does not host = %v, want ErrNotFound", err)
	}
	// DM is the default app's, which nobody here is connected as.
	if _, err := fake.DM("U1", "hello default"); !errors.Is(err, fakeslack.ErrNoConnection) {
		t.Errorf("DM to the default app = %v, want ErrNoConnection", err)
	}
}

func TestTheDefaultAppHearsEveryChannelUnlessMembershipIsStrict(t *testing.T) {
	fake := twoApps(t, fakeslack.WithoutOwnPostEvents(), fakeslack.WithStrictMembership())
	smc := runClient(t, fake.URL()) // any token: the default app
	waitEvent(t, smc, socketmode.EventTypeHello)
	fake.SetMembers("C1", "U1")
	if _, err := fake.Mention("C1", "U1", "<@UFAKEBOT> anyone?"); !errors.Is(err, fakeslack.ErrNoConnection) {
		t.Fatalf("Mention while the default bot is not in the channel = %v, want ErrNoConnection", err)
	}
	if err := fake.Join("C1", "AFAKEAPP"); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if _, err := fake.Mention("C1", "U1", "<@UFAKEBOT> anyone?"); err != nil {
		t.Fatalf("Mention after the default bot joined: %v", err)
	}
	if _, p := nextPayload(t, smc); p.Event.Type != "app_mention" || p.APIAppID != "AFAKEAPP" {
		t.Errorf("default app got %+v, want the app_mention", p)
	}
}

func TestSeveralSocketsOfOneApp(t *testing.T) {
	// slashCommand envelopes need no channel, so they show where an app's envelopes go.
	send := func(t *testing.T, fake *fakeslack.Server, n int) {
		t.Helper()
		for range n {
			if _, err := fake.SendEnvelopeTo("AAPPA", "slash_commands", slashCommand); err != nil {
				t.Fatalf("SendEnvelopeTo: %v", err)
			}
		}
	}
	count := func(t *testing.T, smc *socketmode.Client, want int) {
		t.Helper()
		for range want {
			ev := waitEvent(t, smc, socketmode.EventTypeSlashCommand)
			if err := smc.Ack(*ev.Request); err != nil {
				t.Fatalf("Ack: %v", err)
			}
		}
		quiet(t, "this client", smc)
	}

	t.Run("by default every envelope goes to the newest", func(t *testing.T) {
		fake := twoApps(t)
		oldest, newest := socketAs(t, fake, appA), socketAs(t, fake, appA)
		if n := fake.AppConnections("AAPPA"); n != 2 {
			t.Fatalf("a has %d connections, want 2", n)
		}
		send(t, fake, 3)
		count(t, newest, 3)
		quiet(t, "the oldest", oldest)
	})

	t.Run("with spread each goes to one, in turn", func(t *testing.T) {
		fake := twoApps(t, fakeslack.WithSocketSpread())
		first, second := socketAs(t, fake, appA), socketAs(t, fake, appA)
		send(t, fake, 1)
		ev := waitEvent(t, first, socketmode.EventTypeSlashCommand)
		_ = first.Ack(*ev.Request)
		quiet(t, "the second", second)
		send(t, fake, 1)
		count(t, second, 1)
		send(t, fake, 1)
		count(t, first, 1)
		quiet(t, "the second", second)
	})
}

func TestEnvelopesNameTheirApp(t *testing.T) {
	fake := twoApps(t)
	smcA := socketAs(t, fake, appA)
	if _, err := fake.SendEnvelopeTo("ANOBODY", "slash_commands", slashCommand); !errors.Is(err, fakeslack.ErrNotFound) {
		t.Errorf("SendEnvelopeTo an app the Server does not host = %v, want ErrNotFound", err)
	}
	if _, err := fake.SendEnvelopeTo("AAPPB", "slash_commands", slashCommand); !errors.Is(err, fakeslack.ErrNoConnection) {
		t.Errorf("SendEnvelopeTo b, which is not connected = %v, want ErrNoConnection", err)
	}
	// SendEnvelope is the default app's, and a's connection is not.
	if _, err := fake.SendEnvelope("slash_commands", slashCommand); !errors.Is(err, fakeslack.ErrNoConnection) {
		t.Errorf("SendEnvelope with only a connected = %v, want ErrNoConnection", err)
	}
	quiet(t, "a", smcA)
}

func TestSocketsAreTheAppsThatOpenedThem(t *testing.T) {
	fake := twoApps(t)
	socketAs(t, fake, appA)
	socketAs(t, fake, appB)
	runClient(t, fake.URL())
	eventually(t, "three connections", func() bool { return fake.Connections() == 3 })
	for app, want := range map[string]int{"AAPPA": 1, "AAPPB": 1, "AFAKEAPP": 1, "ANOBODY": 0} {
		if n := fake.AppConnections(app); n != want {
			t.Errorf("AppConnections(%s) = %d, want %d", app, n, want)
		}
	}
}

func TestControlSurfaceNamesTheApp(t *testing.T) {
	fake := twoApps(t, fakeslack.WithoutOwnPostEvents())
	smcB := socketAs(t, fake, appB)

	var apps []map[string]any
	if s := control(t, fake, "GET", "/_control/apps", nil, &apps); s != 200 || len(apps) != 3 ||
		apps[0]["app_id"] != "AFAKEAPP" || apps[2]["app_id"] != "AAPPB" || apps[2]["connections"] != float64(1) || apps[1]["connections"] != float64(0) {
		t.Fatalf("apps = %d, %v; want the default app first, then a and b with b's one connection", s, apps)
	}
	if all := toString(apps); strings.Contains(all, "token") || strings.Contains(all, "xoxb-") || strings.Contains(all, "xapp-") {
		t.Errorf("apps = %s, want no token", all)
	}

	var n map[string]int
	for path, want := range map[string]int{"/_control/connections": 1, "/_control/connections?app=AAPPB": 1, "/_control/connections?app=AAPPA": 0} {
		if s := control(t, fake, "GET", path, nil, &n); s != 200 || n["connections"] != want {
			t.Errorf("GET %s = %d, %v; want %d connections", path, s, n, want)
		}
	}
	if s := control(t, fake, "GET", "/_control/connections?app=ANOBODY", nil, nil); s != 404 {
		t.Errorf("connections of an unknown app = %d, want 404", s)
	}

	// join puts an app's bot among the members; the default app is the empty app.
	fake.SetMembers("C1", "U1")
	for _, body := range []any{map[string]any{"channel": "C1", "app": "AAPPB"}, map[string]any{"channel": "C1"}} {
		if s := control(t, fake, "POST", "/_control/join", body, nil); s != 200 {
			t.Fatalf("join %v = %d", body, s)
		}
	}
	if members, _, err := webAs(fake, appB).GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: "C1"}); err != nil ||
		!slices.Equal(members, []string{"U1", "UBOTB", "UFAKEBOT"}) {
		t.Errorf("members = %v, %v; want U1, then b's bot and the default app's", members, err)
	}
	for _, body := range []any{map[string]any{"channel": "C1", "app": "ANOBODY"}, map[string]any{"app": "AAPPA"}} {
		want := map[bool]int{true: 404, false: 400}[body.(map[string]any)["channel"] != nil]
		if s := control(t, fake, "POST", "/_control/join", body, nil); s != want {
			t.Errorf("join %v = %d, want %d", body, s, want)
		}
	}

	// dm names the app the user writes to.
	var dm fakeslack.UserMessage
	if s := control(t, fake, "POST", "/_control/dm", map[string]any{"app": "AAPPB", "user": "U1", "text": "hi b"}, &dm); s != 200 ||
		dm.Channel != fake.DMChannelFor("AAPPB", "U1") || len(dm.AppEnvelopes["AAPPB"]) != 1 {
		t.Fatalf("dm = %d, %+v; want b's channel and one envelope for b", s, dm)
	}
	if _, p := nextPayload(t, smcB); p.Event.Text != "hi b" {
		t.Errorf("b got %+v, want the DM", p.Event)
	}
	if s := control(t, fake, "POST", "/_control/dm", map[string]any{"app": "ANOBODY", "user": "U1", "text": "hi"}, nil); s != 404 {
		t.Errorf("dm to an unknown app = %d, want 404", s)
	}
	if s := control(t, fake, "POST", "/_control/dm", map[string]any{"user": "U1", "text": "hi"}, nil); s != 409 {
		t.Errorf("dm to the default app, which is not connected = %d, want 409", s)
	}

	// messages and ephemerals can be read back for one app.
	post(t, webAs(fake, appA), text("from a"))
	post(t, webAs(fake, appB), text("from b"))
	if _, err := webAs(fake, appB).PostEphemeral("C1", "U1", text("psst")); err != nil {
		t.Fatalf("PostEphemeral: %v", err)
	}
	var msgs []map[string]any
	for path, want := range map[string]string{
		"/_control/messages?channel=C1&app=AAPPA": "from a", "/_control/messages?channel=C1&app=AAPPB": "from b",
	} {
		if s := control(t, fake, "GET", path, nil, &msgs); s != 200 || len(msgs) != 1 || msgs[0]["text"] != want {
			t.Errorf("GET %s = %d, %v; want just %q", path, s, msgs, want)
		}
	}
	if s := control(t, fake, "GET", "/_control/messages?channel=C1", nil, &msgs); s != 200 || len(msgs) != 2 {
		t.Errorf("GET messages without an app = %d, %d messages; want every message", s, len(msgs))
	}
	var eph []map[string]any
	if s := control(t, fake, "GET", "/_control/ephemerals?channel=C1&app=AAPPA", nil, &eph); s != 200 || len(eph) != 0 {
		t.Errorf("a's ephemerals = %d, %v; want none", s, eph)
	}
	if s := control(t, fake, "GET", "/_control/ephemerals?channel=C1&app=AAPPB", nil, &eph); s != 200 || len(eph) != 1 {
		t.Errorf("b's ephemerals = %d, %v; want one", s, eph)
	}
	for _, path := range []string{"/_control/messages?channel=C1&app=ANOBODY", "/_control/ephemerals?channel=C1&app=ANOBODY"} {
		if s := control(t, fake, "GET", path, nil, nil); s != 404 {
			t.Errorf("GET %s = %d, want 404", path, s)
		}
	}
}

// toString is the JSON text of v, for a check on what a response shows.
func toString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

package faketeams_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/teams"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/test/faketeams"
)

// The world of these tests: fake IDs of the shape of real ones, which no tenant has.
const (
	tenant    = "e2e0e2e0-0000-4000-8000-000000000001"
	group     = "e2e0e2e0-0000-4000-8000-0000000000c1"
	app       = "e2e0e2e0-0000-4000-8000-0000000000a1"
	workflows = "e2e0e2e0-0000-4000-8000-0000000000f1"
	person    = "e2e0e2e0-0000-4000-8000-00000000b001"
	team      = "19:fake-general@thread.tacv2"
	channel   = "19:fake-alerts@thread.tacv2"
	secretVal = "fake-teams-client-secret"
	connName  = "teams-e2e/teams-push"
)

// sink is the broker's intake as the adapter sees it: it binds channel, takes Workflows' posts there, and records what it is given.
type sink struct {
	mu           sync.Mutex
	messages     []chat.Inbound
	interactions []chat.Interaction
}

func (s *sink) Message(_ context.Context, m chat.Inbound) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
	return nil
}

func (s *sink) Interaction(_ context.Context, i chat.Interaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interactions = append(s.interactions, i)
	return nil
}

func (s *sink) BoundChannels() []string                   { return []string{channel} }
func (s *sink) AcceptsBot(ch, bot string) bool            { return ch == channel && bot == workflows }
func (s *sink) AcceptsBotMention(string, string) bool     { return false }
func (s *sink) message(pred func(chat.Inbound) bool) bool { return s.find(pred) != nil }

func (s *sink) find(pred func(chat.Inbound) bool) *chat.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		if pred(s.messages[i]) {
			m := s.messages[i]
			return &m
		}
	}
	return nil
}

// world is the fake, served over TLS, and a Teams client of the real adapter that runs against it, its route served over plain HTTP.
type world struct {
	t       *testing.T
	fake    *faketeams.Service
	control *faketeams.Control
	client  *teams.Client
	convs   *teams.MemoryConversations
	sink    *sink
}

// newWorld starts the fake and the adapter. With push the connection has a notificationURL, an https URL of its route that the fake
// sends to over plain HTTP (DeliverHTTP); without, it polls Graph. seed records the channel's conversation before the run, as an
// earlier activity would have.
func newWorld(t *testing.T, push, seed bool) *world {
	t.Helper()
	var route atomic.Pointer[http.Handler]
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := route.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(broker.Close)
	ms := httptest.NewUnstartedServer(nil)
	base := "https://" + ms.Listener.Addr().String()
	fake, err := faketeams.NewService(faketeams.Config{PublicURL: base, TenantID: tenant, TeamID: team, GroupID: group, ClientSecret: secretVal,
		Channels: []string{channel}, Bots: map[string]string{app: broker.URL + "/chat/teams/" + connName}, DeliverHTTP: true, Retry: 50 * time.Millisecond,
		RetryFor: 5 * time.Second, Logger: testutil.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ms.Config.Handler = fake.Handler()
	ms.StartTLS()
	t.Cleanup(func() { ms.Close(); fake.Close() })

	convs := teams.NewMemoryConversations()
	if seed {
		if err := convs.Remember(t.Context(), connName, teams.Conversation{Channel: channel, ServiceURL: fake.ServiceURL(), TeamID: team,
			AADGroupID: group, TenantID: tenant}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := teams.Config{Connection: connName, TenantID: tenant, AppID: app, ClientSecret: secret.New(secretVal), TokenURL: base + "/token",
		MetadataURL: base + "/openid", GraphURL: base + "/v1.0", HTTPClient: ms.Client(), Conversations: convs, Logger: testutil.DiscardLogger()}
	if push {
		cfg.NotificationURL = "https://" + strings.TrimPrefix(broker.URL, "http://") + "/chat/teams/" + connName
	}
	client, err := teams.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := client.HTTPHandler()
	route.Store(&h)
	ctx, cancel := context.WithCancel(context.Background())
	w := &world{t: t, fake: fake, control: faketeams.NewControl(controlURL(t, fake)), client: client, convs: convs, sink: &sink{}}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx, w.sink) }()
	t.Cleanup(func() {
		cancel()
		_ = <-done // Run's error says only that it was cancelled
	})
	return w
}

// controlURL serves the fake's control API over plain HTTP, as the tier's port 8080 does.
func controlURL(t *testing.T, fake *faketeams.Service) string {
	t.Helper()
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// eventually fails the test unless cond holds within testutil's default timeout, checking it every 50 milliseconds: each check is a
// call of the fake.
func (w *world) eventually(what string, cond func() bool) {
	w.t.Helper()
	testutil.Eventually(w.t, what, cond, testutil.Every(50*time.Millisecond))
}

func (w *world) messages(root string) []faketeams.MessageView {
	w.t.Helper()
	ms, err := w.control.Messages(w.t.Context(), channel, root)
	if err != nil {
		w.t.Fatal(err)
	}
	return ms
}

// The adapter of internal/chat/teams, which the broker runs, runs on the fake as it does on Microsoft: an install tells it the team's
// record (its General channel's group and Bot Connector), and a mention the channel's; the token of a mention verifies against the fake's keys and the mention reaches the sink; a post,
// an edit and a buttons card land in the channel; a press of a button reaches the sink and is answered in the request; and with a
// notificationURL the adapter subscribes to the channel, after the validation handshake, and a Workflows post reaches it through a
// change notification, with its card's text.
func TestTheTeamsAdapterRunsOnTheFake(t *testing.T) {
	t.Parallel()
	w := newWorld(t, true, true)
	ctx := t.Context()

	w.eventually("the install to be taken", func() bool {
		d, err := w.control.Install(ctx, faketeams.Install{App: app, Channel: channel})
		return err == nil && d.Status == http.StatusOK
	})
	w.eventually("the team's conversation to be recorded from the install", func() bool {
		c, ok, err := w.convs.Lookup(ctx, connName, team)
		return err == nil && ok && c.AADGroupID == group && c.ServiceURL == w.fake.ServiceURL()
	})

	m, err := w.control.Mention(ctx, faketeams.Mention{App: app, Channel: channel, User: person, Name: "Ada", Text: "is the api up?"})
	if err != nil || m.Status != http.StatusOK {
		t.Fatalf("Mention = %+v, %v; want the bot's 200", m, err)
	}
	w.eventually("the mention to reach the sink", func() bool {
		return w.sink.message(func(in chat.Inbound) bool {
			return in.At.ID == m.ID && in.Mentioned && in.User == person && strings.Contains(in.Text, "is the api up?")
		})
	})

	target := chat.Target{Conn: connName, Channel: channel, Thread: m.ID}
	reply, err := w.client.Post(ctx, target, chat.Say(chat.Message{Fallback: "Looking.", Parts: []chat.Part{chat.Body{Markdown: "Looking."}}}), "tag-1")
	if err != nil {
		t.Fatal(err)
	}
	final := chat.Say(chat.Message{Fallback: "It is up.", Parts: []chat.Part{chat.Body{Markdown: "It is up."},
		chat.Buttons{Buttons: []chat.Button{{ID: "feedback-useful", Label: "Useful", Value: "v-useful", Primary: true}}}}})
	if err := w.client.Update(ctx, reply, final); err != nil {
		t.Fatal(err)
	}
	thread := w.messages(m.ID)
	if len(thread) != 3 || thread[1].ID != reply.ID || thread[1].Edits != 1 || !strings.Contains(thread[1].Text, "It is up.") ||
		!strings.Contains(thread[1].Versions[0], "Looking.") || len(thread[2].Cards) != 1 || thread[2].Kind != faketeams.SenderBot {
		t.Fatalf("the thread is %+v; want the mention, the reply edited once to its final text, and the buttons card under it", thread)
	}

	p, err := w.control.Press(ctx, faketeams.Press{App: app, Message: thread[2].ID, User: person, Action: "Useful"})
	if err != nil || p.Status != http.StatusOK || !strings.Contains(p.Body, "received your press") {
		t.Fatalf("Press = %+v, %v; want the bot's 200 that says it received the press", p, err)
	}
	w.eventually("the press to reach the sink", func() bool {
		w.sink.mu.Lock()
		defer w.sink.mu.Unlock()
		return len(w.sink.interactions) == 1 && w.sink.interactions[0].User == person
	})

	w.eventually("the subscription to the channel", func() bool {
		subs, err := w.control.Subscriptions(ctx)
		return err == nil && len(subs) == 1 && subs[0].Channel == channel && subs[0].App == app
	})
	card := json.RawMessage(`{"type":"AdaptiveCard","version":"1.4","body":[{"type":"TextBlock","text":"[FIRING] DiskFull"},` +
		`{"type":"FactSet","facts":[{"title":"namespace","value":"prod"}]}]}`)
	alert, err := w.control.Workflows(ctx, faketeams.Workflows{App: workflows, Channel: channel, Card: card})
	if err != nil {
		t.Fatal(err)
	}
	w.eventually("the Workflows post to reach the sink through a notification", func() bool {
		return w.sink.message(func(in chat.Inbound) bool {
			return in.At.ID == alert && in.Bot != nil && in.Bot.ID == workflows && strings.Contains(in.Bot.Body, "[FIRING] DiskFull") &&
				strings.Contains(in.Bot.Body, "namespace: prod")
		})
	})
	stats, err := w.control.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SubscriptionCreates[channel] != 1 {
		t.Errorf("the fake made %d subscriptions to the channel; want 1", stats.SubscriptionCreates[channel])
	}
}

// Without a notificationURL the adapter polls the channel's messages/delta on the fake, and a Workflows post made after its first sync
// reaches the sink through a poll with the delta token. Nothing is recorded before the run: the install records the team, which tells
// the poller nothing of the channel, and a person's mention in the channel records the channel's group, which the poll then reads.
func TestTheTeamsAdapterPollsTheFake(t *testing.T) {
	t.Parallel()
	w := newWorld(t, false, false)
	ctx := t.Context()
	w.eventually("the install to be taken", func() bool {
		d, err := w.control.Install(ctx, faketeams.Install{App: app, Channel: channel})
		return err == nil && d.Status == http.StatusOK
	})
	if _, ok, err := w.convs.Lookup(ctx, connName, channel); err != nil || ok {
		t.Fatalf("the channel's conversation after the install alone: recorded %v (%v); want none, the install is the team's", ok, err)
	}
	m, err := w.control.Mention(ctx, faketeams.Mention{App: app, Channel: channel, User: person, Name: "Ada", Text: "hello"})
	if err != nil || m.Status != http.StatusOK {
		t.Fatalf("Mention = %+v, %v; want the bot's 200", m, err)
	}
	w.eventually("the channel's conversation to be recorded from the mention", func() bool {
		c, ok, err := w.convs.Lookup(ctx, connName, channel)
		return err == nil && ok && c.AADGroupID == group && c.TeamID == team
	})
	w.eventually("a poll with a delta token", func() bool {
		st, err := w.control.Stats(ctx)
		return err == nil && st.Reads[channel].Synced >= 1
	})
	card := json.RawMessage(`{"type":"AdaptiveCard","version":"1.4","body":[{"type":"TextBlock","text":"[FIRING] PodCrashLooping"}]}`)
	alert, err := w.control.Workflows(ctx, faketeams.Workflows{App: workflows, Channel: channel, Card: card})
	if err != nil {
		t.Fatal(err)
	}
	w.eventually("the Workflows post to reach the sink through a poll", func() bool {
		return w.sink.message(func(in chat.Inbound) bool {
			return in.At.ID == alert && in.Bot != nil && strings.Contains(in.Bot.Body, "[FIRING] PodCrashLooping")
		})
	})
	if subs, err := w.control.Subscriptions(ctx); err != nil || len(subs) != 0 {
		t.Errorf("Subscriptions = %+v, %v; want none for a connection that polls", subs, err)
	}
}

// fakeOnly is the fake alone, served over TLS, with a bot whose endpoint is endpoint.
func fakeOnly(t *testing.T, endpoint string) (*faketeams.Service, *httptest.Server) {
	t.Helper()
	ms := httptest.NewUnstartedServer(nil)
	fake, err := faketeams.NewService(faketeams.Config{PublicURL: "https://" + ms.Listener.Addr().String(), TenantID: tenant, TeamID: team,
		GroupID: group, ClientSecret: secretVal, Bots: map[string]string{app: endpoint}, Logger: testutil.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ms.Config.Handler = fake.Handler()
	ms.StartTLS()
	t.Cleanup(func() { ms.Close(); fake.Close() })
	return fake, ms
}

// tokenOf asks the fake for a token of scope with the client secret, and returns the status and the token.
func tokenOf(t *testing.T, ms *httptest.Server, clientSecret, scope string) (int, string) {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {app}, "client_secret": {clientSecret}, "scope": {scope}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ms.URL+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := ms.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.AccessToken
}

// call makes one call of the fake with token, and returns the status.
func call(t *testing.T, ms *httptest.Server, method, path, token, body string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, ms.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ms.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// What the fake refuses, as Microsoft does: a token for another client secret or scope; the Bot Connector without a bot token, or with
// Graph's; a post to a channel the team does not have; a subscription whose validation handshake is not answered with its token; and a
// second live subscription of the app to the same channel (409).
func TestTheFakeRefusesWhatMicrosoftRefuses(t *testing.T) {
	t.Parallel()
	var handshakes atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		if r.URL.Query().Get("validationToken") != "" && r.URL.Query().Get("wrong") == "" {
			_, _ = fmt.Fprint(w, r.URL.Query().Get("validationToken"))
		}
	}))
	t.Cleanup(endpoint.Close)
	fake, ms := fakeOnly(t, endpoint.URL)
	ctl := faketeams.NewControl(controlURL(t, fake))
	if _, err := ctl.Install(t.Context(), faketeams.Install{App: app, Channel: channel}); err != nil {
		t.Fatal(err)
	}

	if status, _ := tokenOf(t, ms, "not-the-secret", "https://api.botframework.com/.default"); status != http.StatusUnauthorized {
		t.Errorf("a token for another client secret: %d; want 401", status)
	}
	if status, _ := tokenOf(t, ms, secretVal, "https://example.test/.default"); status != http.StatusBadRequest {
		t.Errorf("a token for another scope: %d; want 400", status)
	}
	_, bot := tokenOf(t, ms, secretVal, "https://api.botframework.com/.default")
	_, graph := tokenOf(t, ms, secretVal, "https://graph.microsoft.com/.default")
	post := "/teams/v3/conversations/" + url.PathEscape(channel) + "/activities"
	for name, c := range map[string]struct {
		path, token string
		want        int
	}{
		"no token":        {post, "", http.StatusUnauthorized},
		"a Graph token":   {post, graph, http.StatusUnauthorized},
		"another channel": {"/teams/v3/conversations/" + url.PathEscape("19:other@thread.tacv2") + "/activities", bot, http.StatusNotFound},
		"the bot's token": {post, bot, http.StatusCreated},
	} {
		if got := call(t, ms, http.MethodPost, c.path, c.token, `{"type":"message","text":"hi"}`); got != c.want {
			t.Errorf("%s: a post answered %d; want %d", name, got, c.want)
		}
	}

	sub := func(u string) string {
		return fmt.Sprintf(`{"changeType":"created,updated","notificationUrl":%q,"lifecycleNotificationUrl":%q,"resource":"/teams/%s/channels/%s/messages",`+
			`"expirationDateTime":%q,"clientState":"state"}`, u, u, group, channel, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}
	if got := call(t, ms, http.MethodPost, "/v1.0/subscriptions", graph, sub(endpoint.URL+"/?wrong=1")); got != http.StatusBadRequest {
		t.Errorf("a subscription whose handshake is not answered with its token: %d; want 400", got)
	}
	if got := call(t, ms, http.MethodPost, "/v1.0/subscriptions", graph, sub(endpoint.URL)); got != http.StatusCreated {
		t.Errorf("a subscription: %d; want 201", got)
	}
	if got := call(t, ms, http.MethodPost, "/v1.0/subscriptions", graph, sub(endpoint.URL)); got != http.StatusConflict {
		t.Errorf("a second subscription to the channel: %d; want 409", got)
	}
	if handshakes.Load() < 2 {
		t.Errorf("the fake made %d validation handshakes; want one for each subscription it made or refused for it", handshakes.Load())
	}
}

// The install the fake sends a bot is a conversationUpdate of eventType teamMemberAdded on the team's conversation, whose channelData
// holds the team's record and no channel: not the channel the install names, which the bot learns from the first activity in it.
func TestTheInstallCarriesTheTeamsRecordAlone(t *testing.T) {
	t.Parallel()
	got := make(chan []byte, 1)
	bot := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case got <- body:
		default:
		}
	}))
	t.Cleanup(bot.Close)
	fake, _ := fakeOnly(t, bot.URL)
	delivered, err := faketeams.NewControl(controlURL(t, fake)).Install(t.Context(), faketeams.Install{App: app, Channel: channel})
	if err != nil {
		t.Fatal(err)
	}
	if delivered.Status != http.StatusOK {
		t.Fatalf("the bot answered the install %+v; want 200", delivered)
	}
	var a struct {
		Type         string `json:"type"`
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
		ChannelData map[string]json.RawMessage `json:"channelData"`
	}
	select {
	case body := <-got:
		if err := json.Unmarshal(body, &a); err != nil {
			t.Fatalf("the install is not an activity in JSON: %v", err)
		}
	default: // Install returns once the bot has answered, so the bot has the install by now if it was sent one
		t.Fatalf("the bot was sent no install: %+v", delivered)
	}
	if a.Type != "conversationUpdate" || a.Conversation.ID != team {
		t.Errorf("the install is a %q on %q; want a conversationUpdate on the team's conversation %q", a.Type, a.Conversation.ID, team)
	}
	if string(a.ChannelData["eventType"]) != `"teamMemberAdded"` || a.ChannelData["team"] == nil || a.ChannelData["tenant"] == nil {
		t.Errorf("the install's channelData %s; want the eventType teamMemberAdded, the team and the tenant", a.ChannelData)
	}
	for _, key := range []string{"teamsChannelId", "channel"} {
		if v, ok := a.ChannelData[key]; ok {
			t.Errorf("the install's channelData has %s %s; want no channel, only the team's record", key, v)
		}
	}
}

// callBody makes one call of the fake with token, and returns the status and the body.
func callBody(t *testing.T, ms *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, ms.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ms.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(got)
}

// An activity of both text and a card is answered 202 with no body, so no ID, and shown as two messages, the text and then the card on
// its own, as Teams was measured to answer and show one; text alone, or a card alone, is answered 201 with its ID.
func TestTheFakeAnswersTextWithACardAsTeamsDoes(t *testing.T) {
	t.Parallel()
	fake, ms := fakeOnly(t, "http://127.0.0.1:1/unused")
	ctl := faketeams.NewControl(controlURL(t, fake))
	if _, err := ctl.Install(t.Context(), faketeams.Install{App: app, Channel: channel}); err != nil {
		t.Fatal(err)
	}
	_, bot := tokenOf(t, ms, secretVal, "https://api.botframework.com/.default")
	post := "/teams/v3/conversations/" + url.PathEscape(channel) + "/activities"
	card := `{"contentType":"application/vnd.microsoft.card.adaptive","content":{"type":"AdaptiveCard","version":"1.4","actions":[{"type":"Action.Execute","title":"Useful","verb":"kourier","data":{}}]}}`

	status, body := callBody(t, ms, http.MethodPost, post, bot, `{"type":"message","text":"It is up.","attachments":[`+card+`]}`)
	if status != http.StatusAccepted || body != "" {
		t.Errorf("text and a card: %d %q; want 202 with no body", status, body)
	}
	msgs, err := ctl.Messages(t.Context(), channel, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Text != "It is up." || len(msgs[0].Cards) != 0 || msgs[1].Text != "" || len(msgs[1].Cards) != 1 {
		t.Fatalf("the channel holds %+v; want the text, then the card on its own", msgs)
	}
	for name, activity := range map[string]string{
		"text alone":   `{"type":"message","text":"Looking."}`,
		"a card alone": `{"type":"message","attachments":[` + card + `]}`,
	} {
		status, body := callBody(t, ms, http.MethodPost, post, bot, activity)
		var out struct {
			ID string `json:"id"`
		}
		if status != http.StatusCreated || json.Unmarshal([]byte(body), &out) != nil || out.ID == "" {
			t.Errorf("%s: %d %q; want 201 with the message's ID", name, status, body)
		}
	}
}

// A renewal makes the validation handshake with the subscription's URLs, as a create does: one answered with its token moves the
// expiry, and one that is not is refused with 400 and leaves it.
func TestTheFakeValidatesARenewal(t *testing.T) {
	t.Parallel()
	var refuse atomic.Bool
	var handshakes atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := r.URL.Query().Get("validationToken"); tok != "" {
			handshakes.Add(1)
			if !refuse.Load() {
				_, _ = fmt.Fprint(w, tok)
			}
		}
	}))
	t.Cleanup(endpoint.Close)
	fake, ms := fakeOnly(t, endpoint.URL)
	ctl := faketeams.NewControl(controlURL(t, fake))
	if _, err := ctl.Install(t.Context(), faketeams.Install{App: app, Channel: channel}); err != nil {
		t.Fatal(err)
	}
	_, graph := tokenOf(t, ms, secretVal, "https://graph.microsoft.com/.default")
	sub := fmt.Sprintf(`{"changeType":"created,updated","notificationUrl":%q,"lifecycleNotificationUrl":%q,"resource":"/teams/%s/channels/%s/messages",`+
		`"expirationDateTime":%q,"clientState":"state"}`, endpoint.URL, endpoint.URL+"/lifecycle", group, channel, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	status, body := callBody(t, ms, http.MethodPost, "/v1.0/subscriptions", graph, sub)
	var created struct {
		ID string `json:"id"`
	}
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &created) != nil {
		t.Fatalf("a subscription: %d %q; want 201", status, body)
	}
	if got := handshakes.Load(); got != 2 {
		t.Errorf("the create made %d handshakes; want one for each URL", got)
	}

	renew := func(at time.Time) int {
		status, _ := callBody(t, ms, http.MethodPatch, "/v1.0/subscriptions/"+created.ID, graph, fmt.Sprintf(`{"expirationDateTime":%q}`, at.UTC().Format(time.RFC3339)))
		return status
	}
	later := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if got := renew(later); got != http.StatusOK || handshakes.Load() != 4 {
		t.Errorf("a renewal whose URLs answer: %d after %d handshakes; want 200 after two more", got, handshakes.Load())
	}
	refuse.Store(true)
	if got := renew(later.Add(time.Hour)); got != http.StatusBadRequest {
		t.Errorf("a renewal whose URLs do not answer: %d; want 400", got)
	}
	subs, err := ctl.Subscriptions(t.Context())
	if err != nil || len(subs) != 1 || !subs[0].Expires.Equal(later) {
		t.Errorf("Subscriptions = %+v, %v; want the one, expiring at %s, as the refused renewal left it", subs, err, later)
	}
	if st, err := ctl.Stats(t.Context()); err != nil || st.SubscriptionRenewals[channel] != 1 {
		t.Errorf("Stats renewals = %v, %v; want the one that passed", st.SubscriptionRenewals, err)
	}
}

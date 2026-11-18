package teams

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// connRequest is one call that the fake Bot Connector took.
type connRequest struct {
	method, path string // path as it came on the wire, escaped
	auth         string
	activity     outActivity
	raw          map[string]any
}

// fakeConnector is the Bot Connector and Entra's token endpoint on one TLS server. It records each call, answers a post with a new
// activity ID, an update with 200, and a team's channels from teams; fail, if set, answers a call instead.
type fakeConnector struct {
	srv *httptest.Server

	mu     sync.Mutex
	reqs   []connRequest                                            // guarded by mu
	teams  map[string][]string                                      // guarded by mu: the channels of each team
	fail   func(r *http.Request) (status int, body string, ok bool) // guarded by mu
	nextID int                                                      // guarded by mu
	tokens int                                                      // guarded by mu: the token grants taken
}

func newFakeConnector(t testing.TB) *fakeConnector {
	t.Helper()
	f := &fakeConnector{teams: map[string][]string{}, nextID: 1700000000000}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// serviceURL is the fake's Bot Connector, as a channel's record names it.
func (f *fakeConnector) serviceURL() string { return f.srv.URL + "/amer/" + testTenant + "/" }

func (f *fakeConnector) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if r.URL.Path == "/token" {
		f.mu.Lock()
		f.tokens++
		n := f.tokens
		f.mu.Unlock()
		if fail != nil {
			if status, body, ok := fail(r); ok {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
				return
			}
		}
		_, _ = fmt.Fprintf(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"fake-bot-token-%d"}`, n)
		return
	}
	body, _ := io.ReadAll(r.Body)
	req := connRequest{method: r.Method, path: r.URL.EscapedPath(), auth: r.Header.Get("Authorization")}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &req.activity)
		_ = json.Unmarshal(body, &req.raw)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.nextID++
	id := f.nextID
	f.mu.Unlock()
	if fail != nil {
		if status, body, ok := fail(r); ok {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
	}
	prefix := "/amer/" + testTenant + "/v3/"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	switch {
	case !ok:
		http.Error(w, `{"error":{"code":"BadPath"}}`, http.StatusNotFound)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "teams/"):
		team := strings.TrimSuffix(strings.TrimPrefix(rest, "teams/"), "/conversations")
		f.mu.Lock()
		chans, known := f.teams[team]
		f.mu.Unlock()
		if !known {
			http.Error(w, `{"error":{"code":"NotFound","message":"team"}}`, http.StatusNotFound)
			return
		}
		var out struct {
			Conversations []map[string]string `json:"conversations"`
		}
		for _, c := range chans {
			out.Conversations = append(out.Conversations, map[string]string{"id": c, "name": "Example Team"})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost:
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":"%d"}`, id)
	case r.Method == http.MethodPut:
		_, _ = fmt.Fprintf(w, `{"id":"%s"}`, rest[strings.LastIndex(rest, "/")+1:])
	default:
		http.Error(w, "unexpected", http.StatusMethodNotAllowed)
	}
}

// setFail sets fail.
func (f *fakeConnector) setFail(fail func(r *http.Request) (status int, body string, ok bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

// setTeam lists channels as the channels of team.
func (f *fakeConnector) setTeam(team string, channels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teams[team] = channels
}

// take returns, and forgets, the calls taken since the last.
func (f *fakeConnector) take() []connRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.reqs
	f.reqs = nil
	return out
}

// clientTest is a Client of the test connection on the fake, with its store, and the Bot Framework's key that signs the activities a
// test sends it.
type clientTest struct {
	c     *Client
	f     *fakeConnector
	convs *MemoryConversations
	key   signingKey
}

func newClientTest(t testing.TB) *clientTest {
	t.Helper()
	f := newFakeConnector(t)
	key := newSigningKey(t, "k", "msteams")
	bf := newFakeBotFramework(t, key)
	convs := NewMemoryConversations()
	c, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret),
		TokenURL: f.srv.URL + "/token", MetadataURL: bf.metadataURL(), HTTPClient: f.srv.Client(), Conversations: convs,
		Logger: testutil.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return &clientTest{c: c, f: f, convs: convs, key: key}
}

// record gives channel a record at the fake's service URL, in team.
func (ct *clientTest) record(t testing.TB, channel, team string) {
	t.Helper()
	if err := ct.convs.Remember(t.Context(), testConn, Conversation{Channel: channel, ServiceURL: ct.f.serviceURL(), TeamID: team,
		TenantID: testTenant}); err != nil {
		t.Fatal(err)
	}
}

// shownBy is what Teams shows of the activities the fake took, as CommonMark reads their text (Teams' markdown is unmeasured, so
// code is not taken for inert): each mention entity's text, found in the text, is the mention of its user, shown as its name; and
// the text is split into its paragraphs, each a piece that a place's limit holds. What the reading finds acting is put on the first
// piece that holds the conformance canary (or the first piece), so that every construct is checked once, against a piece that
// carries hostile text.
func shownBy(reqs []connRequest) []chattest.Shown {
	var out []chattest.Shown
	for _, r := range reqs {
		if r.method != http.MethodPost && r.method != http.MethodPut {
			continue
		}
		text := r.activity.Text
		var active []chattest.Active
		for _, e := range r.activity.Entities {
			if e.Type != "mention" {
				active = append(active, chattest.Active{Kind: chattest.KindHTML, Text: "entity " + e.Type})
				continue
			}
			i := strings.Index(text, e.Text)
			if i < 0 || e.Text == "" {
				continue // an entity whose text is not there mentions nobody
			}
			text = text[:i] + e.Mentioned.Name + text[i+len(e.Text):]
			active = append(active, chattest.Active{Kind: chattest.KindMention, Text: e.Mentioned.ID})
		}
		scan := chattest.ScanCommonMark(text)
		active = append(active, scan.Active...)
		pieces := strings.Split(scan.Displayed, "\n\n")
		at := 0
		for i, p := range pieces {
			if strings.Contains(p, "zq7kx3") {
				at = i
				break
			}
		}
		for i, p := range pieces {
			s := chattest.Shown{Displayed: p}
			if i == at {
				s.Active = active
			}
			out = append(out, s)
		}
	}
	return out
}

// inert checks that every activity is a plain message in markdown with no more than its text and its mentions: no attachment, card
// or suggested action that could carry a link or a preview.
func inert(reqs []connRequest) error {
	for _, r := range reqs {
		if r.raw == nil {
			continue
		}
		for k := range r.raw {
			switch k {
			case "type", "text", "textFormat", "entities":
			default:
				return fmt.Errorf("%s %s carried %q", r.method, r.path, k)
			}
		}
		if r.activity.Type != "message" || r.activity.TextFormat != "markdown" {
			return fmt.Errorf("%s %s is a %q of the format %q", r.method, r.path, r.activity.Type, r.activity.TextFormat)
		}
	}
	return nil
}

// The groups and the channel states that Teams has no such thing as, with why.
var (
	absentChannels = map[chattest.ChannelState]string{
		chattest.ChannelNotJoined: "the Bot Connector lists the team's channels for the bot, and has no channel that is there and that " +
			"the bot is not in: one it does not list is not found",
		chattest.ChannelArchived:     "the Bot Connector says nothing of an archive, and no Microsoft Graph read is made yet",
		chattest.ChannelScopeMissing: "the bot's token has no permission of its own for the read: there is no missing scope to report",
	}
	absentGroups = map[chattest.GroupState]string{
		chattest.GroupReadable:     "Microsoft Teams has no approver groups: a Binding of a Teams connection takes none",
		chattest.GroupScopeMissing: "Microsoft Teams has no approver groups to lack a permission for",
	}
)

// teamsSubject is the conformance Subject of a Client on the fake.
func teamsSubject(t *testing.T) chattest.Subject {
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	unknown := "19:KourierStandInUnlistedChannel000000000000a@thread.tacv2"
	ct.record(t, unknown, testChannel)
	ct.f.setTeam(testChannel, testChannel)
	var pending []connRequest
	var pmu sync.Mutex
	drain := func() []connRequest {
		pmu.Lock()
		defer pmu.Unlock()
		pending = append(pending, ct.f.take()...)
		out := pending
		pending = nil
		return out
	}
	var forInert []connRequest
	tooLarge := chat.Prompt{ID: "ix-large"}
	for range 40 {
		tooLarge.Questions = append(tooLarge.Questions, chat.Question{Text: strings.Repeat("q", maxTextUnits)})
	}
	return chattest.Subject{
		Platform: ct.c,
		Sent: func() []chattest.Shown {
			reqs := drain()
			pmu.Lock()
			forInert = append(forInert, reqs...)
			pmu.Unlock()
			return shownBy(reqs)
		},
		Inert: func() error {
			reqs := drain()
			pmu.Lock()
			all := slices.Concat(forInert, reqs)
			forInert = nil
			pmu.Unlock()
			return inert(all)
		},
		AllowAutolink: true,
		Target:        chat.Target{Conn: testConn, Channel: testChannel, Thread: "1700000000000"},
		TooLarge:      &tooLarge,
		MentionUser:   testUser,
		Channels: func(st chattest.ChannelState) string {
			if st == chattest.ChannelJoined {
				return testChannel
			}
			return unknown
		},
		Groups:         func(chattest.GroupState) string { return "S0UNKNOWN" },
		AbsentChannels: absentChannels,
		AbsentGroups:   absentGroups,
	}
}

func TestTeamsAdapterPassesConformanceAgainstTheFakeBotConnector(t *testing.T) {
	chattest.ConformanceSharded(t, 8, teamsSubject)
}

// The reading of the wire is itself tested: what it says of text that Teams would act on must be what it is.
func TestTheTeamsReadingOfTheWireFindsWhatTeamsWouldActOn(t *testing.T) {
	t.Parallel()
	post := func(text string, entities ...mentionEntity) connRequest {
		return connRequest{method: http.MethodPost, activity: outActivity{Type: "message", Text: text, TextFormat: "markdown", Entities: entities}}
	}
	mention := mentionEntity{Type: "mention", Text: "<at>someone</at>", Mentioned: mentioned{ID: testUser, Name: "someone"}}
	for _, tc := range []struct {
		name string
		req  connRequest
		want []chattest.Kind
	}{
		{"a raw at tag with no entity", post("<at>x</at> hi"), []chattest.Kind{chattest.KindHTML, chattest.KindHTML}},
		{"a mention entity", post("<at>someone</at> hi", mention), []chattest.Kind{chattest.KindMention}},
		{"a second at tag beside a mention", post("<at>someone</at> <at>someone</at>", mention), []chattest.Kind{chattest.KindMention, chattest.KindHTML, chattest.KindHTML}},
		{"an entity whose text is not there", post("hi", mention), nil},
		{"a markdown link", post("[x](https://evil.test)"), []chattest.Kind{chattest.KindLink}},
		{"escaped text", post("&lt;at&gt;x&lt;/at&gt; &#91;x&#93;(y) \\*b\\*"), nil},
	} {
		var kinds []chattest.Kind
		for _, s := range shownBy([]connRequest{tc.req}) {
			for _, a := range s.Active {
				kinds = append(kinds, a.Kind)
			}
		}
		if fmt.Sprint(kinds) != fmt.Sprint(tc.want) {
			t.Errorf("%s: found %v, want %v", tc.name, kinds, tc.want)
		}
	}
}

package teams

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// testRoot is the root of the thread that these tests read, and replyID(n) its n-th reply: IDs that order as Teams' do, a post's
// creation in milliseconds.
const testRoot = "1794965000000"

func replyID(n int) string { return strconv.Itoa(1794965000000 + n*1000) }

// threadTest is a Client on the fake whose channel is recorded with its team's group, and the thread target.
func threadTest(t *testing.T) (*clientTest, chat.Target) {
	t.Helper()
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	ct.f.putMessage(testChannel, person(testRoot, "", testUser, "<p>the root</p>"))
	return ct, chat.Target{Conn: testConn, Channel: testChannel, Thread: testRoot}
}

// replies puts n replies of a person in the thread, the k-th saying "reply k".
func (ct *clientTest) replies(n int) {
	for k := 1; k <= n; k++ {
		ct.f.putMessage(testChannel, person(replyID(k), testRoot, testUser, "<p>reply "+strconv.Itoa(k)+"</p>"))
	}
}

func idsOf(msgs []chat.HistoryMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Message.At.ID
	}
	return out
}

func replyIDs(from, to int) []string {
	var out []string
	for k := from; k <= to; k++ {
		out = append(out, replyID(k))
	}
	return out
}

// The replies come from Graph's replies of the root, in the channel's team named by its Microsoft Entra group, with the app's Graph
// token, $top 50: the window's newest limit of them, oldest first, whatever order Graph gave, with how many older ones were left out.
func TestThreadBetweenReadsTheRepliesThroughGraph(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ct.replies(8)
	msgs, dropped, err := ct.c.ThreadBetween(t.Context(), target, replyID(1), replyID(8), 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(msgs); !slices.Equal(got, replyIDs(4, 7)) || dropped != 2 {
		t.Fatalf("ThreadBetween = %v, %d dropped; want %v, the newest 4 of replies 2 to 7, and 2 dropped", got, dropped, replyIDs(4, 7))
	}
	m := msgs[0].Message
	want := chat.Inbound{At: chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: replyID(4), Thread: testRoot},
		User: testUser, Text: "reply 4"}
	if m != want || msgs[0].Own || msgs[0].Ignored {
		t.Errorf("message = %+v, want %+v", msgs[0], want)
	}
	reqs := ct.f.graphTaken()
	if len(reqs) != 1 {
		t.Fatalf("Graph took %d calls, want 1: %+v", len(reqs), reqs)
	}
	if want := messagePath(testGroup, testChannel, testRoot) + "/replies"; reqs[0].path != want || reqs[0].query != "$top=50" {
		t.Errorf("call = %+v, want %s?$top=50", reqs[0], want)
	}
	if !strings.HasPrefix(reqs[0].auth, "Bearer fake-graph-token-") {
		t.Errorf("Authorization = %q, want the Graph token", reqs[0].auth)
	}
	ct.f.mu.Lock()
	scopes := slices.Clone(ct.f.graph.scopes)
	ct.f.mu.Unlock()
	if !slices.Equal(scopes, []string{graphScope}) {
		t.Errorf("token grants asked for %v, want Graph's scope once", scopes)
	}

	// The token is kept: a second read asks Entra for none.
	if _, _, err := ct.c.ThreadBetween(t.Context(), target, "", "", 10); err != nil {
		t.Fatal(err)
	}
	ct.f.mu.Lock()
	n := len(ct.f.graph.scopes)
	ct.f.mu.Unlock()
	if n != 1 {
		t.Errorf("%d token grants after two reads, want 1", n)
	}
}

// An empty bound is no bound; the window is exclusive on both sides, and the root is never a reply.
func TestThreadBetweenBounds(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ct.replies(5)
	for _, tc := range []struct {
		name          string
		after, before string
		want          []string
	}{
		{"no bounds", "", "", replyIDs(1, 5)},
		{"after alone", replyID(3), "", replyIDs(4, 5)},
		{"before alone", "", replyID(3), replyIDs(1, 2)},
		{"both", replyID(1), replyID(4), replyIDs(2, 3)},
		{"before the root", "", testRoot, nil},
		{"bounds that are no message", "1794965001500", "1794965004500", replyIDs(2, 4)},
	} {
		msgs, dropped, err := ct.c.ThreadBetween(t.Context(), target, tc.after, tc.before, 100)
		if err != nil || dropped != 0 || !slices.Equal(idsOf(msgs), tc.want) {
			t.Errorf("%s: %v, %d, %v; want %v", tc.name, idsOf(msgs), dropped, err, tc.want)
		}
	}
}

// A window that is empty or inverted is nothing, with no call; a target with no thread or channel, a limit under one, and a bound or
// a thread that is not a Teams message ID are chat.ErrInvalid, with no call.
func TestThreadBetweenMakesNoCallForAnEmptyWindowOrAnInvalidAsk(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ct.replies(3)
	for name, tc := range map[string]struct {
		target        chat.Target
		after, before string
		limit         int
		invalid       bool
	}{
		"empty":            {target, replyID(1), replyID(1), 10, false},
		"inverted":         {target, replyID(2), replyID(1), 10, false},
		"inverted by size": {target, "10000000000000", replyID(1), 10, false},
		"no thread":        {chat.Target{Conn: testConn, Channel: testChannel}, "", "", 10, true},
		"no channel":       {chat.Target{Conn: testConn, Thread: testRoot}, "", "", 10, true},
		"no limit":         {target, "", "", 0, true},
		"a word bound":     {target, "abc", "", 10, true},
		"a word thread":    {chat.Target{Conn: testConn, Channel: testChannel, Thread: "x/../y"}, "", "", 10, true},
		"a signed bound":   {target, "", "-1", 10, true},
	} {
		msgs, dropped, err := ct.c.ThreadBetween(t.Context(), tc.target, tc.after, tc.before, tc.limit)
		if tc.invalid != errors.Is(err, chat.ErrInvalid) || (!tc.invalid && err != nil) || msgs != nil || dropped != 0 {
			t.Errorf("%s: %v, %d, %v", name, idsOf(msgs), dropped, err)
		}
		if n := len(ct.f.graphTaken()); n != 0 {
			t.Errorf("%s: %d Graph calls", name, n)
		}
	}
}

// A thread is read page by page, following each @odata.nextLink, up to threadMaxPages pages: a thread that Graph has more pages of is
// chat.ErrThreadTooLong, with no messages, and a nil error always means the whole thread was read.
func TestThreadBetweenReadsThePagesUpToItsCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		replies int
		tooLong bool
	}{
		{"one page", 2, false},
		{"the cap exactly", 2 * threadMaxPages, false},
		{"past the cap", 2*threadMaxPages + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ct, target := threadTest(t)
			ct.replies(tc.replies)
			ct.f.setGraph(func(g *fakeGraph) { g.pageAt = 2 })
			msgs, dropped, err := ct.c.ThreadBetween(t.Context(), target, "", "", 3)
			calls := len(ct.f.graphTaken())
			switch {
			case tc.tooLong:
				if !errors.Is(err, chat.ErrThreadTooLong) || msgs != nil || dropped != 0 || calls != threadMaxPages {
					t.Errorf("ThreadBetween = %v, %d, %v after %d calls; want ErrThreadTooLong after %d", idsOf(msgs), dropped, err, calls, threadMaxPages)
				}
			case err != nil || !slices.Equal(idsOf(msgs), replyIDs(max(1, tc.replies-2), tc.replies)) || dropped != max(0, tc.replies-3):
				t.Errorf("ThreadBetween = %v, %d, %v", idsOf(msgs), dropped, err)
			}
		})
	}
}

// A next page that names a page the read already took, or a page on another host than Graph's, is an error and no answer: the token
// goes to Graph alone, and a cycle is not read for ever. elsewhere is a server that would answer a page, and must be asked nothing.
func TestThreadBetweenRefusesANextLinkItCannotFollow(t *testing.T) {
	t.Parallel()
	var asked atomic.Int32
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = io.WriteString(w, `{"value":[]}`)
	}))
	t.Cleanup(elsewhere.Close)
	for name, next := range map[string]func(self string, skip int) string{
		"another port": func(string, int) string { return elsewhere.URL + "/v1.0/teams/x/replies?$skiptoken=1" },
		"a cycle":      func(self string, _ int) string { return self + "?$top=50&$skiptoken=0" },
		"another host": func(string, int) string { return "https://graph.example.test/v1.0/teams/x/replies?$skiptoken=1" },
		"another path": func(self string, _ int) string { return strings.Replace(self, "/v1.0/", "/beta/", 1) + "?$skiptoken=1" },
		"plain http": func(self string, _ int) string {
			return strings.Replace(self, "https://", "http://", 1) + "?$skiptoken=1"
		},
		"not a URL":       func(string, int) string { return "%zz" },
		"a relative link": func(string, int) string { return "/v1.0/teams/x/replies?$skiptoken=1" },
		"credentials in it": func(self string, _ int) string {
			return strings.Replace(self, "https://", "https://u:p@", 1) + "?$skiptoken=1"
		},
		"a dot-dot segment": func(self string, _ int) string {
			return strings.Replace(self, "/v1.0/", "/v1.0/../beta/", 1) + "?$skiptoken=1"
		},
		"an escaped dot-dot segment": func(self string, _ int) string {
			return strings.Replace(self, "/v1.0/", "/v1.0/%2e%2E/beta/", 1) + "?$skiptoken=1"
		},
		"a dot segment": func(self string, _ int) string {
			return strings.Replace(self, "/v1.0/", "/v1.0/./", 1) + "?$skiptoken=1"
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ct, target := threadTest(t)
			ct.replies(3)
			ct.f.setGraph(func(g *fakeGraph) { g.pageAt, g.next = 1, next })
			msgs, _, err := ct.c.ThreadBetween(t.Context(), target, "", "", 10)
			if err == nil || msgs != nil || errors.Is(err, chat.ErrThreadTooLong) {
				t.Fatalf("ThreadBetween = %v, %v; want an error", idsOf(msgs), err)
			}
			want := 1 // the first page; a cycle is found on the page that names one already taken
			if name == "a cycle" {
				want = 2
			}
			if offGraph := errors.Is(err, errNotGraph); offGraph == (name == "a cycle") {
				t.Errorf("err = %v; errors.Is(err, errNotGraph) = %v", err, offGraph)
			}
			if n := len(ct.f.graphTaken()); n != want {
				t.Errorf("%d Graph calls, want %d", n, want)
			}
			if reqs := ct.f.take(); len(reqs) != 0 {
				t.Errorf("the token went to a path that is not Graph's: %+v", reqs)
			}
		})
	}
	t.Cleanup(func() {
		if n := asked.Load(); n != 0 {
			t.Errorf("another server was asked %d times", n)
		}
	})
}

// Graph's client follows no redirect: a 3xx of Graph is its answer, and neither the request nor the token goes where it points.
func TestThreadBetweenFollowsNoRedirect(t *testing.T) {
	t.Parallel()
	var elsewhere atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	ct, target := threadTest(t)
	ct.f.setGraph(func(g *fakeGraph) {
		g.redirect = other.URL + "/v1.0/teams/x"
	})
	_, _, err := ct.c.ThreadBetween(t.Context(), target, "", "", 10)
	var ge *GraphError
	if !errors.As(err, &ge) || ge.Status != http.StatusTemporaryRedirect || elsewhere.Load() != 0 {
		t.Fatalf("a redirect: %v, followed %d times", err, elsewhere.Load())
	}
}

// The error of a read that fails in the transport, or that runs out of time, names none of the team, the channel or the thread, which
// the URL of the read holds and the transport's own error repeats.
func TestAFailedThreadReadNamesNoTeamChannelOrThread(t *testing.T) {
	t.Parallel()
	ids := []string{testGroup, testChannel, strings.TrimPrefix(strings.TrimSuffix(testChannel, "@thread.tacv2"), "19:"), testRoot}
	names := func(t *testing.T, what string, err error) {
		t.Helper()
		for _, id := range ids {
			if strings.Contains(err.Error(), id) {
				t.Errorf("%s: %q names %q", what, err, id)
			}
		}
	}

	held, target := threadTest(t)
	held.c.threadTimeout = 50 * time.Millisecond
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	held.f.setGraph(func(g *fakeGraph) { g.hold = hold })
	_, _, err := held.c.ThreadBetween(t.Context(), target, "", "", 10)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held read: %v, want its deadline", err)
	}
	names(t, "a held read", err)

	failed, target := threadTest(t)
	errReset := errors.New("connection reset by the test")
	base := failed.f.srv.Client().Transport
	failed.c.graph.client = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, graphPrefix) {
			return nil, errReset
		}
		return base.RoundTrip(r)
	})}
	_, _, err = failed.c.ThreadFromStart(t.Context(), target, "", 10)
	if !errors.Is(err, errReset) {
		t.Fatalf("a failed read: %v, want the transport's error wrapped", err)
	}
	names(t, "a failed read", err)
}

// Microsoft Entra's refusal of the app's token for Graph (a scope or a consent the app lacks) refuses the read alone: it is
// Permanent, not FatalAuth, which would stop the connection. Entra's refusal of the bot's token still is FatalAuth.
func TestARefusedGraphTokenIsNotTheBots(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		ct, target := threadTest(t)
		ct.f.setFail(func(r *http.Request) (int, string, bool) {
			_ = r.ParseForm()
			return status, `{"error":"invalid_scope","error_codes":[70011]}`, r.URL.Path == "/token" && r.PostForm.Get("scope") == graphScope
		})
		_, _, err := ct.c.ThreadBetween(t.Context(), target, "", "", 10)
		var te *TokenRequestError
		if !errors.As(err, &te) {
			t.Fatalf("%d: %v, want Entra's refusal", status, err)
		}
		if v := ct.c.Classify(chat.OpOther, err); v.Class != chat.Permanent || v.Reason != chat.ReasonOther {
			t.Errorf("%d for the Graph token: %+v, want Permanent, ReasonOther", status, v)
		}
		if !strings.Contains(err.Error(), "Microsoft Graph") {
			t.Errorf("%d: %q does not say the token was Graph's", status, err)
		}
		if _, err := ct.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, chat.Say(chat.Message{Fallback: "x"}), "tag"); err != nil {
			t.Errorf("%d: a post with the bot's token: %v", status, err)
		}

		bot := newClientTest(t)
		bot.record(t, testChannel, testChannel)
		bot.f.setFail(func(r *http.Request) (int, string, bool) {
			_ = r.ParseForm()
			return status, `{"error":"invalid_client"}`, r.URL.Path == "/token" && r.PostForm.Get("scope") == botFrameworkScope
		})
		_, err = bot.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, chat.Say(chat.Message{Fallback: "x"}), "tag")
		if v := bot.c.Classify(chat.OpPost, err); v.Class != chat.FatalAuth {
			t.Errorf("%d for the bot's token: %v, %+v, want FatalAuth", status, err, v)
		}
	}
}

// A message that Graph gives on two pages, or the root given among the replies, is one message, and the root is no reply.
func TestThreadBetweenTakesEachReplyOnce(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ct.replies(3)
	ct.f.setGraph(func(g *fakeGraph) {
		g.pageAt = 2
		g.extra = []map[string]any{person(testRoot, "", testUser, "<p>the root</p>"), person(replyID(2), testRoot, testUser, "<p>again</p>")}
	})
	msgs, dropped, err := ct.c.ThreadBetween(t.Context(), target, "", "", 10)
	if err != nil || dropped != 0 || !slices.Equal(idsOf(msgs), replyIDs(1, 3)) {
		t.Fatalf("ThreadBetween = %v, %d, %v; want each reply once", idsOf(msgs), dropped, err)
	}
}

// What each message of the thread is, decoded as the adapter decodes a live one: a person's post with their Microsoft Entra object ID
// and its HTML as text, a mention of the bot by the mention Graph lists, Agent Kourier's own post, another app's post with its card's
// text, and what is dropped (Ignored, with only where it was): a system event, a deleted message, an app's post with nothing in it, a
// person's with no text or no ID. A message with an ID that is not Teams' is left out.
func TestThreadMessagesAreDecodedAsTheAdapterDecodesThem(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	card := `{"type":"AdaptiveCard","version":"1.4","body":[{"type":"TextBlock","text":"Alert: HighErrorRate"},` +
		`{"type":"FactSet","facts":[{"title":"severity","value":"critical"},{"title":"service","value":"checkout"}]}]}`
	system := person(replyID(5), testRoot, "", "<systemEventMessage/>")
	system["messageType"], system["from"] = "unknownFutureValue", nil
	event := person(replyID(9), testRoot, testUser, "<p>an event with a sender and text</p>")
	event["messageType"] = "chatEvent"
	deleted := person(replyID(6), testRoot, testUser, "<p>a body that a deleted message still holds</p>")
	deleted["deletedDateTime"] = "2026-11-18T01:00:00Z"
	hero := app(replyID(10), testRoot, testWorkflows, "")
	hero["attachments"] = []map[string]any{{"contentType": "application/vnd.microsoft.card.hero", "content": `{"body":[{"type":"TextBlock","text":"x"}]}`}}
	for _, m := range []map[string]any{
		person(replyID(1), testRoot, testUser, `<p><at id="0">Agent Kourier</at>&nbsp;look at <b>this</b> &amp; that</p>`, mentionOfBot()),
		app(replyID(2), testRoot, testAppID, "<p>Agent Kourier's own answer</p>"),
		app(replyID(3), testRoot, testWorkflows, `<attachment id="91fe7b44"></attachment>`, card),
		app(replyID(4), testRoot, testWorkflows, `<attachment id="1"></attachment>`),
		system,
		deleted,
		person(replyID(7), testRoot, testUser, `<p><img src="x.png"></p>`),
		person(replyID(8), testRoot, "", "<p>no Entra ID</p>"),
		person("1794965009000x", testRoot, testUser, "<p>an ID that is not Teams'</p>"),
		event,
		hero,
	} {
		ct.f.putMessage(testChannel, m)
	}
	msgs, dropped, err := ct.c.ThreadBetween(t.Context(), target, "", "", 100)
	if err != nil || dropped != 0 {
		t.Fatalf("ThreadBetween: %d dropped, %v", dropped, err)
	}
	at := func(n int) chat.Ref {
		return chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: replyID(n), Thread: testRoot}
	}
	ignored := func(n int) chat.HistoryMessage {
		return chat.HistoryMessage{Message: chat.Inbound{At: at(n)}, Ignored: true}
	}
	want := []chat.HistoryMessage{
		{Message: chat.Inbound{At: at(1), User: testUser, Text: "<at>Agent Kourier</at> look at this & that", Mentioned: true}},
		{Message: chat.Inbound{At: at(2), Text: "Agent Kourier's own answer"}, Own: true},
		{Message: chat.Inbound{At: at(3), Bot: &chat.BotMessage{ID: testWorkflows, Kind: v1alpha1.SenderKindBot,
			Body: "Alert: HighErrorRate\nseverity: critical\nservice: checkout"}}},
		ignored(4), ignored(5), ignored(6), ignored(7), ignored(8), ignored(9), ignored(10),
	}
	if len(msgs) != len(want) {
		t.Fatalf("%d messages, want %d: %v", len(msgs), len(want), idsOf(msgs))
	}
	for i := range want {
		got, w := msgs[i], want[i]
		if got.Own != w.Own || got.Ignored != w.Ignored || got.Message.At != w.Message.At || got.Message.User != w.Message.User ||
			got.Message.Text != w.Message.Text || got.Message.Mentioned != w.Message.Mentioned ||
			(got.Message.Bot == nil) != (w.Message.Bot == nil) || (w.Message.Bot != nil && *got.Message.Bot != *w.Message.Bot) {
			t.Errorf("message %d = %+v (bot %+v), want %+v (bot %+v)", i, got, got.Message.Bot, w, w.Message.Bot)
		}
	}
}

// ThreadFromStart reads what opened the thread first, which the limit does not count or cut; a root that Graph no longer has starts
// the thread with its first reply; a root that cannot be read otherwise fails the read; and a thread too long to read keeps its root.
func TestThreadFromStartLeadsWithTheRoot(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ct.replies(4)
	msgs, dropped, err := ct.c.ThreadFromStart(t.Context(), target, replyID(4), 2)
	if err != nil || dropped != 1 || !slices.Equal(idsOf(msgs), []string{testRoot, replyID(2), replyID(3)}) {
		t.Fatalf("ThreadFromStart = %v, %d, %v", idsOf(msgs), dropped, err)
	}
	if root := msgs[0].Message; root.Text != "the root" || root.At.Thread != "" || root.User != testUser {
		t.Errorf("root = %+v", root)
	}
	if reqs := ct.f.graphTaken(); len(reqs) != 2 || reqs[0].path != messagePath(testGroup, testChannel, testRoot) {
		t.Errorf("calls = %+v, want the root, then its replies", reqs)
	}

	// Graph answering the root's path with another message is no answer.
	ct.f.setGraph(func(g *fakeGraph) {
		g.msgs[messagePath(testGroup, testChannel, testRoot)] = person(replyID(1), testRoot, testUser, "<p>not the root</p>")
	})
	if msgs, _, err := ct.c.ThreadFromStart(t.Context(), target, "", 10); err == nil || msgs != nil {
		t.Errorf("another message for the root = %v, %v; want an error", idsOf(msgs), err)
	}
	ct.f.putMessage(testChannel, person(testRoot, "", testUser, "<p>the root</p>"))

	ct.f.setGraph(func(g *fakeGraph) { g.pageAt = 1 })
	ct.replies(threadMaxPages + 1)
	msgs, dropped, err = ct.c.ThreadFromStart(t.Context(), target, "", 2)
	if !errors.Is(err, chat.ErrThreadTooLong) || dropped != 0 || !slices.Equal(idsOf(msgs), []string{testRoot}) {
		t.Errorf("a thread too long = %v, %d, %v; want the root alone and ErrThreadTooLong", idsOf(msgs), dropped, err)
	}

	ct.f.setGraph(func(g *fakeGraph) {
		g.pageAt = 0
		g.fail = func(r *http.Request) (int, string, bool) {
			return http.StatusNotFound, `{"error":{"code":"NotFound"}}`, !strings.HasSuffix(r.URL.Path, "/replies")
		}
	})
	msgs, _, err = ct.c.ThreadFromStart(t.Context(), target, replyID(3), 10)
	if err != nil || !slices.Equal(idsOf(msgs), replyIDs(1, 2)) {
		t.Errorf("a root that is gone = %v, %v; want the replies alone", idsOf(msgs), err)
	}

	ct.f.setGraph(func(g *fakeGraph) {
		g.fail = func(r *http.Request) (int, string, bool) {
			return http.StatusServiceUnavailable, `{"error":{"code":"ServiceNotAvailable"}}`, !strings.HasSuffix(r.URL.Path, "/replies")
		}
	})
	ct.f.graphTaken()
	if msgs, _, err := ct.c.ThreadFromStart(t.Context(), target, "", 10); err == nil || msgs != nil {
		t.Errorf("a root that cannot be read = %v, %v; want an error", idsOf(msgs), err)
	}
	if n := len(ct.f.graphTaken()); n != 1 {
		t.Errorf("%d calls after a root that cannot be read, want 1", n)
	}
}

// A channel whose team's Microsoft Entra group is not known (no activity of it has come to the bot, or its record holds no group) cannot
// be read: an error, which is not chat.ErrNoThreadReads (the turn says the replies could not be read), and no call. A store that fails
// is an error too.
func TestAThreadOfAnUnknownTeamIsNotRead(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	other := "19:KourierStandInUnlistedChannel000000000000a@thread.tacv2"
	if err := ct.convs.Remember(t.Context(), testConn, Conversation{Channel: other, ServiceURL: ct.f.serviceURL(), TeamID: testChannel}); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{testChannel, other} {
		_, _, err := ct.c.ThreadBetween(t.Context(), chat.Target{Conn: testConn, Channel: channel, Thread: testRoot}, "", "", 10)
		if err == nil || errors.Is(err, chat.ErrNoThreadReads) || !strings.Contains(err.Error(), "not known") {
			t.Errorf("%s: %v", channel, err)
		}
	}
	if n := len(ct.f.graphTaken()); n != 0 {
		t.Errorf("%d Graph calls", n)
	}

	failing := newClientTest(t)
	failing.c.convs = failingConversations{}
	if _, _, err := failing.c.ThreadBetween(t.Context(), chat.Target{Conn: testConn, Channel: testChannel, Thread: testRoot}, "", "", 10); err == nil ||
		!strings.Contains(err.Error(), "store down") {
		t.Errorf("a store that fails: %v", err)
	}
}

// failingConversations is a store that cannot be read.
type failingConversations struct{}

func (failingConversations) Remember(context.Context, string, Conversation) error {
	return errors.New("store down")
}
func (failingConversations) Lookup(context.Context, string, string) (Conversation, bool, error) {
	return Conversation{}, false, errors.New("store down")
}

// Graph's refusals reach Classify as a *GraphError with its code and Retry-After, and no message: a 429 waits, a 5xx is tried again,
// a 403 is permanent and says how to grant the app its permission, a 404 is a thread that is gone, and a 401 a rejected credential.
func TestGraphRefusalsAreClassified(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		code   string
		class  chat.Class
		reason chat.Reason
		wait   time.Duration
	}{
		{"throttled", http.StatusTooManyRequests, "TooManyRequests", chat.Transient, "", 4 * time.Second},
		{"unavailable", http.StatusServiceUnavailable, "ServiceNotAvailable", chat.Transient, "", 0},
		{"server error", http.StatusInternalServerError, "InternalServerError", chat.Transient, "", 0},
		{"no permission", http.StatusForbidden, "Forbidden", chat.Permanent, chat.ReasonOther, 0},
		{"gone", http.StatusNotFound, "NotFound", chat.Permanent, chat.ReasonChannelGone, 0},
		// The app's Graph token refused is one read refused, not the connection's credentials: those are the bot's token's.
		{"rejected token", http.StatusUnauthorized, "InvalidAuthenticationToken", chat.Permanent, chat.ReasonOther, 0},
		{"bad request", http.StatusBadRequest, "BadRequest", chat.Permanent, chat.ReasonOther, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ct, target := threadTest(t)
			ct.f.setGraph(func(g *fakeGraph) {
				g.fail = func(*http.Request) (int, string, bool) {
					return tc.status, `{"error":{"code":"` + tc.code + `","message":"secret detail of ` + testChannel + `"}}`, true
				}
			})
			_, _, err := ct.c.ThreadBetween(t.Context(), target, "", "", 10)
			var ge *GraphError
			if !errors.As(err, &ge) || ge.Status != tc.status || ge.Code != tc.code {
				t.Fatalf("err = %v, want a GraphError %d %s", err, tc.status, tc.code)
			}
			if text := err.Error(); strings.Contains(text, "secret detail") || strings.Contains(text, testChannel) {
				t.Errorf("err = %q quotes Graph's message or names the channel", text)
			}
			if tc.status == http.StatusForbidden {
				for _, w := range []string{"ChannelMessage.Read.Group", "install"} {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("err = %q does not name %q, the fix", err, w)
					}
				}
			}
			code := tc.code // a Transient verdict carries no code, as the Bot Connector's do not
			if tc.class == chat.Transient {
				code = ""
			}
			v := ct.c.Classify(chat.OpOther, err)
			if v.Class != tc.class || v.Reason != tc.reason || v.Code != code || v.RetryAfter != tc.wait {
				t.Errorf("verdict = %+v, want class %d reason %q code %q wait %v", v, tc.class, tc.reason, code, tc.wait)
			}
		})
	}
}

// A 429 with no Retry-After still waits a second, as the Bot Connector's does.
func TestAGraphRateLimitWithNoWaitIsASecond(t *testing.T) {
	t.Parallel()
	c := newClientTest(t).c
	if v := c.Classify(chat.OpOther, &GraphError{Status: http.StatusTooManyRequests}); v.Class != chat.Transient || v.RetryAfter != time.Second {
		t.Errorf("verdict = %+v", v)
	}
}

// A read is bounded as a whole by threadTimeout, whatever the caller's context, and a page longer than graphMaxBody is an error.
func TestAThreadReadIsBounded(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ct.c.threadTimeout = 50 * time.Millisecond
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	ct.f.setGraph(func(g *fakeGraph) { g.hold = hold })
	start := time.Now()
	if _, _, err := ct.c.ThreadBetween(t.Context(), target, "", "", 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a held read: %v, want its deadline", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the held read took %v", d)
	}

	big, bigTarget := threadTest(t)
	big.replies(1)
	big.f.setGraph(func(g *fakeGraph) { g.big = graphMaxBody })
	if _, _, err := big.c.ThreadBetween(t.Context(), bigTarget, "", "", 10); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("a page past the cap: %v", err)
	}
}

// Each read is counted as ThreadBetween in agentkourier_chat_api_calls_total, by its outcome.
func TestAThreadReadIsCounted(t *testing.T) {
	t.Parallel()
	o := newObserved(t)
	ct, target := threadTest(t)
	ctx := o.ctx
	if _, _, err := ct.c.ThreadBetween(ctx, target, "", "", 10); err != nil {
		t.Fatal(err)
	}
	ct.f.setGraph(func(g *fakeGraph) {
		g.fail = func(*http.Request) (int, string, bool) {
			return http.StatusTooManyRequests, `{"error":{"code":"TooManyRequests"}}`, true
		}
	})
	_, _, _ = ct.c.ThreadFromStart(ctx, target, "", 10)
	unknown := target
	unknown.Channel = "19:no-activity-yet@thread.tacv2"
	if _, _, err := ct.c.ThreadBetween(ctx, unknown, "", "", 10); !errors.Is(err, errTeamNotKnown) {
		t.Fatalf("a channel with no record: %v, want errTeamNotKnown", err)
	}
	ct.f.setGraph(func(g *fakeGraph) {
		g.fail = func(*http.Request) (int, string, bool) {
			return http.StatusUnauthorized, `{"error":{"code":"InvalidAuthenticationToken"}}`, true
		}
	})
	_, _, _ = ct.c.ThreadBetween(ctx, target, "", "", 10)
	if got := o.value(t, "agentkourier_chat_api_calls_total", "api_error"); got != 1 {
		t.Errorf("api_error = %v, want 1 (Graph's 401)", got)
	}
	if got := o.value(t, "agentkourier_chat_api_calls_total", "auth_rejected"); got != 0 {
		t.Errorf("auth_rejected = %v, want 0: Graph's 401 is not the bot's credentials", got)
	}
	if got := o.value(t, "agentkourier_chat_api_calls_total", "error"); got != 1 {
		t.Errorf("error = %v, want 1 (a read of a channel whose team is not known)", got)
	}
	if got := o.value(t, "agentkourier_chat_api_calls_total", "success"); got != 1 {
		t.Errorf("success = %v, want 1", got)
	}
	if got := o.value(t, "agentkourier_chat_api_calls_total", "rate_limited"); got != 1 {
		t.Errorf("rate_limited = %v, want 1", got)
	}
}

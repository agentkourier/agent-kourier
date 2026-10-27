package slack_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

const threadConn = "agent-kourier-system/slack-main"

// pathCounter counts the calls to the Web API by method, and answers conversations.replies with limited (a 429 that asks
// for a wait of 7 s) if it is set.
type pathCounter struct {
	mu      sync.Mutex
	n       map[string]int
	limited atomic.Bool
}

func (c *pathCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	c.mu.Lock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[method]++
	c.mu.Unlock()
	if method == "conversations.replies" && c.limited.Load() {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Header: http.Header{"Retry-After": {"7"}},
			Body: http.NoBody, Request: r}, nil
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (c *pathCounter) calls(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[method]
}

func (c *pathCounter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.n {
		n += v
	}
	return n
}

// thread is a thread in a fake Slack with a Client of page size 2 connected to it, so that people can write in it: the
// bot's own post is its root, and reply adds a person's.
type thread struct {
	t     *testing.T
	fake  *fakeslack.Server
	c     *slack.Client
	count *pathCounter
	root  string
}

func newThread(t *testing.T) *thread {
	t.Helper()
	fake := newFake(t)
	count := &pathCounter{}
	cfg := clientConfig(fake)
	cfg.PageLimit = 2
	cfg.HTTPClient = &http.Client{Transport: count}
	c := slack.New(cfg)
	run(t, c, &sink{})
	waitConnected(t, fake)
	root, err := c.Post(t.Context(), "C1", "", slack.Content{Text: "root"}, "root")
	if err != nil {
		t.Fatal(err)
	}
	return &thread{t: t, fake: fake, c: c, count: count, root: root}
}

// reply adds n replies by U1 and returns their ts, oldest first.
func (th *thread) reply(n int) []string {
	th.t.Helper()
	var out []string
	for i := range n {
		m, err := th.fake.Reply("C1", th.root, "U1", fmt.Sprintf("reply %d", i+1))
		if err != nil {
			th.t.Fatal(err)
		}
		out = append(out, m.TS)
	}
	return out
}

// between reads the thread between two messages, newest limit of them.
func (th *thread) between(after, before string, limit int) ([]chat.HistoryMessage, int, error) {
	th.t.Helper()
	return th.c.ThreadBetween(th.t.Context(), threadConn, "C1", th.root, after, before, limit)
}

func tsOf(msgs []chat.HistoryMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Message.At.ID
	}
	return out
}

func TestThreadBetweenBoundsAreExclusiveAndTheParentIsNotReturned(t *testing.T) {
	th := newThread(t)
	r := th.reply(5)
	for name, tc := range map[string]struct {
		after, before string
		want          []string
	}{
		"both bounds out":          {r[0], r[4], r[1:4]},
		"adjacent bounds, nothing": {r[1], r[2], nil},
		"no lower bound":           {"", r[2], r[:2]},
		"no upper bound":           {r[2], "", r[3:]},
		"no bound":                 {"", "", r},
		"bounds that are no reply": {th.root, "99999999999.000000", r}, // a bound at the parent: the parent is no reply either
	} {
		t.Run(name, func(t *testing.T) {
			got, dropped, err := th.between(tc.after, tc.before, 10)
			if err != nil || dropped != 0 || !slices.Equal(tsOf(got), tc.want) {
				t.Errorf("ThreadBetween(%q, %q) = %v, %d, %v; want %v, 0", tc.after, tc.before, tsOf(got), dropped, err, tc.want)
			}
		})
	}
}

func TestThreadBetweenReturnsMessagesAsTheLiveIntakeDecodesThem(t *testing.T) {
	th := newThread(t)
	person := th.reply(1)[0]
	own, err := th.c.Post(t.Context(), "C1", th.root, slack.Content{Text: "an answer"}, "answer")
	if err != nil {
		t.Fatal(err)
	}
	mention, err := th.fake.Reply("C1", th.root, "U2", botMark+" and this?")
	if err != nil {
		t.Fatal(err)
	}
	got, dropped, err := th.between("", "", 10)
	if err != nil || dropped != 0 || len(got) != 3 {
		t.Fatalf("ThreadBetween = %d messages, %d, %v; want 3, 0", len(got), dropped, err)
	}
	for i, want := range []struct {
		id, user       string
		own, mentioned bool
	}{{person, "U1", false, false}, {own, "", true, false}, {mention.TS, "U2", false, true}} {
		h := got[i]
		wantRef := chat.Ref{Platform: "slack", Conn: threadConn, Channel: "C1", Thread: th.root, ID: want.id}
		if h.Message.At != wantRef || h.Own != want.own || h.Ignored || h.Message.Mentioned != want.mentioned || h.Message.Bot != nil && !want.own {
			t.Errorf("message %d = %+v (own %v, ignored %v); want at %+v, own %v, mentioned %v", i, h.Message, h.Own, h.Ignored, wantRef, want.own, want.mentioned)
		}
		if !want.own && h.Message.User != want.user {
			t.Errorf("message %d is by %q; want %q", i, h.Message.User, want.user)
		}
	}
	if got[0].Message.Text != "reply 1" {
		t.Errorf("the person's text is %q; want it verbatim", got[0].Message.Text)
	}
}

func TestThreadBetweenKeepsTheNewestAndSaysHowManyItLeftOut(t *testing.T) {
	th := newThread(t)
	r := th.reply(6)
	got, dropped, err := th.between("", "", 4)
	if err != nil || dropped != 2 || !slices.Equal(tsOf(got), r[2:]) {
		t.Errorf("ThreadBetween(limit 4 of 6) = %v, %d, %v; want the newest four %v and 2 left out", tsOf(got), dropped, err, r[2:])
	}
	got, dropped, err = th.between("", "", 6)
	if err != nil || dropped != 0 || !slices.Equal(tsOf(got), r) {
		t.Errorf("ThreadBetween(limit 6 of 6) = %v, %d, %v; want all six and none left out", tsOf(got), dropped, err)
	}
}

// The fake's first page of a thread holds the parent and one reply, so five pages of two are the parent and nine replies.
// A window that Slack has more pages of than that is no partial answer: the unread pages are the newest, so the newest
// of the window cannot be returned, and the error says so with no messages.
func TestThreadBetweenReadsFivePagesAtMostAndIsAnErrorBeyondThem(t *testing.T) {
	for name, tc := range map[string]struct {
		replies, limit, wantCalls int
		tooLong                   bool
	}{
		"nine replies fill five pages exactly": {9, 100, 5, false},
		"ten replies need a sixth":             {10, 100, 5, true},
		"twenty replies":                       {20, 100, 5, true},
		"too long whatever the limit":          {20, 3, 5, true},
		"three replies, two pages":             {3, 100, 2, false},
	} {
		t.Run(name, func(t *testing.T) {
			th := newThread(t)
			r := th.reply(tc.replies)
			got, dropped, err := th.between("", "", tc.limit)
			switch {
			case tc.tooLong && (!errors.Is(err, chat.ErrThreadTooLong) || got != nil || dropped != 0):
				t.Errorf("ThreadBetween = %v, %d, %v; want no messages and an error wrapping ErrThreadTooLong", tsOf(got), dropped, err)
			case !tc.tooLong && (err != nil || dropped != 0 || !slices.Equal(tsOf(got), r)):
				t.Errorf("ThreadBetween = %v, %d, %v; want all %d replies, none left out", tsOf(got), dropped, err, len(r))
			}
			if n := th.count.calls("conversations.replies"); n != tc.wantCalls {
				t.Errorf("ThreadBetween read %d pages; want %d, never a sixth", n, tc.wantCalls)
			}
		})
	}
}

// A window that fits the cap is the whole of it, with the limit leaving out exactly the older ones.
func TestThreadBetweenWithinTheCapCountsExactlyWhatTheLimitLeftOut(t *testing.T) {
	th := newThread(t)
	r := th.reply(9) // as many as five pages hold
	got, dropped, err := th.between("", "", 3)
	if err != nil || dropped != 6 || !slices.Equal(tsOf(got), r[6:]) {
		t.Errorf("ThreadBetween = %v, %d, %v; want the newest three %v and exactly 6 older left out", tsOf(got), dropped, err, r[6:])
	}
}

func TestThreadBetweenMakesNoCallForAnEmptyOrInvertedWindow(t *testing.T) {
	th := newThread(t)
	r := th.reply(3)
	before := th.count.total()
	for name, w := range map[string][2]string{
		"the same message":     {r[1], r[1]},
		"inverted":             {r[2], r[0]},
		"inverted by length":   {"10.000001", "9.999999"},
		"inverted by fraction": {"5.000002", "5.000001"},
		"a short fraction":     {"5.1", "5.100000"},
	} {
		got, dropped, err := th.between(w[0], w[1], 10)
		if err != nil || dropped != 0 || len(got) != 0 {
			t.Errorf("%s: ThreadBetween = %v, %d, %v; want nothing", name, tsOf(got), dropped, err)
		}
	}
	if n := th.count.total() - before; n != 0 {
		t.Errorf("an empty or inverted window made %d calls to Slack; want none", n)
	}
	// A window that is not empty does read.
	if got, _, err := th.between("5.000001", "5.000002", 10); err != nil || len(got) != 0 || th.count.total() == before {
		t.Errorf("a window of one microsecond: %v, %v, %d calls; want a read that found nothing", tsOf(got), err, th.count.total()-before)
	}
}

func TestThreadBetweenSurfacesARateLimitAsThePlatformClassifiesIt(t *testing.T) {
	th := newThread(t)
	th.reply(2)
	th.count.limited.Store(true)
	_, _, err := th.between("", "", 10)
	var limited *slackgo.RateLimitedError
	if !errors.As(err, &limited) || limited.RetryAfter != 7*time.Second {
		t.Fatalf("ThreadBetween on a 429 = %v; want a RateLimitedError that asks for 7 s", err)
	}
	p := slack.NewPlatform(th.c, threadConn)
	if v := p.Classify(chat.OpOther, err); v.Class != chat.Transient || v.RetryAfter != 7*time.Second {
		t.Errorf("Classify = %+v; want a transient error that asks for 7 s", v)
	}
	if _, _, err := p.ThreadBetween(t.Context(), chat.Target{Channel: "C1", Thread: th.root}, "", "", 10); !errors.As(err, &limited) {
		t.Errorf("Platform.ThreadBetween on a 429 = %v; want the rate limit, unretried", err)
	}
	if n := th.count.calls("conversations.replies"); n != 2 {
		t.Errorf("%d reads for two attempts; want no retry inside ThreadBetween", n)
	}
}

func TestThreadBetweenFailsOnWhatSlackRefuses(t *testing.T) {
	th := newThread(t)
	th.reply(1)
	if _, _, err := th.c.ThreadBetween(t.Context(), threadConn, "C1", "1.000001", "", "", 10); err == nil || !strings.Contains(err.Error(), "thread_not_found") {
		t.Errorf("a thread that is not there: %v; want thread_not_found", err)
	}
	for name, tc := range map[string]struct {
		thread string
		limit  int
	}{"no thread": {"", 10}, "no room": {th.root, 0}, "negative room": {th.root, -1}} {
		before := th.count.total()
		if _, _, err := th.c.ThreadBetween(t.Context(), threadConn, "C1", tc.thread, "", "", tc.limit); !errors.Is(err, chat.ErrInvalid) {
			t.Errorf("%s: %v; want chat.ErrInvalid", name, err)
		}
		if th.count.total() != before {
			t.Errorf("%s: made a call", name)
		}
	}
}

func TestPlatformThreadBetweenNamesTheConnectionAndTheThread(t *testing.T) {
	th := newThread(t)
	r := th.reply(2)
	p := slack.NewPlatform(th.c, threadConn)
	var tr chat.ThreadReader = p
	got, dropped, err := tr.ThreadBetween(t.Context(), chat.Target{Conn: threadConn, Channel: "C1", Thread: th.root}, "", r[1], 10)
	want := chat.Ref{Platform: "slack", Conn: threadConn, Channel: "C1", Thread: th.root, ID: r[0]}
	if err != nil || dropped != 0 || len(got) != 1 || got[0].Message.At != want {
		t.Errorf("Platform.ThreadBetween = %+v, %d, %v; want the first reply at %+v", got, dropped, err, want)
	}
}

// stubThread serves pages of conversations.replies by cursor and auth.test, and records the form of each read.
type stubThread struct {
	mu    sync.Mutex
	forms []url.Values
}

func (s *stubThread) client(t *testing.T, pages map[string]pg) *slack.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		body := map[string]any{"ok": true, "user_id": "UBOT", "bot_id": "BBOT"}
		if strings.HasSuffix(r.URL.Path, "/conversations.replies") {
			s.mu.Lock()
			s.forms = append(s.forms, r.Form)
			s.mu.Unlock()
			p := pages[r.Form.Get("cursor")]
			body = map[string]any{"ok": true, "messages": p.msgs, "has_more": p.hasMore, "response_metadata": map[string]any{"next_cursor": p.next}}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	cfg := clientConfig(newFake(t))
	cfg.APIURL = srv.URL + "/"
	return slack.New(cfg)
}

func reply(extra map[string]any) map[string]any {
	m := map[string]any{"type": "message", "user": "U1", "text": "hi", "thread_ts": "1.000001"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// The request carries the bounds as oldest and latest, exclusive. What comes back has the parent on every page and the pages
// out of order, and a person's message, Agent Kourier's own, another bot's and one that starts nothing are told apart.
func TestThreadBetweenAsksForExclusiveBoundsAndDecodesWhatComesBack(t *testing.T) {
	parent := map[string]any{"type": "message", "user": "U9", "text": "root", "ts": "1.000001", "thread_ts": "1.000001"}
	stub := &stubThread{}
	c := stub.client(t, map[string]pg{
		"": {msgs: []map[string]any{parent, reply(map[string]any{"ts": "1.000005", "text": "newest"}),
			reply(map[string]any{"ts": "1.000003", "user": "UBOT", "bot_id": "BBOT", "text": "mine"})}, next: "c2", hasMore: true},
		"c2": {msgs: []map[string]any{parent,
			reply(map[string]any{"ts": "1.000004", "user": "", "bot_id": "BOTHER", "subtype": "bot_message", "text": "an alert"}),
			reply(map[string]any{"ts": "1.000002", "subtype": "message_deleted", "text": ""})}},
	})
	got, dropped, err := c.ThreadBetween(t.Context(), threadConn, "C1", "1.000001", "1.000000", "1.000009", 10)
	if err != nil || dropped != 0 {
		t.Fatalf("ThreadBetween = %v, %d, %v; want no error and none left out", tsOf(got), dropped, err)
	}
	if want := []string{"1.000002", "1.000003", "1.000004", "1.000005"}; !slices.Equal(tsOf(got), want) {
		t.Fatalf("ThreadBetween = %v; want %v: oldest first, the parent filtered on both pages", tsOf(got), want)
	}
	if !got[0].Ignored || got[0].Own {
		t.Errorf("a message that starts nothing = %+v; want Ignored and not Own", got[0])
	}
	if !got[1].Own || got[1].Ignored {
		t.Errorf("agent-kourier's own post = %+v; want Own and not Ignored", got[1])
	}
	if h := got[2]; h.Own || h.Ignored || h.Message.Bot == nil || h.Message.Bot.ID != "BOTHER" {
		t.Errorf("another bot's post = %+v; want it a bot's, neither Own nor Ignored", h)
	}
	if h := got[3]; h.Own || h.Ignored || h.Message.Bot != nil || h.Message.User != "U1" || h.Message.Text != "newest" || h.Message.At.Thread != "1.000001" {
		t.Errorf("a person's reply = %+v; want a person's, in the thread", h)
	}
	if len(stub.forms) != 2 {
		t.Fatalf("%d reads; want 2, one per page", len(stub.forms))
	}
	for i, f := range stub.forms {
		if f.Get("channel") != "C1" || f.Get("ts") != "1.000001" || f.Get("oldest") != "1.000000" || f.Get("latest") != "1.000009" || f.Get("inclusive") != "0" {
			t.Errorf("read %d asked for %v; want oldest and latest as given, inclusive 0, of the thread 1.000001 in C1", i, f)
		}
	}
}

// With no bound there is no oldest and no latest in the request: Slack refuses a zero ts.
func TestThreadBetweenSendsNoBoundForAnEmptyOne(t *testing.T) {
	stub := &stubThread{}
	c := stub.client(t, map[string]pg{"": {}})
	if _, _, err := c.ThreadBetween(t.Context(), threadConn, "C1", "1.000001", "", "", 10); err != nil {
		t.Fatal(err)
	}
	if f := stub.forms[0]; f.Has("oldest") || f.Has("latest") || f.Get("inclusive") != "0" {
		t.Errorf("read asked for %v; want no oldest, no latest and inclusive 0", f)
	}
}

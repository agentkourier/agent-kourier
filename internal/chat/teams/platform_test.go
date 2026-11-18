package teams

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// escapedChannel is testChannel in a path, as the Bot Framework's clients escape it.
func escapedChannel() string { return escapeID(testChannel) }

// A post with no thread goes to the channel and starts a thread whose root is the new message; a post in a thread goes to the
// channel with ;messageid= its root. Each carries the bot's token, and goes to the channel's own service URL.
func TestPostStartsAThreadAndRepliesInIt(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	root, err := ct.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, chat.Say(chat.Message{Fallback: "hello"}), "tag")
	if err != nil {
		t.Fatal(err)
	}
	if root.Thread != "" || root.ID == "" || root.Channel != testChannel || root.Platform != PlatformName || root.Conn != testConn {
		t.Fatalf("root = %+v", root)
	}
	reply, err := ct.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel, Thread: root.ID}, chat.Say(chat.Message{Fallback: "again"}), "tag")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Thread != root.ID || reply.ID == root.ID {
		t.Fatalf("reply = %+v", reply)
	}
	reqs := ct.f.take()
	base := "/amer/" + testTenant + "/v3/conversations/"
	want := []string{base + escapedChannel() + "/activities", base + escapedChannel() + "%3Bmessageid%3D" + root.ID + "/activities"}
	for i, r := range reqs {
		if r.method != http.MethodPost || r.path != want[i] {
			t.Errorf("request %d: %s %s, want POST %s", i, r.method, r.path, want[i])
		}
		if r.auth != "Bearer fake-bot-token-1" {
			t.Errorf("request %d: Authorization %q", i, r.auth)
		}
		if r.activity.Type != "message" || r.activity.TextFormat != "markdown" {
			t.Errorf("request %d: %+v", i, r.activity)
		}
	}
	if !strings.Contains(escapedChannel(), "19%3A") || !strings.Contains(escapedChannel(), "%40thread.tacv2") {
		t.Errorf("the channel is escaped as %q", escapedChannel())
	}
}

// An update edits the message in its thread: a root in the conversation that it is the root of, a reply in its root's.
func TestUpdateEditsTheMessageInItsThread(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	base := "/amer/" + testTenant + "/v3/conversations/" + escapedChannel()
	for _, tc := range []struct {
		ref  chat.Ref
		path string
	}{
		{ct.c.RefAt(testChannel, "", "1700000000001"), base + "%3Bmessageid%3D1700000000001/activities/1700000000001"},
		{ct.c.RefAt(testChannel, "1700000000001", "1700000000009"), base + "%3Bmessageid%3D1700000000001/activities/1700000000009"},
	} {
		if err := ct.c.Update(t.Context(), tc.ref, chat.Say(chat.Message{Fallback: "edited"})); err != nil {
			t.Fatal(err)
		}
		reqs := ct.f.take()
		if len(reqs) != 1 || reqs[0].method != http.MethodPut || reqs[0].path != tc.path || reqs[0].activity.Text != "edited" {
			t.Errorf("update of %+v: %+v, want PUT %s", tc.ref, reqs, tc.path)
		}
	}
	// A message whose new content shows nothing keeps its text.
	only := chat.Say(chat.Message{Parts: []chat.Part{chat.Steps{Steps: []chat.Step{{ID: "s", Title: "t"}}}}})
	if err := ct.c.Update(t.Context(), ct.c.RefAt(testChannel, "", "1700000000001"), only); !errors.Is(err, chat.ErrWouldDropText) {
		t.Errorf("an update with no text: %v, want ErrWouldDropText", err)
	}
	if _, err := ct.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, only, "tag"); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("a post with no text: %v, want ErrInvalid", err)
	}
	if n := len(ct.f.take()); n != 0 {
		t.Errorf("%d calls were made for content with no text", n)
	}
}

// roundTripper is an http.RoundTripper of a function.
type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A channel with no record is posted to through the Bot Connector's global route.
func TestPostFallsBackToTheDefaultServiceURL(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var urls []string
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		urls = append(urls, r.URL.String())
		mu.Unlock()
		body := `{"id":"1700000000002"}`
		if r.URL.Host == "login.example.test" {
			body = `{"token_type":"Bearer","expires_in":3599,"access_token":"fake-bot-token"}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
	})}
	c, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret),
		TokenURL: "https://login.example.test/token", HTTPClient: client, Logger: testutil.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, chat.Say(chat.Message{Fallback: "x"}), "tag"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "https://smba.trafficmanager.net/teams/v3/conversations/" + escapedChannel() + "/activities"
	if len(urls) != 2 || urls[1] != want {
		t.Fatalf("calls %v, want the token and then %s", urls, want)
	}
}

// The bot's token goes to an https service URL alone: a record of another form is refused before any call, the token's included, and a
// redirect is not followed.
func TestTheTokenGoesToTheServiceURLAlone(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"http://smba.example.test/", "https://user:pw@smba.example.test/", "https://smba.example.test/?x=1", "/relative/"} {
		var calls atomic.Int32
		convs := NewMemoryConversations()
		c, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret),
			TokenURL: "https://login.example.test/token", Conversations: convs, Logger: testutil.DiscardLogger(),
			HTTPClient: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, fmt.Errorf("no call may be made, and one was: %s %s", r.Method, r.URL.Redacted())
			})}})
		if err != nil {
			t.Fatal(err)
		}
		if err := convs.Remember(t.Context(), testConn, Conversation{Channel: testChannel, ServiceURL: bad}); err != nil {
			t.Fatal(err)
		}
		_, err = c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, chat.Say(chat.Message{Fallback: "x"}), "tag")
		if err == nil || calls.Load() != 0 {
			t.Errorf("service URL %q: %v, and %d calls were made", bad, err, calls.Load())
		}
	}

	var elsewhere atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	ct := newClientTest(t) // the token from the fake, and the post to the redirector: httptest's TLS servers share one certificate
	if err := ct.convs.Remember(t.Context(), testConn, Conversation{Channel: testChannel, ServiceURL: redirector.URL + "/amer/"}); err != nil {
		t.Fatal(err)
	}
	_, err := ct.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, chat.Say(chat.Message{Fallback: "x"}), "tag")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusTemporaryRedirect || elsewhere.Load() != 0 {
		t.Fatalf("a redirect: %v, followed %d times", err, elsewhere.Load())
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	c := newClientTest(t).c
	api := func(status int, code string) error { return &APIError{Status: status, Code: code} }
	for _, tc := range []struct {
		name  string
		op    chat.Op
		err   error
		class chat.Class
		why   chat.Reason
		code  string
		wait  time.Duration
	}{
		{"unknown", chat.OpPost, errors.New("boom"), chat.Transient, "", "", 0},
		{"cancelled", chat.OpPost, context.Canceled, chat.Transient, "", "", 0},
		{"rate limited", chat.OpPost, &APIError{Status: 429, RetryAfter: 7 * time.Second}, chat.Transient, "", "", 7 * time.Second},
		{"rate limited with no wait", chat.OpPost, api(429, ""), chat.Transient, "", "", time.Second},
		{"precondition", chat.OpPost, api(412, "PreconditionFailed"), chat.Transient, "", "", 0},
		{"bad gateway", chat.OpPost, api(502, ""), chat.Transient, "", "", 0},
		{"gateway timeout", chat.OpPost, api(504, ""), chat.Transient, "", "", 0},
		{"service unavailable with a wait", chat.OpPost, &APIError{Status: 503, RetryAfter: time.Minute}, chat.Transient, "", "", 0},
		{"too large", chat.OpPost, api(413, "MessageSizeTooBig"), chat.Permanent, chat.ReasonTooLong, "MessageSizeTooBig", 0},
		{"activity gone", chat.OpPost, api(404, "ActivityNotFoundInConversation"), chat.Permanent, chat.ReasonChannelGone, "ActivityNotFoundInConversation", 0},
		{"bad request", chat.OpPost, api(400, "BadArgument"), chat.Permanent, chat.ReasonInvalidContent, "BadArgument", 0},
		{"unauthorized", chat.OpPost, api(401, "Unauthorized"), chat.FatalAuth, "", "Unauthorized", 0},
		{"disabled by an admin", chat.OpPost, api(403, "BotDisabledByAdmin"), chat.FatalAuth, "", "BotDisabledByAdmin", 0},
		{"not installed for the person", chat.OpPost, api(403, "ForbiddenOperationException"), chat.Permanent, chat.ReasonOther, "ForbiddenOperationException", 0},
		{"another cloud's host", chat.OpPost, api(403, "InvalidBotApiHost"), chat.Permanent, chat.ReasonOther, "InvalidBotApiHost", 0},
		{"not enough permissions", chat.OpOther, api(403, "NotEnoughPermissions"), chat.Permanent, chat.ReasonOther, "NotEnoughPermissions", 0},
		{"forbidden with no code", chat.OpPost, api(403, ""), chat.Permanent, chat.ReasonOther, "", 0},
		{"removed from the team", chat.OpPost, api(403, "BotNotInConversationRoster"), chat.Permanent, chat.ReasonChannelGone, "BotNotInConversationRoster", 0},
		{"blocked", chat.OpPost, api(403, "ConversationBlockedByUser"), chat.Permanent, chat.ReasonChannelGone, "ConversationBlockedByUser", 0},
		{"entra refuses the secret", chat.OpOther, &TokenRequestError{Status: 401, Code: "invalid_client"}, chat.FatalAuth, "", "invalid_client", 0},
		{"entra refuses the tenant", chat.OpPost, &TokenRequestError{Status: 400, Code: "invalid_request"}, chat.FatalAuth, "", "invalid_request", 0},
		{"entra fails", chat.OpPost, &TokenRequestError{Status: 500}, chat.Transient, "", "", 0},
		{"entra rate limits", chat.OpPost, &TokenRequestError{Status: 429, RetryAfter: 2 * time.Second}, chat.Transient, "", "", 2 * time.Second},
		{"invalid", chat.OpPost, chat.ErrInvalid, chat.Permanent, chat.ReasonInvalidContent, "", 0},
		{"too large to fit", chat.OpPost, chat.ErrTooLarge, chat.Permanent, chat.ReasonTooLong, "", 0},
		{"no ephemeral", chat.OpPost, chat.ErrNoEphemeral, chat.Permanent, chat.ReasonOther, "", 0},
		{"no reactions", chat.OpReact, chat.ErrNoReactions, chat.Permanent, chat.ReasonOther, "", 0},
		{"channel not found", chat.OpOther, chat.ErrChannelNotFound, chat.Permanent, chat.ReasonChannelGone, "", 0},
	} {
		v := c.Classify(tc.op, tc.err)
		if v.Class != tc.class || v.Reason != tc.why || v.Code != tc.code || v.RetryAfter != tc.wait {
			t.Errorf("%s: %+v, want class %d reason %q code %q wait %v", tc.name, v, tc.class, tc.why, tc.code, tc.wait)
		}
	}
}

// The Bot Connector's answers reach Classify as an *APIError with its code and Retry-After, and no message.
func TestTheBotConnectorsRefusalIsAnAPIError(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	ct.f.setFail(func(r *http.Request) (int, string, bool) {
		if r.URL.Path == "/token" {
			return 0, "", false
		}
		return http.StatusTooManyRequests, `{"error":{"code":"TooManyRequests","message":"slow down, ` + testChannel + `"}}`, true
	})
	_, err := ct.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, chat.Say(chat.Message{Fallback: "x"}), "tag")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "TooManyRequests" || ae.RetryAfter != 3*time.Second || strings.Contains(err.Error(), "slow down") {
		t.Fatalf("err = %v", err)
	}
	if v := ct.c.Classify(chat.OpPost, err); v.RetryAfter != 3*time.Second {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestChannelAccess(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	ct.f.setTeam(testChannel, testChannel)
	if a, err := ct.c.ChannelAccess(t.Context(), testChannel); err != nil || !a.Member || a.Archived {
		t.Errorf("a listed channel: %+v, %v", a, err)
	}
	unlisted := "19:KourierStandInUnlistedChannel000000000000a@thread.tacv2"
	ct.record(t, unlisted, testChannel)
	if _, err := ct.c.ChannelAccess(t.Context(), unlisted); !errors.Is(err, chat.ErrChannelNotFound) {
		t.Errorf("an unlisted channel: %v", err)
	}
	other := "19:KourierStandInOtherTeamChannel00000000000a@thread.tacv2"
	ct.record(t, other, other) // a team the Bot Connector does not know: 404
	if _, err := ct.c.ChannelAccess(t.Context(), other); !errors.Is(err, chat.ErrChannelNotFound) {
		t.Errorf("a team that is not there: %v", err)
	} else if v := ct.c.Classify(chat.OpOther, err); v.Class != chat.Permanent || v.Reason != chat.ReasonChannelGone || v.Code != "NotFound" {
		t.Errorf("its verdict: %+v", v)
	}
	unheard := "19:KourierStandInUnheardChannel0000000000000a@thread.tacv2"
	if _, err := ct.c.ChannelAccess(t.Context(), unheard); err == nil || errors.Is(err, chat.ErrChannelNotFound) || errors.Is(err, chat.ErrMissingScope) {
		t.Errorf("a channel with no record: %v, want an error that is no verdict", err)
	}
	if _, err := ct.c.ChannelAccess(t.Context(), ""); !errors.Is(err, chat.ErrChannelNotFound) {
		t.Errorf("no channel: %v", err)
	}
	ct.f.setFail(func(r *http.Request) (int, string, bool) {
		if r.URL.Path == "/token" {
			return 0, "", false
		}
		return http.StatusForbidden, `{"error":{"code":"BotNotInConversationRoster"}}`, true
	})
	if _, err := ct.c.ChannelAccess(t.Context(), testChannel); !errors.Is(err, chat.ErrChannelNotFound) {
		t.Errorf("a bot removed from the team: %v", err)
	}
}

// A link to a message needs the team's group, which an activity of the channel told the receiver; without it there is none.
func TestPermalink(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ref := ct.c.RefAt(testChannel, "1700000000001", "1700000000009")
	if _, ok := ct.c.Permalink(ref); ok {
		t.Fatal("a link with no record of the channel's team")
	}
	ct.c.recv.recMu.Lock()
	ct.c.recv.recorded[testChannel] = Conversation{Channel: testChannel, AADGroupID: testGroup}
	ct.c.recv.recMu.Unlock()
	link, ok := ct.c.Permalink(ref)
	want := "https://teams.microsoft.com/l/message/" + url.PathEscape(testChannel) + "/1700000000009?groupId=" + testGroup +
		"&parentMessageId=1700000000001&tenantId=" + testTenant
	if !ok || link != want {
		t.Fatalf("link = %q, %t; want %q", link, ok, want)
	}
	root, _ := ct.c.Permalink(ct.c.RefAt(testChannel, "", "1700000000001"))
	if !strings.Contains(root, "/1700000000001?") || !strings.Contains(root, "parentMessageId=1700000000001") {
		t.Errorf("a root's link = %q", root)
	}
	if _, ok := ct.c.Permalink(chat.Ref{Platform: "slack", Conn: testConn, Channel: testChannel, ID: "1"}); ok {
		t.Error("a link to another platform's message")
	}
}

// Run gets the bot's token first: a refused credential ends it at once, as FatalAuth, before it takes anything. Otherwise it takes
// activities until its context ends, and says so.
func TestRun(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.f.setFail(func(*http.Request) (int, string, bool) {
		return http.StatusUnauthorized, `{"error":"invalid_client","error_codes":[7000215]}`, true
	})
	var states []bool
	ct.c.onConnection = func(up bool) { states = append(states, up) }
	err := ct.c.Run(t.Context(), &recordingSink{})
	if v := ct.c.Classify(chat.OpOther, err); v.Class != chat.FatalAuth || len(states) != 0 {
		t.Fatalf("Run with a refused secret: %v (%+v), states %v", err, v, states)
	}

	ct = newClientTest(t)
	var mu sync.Mutex
	states = nil
	ct.c.onConnection = func(up bool) { mu.Lock(); states = append(states, up); mu.Unlock() }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- ct.c.Run(ctx, &recordingSink{}) }()
	testutil.Eventually(t, "Run takes activities", func() bool { return ct.c.recv.live.Load() != nil })
	h, ok := ct.c.HTTPHandler()
	if !ok || h != http.Handler(ct.c.recv) {
		t.Errorf("HTTPHandler = %v, %t; want the receiver", h, ok)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(states) != 2 || !states[0] || states[1] {
		t.Errorf("states %v, want up then down", states)
	}
}

// What Teams sends while Run waits for the bot's token is taken and counted, as Serve runs from the start; once Entra refuses the
// secret, Run ends Serve with it, and the route answers 503.
func TestRunTakesActivitiesWhileItWaitsForTheToken(t *testing.T) {
	t.Parallel()
	o := newObserved(t)
	ct := newClientTest(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // before the fake's server is closed, which waits for the blocked handler
	ct.f.setFail(func(r *http.Request) (int, string, bool) {
		if r.URL.Path != "/token" {
			return 0, "", false
		}
		<-release
		return http.StatusUnauthorized, `{"error":"invalid_client"}`, true
	})
	done := make(chan error, 1)
	go func() { done <- ct.c.Run(o.ctx, &recordingSink{}) }()
	// Shorter than tokenFetchTimeout, after which a fetch that has had no answer fails and Run would serve anyway.
	testutil.Eventually(t, "Run takes activities while the token is fetched", func() bool { return ct.c.recv.live.Load() != nil },
		testutil.Within(tokenFetchTimeout/3))
	post := func() int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat/teams/"+testConn, bytes.NewReader(fixture(t, "message_plain")))
		req.Header.Set("Authorization", bearer(t, ct.key, claimsAt(time.Now())))
		rec := httptest.NewRecorder()
		ct.c.recv.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(); code != http.StatusOK {
		t.Errorf("an activity while the token is fetched: %d, want 200", code)
	}
	if got := o.value(t, "agentkourier_teams_requests_total", requestAccepted); got != 1 {
		t.Errorf("requests accepted = %v, want 1: the request is counted on Run's instruments", got)
	}
	unblock()
	err := <-done
	if v := ct.c.Classify(chat.OpOther, err); v.Class != chat.FatalAuth {
		t.Fatalf("Run with a refused secret: %v (%+v), want FatalAuth", err, v)
	}
	if ct.c.recv.live.Load() != nil {
		t.Error("the receiver still takes activities after Run returned")
	}
	if code := post(); code != http.StatusServiceUnavailable {
		t.Errorf("an activity after Run returned: %d, want 503", code)
	}
}

// What Teams does not do in this release is said with the contract's errors, and nothing is called.
func TestTheSeamsTeamsDoesNotFillYet(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ctx, c := t.Context(), ct.c
	target := chat.Target{Conn: testConn, Channel: testChannel, Thread: "1700000000001"}
	ref := c.RefAt(testChannel, "1700000000001", "1700000000002")
	caps := c.Capabilities()
	if caps.Streams || caps.Steps || caps.Ephemeral || caps.Busy || caps.AckReaction != "" {
		t.Errorf("caps = %+v", caps)
	}
	if err := c.React(ctx, ref, "eyes"); !errors.Is(err, chat.ErrNoReactions) {
		t.Errorf("React: %v", err)
	}
	if err := c.Ephemeral(ctx, target, testUser, chat.Message{Fallback: "x"}); !errors.Is(err, chat.ErrNoEphemeral) {
		t.Errorf("Ephemeral: %v", err)
	}
	if _, err := c.Stream(ctx, target, testUser, "x"); !errors.Is(err, chat.ErrNoStreams) {
		t.Errorf("Stream: %v", err)
	}
	if err := c.StreamAt(ref).Stop(ctx, "", chat.Message{}, "tag"); !errors.Is(err, chat.ErrNotStreaming) {
		t.Errorf("StreamAt: %v", err)
	}
	if _, found, err := c.Find(ctx, target, "tag", time.Time{}); found || err != nil {
		t.Errorf("Find: %t, %v", found, err)
	}
	if member, err := c.IsMember(ctx, testChannel, testUser); member || err == nil {
		t.Errorf("IsMember: %t, %v; want false and an error", member, err)
	}
	if bot, err := c.IsBot(ctx, testUser); !bot || err == nil {
		t.Errorf("IsBot: %t, %v; want true and an error", bot, err)
	}
	if u, err := c.ResolveUser(ctx, testUser); err != nil || u != (chat.User{ID: testUser}) {
		t.Errorf("ResolveUser: %+v, %v", u, err)
	}
	if in, err := c.IsInAnyGroup(ctx, []string{"S0X"}, testUser); in || !errors.Is(err, chat.ErrGroupsMisconfigured) {
		t.Errorf("IsInAnyGroup: %t, %v", in, err)
	}
	if _, _, err := c.ThreadBetween(ctx, target, "", "", 10); !errors.Is(err, chat.ErrNoThreadReads) {
		t.Errorf("ThreadBetween: %v", err)
	}
	if err := c.MarkBusy(ctx, target, true); !errors.Is(err, chat.ErrNoBusy) {
		t.Errorf("MarkBusy: %v", err)
	}
	if n := len(ct.f.take()); n != 0 {
		t.Errorf("%d calls were made", n)
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	t.Parallel()
	full := Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret)}
	if _, err := New(full); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"no secret":     func(c *Config) { c.ClientSecret = secret.Value{} },
		"no connection": func(c *Config) { c.Connection = "" },
		"no tenant":     func(c *Config) { c.TenantID = "" },
		"no app":        func(c *Config) { c.AppID = "" },
	} {
		cfg := full
		mod(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

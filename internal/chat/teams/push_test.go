package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// testGraphChannel is a second bound channel of the test team.
const testGraphChannel = "19:KourierStandInChannelForTestsOnly0000000000c@thread.tacv2"

// fakeSubs is the app's Microsoft Graph subscriptions, as the fake Graph serves them: a list (paged at pageAt, with a $skiptoken), a
// create, a renew and a delete, each create and renew making the validation handshake to both URLs of the subscription first, and a
// second create of the same resource and change types refused with 409, as Microsoft documents it. The fields are the fakeConnector's,
// guarded by its mu.
type fakeSubs struct {
	byID      map[string]map[string]any
	n         int
	client    *http.Client   // reaches the notification URL for the handshake
	expiresIn time.Duration  // if set, the expiry a create or a renew sets, from now, in place of the one asked for
	hidden    map[string]int // a subscription left out of this many listings
	pageAt    int            // the most a page of the listing holds, 0 for all
	shook     []string       // each URL that answered a handshake as Graph wants
	bodies    []map[string]any
}

// serveSubscriptions answers a call to /subscriptions or /subscriptions/{id}.
func (f *fakeConnector) serveSubscriptions(w http.ResponseWriter, r *http.Request, path string) {
	id := strings.TrimPrefix(strings.TrimPrefix(path, "/subscriptions"), "/")
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	fail := func(status int, code string) {
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q}}`, code)
	}
	f.mu.Lock()
	subs := &f.graph.subs
	sub, known := subs.byID[id]
	if body != nil {
		subs.bodies = append(subs.bodies, body)
	}
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && id == "":
		f.mu.Lock()
		var all []map[string]any
		for _, k := range slices.Sorted(maps.Keys(subs.byID)) {
			if subs.hidden[k] > 0 {
				subs.hidden[k]--
				continue
			}
			listed := maps.Clone(subs.byID[k])
			listed["clientState"] = nil // a listing gives none (measured)
			all = append(all, listed)
		}
		pageAt := subs.pageAt
		f.mu.Unlock()
		skip := 0
		_, _ = fmt.Sscan(r.URL.Query().Get("$skiptoken"), &skip)
		size := len(all) - skip
		if pageAt > 0 {
			size = min(size, pageAt)
		}
		page := map[string]any{"value": all[skip : skip+size]}
		if skip+size < len(all) {
			page["@odata.nextLink"] = fmt.Sprintf("https://%s%s?$skiptoken=%d", r.Host, r.URL.Path, skip+size)
		}
		_ = json.NewEncoder(w).Encode(page)
	case r.Method == http.MethodPost && id == "":
		f.mu.Lock()
		for _, other := range subs.byID {
			if other["resource"] == body["resource"] && other["changeType"] == body["changeType"] {
				f.mu.Unlock()
				fail(http.StatusConflict, "ExtensionError")
				return
			}
		}
		f.mu.Unlock()
		if !f.handshake(r.Context(), str(body["notificationUrl"])) || !f.handshake(r.Context(), str(body["lifecycleNotificationUrl"])) {
			fail(http.StatusBadRequest, "ValidationError")
			return
		}
		f.mu.Lock()
		subs.n++
		made := map[string]any{"id": fmt.Sprintf("sub-%03d", subs.n)}
		for _, k := range []string{"resource", "changeType", "notificationUrl", "lifecycleNotificationUrl", "clientState", "expirationDateTime",
			"includeResourceData"} {
			made[k] = body[k]
		}
		if subs.expiresIn > 0 {
			made["expirationDateTime"] = graphTime(time.Now().Add(subs.expiresIn))
		}
		subs.byID[made["id"].(string)] = made
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(made)
	case r.Method == http.MethodPatch && known:
		if !f.handshake(r.Context(), str(sub["notificationUrl"])) || !f.handshake(r.Context(), str(sub["lifecycleNotificationUrl"])) {
			fail(http.StatusBadRequest, "ValidationError")
			return
		}
		f.mu.Lock()
		sub["expirationDateTime"] = body["expirationDateTime"]
		if subs.expiresIn > 0 {
			sub["expirationDateTime"] = graphTime(time.Now().Add(subs.expiresIn))
		}
		answer := maps.Clone(sub)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(answer)
	case r.Method == http.MethodDelete && known:
		f.mu.Lock()
		delete(subs.byID, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(http.StatusNotFound, "ResourceNotFound")
	}
}

// handshake makes Graph's validation handshake to link, as measured: a POST with the token in the query, an empty text/plain body and no
// Authorization; the answer must be 200, text/plain, and the token decoded.
func (f *fakeConnector) handshake(ctx context.Context, link string) bool {
	f.mu.Lock()
	client := f.graph.subs.client
	n := f.graph.subs.n
	f.mu.Unlock()
	if client == nil || link == "" {
		return false
	}
	token := fmt.Sprintf("Validation: Testing client application reachability for subscription Request-Id: %d+a/b", n)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, link+"?validationToken="+url.QueryEscape(token), http.NoBody)
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || string(got) != token {
		return false
	}
	f.mu.Lock()
	f.graph.subs.shook = append(f.graph.subs.shook, link)
	f.mu.Unlock()
	return true
}

// putSub keeps a subscription of the app, as one made before the test.
func (f *fakeConnector) putSub(id, channel, notify, lifecycle string, expires time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.graph.subs.byID[id] = map[string]any{"id": id, "resource": "/teams/" + testGroup + "/channels/" + channel + "/messages",
		"changeType": subscriptionChangeTypes, "notificationUrl": notify, "lifecycleNotificationUrl": lifecycle,
		"expirationDateTime": graphTime(expires), "clientState": "from-another-run"}
}

// subsTo returns the app's subscriptions to link, by channel, each as Graph holds it.
func (f *fakeConnector) subsTo(link string) map[string]map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]map[string]any{}
	for _, s := range f.graph.subs.byID {
		if s["notificationUrl"] == link {
			_, ch, _ := parseSubscriptionResource(str(s["resource"]))
			out[ch] = maps.Clone(s)
		}
	}
	return out
}

// subCalls returns, and forgets, the Graph calls to the subscriptions since the last, each "METHOD path".
func (f *fakeConnector) subCalls() []string {
	var out []string
	for _, r := range f.graphTaken() {
		if strings.HasPrefix(r.path, "/subscriptions") {
			out = append(out, r.path)
		}
	}
	return out
}

// bindingSink is a recordingSink that names its bound channels (chat.ChannelLister) and the bots it takes (chat.BotFilter and
// chat.BotMentionFilter), as the broker's intake does.
type bindingSink struct {
	recordingSink
	bmu      sync.Mutex
	channels []string
	bots     map[string]bool // channel + "\x00" + bot
}

func (s *bindingSink) BoundChannels() []string {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	return slices.Clone(s.channels)
}

func (s *bindingSink) AcceptsBot(channel, bot string) bool {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	return s.bots[channel+"\x00"+bot]
}

func (s *bindingSink) AcceptsBotMention(channel, bot string) bool { return s.AcceptsBot(channel, bot) }

func (s *bindingSink) bind(channels ...string) {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	s.channels = slices.Sorted(slices.Values(channels))
}

func (s *bindingSink) takeBot(channel, bot string) {
	s.bmu.Lock()
	defer s.bmu.Unlock()
	if s.bots == nil {
		s.bots = map[string]bool{}
	}
	s.bots[channel+"\x00"+bot] = true
}

// pushTest is a Client with a notificationURL, its route served on a plain HTTP server that the fake Graph's handshake reaches, the
// fake Microsoft, and a sink that binds channels.
type pushTest struct {
	*clientTest
	route  *httptest.Server
	url    string // the notificationURL
	sink   *bindingSink
	obs    observed
	state  string
	logs   *syncBuffer
	missed chan string
	cancel context.CancelFunc
	done   chan error
}

func newPushTest(t *testing.T, socket bool) *pushTest {
	t.Helper()
	f := newFakeConnector(t)
	key := newSigningKey(t, "k", "msteams")
	bf := newFakeBotFramework(t, key)
	convs := NewMemoryConversations()
	var handler atomic.Pointer[http.Handler]
	route := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*handler.Load()).ServeHTTP(w, r) }))
	t.Cleanup(route.Close)
	pt := &pushTest{route: route, url: route.URL + "/chat/teams/" + testConn, sink: &bindingSink{}, obs: newObserved(t),
		logs: &syncBuffer{}, missed: make(chan string, 8)}
	c, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret),
		TokenURL: f.srv.URL + "/token", MetadataURL: bf.metadataURL(), GraphURL: f.graphURL(), HTTPClient: f.srv.Client(),
		Conversations: convs, Socket: socket, NotificationURL: pt.url,
		OnNotificationsMissed: func(ch string) { pt.missed <- ch },
		Logger:                slog.New(slog.NewTextHandler(pt.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	c.push.fetchRetry, c.push.fetchRetryMax = time.Millisecond, 5*time.Millisecond
	c.push.subs.every = 5 * time.Millisecond
	c.recv.sinkRetryBase = time.Millisecond
	h, ok := c.HTTPHandler()
	if !ok {
		t.Fatal("a Client with a notificationURL offers no handler")
	}
	handler.Store(&h)
	f.setGraph(func(g *fakeGraph) { g.subs.client = route.Client() })
	pt.clientTest = &clientTest{c: c, f: f, convs: convs, key: key}
	if pt.state, err = clientStateOf(testClientSecret, testConn); err != nil {
		t.Fatal(err)
	}
	return pt
}

// run runs the Client on the observed context until the test ends.
func (pt *pushTest) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(pt.obs.ctx)
	pt.cancel, pt.done = cancel, make(chan error, 1)
	go func() { pt.done <- pt.c.Run(ctx, pt.sink) }()
	t.Cleanup(pt.stop)
}

// stop ends the run and waits for it.
func (pt *pushTest) stop() {
	if pt.cancel != nil {
		pt.cancel()
		_ = <-pt.done
		pt.cancel = nil
	}
}

// settle waits until a reconcile that began after the call has ended.
func (pt *pushTest) settle(t *testing.T) {
	t.Helper()
	n := pt.c.push.subs.rounds.Load()
	testutil.Eventually(t, "a reconcile after this point", func() bool { return pt.c.push.subs.rounds.Load() >= n+2 })
}

// subOf is the ID of the subscription to the connection's URL of channel, and "" if there is none.
func (pt *pushTest) subOf(channel string) string {
	return str(pt.f.subsTo(pt.url)[channel]["id"])
}

// resourceOf is the resource of a notification of the message id of channel, a reply to root unless root is "", as Microsoft documents it.
func resourceOf(channel, root, id string) string {
	if root == "" {
		return fmt.Sprintf("teams('%s')/channels('%s')/messages('%s')", testGroup, channel, id)
	}
	return fmt.Sprintf("teams('%s')/channels('%s')/messages('%s')/replies('%s')", testGroup, channel, root, id)
}

// change is a notification of the subscription sub of a change of the message id of channel (a reply to root unless root is "").
func (pt *pushTest) change(sub, channel, changeType, root, id string) map[string]any {
	return map[string]any{"subscriptionId": sub, "subscriptionExpirationDateTime": graphTime(time.Now().Add(time.Hour)),
		"changeType": changeType, "resource": resourceOf(channel, root, id), "clientState": pt.state, "tenantId": testTenant,
		"resourceData": map[string]any{"id": id, "@odata.type": "#Microsoft.Graph.chatMessage", "@odata.id": resourceOf(channel, root, id)}}
}

// lifecycle is a lifecycle notification of the subscription sub.
func (pt *pushTest) lifecycle(sub, event string) map[string]any {
	return map[string]any{"subscriptionId": sub, "subscriptionExpirationDateTime": graphTime(time.Now().Add(time.Hour)),
		"lifecycleEvent": event, "resource": "/teams/" + testGroup + "/channels/" + testChannel + "/messages", "clientState": pt.state,
		"tenantId": testTenant}
}

// notify sends Graph's body of the notifications items to the route, and returns the status and body of the answer.
func (pt *pushTest) notify(t *testing.T, items ...map[string]any) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"value": items})
	if err != nil {
		t.Fatal(err)
	}
	return pt.post(t, "", body)
}

// post sends body to the route, with the query query, and returns the status and body of the answer.
func (pt *pushTest) post(t *testing.T, query string, body []byte) (int, string) {
	t.Helper()
	link := pt.url
	if query != "" {
		link += "?" + query
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, link, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := pt.route.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(got)
}

// counted is agentkourier_teams_graph_notifications_total of kind and outcome.
func (pt *pushTest) counted(t *testing.T, kind, outcome string) float64 {
	t.Helper()
	return pt.obs.series(t, "agentkourier_teams_graph_notifications_total", kind, outcome)
}

// subscribed runs pt with channels bound and recorded, and waits until each has its subscription.
func (pt *pushTest) subscribed(t *testing.T, channels ...string) {
	t.Helper()
	for _, ch := range channels {
		pt.record(t, ch, testChannel)
	}
	pt.sink.bind(channels...)
	pt.run(t)
	testutil.Eventually(t, "a subscription of each bound channel", func() bool {
		for _, ch := range channels {
			if _, held := pt.c.push.subs.snapshot(ch); !held {
				return false
			}
		}
		return true
	})
	pt.f.subCalls()
}

// The workflows' card of an alert, as Graph gives it: the JSON of an Adaptive Card in a string.
const alertCard = `{"type":"AdaptiveCard","version":"1.4","body":[{"type":"TextBlock","text":"[FIRING:1] KubePodCrashLooping","weight":"Bolder"},` +
	`{"type":"FactSet","facts":[{"title":"severity","value":"critical"},{"title":"namespace","value":"payments"}]},` +
	`{"type":"ActionSet","actions":[{"type":"Action.OpenUrl","title":"Runbook","url":"https://runbooks.example.test/crashloop"}]}]}`

// Each bound channel gets one subscription, made as the adapter documents it, after Graph's validation handshake to the notification URL
// and to the lifecycle URL, which are the same; a reconcile with nothing changed calls Graph for nothing.
func TestGraphSubscribesEachBoundChannelOnce(t *testing.T) {
	pt := newPushTest(t, false)
	pt.record(t, testChannel, testChannel)
	pt.record(t, testGraphChannel, testChannel)
	pt.sink.bind(testChannel, testGraphChannel)
	asked := time.Now()
	pt.run(t)
	testutil.Eventually(t, "two subscriptions", func() bool { return len(pt.f.subsTo(pt.url)) == 2 })
	pt.settle(t)
	calls := pt.f.subCalls()
	if posts := countOf(calls, "/subscriptions"); posts != 3 { // one listing, two creates
		t.Errorf("calls to /subscriptions = %v; want one listing and two creates", calls)
	}
	for _, ch := range []string{testChannel, testGraphChannel} {
		s := pt.f.subsTo(pt.url)[ch]
		want := map[string]any{"changeType": "created,updated", "notificationUrl": pt.url, "lifecycleNotificationUrl": pt.url,
			"resource": "/teams/" + testGroup + "/channels/" + ch + "/messages", "clientState": pt.state, "includeResourceData": false}
		for k, v := range want {
			if s[k] != v {
				t.Errorf("%s: subscription's %s = %v; want %v", ch, k, s[k], v)
			}
		}
		exp, err := time.Parse(time.RFC3339Nano, str(s["expirationDateTime"]))
		if err != nil || exp.Before(asked.Add(subscriptionLifetime-time.Minute)) || exp.After(time.Now().Add(subscriptionLifetime)) {
			t.Errorf("%s: subscription expires %v (%v); want %v from now", ch, s["expirationDateTime"], err, subscriptionLifetime)
		}
	}
	pt.f.mu.Lock()
	shook := slices.Clone(pt.f.graph.subs.shook)
	pt.f.mu.Unlock()
	if len(shook) != 4 {
		t.Errorf("handshakes answered = %d; want 4, to the notification and the lifecycle URL of each create", len(shook))
	}
	if got := pt.counted(t, notifyValidation, notifiedAnswered); got != 4 {
		t.Errorf("notifications{validation,answered} = %v; want 4", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelSubscribed); got != 2 {
		t.Errorf("channels{subscribed} = %v; want 2", got)
	}
	if got := pt.obs.series(t, "agentkourier_chat_api_calls_total", "Subscribe", "success"); got != 2 {
		t.Errorf("chat_api_calls_total{Subscribe,success} = %v; want 2", got)
	}
}

// countOf is how many of calls are path.
func countOf(calls []string, path string) int {
	n := 0
	for _, c := range calls {
		if c == path {
			n++
		}
	}
	return n
}

// At the start of a run the app's subscriptions are listed, in pages, and those to the connection's URL that can be kept are adopted:
// one near its expiry is renewed, and the rest to that URL (of a channel no Binding names, a second of a channel, one with another
// lifecycle URL, one that expired) are deleted, and made anew for a bound channel. One to another URL is left as it is.
func TestGraphAdoptsTheSubscriptionsOfTheConnectionsURLAtStart(t *testing.T) {
	pt := newPushTest(t, false)
	const unbound = "19:KourierStandInChannelForTestsOnly0000000000d@thread.tacv2"
	const fourth = "19:KourierStandInChannelForTestsOnly0000000000e@thread.tacv2"
	other := "https://another.example.test/chat/teams/ns/other"
	now := time.Now()
	pt.f.setGraph(func(g *fakeGraph) { g.subs.pageAt = 2 })
	pt.f.putSub("sub-a-kept", testChannel, pt.url, pt.url, now.Add(20*time.Hour))
	pt.f.putSub("sub-b-renewed", testGraphChannel, pt.url, pt.url, now.Add(time.Hour))
	pt.f.putSub("sub-c-unbound", unbound, pt.url, pt.url, now.Add(20*time.Hour))
	pt.f.putSub("sub-d-other", testChannel, other, other, now.Add(20*time.Hour))
	pt.f.putSub("sub-e-second", testChannel, pt.url, pt.url, now.Add(20*time.Hour))
	pt.f.putSub("sub-f-lifecycle", fourth, pt.url, other, now.Add(20*time.Hour))
	pt.f.putSub("sub-g-expired", testGraphChannel, pt.url, pt.url, now.Add(-time.Minute))
	// sub-d-other has testChannel's resource, which the fake's 409 would refuse a create of: it is another URL's, and adoption leaves it.
	for _, ch := range []string{testChannel, testGraphChannel, fourth} {
		pt.record(t, ch, testChannel)
	}
	pt.sink.bind(testChannel, testGraphChannel, fourth)
	pt.run(t)
	testutil.Eventually(t, "the fourth channel's subscription", func() bool {
		_, held := pt.c.push.subs.snapshot(fourth)
		return held
	})
	pt.settle(t)
	calls := pt.f.subCalls()
	for _, want := range []string{"/subscriptions/sub-b-renewed", "/subscriptions/sub-c-unbound", "/subscriptions/sub-e-second",
		"/subscriptions/sub-f-lifecycle", "/subscriptions/sub-g-expired"} {
		if countOf(calls, want) != 1 {
			t.Errorf("calls = %v; want one to %s", calls, want)
		}
	}
	for _, untouched := range []string{"/subscriptions/sub-a-kept", "/subscriptions/sub-d-other"} {
		if countOf(calls, untouched) != 0 {
			t.Errorf("calls = %v; want none to %s", calls, untouched)
		}
	}
	held := map[string]string{}
	for _, ch := range []string{testChannel, testGraphChannel, fourth} {
		s, _ := pt.c.push.subs.snapshot(ch)
		held[ch] = s.id
	}
	if held[testChannel] != "sub-a-kept" || held[testGraphChannel] != "sub-b-renewed" || !strings.HasPrefix(held[fourth], "sub-0") {
		t.Errorf("held = %v; want sub-a-kept, sub-b-renewed, and a new one for the fourth channel", held)
	}
	if s, _ := pt.c.push.subs.snapshot(testGraphChannel); s.expires.Before(now.Add(subscriptionLifetime - time.Minute)) {
		t.Errorf("the renewed subscription expires %v; want %v from now", s.expires, subscriptionLifetime)
	}
	if _, ok := pt.f.subsTo(other)[testChannel]; !ok {
		t.Error("the subscription to another URL was deleted; want it left as it is")
	}
}

// A Binding that comes or goes changes the subscriptions at the next reconcile: a channel no Binding names any longer has its subscription
// deleted, a newly bound one gets one, and one that stays bound is not called for.
func TestGraphReconcilesTheSubscriptionsWithTheBindings(t *testing.T) {
	pt := newPushTest(t, false)
	const third = "19:KourierStandInChannelForTestsOnly0000000000d@thread.tacv2"
	pt.record(t, third, testChannel)
	pt.subscribed(t, testChannel, testGraphChannel)
	first, kept := pt.subOf(testChannel), pt.subOf(testGraphChannel)
	pt.sink.bind(testGraphChannel, third)
	testutil.Eventually(t, "the third channel's subscription", func() bool { return pt.subOf(third) != "" })
	pt.settle(t)
	calls := pt.f.subCalls()
	if countOf(calls, "/subscriptions/"+first) != 1 || countOf(calls, "/subscriptions/"+kept) != 0 || countOf(calls, "/subscriptions") != 1 {
		t.Errorf("calls = %v; want the unbound channel's deleted, one create, and nothing for the channel still bound", calls)
	}
	if pt.subOf(testChannel) != "" {
		t.Error("the unbound channel's subscription is still in Graph")
	}
	pt.sink.bind()
	testutil.Eventually(t, "no subscription", func() bool { return len(pt.f.subsTo(pt.url)) == 0 })
	pt.settle(t)
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelSubscribed); got != 0 {
		t.Errorf("channels{subscribed} = %v with nothing bound; want 0", got)
	}
}

// A subscription is renewed once it expires within subscriptionRenewLead, or within half the lifetime Graph granted when that is shorter,
// to subscriptionLifetime from the renew, after the handshake: one that Graph made shorter than asked is not renewed at every reconcile,
// and one with the full lifetime is left until it nears its expiry.
func TestGraphRenewsASubscriptionBeforeItExpires(t *testing.T) {
	pt := newPushTest(t, false)
	clock := &fakeClock{t: time.Now()}
	pt.c.push.subs.now = clock.now
	pt.f.setGraph(func(g *fakeGraph) { g.subs.expiresIn = time.Hour }) // Graph made it shorter than asked
	pt.subscribed(t, testChannel)
	sub := pt.subOf(testChannel)
	pt.settle(t)
	pt.settle(t)
	if calls := pt.f.subCalls(); len(calls) != 0 {
		t.Errorf("calls with %v of the hour Graph granted left = %v; want none before half of it has passed", time.Hour, calls)
	}
	pt.f.setGraph(func(g *fakeGraph) { g.subs.expiresIn = 0 })
	clock.advance(31 * time.Minute)
	testutil.Eventually(t, "a renew to the full lifetime", func() bool {
		s, _ := pt.c.push.subs.snapshot(testChannel)
		return s.expires.After(clock.now().Add(subscriptionRenewLead))
	})
	if calls := pt.f.subCalls(); countOf(calls, "/subscriptions/"+sub) != 1 {
		t.Errorf("calls once half the granted hour passed = %v; want one renew", calls)
	}
	pt.f.mu.Lock()
	last := pt.f.graph.subs.bodies[len(pt.f.graph.subs.bodies)-1]
	pt.f.mu.Unlock()
	if exp, err := time.Parse(time.RFC3339Nano, str(last["expirationDateTime"])); err != nil || len(last) != 1 ||
		exp.Before(clock.now().Add(subscriptionLifetime-time.Minute)) {
		t.Errorf("the renew's body = %v; want only an expirationDateTime %v from now", last, subscriptionLifetime)
	}
	clock.advance(subscriptionLifetime - subscriptionRenewLead - time.Minute)
	pt.settle(t)
	if calls := pt.f.subCalls(); len(calls) != 0 {
		t.Errorf("calls with more than %v left = %v; want none", subscriptionRenewLead, calls)
	}
	clock.advance(2 * time.Minute)
	testutil.Eventually(t, "a renew within the lead", func() bool { return countOf(pt.f.subCalls(), "/subscriptions/"+sub) == 1 })
}

// A bound channel whose team's Microsoft Entra group is not known yet waits, told once at WARN with the fix, and is subscribed at the
// reconcile after an activity of the channel recorded the group.
func TestGraphChannelWaitsForItsTeamsGroup(t *testing.T) {
	pt := newPushTest(t, false)
	pt.sink.bind(testChannel)
	pt.run(t)
	pt.settle(t)
	pt.settle(t)
	if calls := pt.f.subCalls(); countOf(calls, "/subscriptions") != 1 {
		t.Errorf("calls = %v; want the listing alone", calls)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelWaitingGroup); got != 1 {
		t.Errorf("channels{waiting_group} = %v; want 1", got)
	}
	if n := strings.Count(pt.logs.String(), "Install the app in the channel's team"); n != 1 {
		t.Errorf("the WARN with the fix was written %d times; want once:\n%s", n, pt.logs.String())
	}
	pt.c.recv.remember(t.Context(), Conversation{Channel: testChannel, TeamID: testChannel, AADGroupID: testGroup, TenantID: testTenant})
	testutil.Eventually(t, "the subscription", func() bool { return pt.subOf(testChannel) != "" })
	pt.settle(t)
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelWaitingGroup); got != 0 {
		t.Errorf("channels{waiting_group} = %v once subscribed; want 0", got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelSubscribed); got != 1 {
		t.Errorf("channels{subscribed} = %v; want 1", got)
	}
}

// A create that Graph refuses leaves the channel failing, told once at WARN and counted by Classify's outcome, and tried again at each
// reconcile until it is made.
func TestGraphSubscriptionThatFailsIsTriedAgain(t *testing.T) {
	for _, tc := range []struct {
		status  int
		outcome string
	}{
		{http.StatusForbidden, "api_error"},
		{http.StatusUnauthorized, "api_error"},
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusServiceUnavailable, "api_error"},
		{http.StatusInternalServerError, "api_error"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			pt := newPushTest(t, false)
			var failing atomic.Bool
			failing.Store(true)
			pt.f.setGraph(func(g *fakeGraph) {
				g.fail = func(r *http.Request) (int, string, bool) {
					return tc.status, `{"error":{"code":"Failed"}}`, failing.Load() && r.Method == http.MethodPost
				}
			})
			pt.record(t, testChannel, testChannel)
			pt.sink.bind(testChannel)
			pt.run(t)
			testutil.Eventually(t, "the channel failing", func() bool {
				return pt.obs.series(t, "agentkourier_teams_graph_channels", channelFailing) == 1
			})
			pt.settle(t)
			if got := pt.obs.series(t, "agentkourier_chat_api_calls_total", "Subscribe", tc.outcome); got < 2 {
				t.Errorf("chat_api_calls_total{Subscribe,%s} = %v; want a try at each reconcile", tc.outcome, got)
			}
			if n := strings.Count(pt.logs.String(), "level=WARN msg=\"teams: a bound channel's Microsoft Graph subscription could not be made;"); n != 1 {
				t.Errorf("the WARN was written %d times; want once", n)
			}
			failing.Store(false)
			testutil.Eventually(t, "the subscription", func() bool { return pt.subOf(testChannel) != "" })
			pt.settle(t)
			if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelFailing); got != 0 {
				t.Errorf("channels{failing} = %v once made; want 0", got)
			}
		})
	}
}

// A create refused with 409, for a subscription of the app to the connection's URL that the listing did not show, lists again and adopts it.
func TestGraphConflictListsAgainAndAdopts(t *testing.T) {
	pt := newPushTest(t, false)
	pt.f.putSub("sub-hidden", testChannel, pt.url, pt.url, time.Now().Add(20*time.Hour))
	pt.f.setGraph(func(g *fakeGraph) { g.subs.hidden = map[string]int{"sub-hidden": 1} })
	pt.record(t, testChannel, testChannel)
	pt.sink.bind(testChannel)
	pt.run(t)
	testutil.Eventually(t, "the hidden subscription adopted", func() bool {
		s, ok := pt.c.push.subs.snapshot(testChannel)
		return ok && s.id == "sub-hidden"
	})
	if subs := pt.f.subsTo(pt.url); len(subs) != 1 {
		t.Errorf("subscriptions = %v; want the one", subs)
	}
}

// Graph's validation handshake is answered with the token decoded, as text/plain, whether or not the connection runs: Graph makes it to
// the notification URL and to the lifecycle URL within each create and renew.
func TestGraphHandshakeIsAnsweredWithTheDecodedToken(t *testing.T) {
	for _, socket := range []bool{false, true} {
		pt := newPushTest(t, socket)
		status, body := pt.post(t, "validationToken=Validation%3a+Testing+client%2B1+Request-Id%3a+a%2Fb", nil)
		if status != http.StatusOK || body != "Validation: Testing client+1 Request-Id: a/b" {
			t.Errorf("socket %v: handshake = %d %q; want 200 and the token decoded", socket, status, body)
		}
	}
}

// A connection that does not run, as on a replica that does not lead, subscribes to nothing, and answers a notification 503, for Graph to
// send it again to the leader.
func TestGraphNotificationsWaitForTheRun(t *testing.T) {
	pt := newPushTest(t, false)
	pt.record(t, testChannel, testChannel)
	pt.sink.bind(testChannel)
	if status, _ := pt.notify(t, pt.change("sub-001", testChannel, "created", "", "1794971777722")); status != http.StatusServiceUnavailable {
		t.Errorf("a notification with no run = %d; want 503", status)
	}
	if calls := pt.f.subCalls(); len(calls) != 0 {
		t.Errorf("calls with no run = %v; want none", calls)
	}
}

// A sink that names no bound channel, as one that is not the broker's intake, has nothing subscribed: the app's subscriptions are listed
// once, and those to the connection's URL, which nothing reads, are deleted; one to another URL is left as it is.
func TestGraphSinkThatNamesNoChannelHasNoneSubscribed(t *testing.T) {
	pt := newPushTest(t, false)
	other := "https://another.example.test/chat/teams/ns/other"
	pt.f.putSub("sub-stale", testChannel, pt.url, pt.url, time.Now().Add(20*time.Hour))
	pt.f.putSub("sub-other", testGraphChannel, other, other, time.Now().Add(20*time.Hour))
	pt.record(t, testChannel, testChannel)
	ctx, cancel := context.WithCancel(pt.obs.ctx)
	done := make(chan error, 1)
	go func() { done <- pt.c.Run(ctx, &recordingSink{}) }()
	testutil.Eventually(t, "reconciles", func() bool { return pt.c.push.subs.rounds.Load() >= 3 })
	cancel()
	_ = <-done
	if calls := pt.f.subCalls(); countOf(calls, "/subscriptions") != 1 || countOf(calls, "/subscriptions/sub-stale") != 1 || len(calls) != 2 {
		t.Errorf("calls = %v; want one listing and the stale subscription deleted", calls)
	}
	if subs := pt.f.subsTo(pt.url); len(subs) != 0 {
		t.Errorf("subscriptions to the connection's URL = %v; want none", subs)
	}
	if _, ok := pt.f.subsTo(other)[testGraphChannel]; !ok {
		t.Error("the subscription to another URL was deleted; want it left as it is")
	}
}

// A notification whose clientState is not the connection's is dropped before anything is read, counted, and the subscription it names is
// made anew, with the connection's clientState.
func TestGraphNotificationWithAnotherClientStateIsDropped(t *testing.T) {
	pt := newPushTest(t, false)
	pt.subscribed(t, testChannel)
	sub := pt.subOf(testChannel)
	pt.f.putMessage(testChannel, person("1794971777722", "", testUser, "<p>hello</p>"))
	n := pt.change(sub, testChannel, "created", "", "1794971777722")
	n["clientState"] = "not-the-connections-client-state"
	if status, _ := pt.notify(t, n); status != http.StatusAccepted {
		t.Errorf("status = %d; want 202", status)
	}
	testutil.Eventually(t, "the subscription made anew", func() bool { s := pt.subOf(testChannel); return s != "" && s != sub })
	pt.settle(t)
	if got := pt.sink.callsOf("1794971777722"); got != 0 {
		t.Errorf("the sink was given the message %d times; want none", got)
	}
	for _, r := range pt.f.graphTaken() {
		if strings.Contains(r.path, "/messages/") {
			t.Errorf("Graph was read: %s; want nothing read", r.path)
		}
	}
	if got := pt.counted(t, notifyChange, notifiedClientState); got != 1 {
		t.Errorf("notifications{change,client_state} = %v; want 1", got)
	}
}

// Lifecycle notifications: reauthorizationRequired renews the subscription, subscriptionRemoved makes it anew, and missed is told to
// OnNotificationsMissed with the channel; each is counted.
func TestGraphLifecycleNotifications(t *testing.T) {
	for _, tc := range []struct {
		event, outcome string
		check          func(t *testing.T, pt *pushTest, sub string)
	}{
		{"reauthorizationRequired", notifiedReauthorize, func(t *testing.T, pt *pushTest, sub string) {
			testutil.Eventually(t, "a renew", func() bool { return slices.Contains(pt.f.subCalls(), "/subscriptions/"+sub) })
		}},
		{"subscriptionRemoved", notifiedRemoved, func(t *testing.T, pt *pushTest, sub string) {
			testutil.Eventually(t, "the subscription made anew", func() bool { s := pt.subOf(testChannel); return s != "" && s != sub })
		}},
		{"missed", notifiedMissed, func(t *testing.T, pt *pushTest, _ string) {
			select {
			case ch := <-pt.missed:
				if ch != testChannel {
					t.Errorf("missed told of %q; want the channel", ch)
				}
			case <-time.After(5 * time.Second):
				t.Error("missed was not told")
			}
		}},
		{"somethingNew", notifiedOtherLifecycle, func(*testing.T, *pushTest, string) {}},
	} {
		t.Run(tc.event, func(t *testing.T) {
			pt := newPushTest(t, false)
			pt.subscribed(t, testChannel)
			sub := pt.subOf(testChannel)
			if status, _ := pt.notify(t, pt.lifecycle(sub, tc.event)); status != http.StatusAccepted {
				t.Errorf("status = %d; want 202", status)
			}
			tc.check(t, pt, sub)
			if got := pt.counted(t, notifyLifecycle, tc.outcome); got != 1 {
				t.Errorf("notifications{lifecycle,%s} = %v; want 1", tc.outcome, got)
			}
		})
	}
}

// A `created` is read from Graph and handed to the sink as the adapter decodes a thread's message: an alert's card that another app
// (Workflows) posted is a bot's message whose sender is the app and whose body is the card's text, if a trigger of the channel names that
// bot; a person's post is the person's; a reply has its root as its thread. The bot's own post, a system event, an edit and a deletion,
// a message gone before it was read, and what names another channel, another tenant or another subscription, are dropped, and each is
// counted.
func TestGraphCreatedPostsReachTheSink(t *testing.T) {
	pt := newPushTest(t, false)
	pt.subscribed(t, testChannel)
	sub := pt.subOf(testChannel)
	system := message("1794971777705", "", nil, "<systemEventMessage/>", nil, nil)
	system["messageType"] = "unknownFutureValue"
	for _, tc := range []struct {
		name    string
		msg     map[string]any // put in the channel first, unless nil
		taken   bool           // whether a trigger names the other app
		n       func(sub string) map[string]any
		outcome string
		want    *chat.Inbound // what the sink is given, or nil
		reads   int           // the reads of the message
	}{
		{name: "an alert's card, its bot not named",
			msg: app("1794971777702", "", testWorkflows, `<attachment id="d1f0"></attachment>`, alertCard),
			n:   func(s string) map[string]any { return pt.change(s, testChannel, "created", "", "1794971777702") }, outcome: notifiedNotNamed, reads: 1},
		{name: "an alert's card, its bot named", taken: true,
			msg: app("1794971777701", "", testWorkflows, `<attachment id="d1f0"></attachment>`, alertCard),
			n:   func(s string) map[string]any { return pt.change(s, testChannel, "created", "", "1794971777701") }, outcome: notifiedTaken,
			want: &chat.Inbound{At: chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1794971777701"},
				Bot: &chat.BotMessage{ID: testWorkflows, Kind: v1alpha1.SenderKindBot,
					Body: "[FIRING:1] KubePodCrashLooping\nseverity: critical\nnamespace: payments"}}, reads: 1},
		{name: "a person's post", msg: person("1794971777703", "", testUser, "<p>is the <b>payments</b> API down?</p>"),
			n: func(s string) map[string]any { return pt.change(s, testChannel, "created", "", "1794971777703") }, outcome: notifiedTaken,
			want: &chat.Inbound{At: chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1794971777703"},
				User: testUser, Text: "is the payments API down?"}, reads: 1},
		{name: "a person's reply", msg: person("1794971777704", "1794971777703", testUser, "<p>still down</p>"),
			n: func(s string) map[string]any {
				return pt.change(s, testChannel, "created", "1794971777703", "1794971777704")
			}, outcome: notifiedTaken,
			want: &chat.Inbound{At: chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1794971777704",
				Thread: "1794971777703"}, User: testUser, Text: "still down"}, reads: 1},
		{name: "the bot's own post", msg: app("1794971777706", "", testAppID, "<p>Looking into it.</p>"),
			n: func(s string) map[string]any { return pt.change(s, testChannel, "created", "", "1794971777706") }, outcome: notifiedOwn, reads: 1},
		{name: "a system event", msg: system,
			n: func(s string) map[string]any { return pt.change(s, testChannel, "created", "", "1794971777705") }, outcome: notifiedIgnored, reads: 1},
		{name: "a message gone before it is read",
			n: func(s string) map[string]any { return pt.change(s, testChannel, "created", "", "1794971777707") }, outcome: notifiedGone, reads: 1},
		{name: "an edit", msg: person("1794971777708", "", testUser, "<p>edited</p>"),
			n: func(s string) map[string]any { return pt.change(s, testChannel, "updated", "", "1794971777708") }, outcome: notifiedEdit},
		{name: "a deletion",
			n: func(s string) map[string]any { return pt.change(s, testChannel, "deleted", "", "1794971777709") }, outcome: notifiedEdit},
		{name: "another channel's message",
			n: func(s string) map[string]any { return pt.change(s, testGraphChannel, "created", "", "1794971777710") }, outcome: notifiedMalformed},
		{name: "another tenant", n: func(s string) map[string]any {
			n := pt.change(s, testChannel, "created", "", "1794971777711")
			n["tenantId"] = "99999999-2222-4333-8444-555555555555"
			return n
		}, outcome: notifiedWrongTenant},
		{name: "another subscription",
			n: func(string) map[string]any {
				return pt.change("sub-unknown", testChannel, "created", "", "1794971777712")
			}, outcome: notifiedUnknownSub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pt.graphTaken()
			if tc.msg != nil {
				pt.f.putMessage(testChannel, tc.msg)
			}
			if tc.taken {
				pt.sink.takeBot(testChannel, testWorkflows)
			}
			n := tc.n(sub)
			id := n["resourceData"].(map[string]any)["id"].(string)
			before := pt.counted(t, notifyChange, tc.outcome)
			if status, body := pt.notify(t, n); status != http.StatusAccepted {
				t.Fatalf("status = %d %s; want 202", status, body)
			}
			testutil.Eventually(t, "the outcome "+tc.outcome, func() bool { return pt.counted(t, notifyChange, tc.outcome) == before+1 })
			var got *chat.Inbound
			for _, m := range pt.sink.got() {
				if m.At.ID == id {
					got = &m
				}
			}
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("the sink was given %+v; want nothing", *got)
			case tc.want != nil && got == nil:
				t.Errorf("the sink was given nothing; want %+v", *tc.want)
			case tc.want != nil && !sameInbound(*got, *tc.want):
				t.Errorf("the sink was given %+v (bot %+v); want %+v (bot %+v)", *got, got.Bot, *tc.want, tc.want.Bot)
			}
			reads := 0
			for _, r := range pt.f.graphTaken() {
				if strings.HasSuffix(r.path, "/"+id) {
					reads++
				}
			}
			if reads != tc.reads {
				t.Errorf("reads of the message = %d; want %d", reads, tc.reads)
			}
		})
	}
}

// graphTaken forgets the Graph calls taken so far.
func (pt *pushTest) graphTaken() { pt.f.graphTaken() }

// sameInbound compares what the tests set of a message the sink was given.
func sameInbound(a, b chat.Inbound) bool {
	if a.At != b.At || a.User != b.User || a.Text != b.Text || a.Mentioned != b.Mentioned || (a.Bot == nil) != (b.Bot == nil) {
		return false
	}
	return a.Bot == nil || *a.Bot == *b.Bot
}

// A mention of the bot comes both as an activity and as a Graph notification: it is handed to the sink once, whichever comes first, and the
// activity's record of its channel is kept either way.
func TestGraphAndAnActivityTakeAMentionOnce(t *testing.T) {
	for _, activityFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("activity first %v", activityFirst), func(t *testing.T) {
			pt := newPushTest(t, false)
			pt.subscribed(t, testChannel)
			const id = "1794965322719"
			pt.f.putMessage(testChannel, person(id, "", "00000000-0000-4000-8000-000000000001",
				`<p><at id="0">Agent Kourier</at>&nbsp;echo two-socket test</p>`, mentionOfBot()))
			activity := func() answer {
				ans := pt.c.recv.takePayload(t.Context(), inChannel(t, "message_mention", testChannel, id))
				if ans.status != http.StatusOK {
					t.Fatalf("the activity was answered %d", ans.status)
				}
				return ans
			}
			notification := func() {
				if status, _ := pt.notify(t, pt.change(pt.subOf(testChannel), testChannel, "created", "", id)); status != http.StatusAccepted {
					t.Fatalf("the notification was answered %d", status)
				}
			}
			if activityFirst {
				activity()
				testutil.Eventually(t, "the sink has it", func() bool { return pt.sink.has(id) })
				pt.f.graphTaken()
				notification()
				testutil.Eventually(t, "the notification counted as a duplicate", func() bool {
					return pt.counted(t, notifyChange, notifiedDuplicate) == 1
				})
				for _, r := range pt.f.graphTaken() {
					if strings.HasSuffix(r.path, "/"+id) {
						t.Errorf("the message was read from Graph (%s) after the activity took it; want no read", r.path)
					}
				}
			} else {
				notification()
				testutil.Eventually(t, "the sink has it", func() bool { return pt.sink.has(id) })
				if ans := activity(); ans.outcome != requestDuplicate {
					t.Errorf("the activity after the notification was %s; want %s", ans.outcome, requestDuplicate)
				}
				testutil.Eventually(t, "the duplicate activity's record of its channel", func() bool {
					_, ok := pt.c.Permalink(pt.c.ref(testChannel, "", id))
					return ok
				}, testutil.Within(5*time.Second))
			}
			pt.flushLane(t)
			if got := pt.sink.callsOf(id); got != 1 {
				t.Errorf("the sink was given the mention %d times; want once", got)
			}
		})
	}
}

// flushLane waits until what is on the test channel's lane has been handed on: a person's post put on it after is in the sink.
func (pt *pushTest) flushLane(t *testing.T) {
	t.Helper()
	id := fmt.Sprintf("%s%06d", flushPrefix, 1)
	if ans := pt.c.recv.takePayload(t.Context(), inChannel(t, "message_plain", testChannel, id)); ans.status != http.StatusOK {
		t.Fatalf("flush: %d", ans.status)
	}
	testutil.Eventually(t, "the flush in the sink", func() bool { return pt.sink.has(id) })
}

// A read of a notified message that may pass on another try (429, 5xx) is made again, fetchAttempts times in all; one refused for good
// (401, 403) is not. A message that cannot be read is dropped and counted, as is each call.
func TestGraphReadOfANotifiedMessageIsTriedAgain(t *testing.T) {
	for _, tc := range []struct {
		status   int
		failures int
		reads    int
		outcome  string
		calls    map[string]int // chat_api_calls_total{FetchMessage} by outcome
	}{
		{http.StatusTooManyRequests, 2, 3, notifiedTaken, map[string]int{"rate_limited": 2, "success": 1}},
		{http.StatusServiceUnavailable, fetchAttempts, fetchAttempts, notifiedFetchFailed, map[string]int{"api_error": fetchAttempts}},
		{http.StatusForbidden, 1, 1, notifiedFetchFailed, map[string]int{"api_error": 1}},
		{http.StatusUnauthorized, 1, 1, notifiedFetchFailed, map[string]int{"api_error": 1}},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			pt := newPushTest(t, false)
			pt.subscribed(t, testChannel)
			const id = "1794971777720"
			pt.f.putMessage(testChannel, person(id, "", testUser, "<p>hi</p>"))
			var left atomic.Int32
			left.Store(int32(tc.failures))
			pt.f.setGraph(func(g *fakeGraph) {
				g.fail = func(r *http.Request) (int, string, bool) {
					return tc.status, `{"error":{"code":"Failed"}}`, strings.HasSuffix(r.URL.Path, "/"+id) && left.Add(-1) >= 0
				}
			})
			pt.graphTaken()
			if status, _ := pt.notify(t, pt.change(pt.subOf(testChannel), testChannel, "created", "", id)); status != http.StatusAccepted {
				t.Fatalf("status = %d", status)
			}
			testutil.Eventually(t, "the outcome", func() bool { return pt.counted(t, notifyChange, tc.outcome) == 1 })
			reads := 0
			for _, r := range pt.f.graphTaken() {
				if strings.HasSuffix(r.path, "/"+id) {
					reads++
				}
			}
			if reads != tc.reads {
				t.Errorf("reads = %d; want %d", reads, tc.reads)
			}
			for outcome, want := range tc.calls {
				if got := pt.obs.series(t, "agentkourier_chat_api_calls_total", "FetchMessage", outcome); got != float64(want) {
					t.Errorf("chat_api_calls_total{FetchMessage,%s} = %v; want %d", outcome, got, want)
				}
			}
		})
	}
}

// A body of notifications past notificationMaxBody is refused 413, and one that is not a body of Graph's notifications goes to the
// receiver in http mode (which wants a token: 401) and is refused 400 in socket mode, where only Graph sends over HTTPS.
func TestGraphRouteTellsGraphsRequestsApart(t *testing.T) {
	pt := newPushTest(t, false)
	big, _ := json.Marshal(map[string]any{"value": []any{map[string]any{"pad": strings.Repeat("x", notificationMaxBody)}}})
	if status, _ := pt.post(t, "", big); status != http.StatusRequestEntityTooLarge {
		t.Errorf("a body past the cap = %d; want 413", status)
	}
	if status, _ := pt.post(t, "", fixture(t, "message_plain")); status != http.StatusUnauthorized {
		t.Errorf("an activity with no token in http mode = %d; want 401, the receiver's", status)
	}
	if status, _ := pt.post(t, "", []byte(`{"value":"not a list"}`)); status != http.StatusUnauthorized {
		t.Errorf("a body that is no batch of notifications = %d; want 401, the receiver's", status)
	}
	if _, ok := mustHandler(t, pt.c).(httpsRoute); !ok {
		t.Error("the handler in http mode with a notificationURL is not httpsRoute")
	}

	st := newPushTest(t, true)
	status, body := st.post(t, "", fixture(t, "message_plain"))
	if status != http.StatusBadRequest || !strings.Contains(body, "socket mode") {
		t.Errorf("an activity over HTTPS in socket mode = %d %q; want 400", status, body)
	}
	if status, _ := st.notify(t, st.change("sub-001", testChannel, "created", "", "1794971777730")); status != http.StatusServiceUnavailable {
		t.Errorf("a notification to a socket-mode connection that does not run = %d; want 503", status)
	}
	if _, ok := mustHandler(t, st.c).(chat.HTTPDeadline); ok {
		t.Error("the socket-mode route asks for a longer deadline; Graph's requests are answered at once")
	}

	sc, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret), Socket: true})
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := sc.HTTPHandler(); ok || h != nil {
		t.Errorf("a socket-mode Client with no notificationURL offers %v; want no handler", h)
	}
}

func mustHandler(t *testing.T, c *Client) http.Handler {
	t.Helper()
	h, ok := c.HTTPHandler()
	if !ok {
		t.Fatal("no handler")
	}
	return h
}

// The clientState is derived from the client secret, under its own info string, the same for every replica and run of a connection, and
// another for another secret or connection; it is no other key of the secret.
func TestClientStateIsDerivedFromTheSecret(t *testing.T) {
	a, err := clientStateOf(testClientSecret, testConn)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := clientStateOf(testClientSecret, testConn)
	otherSecret, _ := clientStateOf(testClientSecret+"-rotated", testConn)
	otherConn, _ := clientStateOf(testClientSecret, "ns/other")
	key, _ := buttonKey(testClientSecret)
	if a != again || a == otherSecret || a == otherConn || len(a) != 43 || strings.Contains(a, testClientSecret) ||
		bytes.Contains([]byte(a), key) {
		t.Errorf("clientStates %q %q %q %q; want one stable value of 43 characters per secret and connection", a, again, otherSecret, otherConn)
	}
}

// parseMessageResource reads the resource of a notification in Microsoft's documented form and as path segments, and refuses others.
func TestParseMessageResource(t *testing.T) {
	for _, tc := range []struct {
		res  string
		want messageRef
		ok   bool
	}{
		{resourceOf(testChannel, "", "1794971777722"), messageRef{testGroup, testChannel, "", "1794971777722"}, true},
		{resourceOf(testChannel, "1794971777700", "1794971777722"), messageRef{testGroup, testChannel, "1794971777700", "1794971777722"}, true},
		{"/teams/" + testGroup + "/channels/" + testChannel + "/messages/1794971777722", messageRef{testGroup, testChannel, "", "1794971777722"}, true},
		{"teams/" + testGroup + "/channels/" + url.PathEscape(testChannel) + "/messages/1794971777700/replies/1794971777722",
			messageRef{testGroup, testChannel, "1794971777700", "1794971777722"}, true},
		{"teams('" + testGroup + "')/channels('" + testChannel + "')/messages", messageRef{}, false},
		{"teams('" + testGroup + "')/channels('" + testChannel + "')/messages('not-an-id')", messageRef{}, false},
		{"chats('19:x')/messages('1794971777722')", messageRef{}, false},
		{"teams('" + testGroup + "')/channels('" + testChannel + "')/messages('1')/hostedContents('1')", messageRef{}, false},
		{"teams('" + testGroup + "/channels('c')/messages('1')", messageRef{}, false},
		{"", messageRef{}, false},
	} {
		got, ok := parseMessageResource(tc.res)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseMessageResource(%q) = %+v, %v; want %+v, %v", tc.res, got, ok, tc.want, tc.ok)
		}
	}
}

// The text of a message's cards is kept to graphTextMax bytes.
func TestCardsTextIsCapped(t *testing.T) {
	block, _ := json.Marshal(map[string]any{"type": "AdaptiveCard", "body": []any{map[string]any{"type": "TextBlock",
		"text": strings.Repeat("é", graphTextMax)}}})
	m := graphMessage{Attachments: []graphAttached{{ContentType: cardContentTypeIn, Content: json.RawMessage(mustJSON(t, string(block)))}}}
	if got := m.cardsText(); len(got) > graphTextMax || len(got) < graphTextMax-1 || !strings.HasPrefix(got, "é") {
		t.Errorf("cardsText is %d bytes; want at most %d, cut at a rune", len(got), graphTextMax)
	}
}

// A listing after a 409 keeps the subscriptions the run holds already: a bound channel's create refused because the app has a
// subscription to the connection's URL that the run's listing did not show lists again, adopts that one, and leaves the held one of
// another channel as it is, in Graph and in the run.
func TestGraphListingAfterAConflictKeepsTheHeldSubscriptions(t *testing.T) {
	pt := newPushTest(t, false)
	pt.subscribed(t, testChannel)
	held := pt.subOf(testChannel)
	pt.record(t, testGraphChannel, testChannel)
	pt.f.putSub("sub-unlisted", testGraphChannel, pt.url, pt.url, time.Now().Add(20*time.Hour)) // made after the run's listing
	pt.sink.bind(testChannel, testGraphChannel)
	testutil.Eventually(t, "the unlisted subscription adopted", func() bool {
		s, ok := pt.c.push.subs.snapshot(testGraphChannel)
		return ok && s.id == "sub-unlisted"
	})
	pt.settle(t)
	if s, _ := pt.c.push.subs.snapshot(testChannel); s.id != held {
		t.Errorf("the held subscription is %q; want %q", s.id, held)
	}
	if got := pt.subOf(testChannel); got != held {
		t.Errorf("Graph holds %q for the first channel; want %q, which the listing after the 409 kept", got, held)
	}
	if calls := pt.f.subCalls(); countOf(calls, "/subscriptions/"+held) != 0 {
		t.Errorf("calls = %v; want none to the held subscription", calls)
	}
}

// A bound channel whose resource the app has a subscription to on another URL (a changed notificationURL, or another broker of the
// app) is not a loop of creates that Graph refuses: the listing that shows it holds the channel back, failing, told once at WARN with
// the fix, the other subscription's URL without its query and its expiry (and never a clientState), until subscriptionConflictRetry has passed; a 409 then lists
// again at the next reconcile, and not at once. Once the other subscription is gone the channel is subscribed.
func TestGraphConflictWithAnotherURLIsNotALoop(t *testing.T) {
	pt := newPushTest(t, false)
	clock := &fakeClock{t: time.Now()}
	pt.c.push.subs.now = clock.now
	pt.c.push.subs.every = time.Hour // only the test's pokes reconcile
	const otherPath = "https://another.example.test/chat/teams/ns/other"
	other := "https://tool:fake-userinfo-secret@another.example.test/chat/teams/ns/other?token=fake-query-token"
	otherExpires := time.Now().Add(20 * time.Hour)
	pt.f.putSub("sub-other", testChannel, other, other, otherExpires)
	pt.record(t, testChannel, testChannel)
	pt.sink.bind(testChannel)
	subs := pt.c.push.subs
	pt.run(t)
	testutil.Eventually(t, "the first reconcile", func() bool { return subs.rounds.Load() >= 1 })
	round := func() { t.Helper(); pt.reconcileNow(t) }
	quiet := func(what string, want ...string) {
		t.Helper()
		n := subs.rounds.Load()
		time.Sleep(200 * time.Millisecond)
		if got := subs.rounds.Load(); got != n {
			t.Errorf("%s: %d reconciles that nothing asked for; want none", what, got-n)
		}
		if calls := pt.f.subCalls(); !slices.Equal(calls, want) {
			t.Errorf("%s: calls = %v; want %v", what, calls, want)
		}
	}
	quiet("the first reconcile", "/subscriptions") // the listing, and no create
	round()
	round()
	quiet("two reconciles while the channel is held back")
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelFailing); got != 1 {
		t.Errorf("channels{failing} = %v; want 1", got)
	}

	clock.advance(subscriptionConflictRetry + time.Second)
	round()
	quiet("the try after subscriptionConflictRetry", "/subscriptions") // the create, refused with 409, and no listing at once
	round()
	quiet("the reconcile after the 409", "/subscriptions") // the listing, which holds the channel back again
	round()
	quiet("a reconcile after that")

	logs := pt.logs.String()
	if n := strings.Count(logs, "for another notification URL"); n != 1 {
		t.Errorf("the WARN with the fix was written %d times; want once:\n%s", n, logs)
	}
	if !strings.Contains(logs, "otherExpires="+otherExpires.UTC().Format("2006-01-02T15:04:05")) {
		t.Errorf("the WARN names no expiry of the other subscription:\n%s", logs)
	}
	if n := strings.Count(logs, "level=WARN msg=\"teams: a bound channel's Microsoft Graph subscription could not be made"); n != 0 {
		t.Errorf("the 409 was written at WARN %d times; want only the WARN with the fix", n)
	}
	for _, s := range []string{pt.state, "from-another-run", "fake-userinfo-secret", "fake-query-token"} {
		if strings.Contains(logs, s) {
			t.Errorf("the logs hold %q, a clientState or a credential of the other URL", s)
		}
	}
	if !strings.Contains(logs, "otherNotificationURL="+otherPath+" ") {
		t.Errorf("the WARN does not name the other URL's scheme, host and path %q:\n%s", otherPath, logs)
	}

	pt.f.mu.Lock()
	delete(pt.f.graph.subs.byID, "sub-other")
	pt.f.mu.Unlock()
	clock.advance(subscriptionConflictRetry + time.Second)
	round()
	if pt.subOf(testChannel) == "" {
		t.Fatal("the channel is not subscribed once the other subscription is gone")
	}
	if got := pt.obs.series(t, "agentkourier_teams_graph_channels", channelFailing); got != 0 {
		t.Errorf("channels{failing} = %v once subscribed; want 0", got)
	}
}

// A notification is answered 503, and counted as not_running, until the subscriptions of the run have been listed: while the listing
// fails or has not answered, the connection does not know which subscriptions it holds. After a listing it is answered 202.
func TestGraphNotificationsWaitForTheListing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stall func(pt *pushTest) (release func())
	}{
		{"fails", func(pt *pushTest) func() {
			var failing atomic.Bool
			failing.Store(true)
			pt.f.setGraph(func(g *fakeGraph) {
				g.fail = func(r *http.Request) (int, string, bool) {
					return http.StatusServiceUnavailable, `{"error":{"code":"Failed"}}`,
						failing.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/subscriptions")
				}
			})
			return func() { failing.Store(false) }
		}},
		{"blocks", func(pt *pushTest) func() {
			hold := make(chan struct{})
			pt.f.setGraph(func(g *fakeGraph) { g.hold = hold })
			return func() { close(hold) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pt := newPushTest(t, false)
			release := tc.stall(pt)
			pt.record(t, testChannel, testChannel)
			pt.sink.bind(testChannel)
			pt.run(t)
			testutil.Eventually(t, "a listing asked for", func() bool { return len(pt.f.subCalls()) > 0 })
			n := pt.change("sub-not-held", testChannel, "created", "", "1794971777722")
			if status, _ := pt.notify(t, n); status != http.StatusServiceUnavailable {
				t.Errorf("a notification before the listing = %d; want 503", status)
			}
			if got := pt.counted(t, notifyChange, notifiedNotRunning); got != 1 {
				t.Errorf("notifications{change,not_running} = %v; want 1", got)
			}
			release()
			testutil.Eventually(t, "the subscription", func() bool { return pt.subOf(testChannel) != "" })
			if status, _ := pt.notify(t, n); status != http.StatusAccepted {
				t.Errorf("a notification after the listing = %d; want 202", status)
			}
			if got := pt.counted(t, notifyChange, notifiedUnknownSub); got != 1 {
				t.Errorf("notifications{change,unknown_subscription} = %v; want 1", got)
			}
		})
	}
}

// A body that anyone may send is bounded before its notifications are checked: one with more than notificationMaxItems is answered 400
// and nothing of it is taken; the notifications of a body whose clientState is not the connection's are logged in one WARN line for
// the body, and the lines are paced, as is that of a body too long.
func TestGraphBodiesThatAnyoneMaySendAreBounded(t *testing.T) {
	pt := newPushTest(t, false)
	pt.subscribed(t, testChannel)
	items := func(n int) []byte {
		return []byte(`{"value":[` + strings.TrimSuffix(strings.Repeat(`{},`, n), ",") + `]}`)
	}
	warns := func() int { return strings.Count(pt.logs.String(), "level=WARN") }
	if status, _ := pt.post(t, "", items(notificationMaxItems+1)); status != http.StatusBadRequest {
		t.Errorf("a body of %d notifications = %d; want 400", notificationMaxItems+1, status)
	}
	if got := pt.counted(t, notifyChange, notifiedClientState); got != 0 {
		t.Errorf("notifications{change,client_state} = %v after a body refused; want none taken", got)
	}
	before := warns()
	if status, _ := pt.post(t, "", items(notificationMaxItems)); status != http.StatusAccepted {
		t.Errorf("a body of %d notifications = %d; want 202", notificationMaxItems, status)
	}
	for range 5 {
		pt.post(t, "", items(1))
	}
	if got := warns() - before; got != 1 {
		t.Errorf("WARN lines for 6 bodies of notifications with another clientState = %d; want 1:\n%s", got, pt.logs.String())
	}
	if !strings.Contains(pt.logs.String(), fmt.Sprintf("count=%d", notificationMaxItems)) {
		t.Errorf("the WARN line does not count the body's notifications:\n%s", pt.logs.String())
	}
	if got := pt.counted(t, notifyChange, notifiedClientState); got != notificationMaxItems+5 {
		t.Errorf("notifications{change,client_state} = %v; want %d", got, notificationMaxItems+5)
	}
	before = warns()
	long := []byte(`{"value":[{}],"padding":"` + strings.Repeat("x", notificationMaxBody) + `"}`)
	for range 3 {
		if status, _ := pt.post(t, "", long); status != http.StatusRequestEntityTooLarge {
			t.Errorf("a body too long = %d; want 413", status)
		}
	}
	if got := warns() - before; got != 1 {
		t.Errorf("WARN lines for 3 bodies too long = %d; want 1", got)
	}
}

// A 403 of a call about a channel's messages (a make or a renew of its subscription) is told the fix of the resource-specific
// permission; one of a listing or a delete of the subscriptions, which that permission does not grant, is not.
func TestGraphForbiddenNamesThePermissionOnlyForAChannel(t *testing.T) {
	pt := newPushTest(t, false)
	pt.f.setGraph(func(g *fakeGraph) {
		g.fail = func(*http.Request) (int, string, bool) {
			return http.StatusForbidden, `{"error":{"code":"Forbidden"}}`, true
		}
	})
	subs := pt.c.push.subs
	for _, tc := range []struct {
		operation, method string
		link              string
		fix               bool
	}{
		{"Subscribe", http.MethodPost, pt.c.graph.resource("subscriptions"), true},
		{"Renew", http.MethodPatch, pt.c.graph.resource("subscriptions", "sub-001"), true},
		{"ListSubscriptions", http.MethodGet, pt.c.graph.resource("subscriptions"), false},
		{"Unsubscribe", http.MethodDelete, pt.c.graph.resource("subscriptions", "sub-001"), false},
	} {
		err := subs.call(t.Context(), tc.operation, tc.method, tc.link, nil, nil)
		var ge *GraphError
		if !errors.As(err, &ge) || ge.Status != http.StatusForbidden {
			t.Fatalf("%s: err = %v; want a GraphError 403", tc.operation, err)
		}
		if got := strings.Contains(err.Error(), "ChannelMessage.Read.Group"); got != tc.fix {
			t.Errorf("%s: err = %q; names the permission's fix: %v, want %v", tc.operation, err, got, tc.fix)
		}
	}
}

// reconcileNow asks the subscriber of a run whose reconcile period is long for one reconcile, and waits until it has ended.
func (pt *pushTest) reconcileNow(t *testing.T) {
	t.Helper()
	subs := pt.c.push.subs
	n := subs.rounds.Load()
	subs.poke()
	testutil.Eventually(t, "a reconcile", func() bool { return subs.rounds.Load() >= n+1 })
}

// A channel that the app's subscription for another URL holds is tried again when that subscription expires, if it expires before
// subscriptionConflictRetry has passed.
func TestGraphConflictWithAnotherURLIsTriedAgainAtItsExpiry(t *testing.T) {
	pt := newPushTest(t, false)
	clock := &fakeClock{t: time.Now()}
	pt.c.push.subs.now = clock.now
	pt.c.push.subs.every = time.Hour // only the test's pokes reconcile
	other := "https://another.example.test/chat/teams/ns/other"
	soon := subscriptionConflictRetry / 4
	pt.f.putSub("sub-other", testChannel, other, other, clock.now().Add(soon))
	pt.record(t, testChannel, testChannel)
	pt.sink.bind(testChannel)
	pt.run(t)
	testutil.Eventually(t, "the first reconcile", func() bool { return pt.c.push.subs.rounds.Load() >= 1 })
	pt.f.subCalls()
	clock.advance(soon - time.Second)
	pt.reconcileNow(t)
	if calls := pt.f.subCalls(); len(calls) != 0 {
		t.Errorf("calls before the other subscription expires = %v; want none", calls)
	}
	pt.f.mu.Lock()
	delete(pt.f.graph.subs.byID, "sub-other") // Graph drops it at its expiry
	pt.f.mu.Unlock()
	clock.advance(2 * time.Second)
	pt.reconcileNow(t)
	if pt.subOf(testChannel) == "" {
		t.Errorf("the channel is not subscribed once the other subscription expired, %v before subscriptionConflictRetry", subscriptionConflictRetry-soon)
	}
}

// A listing after a 409 that keeps failing does not stop the renews: the subscriptions the run holds are still renewed in their window,
// and only the make of the channel that conflicted waits for a listing that succeeds.
func TestGraphHeldSubscriptionsAreRenewedWhileAListingAfterAConflictFails(t *testing.T) {
	pt := newPushTest(t, false)
	clock := &fakeClock{t: time.Now()}
	pt.c.push.subs.now = clock.now
	pt.f.setGraph(func(g *fakeGraph) { g.subs.expiresIn = time.Hour }) // renewed once half of it has passed
	pt.subscribed(t, testChannel)
	held := pt.subOf(testChannel)
	pt.f.setGraph(func(g *fakeGraph) {
		g.fail = func(r *http.Request) (int, string, bool) {
			return http.StatusForbidden, `{"error":{"code":"Forbidden"}}`, r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/subscriptions")
		}
	})
	other := "https://another.example.test/chat/teams/ns/other"
	pt.f.putSub("sub-other", testGraphChannel, other, other, time.Now().Add(20*time.Hour)) // the 409 of the second channel's make
	pt.record(t, testGraphChannel, testChannel)
	pt.sink.bind(testChannel, testGraphChannel)
	testutil.Eventually(t, "the listing after the 409 failing", func() bool {
		return pt.obs.series(t, "agentkourier_chat_api_calls_total", "ListSubscriptions", "api_error") >= 2
	})
	clock.advance(31 * time.Minute)
	testutil.Eventually(t, "the held subscription renewed", func() bool { return countOf(pt.f.subCalls(), "/subscriptions/"+held) > 0 })
	pt.settle(t)
	if s, _ := pt.c.push.subs.snapshot(testChannel); s.id != held {
		t.Errorf("the held subscription is %q; want %q, kept", s.id, held)
	}
	pt.f.mu.Lock()
	makes := 0
	for _, b := range pt.f.graph.subs.bodies {
		if strings.Contains(str(b["resource"]), testGraphChannel) {
			makes++
		}
	}
	pt.f.mu.Unlock()
	if makes != 1 {
		t.Errorf("makes of the conflicting channel = %d; want the one refused with 409, and none while the listing fails", makes)
	}
	if n := strings.Count(pt.logs.String(), "those held are still renewed"); n != 1 {
		t.Errorf("the WARN of the failing listing was written %d times; want once", n)
	}
}

// A message read through Graph that the sink does not take in its tries is lost under a reason of its own, graph_not_taken, and not
// counted with the activities that were not taken.
func TestGraphPostNotTakenIsCountedApartFromActivities(t *testing.T) {
	pt := newPushTest(t, false)
	pt.subscribed(t, testChannel)
	const id = "1794971777720"
	pt.f.putMessage(testChannel, person(id, "", testUser, "<p>is it down?</p>"))
	pt.sink.mu.Lock()
	pt.sink.failing = map[string]int{id: failAlways}
	pt.sink.mu.Unlock()
	if status, body := pt.notify(t, pt.change(pt.subOf(testChannel), testChannel, "created", "", id)); status != http.StatusAccepted {
		t.Fatalf("status = %d %s; want 202", status, body)
	}
	testutil.Eventually(t, "the message counted not taken", func() bool { return pt.counted(t, notifyChange, notifiedNotTaken) == 1 })
	if got := pt.obs.series(t, "agentkourier_teams_activities_lost_total", lostGraphNotTaken); got != 1 {
		t.Errorf("lost{%s} = %v; want 1", lostGraphNotTaken, got)
	}
	if got := pt.obs.series(t, "agentkourier_teams_activities_lost_total", lostNotTaken); got != 0 {
		t.Errorf("lost{%s} = %v; want 0: a Graph message is no activity", lostNotTaken, got)
	}
}

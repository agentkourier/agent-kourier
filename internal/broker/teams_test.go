package broker

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/teams"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// The stand-ins of a Microsoft Teams connection's tenant and app; no tenant has them.
const (
	teamsTestTenant = "11111111-2222-4333-8444-555555555555"
	teamsTestApp    = "22222222-3333-4444-8555-666666666666"
)

// fakeMicrosoft is, for a Teams connection in a test, Microsoft Entra's token endpoint, which issues a token for any client secret, and
// the Bot Framework's OpenID metadata, whose key set is empty: so the connection runs and takes no activity, and nothing leaves the test.
type fakeMicrosoft struct {
	srv    *httptest.Server
	tokens atomic.Int32
}

func newFakeMicrosoft(t *testing.T) *fakeMicrosoft {
	t.Helper()
	f := &fakeMicrosoft{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		f.tokens.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": 3599, "access_token": "fake-bot-token"})
	})
	mux.HandleFunc("GET /openid", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": "https://api.botframework.com", "jwks_uri": f.srv.URL + "/keys"})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{}})
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// point is RunConfig.teamsFakes: the Teams client calls f and nothing else.
func (f *fakeMicrosoft) point(tc *teams.Config) {
	tc.TokenURL, tc.MetadataURL, tc.HTTPClient = f.srv.URL+"/token", f.srv.URL+"/openid", f.srv.Client()
}

// teamsConn is the Connection of a Microsoft Teams ChatConnection ns/name in http mode, with a fake client secret.
func teamsConn(ns, name string) Connection {
	return Connection{Ref: v1alpha1.ObjectRef{Namespace: ns, Name: name}, Platform: v1alpha1.PlatformTeams,
		Credentials: map[string]secret.Value{v1alpha1.TeamsClientSecretKey: secret.New("fake-client-secret")},
		Teams:       v1alpha1.TeamsConnectionSpec{TenantID: teamsTestTenant, AppID: teamsTestApp}}
}

// A Microsoft Teams connection is built as the Teams adapter, which takes its requests on the chat listener and says what it cannot do; a
// broker with no chat listener, a connection with no client secret or tenant, or one with no store for its conversations, is a wiring error.
func TestNewPlatformBuildsTheTeamsAdapter(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cfg := RunConfig{ChatListener: ln}
	built, err := cfg.newPlatform(teamsConn("team-a", "teams"), nil, nil, teams.NewMemoryConversations())
	if err != nil || built.Platform == nil || built.closeIdle == nil {
		t.Fatalf("newPlatform(teams) = %+v, %v; want the Teams adapter, and a way to close its connections", built, err)
	}
	if _, ok := built.Platform.(*teams.Client); !ok {
		t.Errorf("newPlatform(teams) built a %T", built.Platform)
	}
	if h, ok := receiverOf(built.Platform); !ok || h == nil {
		t.Error("the Teams adapter offers no handler for the chat listener")
	}
	if got := built.Capabilities(); got.Streams || got.Ephemeral || got.AckReaction != "" {
		t.Errorf("the adapter's capabilities are %+v; want no streams, no ephemeral messages and no reactions", got)
	}

	var none RunConfig
	if _, err := none.newPlatform(teamsConn("team-a", "teams"), nil, nil, teams.NewMemoryConversations()); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "AGENTKOURIER_CHAT_LISTEN") {
		t.Errorf("a teams connection on a broker with no chat listener: %v; want a wiring error that names AGENTKOURIER_CHAT_LISTEN", err)
	}
	if err := none.needsChatListener(teamsConn("team-a", "teams")); !errors.Is(err, ErrWiring) {
		t.Errorf("needsChatListener(teams) with no chat listener = %v; want a wiring error, which stops a start with it as the own connection", err)
	}
	for name, edit := range map[string]func(*Connection){
		"no client secret": func(c *Connection) { c.Credentials = nil },
		"no tenant":        func(c *Connection) { c.Teams.TenantID = "" },
		"no app":           func(c *Connection) { c.Teams.AppID = "" },
	} {
		c := teamsConn("team-a", "teams")
		edit(&c)
		if built, err := cfg.newPlatform(c, nil, nil, teams.NewMemoryConversations()); !errors.Is(err, ErrWiring) || built.Platform != nil {
			t.Errorf("%s: newPlatform = %+v, %v; want a wiring error and no platform", name, built, err)
		}
	}
	if built, err := cfg.newPlatform(teamsConn("team-a", "teams"), nil, nil, nil); !errors.Is(err, ErrWiring) || built.Platform != nil {
		t.Errorf("no conversation store: newPlatform = %+v, %v; want a wiring error and no platform, not one that forgets its channels at a restart", built, err)
	}
}

// teamsConnection puts the Microsoft Teams ChatConnection ns/name, in mode, with its Secret creds-teams, in w's cluster.
func (w *connWorld) teamsConnection(ns, name string, mode v1alpha1.TeamsMode) {
	w.t.Helper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "creds-teams"},
		Data: map[string][]byte{v1alpha1.TeamsClientSecretKey: []byte("fake-client-secret")}}
	if _, err := w.kube.CoreV1().Secrets(ns).Create(w.t.Context(), s, metav1.CreateOptions{}); err != nil {
		w.t.Fatal(err)
	}
	conn := crObject(w.t, "chatconnection.yaml")
	conn.SetNamespace(ns)
	conn.SetName(name)
	conn.SetUID(types.UID("uid-" + ns + "-" + name))
	unstructured.RemoveNestedField(conn.Object, "spec", "allowedNamespaces")
	_ = unstructured.SetNestedField(conn.Object, v1alpha1.PlatformTeams, "spec", "platform")
	_ = unstructured.SetNestedField(conn.Object, "creds-teams", "spec", "credentialsSecretRef", "name")
	_ = unstructured.SetNestedStringMap(conn.Object, map[string]string{"tenantID": teamsTestTenant, "appID": teamsTestApp, "mode": string(mode)},
		"spec", "teams")
	if err := w.dyn.Tracker().Create(crConnections, conn, ns); err != nil {
		w.t.Fatal(err)
	}
}

// A served Microsoft Teams connection in http mode runs beside the own Slack one: it gets the bot's token, and its route on the chat
// listener reaches the Teams adapter, which refuses a request with no token (401). One in socket mode is not started, its route has no
// handler (503), and the supervisor says why.
func TestARunServesATeamsConnectionInHTTPModeAndNotOneInSocketMode(t *testing.T) {
	ms := newFakeMicrosoft(t)
	w := newConnWorld(t, nil)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	w.c.ChatListener = ln
	w.c.teamsFakes = ms.point
	w.teamsConnection("team-a", "teams", v1alpha1.TeamsModeHTTP)
	w.teamsConnection("team-b", "teams", v1alpha1.TeamsModeSocket)
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)

	addr := ln.Addr().String()
	w.eventually("the teams connection's route to reach the Teams adapter", func() bool {
		return chatPost(t, http.MethodPost, addr, "/chat/teams/team-a/teams") == http.StatusUnauthorized
	})
	r.waitMetric(`agentkourier_chat_connected{connection="team-a/teams"} 1`)
	if n := ms.tokens.Load(); n < 1 {
		t.Errorf("the bot's token was fetched %d times; want it fetched before the connection was up", n)
	}
	if code := chatPost(t, http.MethodPost, addr, "/chat/teams/team-b/teams"); code != http.StatusServiceUnavailable {
		t.Errorf("POST to the socket-mode connection's route = %d; want 503, as no handler runs for it", code)
	}
	w.eventually("the supervisor to say why the socket-mode connection is not started", func() bool {
		return strings.Contains(w.logs.String(), `msg="the ChatConnection is not started: Socket mode is not supported yet for Microsoft Teams" connection=team-b/teams`)
	})
	r.waitMetric(`agentkourier_chat_connected{connection="team-b/teams"} 0`)
}

// connectorCalls is, for a Teams client in a test, Microsoft Entra and the Bot Connector at every URL: it answers a token for the token
// URL and a posted message for any other, records each URL it was asked for, and dials nothing.
type connectorCalls struct {
	mu   sync.Mutex
	urls []string
}

func (c *connectorCalls) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.urls = append(c.urls, r.URL.String())
	c.mu.Unlock()
	body := `{"id":"1794965000201"}`
	if r.URL.Host == "login.example.test" {
		body = `{"token_type":"Bearer","expires_in":3599,"access_token":"fake-bot-token"}`
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

// posts are the URLs of the calls that were not for a token.
func (c *connectorCalls) posts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, u := range c.urls {
		if !strings.HasPrefix(u, "https://login.example.test/") {
			out = append(out, u)
		}
	}
	return out
}

// The leader builds a Microsoft Teams platform over the broker's store: a channel that an earlier leader recorded, before a restart or a
// takeover, is posted to through the service URL it recorded, and a channel with no record through Microsoft's global route.
func TestOpenPlatformKeepsTheTeamsConversationsInTheStore(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	const (
		recorded = "19:KourierStandInChannelForTestsOnly0000000000a@thread.tacv2"
		unheard  = "19:KourierStandInUnheardChannel0000000000000a@thread.tacv2"
		conn     = "team-a/teams"
	)
	st := sqlitetest.Open(t)
	if err := st.RememberTeamsConversation(t.Context(), store.TeamsConversation{Connection: conn, Channel: recorded,
		ServiceURL: "https://smba.example.test/amer/" + teamsTestTenant + "/", TenantID: teamsTestTenant, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	calls := &connectorCalls{}
	h := &leaderHalf{log: testutil.DiscardLogger(), st: st, cfg: RunConfig{ChatListener: ln, teamsFakes: func(tc *teams.Config) {
		tc.TokenURL, tc.HTTPClient = "https://login.example.test/token", &http.Client{Transport: calls}
	}}}
	built, err := h.openPlatform(teamsConn("team-a", "teams"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{recorded, unheard} {
		if _, err := built.Post(t.Context(), chat.Target{Conn: conn, Channel: channel}, chat.Say(chat.Message{Fallback: "x"}), "tag"); err != nil {
			t.Fatalf("Post to %s: %v", channel, err)
		}
	}
	got := calls.posts()
	if len(got) != 2 || !strings.HasPrefix(got[0], "https://smba.example.test/amer/"+teamsTestTenant+"/v3/conversations/") ||
		!strings.HasPrefix(got[1], teams.DefaultServiceURL+"v3/conversations/") {
		t.Errorf("the posts went to %v; want the recorded channel's service URL, then the global route", got)
	}
}

// What was measured on a test tenant on 2026-11-18, from an invoke's arrival to its answer: the Teams client showed an answer given at
// 12.5 seconds, and gave up on one given at 13 (Teams' service held the request until about 13.9). The measurement's clock started
// when the invoke reached the server, so it did not see the network's share; teamsNetworkMargin is kept for it.
const (
	teamsClientShows   = 12500 * time.Millisecond
	teamsNetworkMargin = 1500 * time.Millisecond
)

// The chat listener gives a Microsoft Teams connection's route the longer deadline its receiver asks for, and the request's whole bound
// there (its read, that deadline and the answer's write) ends inside what the Teams client still shows, less a margin for the network,
// so that a press answered at the deadline is still shown to the person who pressed.
func TestTheTeamsRouteIsAnsweredBeforeTeamsGivesUpOnAnInvoke(t *testing.T) {
	recv, err := teams.NewReceiver(teams.ReceiverConfig{Connection: "ns/teams", TenantID: teamsTestTenant, AppID: teamsTestApp,
		Logger: testutil.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	routes := newChatRoutes(func(string) {}, testutil.DiscardLogger())
	route := routes.register(v1alpha1.PlatformTeams, "ns/teams", recv)
	defer route.drop()
	if route.timeout <= chatHandlerTimeout {
		t.Errorf("the Teams route's deadline is %v, no longer than the default %v", route.timeout, chatHandlerTimeout)
	}
	if bound := chatReadTimeout + route.timeout + chatAnswerTimeout; bound > teamsClientShows-teamsNetworkMargin {
		t.Errorf("a Teams request is bounded at %v; the client shows an answer up to %v, and %v of that is kept for the network",
			bound, teamsClientShows, teamsNetworkMargin)
	}
}

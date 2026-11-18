package broker

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/teams"
	"github.com/agentkourier/agent-kourier/internal/secret"
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
// broker with no chat listener, or a connection with no client secret or tenant, is a wiring error.
func TestNewPlatformBuildsTheTeamsAdapter(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cfg := RunConfig{ChatListener: ln}
	built, err := cfg.newPlatform(teamsConn("team-a", "teams"), nil, nil)
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
	if _, err := none.newPlatform(teamsConn("team-a", "teams"), nil, nil); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "AGENTKOURIER_CHAT_LISTEN") {
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
		if built, err := cfg.newPlatform(c, nil, nil); !errors.Is(err, ErrWiring) || built.Platform != nil {
			t.Errorf("%s: newPlatform = %+v, %v; want a wiring error and no platform", name, built, err)
		}
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

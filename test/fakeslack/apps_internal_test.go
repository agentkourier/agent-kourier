package fakeslack

import (
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// TestRevokingBetweenTheLinkCheckAndRegistrationStillClosesTheSocket stops a websocket after it is open and
// before it is registered, revokes its app there, and checks that the client is sent nothing and the socket closes.
func TestRevokingBetweenTheLinkCheckAndRegistrationStillClosesTheSocket(t *testing.T) {
	app := App{AppID: "AAPPX", BotUser: "UX", BotID: "BX", BotToken: "xoxb-x-token", AppToken: "xapp-x-token"}
	s := New(WithApp(app))
	t.Cleanup(s.Close)
	s.beforeSocketAdd = func() {
		if err := s.SetRevoked("AAPPX", true); err != nil {
			t.Errorf("SetRevoked: %v", err)
		}
	}
	ws, resp, err := websocket.DefaultDialer.DialContext(t.Context(), "ws"+strings.TrimPrefix(s.URL(), "http")+socketPath+"?app=AAPPX", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = resp.Body.Close()
	defer func() { _ = ws.Close() }()
	if _, msg, err := ws.ReadMessage(); err == nil {
		t.Fatalf("a revoked app's socket was sent %s, want it closed with nothing sent", msg)
	}
	if n := s.AppConnections("AAPPX"); n != 0 {
		t.Errorf("AppConnections = %d, want 0", n)
	}
}

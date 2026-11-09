package broker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// After a network blip a Socket Mode reconnect asks Slack for a new connection (apps.connections.open) over the Web API client,
// which slack-go gives no deadline. A Slack that takes the request and never answers must end the call at the client's bound, so
// that Run returns the failure and the supervisor connects again, instead of holding the app off Socket Mode for minutes.
func TestSlackClientBoundsTheCallThatOpensTheConnection(t *testing.T) {
	var opened atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"UBOT","bot_id":"BBOT","url":"https://example.slack.com/"}`))
		case "/apps.connections.open":
			opened.Add(1)
			<-release // accepted and never answered
		}
	}))
	defer srv.Close()
	defer close(release) // runs before srv.Close, which waits for the handler

	cfg := RunConfig{
		slackHTTPTimeout: 200 * time.Millisecond,
		newSlack: func(sc slack.Config) slackAPI {
			sc.APIURL = srv.URL + "/"
			return slack.New(sc)
		},
	}
	conn := Connection{
		Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{"botToken": secret.New("xoxb-fake"), "appToken": secret.New("xapp-fake")},
	}
	built, err := cfg.newPlatform(conn, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer built.closeIdle()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err = built.Run(ctx, nil)
	if ctx.Err() != nil {
		t.Fatalf("Run = %v after %v, and only the test's own deadline ended it; want the client's bound to end the call that opens the connection", err, time.Since(start))
	}
	if err == nil || errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v; want the failed attempt to open the connection, so that the supervisor connects again", err)
	}
	if opened.Load() == 0 {
		t.Error("the fake never saw apps.connections.open: the test did not reach the call it bounds")
	}
}

// An unset bound is the 30 seconds that the CLI's own Slack clients use, and the client carries it as its timeout: it covers the
// dial, the TLS handshake, the response headers and the body, so a call that stalls at any of them ends.
func TestSlackClientDefaultBound(t *testing.T) {
	var saw slack.Config
	cfg := RunConfig{newSlack: func(sc slack.Config) slackAPI { saw = sc; return idleSlack{} }}
	conn := Connection{
		Ref: v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{"botToken": secret.New("xoxb-fake"), "appToken": secret.New("xapp-fake")},
	}
	if _, err := cfg.newPlatform(conn, nil, nil); err != nil || saw.HTTPClient == nil {
		t.Fatalf("newPlatform = %v; client config %+v", err, saw)
	}
	if got := saw.HTTPClient.Timeout; got != 30*time.Second {
		t.Errorf("the Slack client's timeout is %v; want 30s", got)
	}
}

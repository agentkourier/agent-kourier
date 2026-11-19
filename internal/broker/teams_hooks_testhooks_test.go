//go:build testhooks

package broker

import (
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/teams"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// A test image's Microsoft Teams connection, given the fake Teams service by the test hooks, makes its every call to the fake: its
// token, metadata, Graph and socket endpoints are the fake's, its token comes from the fake over TLS that trusts the fake's certificate,
// and a post to a channel that no activity recorded, which would go to Microsoft's global Bot Connector, is refused before it leaves.
// A hook that is wrong (a CA that is no certificate) is a wiring error, not a connection to Microsoft.
func TestATestImagesTeamsConnectionCallsTheFakeAlone(t *testing.T) {
	ms := newFakeMicrosoft(t)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ms.srv.Certificate().Raw}))
	var got teams.Config
	cfg := RunConfig{ChatListener: ln, hooks: testhooks.Overrides{TeamsURL: ms.srv.URL, TeamsCA: ca},
		teamsFakes: func(tc *teams.Config) { got = *tc }}
	built, err := cfg.newPlatform(teamsConn("team-a", "teams"), nil, nil, teams.NewMemoryConversations(), nil)
	if err != nil {
		t.Fatalf("newPlatform(teams) = %v; want the adapter, on the fake", err)
	}
	t.Cleanup(built.closeIdle)
	if got.TokenURL != ms.srv.URL+"/token" || got.MetadataURL != ms.srv.URL+"/openid" || got.GraphURL != ms.srv.URL+"/v1.0" ||
		got.SocketURL == nil || got.SocketURL("amer") != ms.srv.URL+"/amer/v3/websockets/connect" || got.SocketTLS == nil {
		t.Errorf("the Teams config points at token %q, metadata %q, graph %q; want the fake's every endpoint", got.TokenURL, got.MetadataURL, got.GraphURL)
	}

	_, err = built.Post(t.Context(), chat.Target{Conn: "team-a/teams", Channel: "19:unrecorded@thread.tacv2"},
		chat.Say(chat.Message{Fallback: "hello"}), "")
	if err == nil || !strings.Contains(err.Error(), "fake Teams service alone") {
		t.Errorf("a post to a channel with no record = %v; want it refused before it leaves for Microsoft's Bot Connector", err)
	}
	if n := ms.tokens.Load(); n < 1 {
		t.Errorf("the fake issued %d tokens; want the post's bot token from the fake, over TLS that trusts its certificate", n)
	}

	cfg.hooks.TeamsCA = "not a certificate"
	if built, err := cfg.newPlatform(teamsConn("team-a", "teams"), nil, nil, teams.NewMemoryConversations(), nil); !errors.Is(err, ErrWiring) || built.Platform != nil {
		t.Errorf("a fake whose CA is no certificate: newPlatform = %+v, %v; want a wiring error", built, err)
	}
}

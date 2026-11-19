//go:build !testhooks

package broker

import (
	"net"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/teams"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// A production binary's Microsoft Teams connection talks to Microsoft alone: the environment variables that point a test image's at the
// fake Teams service change nothing in this build, where testhooks.Teams has no fake to give.
func TestATeamsConnectionTakesNoFakeFromTheEnvironment(t *testing.T) {
	t.Setenv(testhooks.TeamsURLEnv, "https://evil.example")
	t.Setenv(testhooks.TeamsCAEnv, "-----BEGIN CERTIFICATE-----")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var got teams.Config
	cfg := RunConfig{ChatListener: ln, teamsFakes: func(tc *teams.Config) { got = *tc }}
	built, err := cfg.newPlatform(teamsConn("team-a", "teams"), nil, nil, teams.NewMemoryConversations(), nil)
	if err != nil {
		t.Fatalf("newPlatform(teams) = %v", err)
	}
	t.Cleanup(built.closeIdle)
	if got.TokenURL != "" || got.MetadataURL != "" || got.GraphURL != "" || got.SocketURL != nil || got.SocketTLS != nil {
		t.Errorf("the Teams config in the default build points at token %q, metadata %q, graph %q; want Microsoft's own (all empty)",
			got.TokenURL, got.MetadataURL, got.GraphURL)
	}
}

package slack_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// ref is a message at ts of channel, as a Platform names it.
func ref(channel, ts string) chat.Ref {
	return chat.Ref{Platform: slack.PlatformName, Conn: intakeConn, Channel: channel, ID: ts}
}

// The permalink of a message is built from the workspace URL that auth.test returned when Run connected, with no call
// of its own: it is for the acknowledgement path, which allows Slack's API none. Until Run has asked, there is none.
func TestPermalinkIsBuiltFromTheCachedWorkspaceURL(t *testing.T) {
	fake := newFake(t, fakeslack.WithTeam("T0EXAMPLE1", "example"))
	client := slack.New(clientConfig(fake))
	p := slack.NewPlatform(client, intakeConn)
	var _ chat.Permalinker = p

	if got, ok := p.Permalink(ref("C0123456789", "1759560000.123456")); ok || got != "" {
		t.Fatalf("Permalink before Run = %q, %v; want none: auth.test has not been asked", got, ok)
	}
	run(t, client, &sink{})
	waitConnected(t, fake)

	got, ok := p.Permalink(ref("C0123456789", "1759560000.123456"))
	if want := "https://example.slack.com/archives/C0123456789/p1759560000123456"; !ok || got != want {
		t.Errorf("Permalink = %q, %v; want %q", got, ok, want)
	}
}

// What is not a channel and a ts has no permalink, so that nothing the platform did not make is put in a link.
func TestPermalinkRefusesWhatIsNotAChannelAndATimestamp(t *testing.T) {
	fake := newFake(t)
	client := slack.New(clientConfig(fake))
	p := slack.NewPlatform(client, intakeConn)
	run(t, client, &sink{})
	waitConnected(t, fake)

	for _, r := range []chat.Ref{
		ref("", "1759560000.123456"), ref("C1", ""), ref("C1", "1759560000"), ref("C1", "17.5.6"), ref("c1", "1759560000.123456"),
		ref("C1/../../x", "1759560000.123456"), ref("C1", "1759560000.123456?x=1"), ref("C1", "1759560000.12345 6"),
	} {
		if got, ok := p.Permalink(r); ok || got != "" {
			t.Errorf("Permalink(%+v) = %q, %v; want none", r, got, ok)
		}
	}
}

// A workspace URL that is not an https URL with a host and nothing else is not used: the link would send a reader
// somewhere that is not Slack's, or carry credentials.
func TestPermalinkWantsAnHTTPSWorkspaceURL(t *testing.T) {
	for _, url := range []string{"", "http://example.slack.com/", "javascript:alert(1)", "https:///", "example.slack.com", "https://user:pw@example.slack.com/"} {
		// auth.test answers with url; every other call, the connection's included, is refused as a bad token,
		// which ends Run at once.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			answer := map[string]any{"ok": false, "error": "invalid_auth"}
			if r.URL.Path == "/auth.test" {
				answer = map[string]any{"ok": true, "url": url, "team_id": "T1", "user_id": "UFAKEBOT", "bot_id": "BFAKEBOT"}
			}
			_ = json.NewEncoder(w).Encode(answer)
		}))
		cfg := clientConfig(newFake(t))
		cfg.APIURL = srv.URL + "/"
		client := slack.New(cfg)
		_, wait := run(t, client, &sink{})
		_ = wait()
		srv.Close()
		if got, ok := slack.NewPlatform(client, intakeConn).Permalink(ref("C1", "1759560000.123456")); ok || got != "" {
			t.Errorf("auth.test url %q: Permalink = %q, %v; want none", url, got, ok)
		}
	}
}

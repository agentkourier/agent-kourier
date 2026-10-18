package slack_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

const channelsToken = "xoxb-CHANNELS-0123456789-abcdef"

// channelsStub answers conversations.list from pages (the cursor "" gets pages[0], the cursor "p1" gets pages[1], and so
// on) or, if fail is set, with that Slack error. It records the types it was asked for.
type channelsStub struct {
	mu    sync.Mutex
	types []string
	calls int
}

func (s *channelsStub) serve(t *testing.T, pages [][]map[string]string, fail string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/conversations.list") {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		s.mu.Lock()
		s.types = append(s.types, r.Form.Get("types"))
		s.calls++
		s.mu.Unlock()
		if fail != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fail})
			return
		}
		page, next := 0, ""
		if c := r.Form.Get("cursor"); c != "" {
			page = int(c[1] - '0')
		}
		if page+1 < len(pages) {
			next = "p" + string(rune('0'+page+1))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": pages[page], "response_metadata": map[string]string{"next_cursor": next}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

func listedChannel(id, name string) map[string]string { return map[string]string{"id": id, "name": name} }

func channelsClient(url string) *slack.Client {
	return slack.New(slack.Config{BotToken: secret.New(channelsToken), APIURL: url, PageLimit: 2})
}

// ChannelID finds a channel by its name over as many pages as it takes, public and private ones, and accepts the name
// with a leading # and in any case.
func TestChannelIDFindsAChannelByName(t *testing.T) {
	stub := &channelsStub{}
	url := stub.serve(t, [][]map[string]string{
		{listedChannel("C1", "general"), listedChannel("C2", "random")},
		{listedChannel("G3", "private-ops"), listedChannel("C4", "alerts")},
	}, "")
	c := channelsClient(url)
	for _, tc := range []struct{ name, want string }{
		{"general", "C1"}, {"#alerts", "C4"}, {"Private-Ops", "G3"}, {" alerts ", "C4"},
	} {
		got, err := c.ChannelID(t.Context(), tc.name)
		if err != nil || got != tc.want {
			t.Errorf("ChannelID(%q) = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	for _, types := range stub.types {
		if types != "public_channel,private_channel" {
			t.Errorf("asked for types %q; want public and private channels", types)
		}
	}
}

// A name that no page holds is ErrChannelNotFound, after the last page was read.
func TestChannelIDNotFound(t *testing.T) {
	stub := &channelsStub{}
	url := stub.serve(t, [][]map[string]string{{listedChannel("C1", "general")}, {listedChannel("C2", "random")}}, "")
	_, err := channelsClient(url).ChannelID(t.Context(), "nope")
	if !errors.Is(err, slack.ErrChannelNotFound) {
		t.Fatalf("err = %v; want ErrChannelNotFound", err)
	}
	if stub.calls != 2 {
		t.Errorf("made %d calls; want 2, one per page", stub.calls)
	}
}

// An empty name is refused without a call.
func TestChannelIDRefusesAnEmptyName(t *testing.T) {
	stub := &channelsStub{}
	url := stub.serve(t, [][]map[string]string{{}}, "")
	for _, name := range []string{"", "  ", "#"} {
		if _, err := channelsClient(url).ChannelID(t.Context(), name); err == nil || errors.Is(err, slack.ErrChannelNotFound) {
			t.Errorf("ChannelID(%q) err = %v; want an error that is not ErrChannelNotFound", name, err)
		}
	}
	if stub.calls != 0 {
		t.Errorf("made %d calls; want none", stub.calls)
	}
}

// Slack's error code survives for the caller to act on, and the token does not, even if Slack quotes it.
func TestChannelIDErrorKeepsTheCodeAndDropsTheToken(t *testing.T) {
	stub := &channelsStub{}
	url := stub.serve(t, nil, "missing_scope for "+channelsToken)
	_, err := channelsClient(url).ChannelID(t.Context(), "general")
	if err == nil {
		t.Fatal("no error")
	}
	var se slackgo.SlackErrorResponse
	if !errors.As(err, &se) || !strings.HasPrefix(se.Err, "missing_scope") {
		t.Errorf("err = %v; want a Slack error that starts missing_scope", err)
	}
	if strings.Contains(err.Error(), channelsToken) || strings.Contains(se.Err, channelsToken) {
		t.Errorf("the token is in the error: %v", err)
	}
	if !strings.Contains(err.Error(), "conversations.list") {
		t.Errorf("err = %v; want it to name conversations.list", err)
	}
}

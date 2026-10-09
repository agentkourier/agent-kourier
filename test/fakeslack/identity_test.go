package fakeslack_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

const testAppToken = "xapp-test-token" // obviously fake

// call sends a request the way slack-go does and returns the response body:
// the token in the Authorization header if header is set, and either a JSON body
// (as apps.connections.open sends) or a form carrying formToken.
func call(t *testing.T, fake *fakeslack.Server, method, header, formToken string, asJSON bool) string {
	t.Helper()
	form := url.Values{"channel": {"C1"}, "text": {"hi"}}
	if formToken != "" {
		form.Set("token", formToken)
	}
	body, contentType := form.Encode(), "application/x-www-form-urlencoded"
	if asJSON {
		body, contentType = "{}", "application/json"
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fake.URL()+"/"+method, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	if header != "" {
		req.Header.Set("Authorization", "Bearer "+header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return string(out)
}

// TestAuthDependsOnMethod checks that each method takes only its own token
// type, in the header or the form: apps.connections.open an app-level token, the
// Web API a bot token.
func TestAuthDependsOnMethod(t *testing.T) {
	const post, open = "chat.postMessage", "apps.connections.open"
	tests := []struct {
		name, method, header, formToken string
		asJSON                          bool
		want                            string
	}{
		{"web, bot token in the form", post, "", testToken, false, `"ok":true`},
		{"web, bot token in the header", post, testToken, "", false, `"ok":true`},
		{"web, app token in the form", post, "", testAppToken, false, `"error":"invalid_auth"`},
		{"web, app token in the header", post, testAppToken, "", false, `"error":"invalid_auth"`},
		{"web, no token", post, "", "", false, `"error":"not_authed"`},
		{"open, app token in the header, JSON body", open, testAppToken, "", true, `"url":"ws://`},
		{"open, app token in the form", open, "", testAppToken, false, `"url":"ws://`},
		{"open, bot token in the header", open, testToken, "", true, `"error":"invalid_auth"`},
		{"open, no token", open, "", "", true, `"error":"not_authed"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, _ := newFake(t)
			if body := call(t, fake, tc.method, tc.header, tc.formToken, tc.asJSON); !strings.Contains(body, tc.want) {
				t.Fatalf("%s = %s; want %s", tc.method, body, tc.want)
			}
		})
	}
}

func TestAuthTestReturnsTheBotAndTeam(t *testing.T) {
	tests := []struct {
		name                      string
		opts                      []fakeslack.Option
		user, bot, team, teamName string
	}{
		{"defaults", nil, "UFAKEBOT", "BFAKEBOT", "TFAKE", "fakeworkspace"},
		{"custom", []fakeslack.Option{fakeslack.WithBot("UBOT2", "BBOT2", "AAPP2"), fakeslack.WithTeam("T2", "acme")}, "UBOT2", "BBOT2", "T2", "acme"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, api := newFake(t, tc.opts...)
			got, err := api.AuthTest()
			if err != nil || got.UserID != tc.user || got.BotID != tc.bot || got.TeamID != tc.team || got.Team != tc.teamName {
				t.Fatalf("auth.test = %+v, %v; want user %s, bot %s, team %s (%s)", got, err, tc.user, tc.bot, tc.team, tc.teamName)
			}
		})
	}
}

func TestBotPostsCarryBotIdentity(t *testing.T) {
	methods := map[string]func(api *slack.Client, root string) (string, error){
		"chat.postMessage": func(api *slack.Client, root string) (string, error) {
			_, ts, err := api.PostMessage("D1", text("hi"), slack.MsgOptionTS(root))
			return ts, err
		},
		"chat.startStream": func(api *slack.Client, root string) (string, error) {
			_, ts, err := api.StartStream("D1", slack.MsgOptionTS(root), slack.MsgOptionMarkdownText("hi"))
			return ts, err
		},
		"chat.update": func(api *slack.Client, root string) (string, error) {
			_, ts, _, err := api.UpdateMessage("D1", root, text("edited"))
			return ts, err
		},
	}
	identities := []struct {
		name           string
		opts           []fakeslack.Option
		user, bot, app string
	}{
		{"defaults", nil, "UFAKEBOT", "BFAKEBOT", "AFAKEAPP"},
		{"custom", []fakeslack.Option{fakeslack.WithBot("UBOT2", "BBOT2", "AAPP2")}, "UBOT2", "BBOT2", "AAPP2"},
	}
	for method, post := range methods {
		for _, id := range identities {
			t.Run(method+"/"+id.name, func(t *testing.T) {
				fake, api := newFake(t, id.opts...)
				_, root, err := api.PostMessage("D1", text("root"))
				if err != nil {
					t.Fatalf("post root: %v", err)
				}
				ts, err := post(api, root)
				if err != nil {
					t.Fatalf("%s: %v", method, err)
				}
				var m fakeslack.Message
				for _, x := range fake.Messages("D1") {
					if x.TS == ts {
						m = x
					}
				}
				if m.User != id.user || m.BotID != id.bot || m.AppID != id.app {
					t.Fatalf("stored post has user/bot_id/app_id = %q/%q/%q; want %q/%q/%q", m.User, m.BotID, m.AppID, id.user, id.bot, id.app)
				}
				if method == "chat.update" && (m.Edited == nil || m.Edited.User != id.user) {
					t.Fatalf("edited = %+v; want the bot user %q", m.Edited, id.user)
				}
				assertOnTheWire(t, fake, api, root, ts, id.user, id.bot, id.app)
			})
		}
	}
}

// assertOnTheWire reads message ts back through conversations.replies of the
// thread root and checks the identity fields Agent Kourier would see. slack-go does
// not decode app_id, so that one is read from the raw response.
func assertOnTheWire(t *testing.T, fake *fakeslack.Server, api *slack.Client, root, ts, user, bot, app string) {
	t.Helper()
	replies, _, _, err := api.GetConversationReplies(&slack.GetConversationRepliesParameters{ChannelID: "D1", Timestamp: root})
	if err != nil {
		t.Fatalf("replies: %v", err)
	}
	found := 0
	for _, m := range replies {
		if m.Timestamp == ts {
			found++
			if m.User != user || m.BotID != bot {
				t.Fatalf("message %s has user/bot_id = %q/%q; want %q/%q", ts, m.User, m.BotID, user, bot)
			}
		}
	}
	var raw struct {
		Messages []struct {
			TS    string `json:"ts"`
			AppID string `json:"app_id"`
		}
	}
	body := rawBody(t, fake, "conversations.replies", url.Values{"token": {testToken}, "channel": {"D1"}, "ts": {root}})
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode replies: %v", err)
	}
	for _, m := range raw.Messages {
		if m.TS == ts {
			found++
			if m.AppID != app {
				t.Fatalf("message %s has app_id %q; want %q", ts, m.AppID, app)
			}
		}
	}
	if found != 2 {
		t.Fatalf("message %s found %d times in the thread; want once in each decoding", ts, found)
	}
}

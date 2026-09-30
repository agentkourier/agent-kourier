package fakeslack_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

const testAppToken = "xapp-test-token" // obviously fake

// TestAuthDependsOnMethod checks that each method takes only its own token
// type: apps.connections.open an app-level token, the Web API a bot token.
func TestAuthDependsOnMethod(t *testing.T) {
	tests := []struct {
		name, method, token, want string
	}{
		{"web method, bot token", "chat.postMessage", testToken, `"ok":true`},
		{"web method, app token", "chat.postMessage", testAppToken, `"error":"invalid_auth"`},
		{"web method, no token", "chat.postMessage", "", `"error":"not_authed"`},
		{"socket open, bot token", "apps.connections.open", testToken, `"error":"invalid_auth"`},
		{"socket open, no token", "apps.connections.open", "", `"error":"not_authed"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, _ := newFake(t)
			form := url.Values{"channel": {"C1"}, "text": {"hi"}}
			if tc.token != "" {
				form.Set("token", tc.token)
			}
			if body := rawBody(t, fake, tc.method, form); !strings.Contains(body, tc.want) {
				t.Fatalf("%s = %s; want %s", tc.method, body, tc.want)
			}
		})
	}
}

func TestBotPostsCarryBotIdentity(t *testing.T) {
	methods := map[string]func(api *slack.Client, root string) error{
		"chat.postMessage": func(api *slack.Client, _ string) error {
			_, _, err := api.PostMessage("D1", text("hi"))
			return err
		},
		"chat.startStream": func(api *slack.Client, root string) error {
			_, _, err := api.StartStream("D1", slack.MsgOptionTS(root), slack.MsgOptionMarkdownText("hi"))
			return err
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
				if err := post(api, root); err != nil {
					t.Fatalf("%s: %v", method, err)
				}
				msgs := fake.Messages("D1")
				m := msgs[len(msgs)-1]
				if m.User != id.user || m.BotID != id.bot || m.AppID != id.app {
					t.Fatalf("bot post has user/bot_id/app_id = %q/%q/%q; want %q/%q/%q",
						m.User, m.BotID, m.AppID, id.user, id.bot, id.app)
				}
				// The fields must reach the wire, where courier reads them.
				body := rawBody(t, fake, "conversations.history", url.Values{"token": {testToken}, "channel": {"D1"}})
				for _, want := range []string{`"bot_id":"` + id.bot + `"`, `"app_id":"` + id.app + `"`} {
					if !strings.Contains(body, want) {
						t.Fatalf("history = %s; want %s", body, want)
					}
				}
			})
		}
	}
}

package broker

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// fakeSlackWeb answers auth.test and bots.info as Slack does, for the bot tokens it knows. A token is a workspace, a bot user, a bot ID and
// the app the bot belongs to; a method listed in fail answers its error.
type fakeSlackWeb struct {
	srv *httptest.Server

	mu    sync.Mutex
	bots  map[string]struct{ team, user, bot, app string } // by bot token
	fail  map[string]string                                // method -> Slack error code
	calls []string
}

func newFakeSlackWeb(t *testing.T) *fakeSlackWeb {
	t.Helper()
	f := &fakeSlackWeb{bots: map[string]struct{ team, user, bot, app string }{}, fail: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		_ = r.ParseForm()
		token := cmp.Or(r.Form.Get("token"), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.mu.Lock()
		f.calls = append(f.calls, method)
		code, failing := f.fail[method]
		b, known := f.bots[token]
		f.mu.Unlock()
		reply := map[string]any{"ok": true}
		switch {
		case failing:
			reply = map[string]any{"ok": false, "error": code}
		case !known:
			reply = map[string]any{"ok": false, "error": "invalid_auth"}
		case method == "auth.test":
			reply["team_id"], reply["user_id"], reply["bot_id"] = b.team, b.user, b.bot
		case method == "bots.info" && r.Form.Get("bot") == b.bot:
			reply["bot"] = map[string]any{"id": b.bot, "user_id": b.user, "app_id": b.app}
		default:
			reply = map[string]any{"ok": false, "error": "bot_not_found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlackWeb) url() string { return f.srv.URL + "/" }

func connWithBotToken(tok string) Connection {
	return Connection{Ref: v1alpha1.ObjectRef{Namespace: "team-a", Name: "slack"}, Platform: v1alpha1.PlatformSlack,
		Credentials: map[string]secret.Value{"botToken": secret.New(tok), "appToken": secret.New("xapp-fake")}}
}

// One app installed in two workspaces has two bot users and two bot IDs, and one app ID: the identity says the app, so the two connections
// are one app. Two different apps in one workspace are two.
func TestTheIdentityOfAConnectionIsTheAppItsBotBelongsTo(t *testing.T) {
	f := newFakeSlackWeb(t)
	f.bots["xoxb-w1"] = struct{ team, user, bot, app string }{"TW1", "UW1", "BW1", "AAPP"}
	f.bots["xoxb-w2"] = struct{ team, user, bot, app string }{"TW2", "UW2", "BW2", "AAPP"}
	f.bots["xoxb-other"] = struct{ team, user, bot, app string }{"TW1", "UOTHER", "BOTHER", "AOTHER"}

	w1, err := slackIdentity(t.Context(), connWithBotToken("xoxb-w1"), f.url())
	if err != nil {
		t.Fatal(err)
	}
	w2, err := slackIdentity(t.Context(), connWithBotToken("xoxb-w2"), f.url())
	if err != nil {
		t.Fatal(err)
	}
	other, err := slackIdentity(t.Context(), connWithBotToken("xoxb-other"), f.url())
	if err != nil {
		t.Fatal(err)
	}
	if w1.App != "AAPP" || w1.App != w2.App || w1.Team == w2.Team || w1.Bot != "BW1" || w1.User != "UW1" {
		t.Errorf("identities of one app in two workspaces = %+v and %+v; want one app ID, and the bot and user of each", w1, w2)
	}
	if other.App == w1.App {
		t.Errorf("another app in the same workspace has the app ID %q; want it told apart", other.App)
	}
}

// A call that fails is an error and never a guess from the workspace and user: bots.info that fails, or answers with no app, or auth.test that
// gives no bot, is no identity. The token never reaches the error, and one that Slack refuses wraps errCredentialsRejected.
func TestAnIdentityCallThatFailsIsAnErrorAndNeverAGuess(t *testing.T) {
	f := newFakeSlackWeb(t)
	f.bots["xoxb-fake-token-w1"] = struct{ team, user, bot, app string }{"TW1", "UW1", "BW1", "AAPP"}
	conn := connWithBotToken("xoxb-fake-token-w1")

	f.fail["bots.info"] = "missing_scope"
	id, err := slackIdentity(t.Context(), conn, f.url())
	if err == nil || id.known() || !strings.Contains(err.Error(), "bots.info") || !strings.Contains(err.Error(), "missing_scope") || errors.Is(err, errCredentialsRejected) {
		t.Errorf("slackIdentity with bots.info failing = %+v, %v; want an error naming it, which is no rejection, and no identity", id, err)
	}
	if strings.Contains(err.Error(), "xoxb-fake-token") {
		t.Errorf("the error holds the token: %v", err)
	}

	delete(f.fail, "bots.info")
	f.bots["xoxb-fake-token-w1"] = struct{ team, user, bot, app string }{"TW1", "UW1", "BW1", ""}
	if id, err := slackIdentity(t.Context(), conn, f.url()); err == nil || id.known() {
		t.Errorf("slackIdentity with no app in the answer = %+v, %v; want an error", id, err)
	}

	f.fail["auth.test"] = "token_revoked"
	if _, err := slackIdentity(t.Context(), conn, f.url()); !errors.Is(err, errCredentialsRejected) {
		t.Errorf("slackIdentity with the token refused = %v; want errCredentialsRejected", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if last := f.calls[len(f.calls)-1]; last != "auth.test" {
		t.Errorf("the last call was %s; want bots.info not to be asked when auth.test failed", last)
	}
}

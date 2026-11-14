//go:build testhooks

package broker

import (
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// fakeWorld is a connWorld whose Slack is the fake, through the real client and the real identity calls (auth.test and bots.info), with the
// own connection and each team app registered under its own tokens and the fake strict about them.
func fakeWorld(t *testing.T) (*connWorld, *fakeslack.Server, map[string]fakeslack.App) {
	t.Helper()
	apps := map[string]fakeslack.App{
		"own":    {AppID: "AOWN", BotUser: "UOWN", BotID: "BOWN", BotToken: "xoxb-own-token", AppToken: "xapp-own-token"},
		"team-a": {AppID: "AAPPA", BotUser: "UBOTA", BotID: "BBOTA", BotToken: "xoxb-a-token", AppToken: "xapp-a-token"},
		"team-b": {AppID: "AAPPB", BotUser: "UBOTB", BotID: "BBOTB", BotToken: "xoxb-b-token", AppToken: "xapp-b-token"},
	}
	opts := []fakeslack.Option{fakeslack.WithStrictTokens()}
	for _, a := range apps {
		opts = append(opts, fakeslack.WithApp(a))
	}
	fake := fakeslack.New(opts...)
	t.Cleanup(fake.Close)

	w := newConnWorld(t, nil)
	w.c.newSlack, w.c.identify = nil, nil // the real client and the real identity calls, against the fake
	w.c.hooks.SlackAPIURL = fake.URL() + "/"
	own := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "agent-kourier-system", Name: "slack-main"},
		Data: map[string][]byte{"botToken": []byte(apps["own"].BotToken), "appToken": []byte(apps["own"].AppToken)}}
	if _, err := w.kube.CoreV1().Secrets("agent-kourier-system").Update(t.Context(), own, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	return w, fake, apps
}

// A team connection is started on the identity Slack gives: auth.test and then bots.info, made against the fake as a broker makes them against
// Slack. Each app runs its own Socket Mode connection, and a bots.info that fails (the fake answers missing_scope) holds the start back until it works.
func TestATeamConnectionStartsOnTheIdentityBotsInfoGives(t *testing.T) {
	t.Parallel()
	w, fake, apps := fakeWorld(t)
	runsAlongside(&w.c)
	for _, ns := range []string{"team-a", "team-b"} {
		w.secretOf(ns, apps[ns].BotToken, apps[ns].AppToken)
		w.connection(ns)
	}
	fake.SetMissingScope("bots.info", true)
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)

	w.eventually("the own connection to run", func() bool { return fake.AppConnections("AOWN") == 1 })
	w.waitForLog("is not known yet, so no team connection starts")
	if fake.AppConnections("AAPPA") != 0 || fake.AppConnections("AAPPB") != 0 {
		t.Fatalf("team connections run (a: %d, b: %d) while bots.info fails; want them held back", fake.AppConnections("AAPPA"), fake.AppConnections("AAPPB"))
	}

	fake.SetMissingScope("bots.info", false)
	w.eventually("both team apps to connect once bots.info answers", func() bool {
		return fake.AppConnections("AAPPA") == 1 && fake.AppConnections("AAPPB") == 1
	})
	if fake.AppConnections("AOWN") != 1 {
		t.Errorf("the own app has %d connections; want 1", fake.AppConnections("AOWN"))
	}
	if strings.Contains(w.logs.String(), "xoxb-") || strings.Contains(w.logs.String(), "xapp-") {
		t.Error("the log holds a token")
	}
}

// A team app whose token the fake refuses (revoked) is not started and is not asked about again: Slack's answer is that the token is no good.
func TestATeamConnectionWhoseTokenSlackRefusesIsNotStartedOrAskedAgain(t *testing.T) {
	t.Parallel()
	w, fake, apps := fakeWorld(t)
	runsAlongside(&w.c)
	w.secretOf("team-a", apps["team-a"].BotToken, apps["team-a"].AppToken)
	w.connection("team-a")
	if err := fake.SetRevoked("AAPPA", true); err != nil {
		t.Fatal(err)
	}
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)

	w.waitForLog("the platform rejected the ChatConnection's credentials")
	if fake.AppConnections("AAPPA") != 0 {
		t.Errorf("the revoked app has %d connections; want none", fake.AppConnections("AAPPA"))
	}
	if got := strings.Count(w.logs.String(), "the platform rejected the ChatConnection's credentials"); got != 1 {
		t.Errorf("the rejection is in the log %d times; want it said once, and the token not asked about again", got)
	}
}

// waitForLog waits for a line of the broker's log that holds text.
func (w *connWorld) waitForLog(text string) {
	w.t.Helper()
	w.eventually("the log to say "+text, func() bool { return strings.Contains(w.logs.String(), text) })
}

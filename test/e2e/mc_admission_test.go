//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
)

// TestMC_ATeamTakenOffTheAdmissionListIsNotServed is the scenario of the platform's admission list (config.connectionNamespaces, which the broker reads at
// start, so that a change to it is a Helm upgrade and a rollout). hack/kind/two-apps.sh (make e2e-two-apps) runs it in two phases around the upgrades, and
// it skips without one:
//
//	removed   team-a is taken off the list. Its ChatConnection is still in the cluster and the broker does not serve it: its Binding says
//	          Accepted=False with the reason ConnectionNotServed (the broker served it before and does not now, which it knows from the record in its store,
//	          since a restart forgets what it ran), its app has no client, and a mention of its bot gets no reply. The platform's connection and Binding are
//	          not touched: its Binding answers, through the platform's app.
//	restored  team-a is put back on the list. The broker serves the connection again, its Binding is accepted and ready, and answers through team-a's app,
//	          and the platform's Binding goes on answering through its own.
//
// The fast tier on Postgres with two replicas, on custom resources, with E2E_TWO_APPS=1.
func TestMC_ATeamTakenOffTheAdmissionListIsNotServed(t *testing.T) {
	env := NewEnv(os.Getenv)
	switch env.Phase {
	case "":
		t.Skip("this scenario is run in phases by hack/kind/two-apps.sh (make e2e-two-apps), which changes the platform's admission list with Helm between them")
	case "removed", "restored":
	default:
		t.Fatalf("E2E_PHASE is %q; want removed or restored", env.Phase)
	}
	if err := CheckTier(env.Tier); err != nil {
		t.Fatal(err)
	}
	if env.Tier != "fast" || env.ConfigSource != "crds" || env.ConnectionNamespaces == "" {
		t.Fatalf("phase %s runs in the fast tier on custom resources with an admission list, brought up with E2E_TWO_APPS=1; this one is tier %s, config source %s, admitted namespaces %q", env.Phase, env.Tier, env.ConfigSource, env.ConnectionNamespaces)
	}
	if want := env.Phase == "restored"; env.Admits(kube.MultiTeamNamespace) != want {
		t.Fatalf("phase %s with the admission list %q: team-a should be on it only in phase restored", env.Phase, env.ConnectionNamespaces)
	}
	m := newMCBare(t)
	if err := m.slack.SetMembers(m.ctx, ChannelMCPlatform, AppPlatform.BotUser); err != nil {
		t.Fatal(err)
	}
	if err := m.slack.Join(m.ctx, ChannelMCTeamA, AppTeamA.AppID); err != nil {
		t.Fatal(err)
	}

	if env.Phase == "restored" {
		m.waitApp(AppTeamA, 3*time.Minute)
		m.waitUp(ConnTeamA)
		m.waitCondition(kube.MultiTeamNamespace, "team-a", "Accepted", "True", "")
		m.waitCondition(kube.MultiTeamNamespace, "team-a", "Ready", "True", "")
		m.postedBy(m.answered(AppTeamA, ChannelMCTeamA, crServeTimeout), AppTeamA)
		m.postedBy(m.answered(AppPlatform, ChannelMCPlatform, crServeTimeout), AppPlatform)
		return
	}

	cond := m.waitCondition(kube.MultiTeamNamespace, "team-a", "Accepted", "False", "ConnectionNotServed")
	if msg := fmt.Sprint(cond["message"]); !strings.Contains(msg, "team-a/team-a") || !strings.Contains(msg, "not served by this broker") {
		t.Errorf("Accepted says %q; want it to name the connection team-a/team-a and say that this broker does not serve it", msg)
	}
	m.waitCondition(kube.MultiTeamNamespace, "team-a", "Ready", "False", "Accepted:ConnectionNotServed")

	// The platform's connection and Binding are the broker's still.
	m.postedBy(m.answered(AppPlatform, ChannelMCPlatform, crServeTimeout), AppPlatform)
	m.waitUp(ConnPlatform)
	m.waitPlatformBindingReady()

	// team-a's app has no client, and a mention of its bot, which the fake sends to the apps that are connected and in the channel, gets no reply.
	um, err := m.say(AppTeamA, ChannelMCTeamA, mcUser, "anyone there?")
	if err != nil {
		t.Fatal(err)
	}
	err = Consistently(m.ctx, 20*time.Second, "team-a's app to have no client and its channel no reply", func(ctx context.Context) (bool, error) {
		n, err := m.slack.AppConnections(ctx, AppTeamA.AppID)
		if err != nil || n != 0 {
			return false, err
		}
		th, err := m.slack.Thread(ctx, ChannelMCTeamA, um.TS)
		return len(BotMessages(th)) == 0, err
	})
	if err != nil {
		th, _ := m.slack.Thread(context.WithoutCancel(m.ctx), ChannelMCTeamA, um.TS)
		t.Errorf("%v\nthe thread: %s", err, describe(th))
	}
}

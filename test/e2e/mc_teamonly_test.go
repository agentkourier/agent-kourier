//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
)

// TestMC_ABrokerWithNoOwnConnectionServesTheTeams is the scenario of a broker that has no ChatConnection of its own (spec D53): the Deployment has no
// AGENTKOURIER_CHAT_CONNECTION, and the broker serves the ChatConnections of the namespaces in AGENTKOURIER_CONNECTION_NAMESPACES, each as a team's.
// hack/kind/two-apps.sh (make e2e-two-apps) runs it in three phases between Helm upgrades (the admission list and the variable are read at start, so
// each change is an upgrade and a rollout), and it skips without one. The script checks, for each phase, that the Deployment has no own connection
// and what the platform's Bindings say, which the runner has no right to read.
//
//	no-own             config.chatConnection is empty and the list is the tier's team namespaces. The platform's ChatConnection (the broker's own until
//	                   now) is not on the list, so it is not served: its app has no client, a mention of its bot gets no reply, and the leader has no
//	                   connection series for it, and counts its Bindings under ConnectionNotServed. team-a's connection is served and answers through its
//	                   app. Every replica is Ready, a standby included, and one leads although no connection of its own is up.
//	platform-admitted  the platform's namespace is put on the list. The same connection is served as a team connection: it is up, its Binding is ready
//	                   and answers through the platform's app, no Binding is counted under ConnectionNotServed, and team-a's goes on answering.
//	takeover           on two replicas, in the state of platform-admitted: the leader's container is killed and the replica that takes over serves every
//	                   team connection, both apps with their one client, both Bindings answering, and every replica Ready.
//
// The fast tier on custom resources, with E2E_TWO_APPS=1; takeover needs Postgres and two replicas (E2E_STORE=postgres E2E_REPLICAS=2).
func TestMC_ABrokerWithNoOwnConnectionServesTheTeams(t *testing.T) {
	env := NewEnv(os.Getenv)
	switch env.Phase {
	case "":
		t.Skip("this scenario is run in phases by hack/kind/two-apps.sh (make e2e-two-apps), which changes the platform's admission list and drops its own connection with Helm between them")
	case "no-own", "platform-admitted", "takeover":
	default:
		t.Fatalf("E2E_PHASE is %q; want no-own, platform-admitted or takeover", env.Phase)
	}
	if err := CheckTier(env.Tier); err != nil {
		t.Fatal(err)
	}
	if env.Tier != "fast" || env.ConfigSource != "crds" || !env.Admits(kube.MultiTeamNamespace) {
		t.Fatalf("phase %s runs in the fast tier on custom resources with team-a's namespace on the admission list, brought up with E2E_TWO_APPS=1; this one is tier %s, config source %s, admitted namespaces %q", env.Phase, env.Tier, env.ConfigSource, env.ConnectionNamespaces)
	}
	if want := env.Phase != "no-own"; env.Admits(kube.AgentKourierNamespace) != want {
		t.Fatalf("phase %s with the admission list %q: the platform's namespace %s should be on it only after phase no-own", env.Phase, env.ConnectionNamespaces, kube.AgentKourierNamespace)
	}
	switch env.Phase {
	case "no-own":
		noOwn(t, env)
	case "platform-admitted":
		platformAdmitted(t)
	case "takeover":
		takeoverWithNoOwn(t)
	}
}

// noOwn is phase no-own.
func noOwn(t *testing.T, env Env) {
	m := newMCTeamOnly(t, env)
	// The tier seats the bots when it is brought up; a fake that was restarted since has forgotten.
	if err := m.slack.SetMembers(m.ctx, ChannelMCPlatform, AppPlatform.BotUser); err != nil {
		t.Fatal(err)
	}
	if err := m.slack.Join(m.ctx, ChannelMCTeamA, AppTeamA.AppID); err != nil {
		t.Fatal(err)
	}

	m.readyEverywhere()
	m.waitApp(AppTeamA, 3*time.Minute)
	m.waitConnection(ConnTeamA, 1)
	m.waitCondition(kube.MultiTeamNamespace, "team-a", "Accepted", "True", "")
	m.waitCondition(kube.MultiTeamNamespace, "team-a", "Ready", "True", "")
	m.postedBy(m.answered(AppTeamA, ChannelMCTeamA, crServeTimeout), AppTeamA)

	// The platform's connection is not on the list, so it is not served: no series for it that says it is up, and its Bindings (mc-platform, mc-platform-final
	// and mc-platform-loop, which the script reads) are counted under ConnectionNotServed.
	err := Wait(m.ctx, 2*time.Minute, "the leader to have no series that says "+ConnPlatform+" is up", func(ctx context.Context) (bool, error) {
		v, err := m.leaderSample(ctx, "agentkourier_chat_connected", map[string]string{"connection": ConnPlatform})
		if errors.Is(err, ErrNoSample) {
			return true, nil
		}
		return v == 0, err
	})
	if err != nil {
		t.Error(err)
	}
	m.waitRejections("ConnectionNotServed", func(n float64) bool { return n >= 3 }, "at least 3 (the platform's three Bindings of the tier with two apps)")

	// Its app has no client, and a mention of its bot gets no reply.
	err = Wait(m.ctx, 2*time.Minute, "no client of the platform's app", func(ctx context.Context) (bool, error) {
		n, err := m.slack.AppConnections(ctx, AppPlatform.AppID)
		return n == 0, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if um, err := m.say(AppPlatform, ChannelMCPlatform, mcUser, "anyone there?"); err != nil {
		// The fake answers 409 when no client of the apps in the channel is connected, which is the answer to a mention of an app nothing serves.
		if !strings.Contains(err.Error(), "status 409") {
			t.Errorf("the mention of the platform's bot failed with %v; want it posted, or refused by the fake with 409 because the app has no client", err)
		}
	} else {
		err = Consistently(m.ctx, 20*time.Second, "the platform's app to have no client and its channel no reply", func(ctx context.Context) (bool, error) {
			n, err := m.slack.AppConnections(ctx, AppPlatform.AppID)
			if err != nil || n != 0 {
				return false, err
			}
			th, err := m.slack.Thread(ctx, ChannelMCPlatform, um.TS)
			return len(BotMessages(th)) == 0, err
		})
		if err != nil {
			th, _ := m.slack.Thread(context.WithoutCancel(m.ctx), ChannelMCPlatform, um.TS)
			t.Errorf("%v\nthe thread: %s", err, describe(th))
		}
	}

	// team-a's connection goes on, and every replica is still Ready.
	m.postedBy(m.answered(AppTeamA, ChannelMCTeamA, crServeTimeout), AppTeamA)
	m.readyEverywhere()
}

// platformAdmitted is phase platform-admitted.
func platformAdmitted(t *testing.T) {
	m := newMC(t) // both connections up, which waits for the leader to say so
	m.within = mcProbeWithin
	for _, c := range []struct {
		app     FakeApp
		channel string
	}{{AppPlatform, ChannelMCPlatform}, {AppTeamA, ChannelMCTeamA}} {
		m.postedBy(m.answered(c.app, c.channel, crServeTimeout), c.app)
		m.channelPostedBy(c.channel, c.app)
	}
	m.waitPlatformBindingReady()
	m.waitCondition(kube.MultiTeamNamespace, "team-a", "Ready", "True", "")
	m.waitRejections("ConnectionNotServed", func(n float64) bool { return n == 0 }, "0: the platform's connection is served again")
	m.readyEverywhere()
}

// takeoverWithNoOwn is phase takeover.
func takeoverWithNoOwn(t *testing.T) {
	m, f := newMCFailover(t)
	old := f.leader()
	before := f.restarts()
	if err := f.sig.Kill(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	next := f.takeover(old)
	for _, conn := range []string{ConnPlatform, ConnTeamA} {
		err := Wait(m.ctx, 2*time.Minute, "agentkourier_chat_connected{connection="+conn+"} to be 1 in "+next.ID(), func(ctx context.Context) (bool, error) {
			v, err := f.reps.Counter(ctx, next, "agentkourier_chat_connected", map[string]string{"connection": conn})
			if errors.Is(err, ErrNoSample) {
				return false, nil
			}
			return v == 1, err
		})
		if err != nil {
			t.Error(err)
		}
	}
	for _, app := range []FakeApp{AppPlatform, AppTeamA} {
		err := Consistently(m.ctx, 5*time.Second, "the app "+app.AppID+" to have its one client", func(ctx context.Context) (bool, error) {
			n, err := m.slack.AppConnections(ctx, app.AppID)
			return n == 1, err
		})
		if err != nil {
			t.Error(err)
		}
	}
	for _, c := range []struct {
		app     FakeApp
		channel string
	}{{AppPlatform, ChannelMCPlatform}, {AppTeamA, ChannelMCTeamA}} {
		m.postedBy(m.answered(c.app, c.channel, crServeTimeout), c.app)
	}
	f.settle()
	f.onlyEnded(before, old.Name)
	m.readyEverywhere()
}

// newMCTeamOnly is newMCBare for a broker that may serve no connection of the platform's: it does not wait, as newScenario does, for a leader whose
// connection agent-kourier/slack-fast is up, or for the platform's app to have a client, which is the very thing that is gone.
func newMCTeamOnly(t *testing.T, env Env) *mc {
	t.Helper()
	crs, err := kube.NewCRsInCluster()
	if err != nil {
		t.Fatal(err)
	}
	pods, err := kube.NewInCluster()
	if err != nil {
		t.Fatal(err)
	}
	s := &scenario{t: t, ctx: t.Context(), env: env, slack: NewSlack(env.SlackURL), stub: NewStub(env.StubURL), agentKourier: NewAgentKourier(env.AgentKourierURL), pods: pods, reps: NewReplicas(pods)}
	return &mc{scenario: s, crs: crs, within: statusWithin}
}

// readyEverywhere checks that /readyz answers 200 on every replica, the standby of two included: readiness does not depend on a connection. It returns how
// many replicas there are.
func (m *mc) readyEverywhere() int {
	m.t.Helper()
	all, err := m.reps.All(m.ctx)
	if err != nil {
		m.t.Fatal(err)
	}
	if len(all) == 0 {
		m.t.Fatal("no Agent Kourier replica is running")
	}
	for _, r := range all {
		if err := NewAgentKourier(replicaURL(r)).WaitReady(m.ctx, 2*time.Minute); err != nil {
			m.t.Errorf("replica %s: %v", r.ID(), err)
		}
	}
	return len(all)
}

// leaderSample reads a sample of the one replica that leads, which with no connection of its own is any replica that holds the lease: unlike
// (*Replicas).WaitLeader it does not ask for the platform's connection to be up. It waits for exactly one leader.
func (m *mc) leaderSample(ctx context.Context, name string, labels map[string]string) (float64, error) {
	leaders, err := m.reps.Leaders(ctx)
	if err != nil {
		return 0, err
	}
	if len(leaders) != 1 {
		return 0, fmt.Errorf("%d replicas say they lead; want 1", len(leaders))
	}
	return m.reps.Counter(ctx, leaders[0], name, labels)
}

// waitConnection waits until the leader says agentkourier_chat_connected is want for the connection.
func (m *mc) waitConnection(conn string, want float64) {
	m.t.Helper()
	err := Wait(m.ctx, 3*time.Minute, fmt.Sprintf("agentkourier_chat_connected{connection=%s} to be %v in the leader", conn, want), func(ctx context.Context) (bool, error) {
		v, err := m.leaderSample(ctx, "agentkourier_chat_connected", map[string]string{"connection": conn})
		if errors.Is(err, ErrNoSample) {
			return false, nil
		}
		return v == want, err
	})
	if err != nil {
		m.t.Fatal(err)
	}
}

// waitRejections waits until the leader's agentkourier_config_binding_rejections for the reason satisfies ok; want says what ok asks, for the failure.
func (m *mc) waitRejections(reason string, ok func(float64) bool, want string) {
	m.t.Helper()
	err := Wait(m.ctx, 2*time.Minute, fmt.Sprintf("agentkourier_config_binding_rejections{reason=%s} to be %s", reason, want), func(ctx context.Context) (bool, error) {
		v, err := m.leaderSample(ctx, "agentkourier_config_binding_rejections", map[string]string{"reason": reason})
		if errors.Is(err, ErrNoSample) {
			return false, nil
		}
		return ok(v), err
	})
	if err != nil {
		m.t.Fatal(err)
	}
}

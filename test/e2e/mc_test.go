//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// The scenarios of a broker that serves several ChatConnections, each with a Slack app of its own (spec D52): the platform's connection (slack-fast, the
// fake Slack's default app) and team-a's, which brings its own app, its own Secret and the Role that grants it (hack/kind/fast/two-apps), with a Binding on
// each. The fast tier on custom resources, brought up with E2E_TWO_APPS=1 (hack/kind/up-fast.sh), on Postgres with two replicas for the failover cases
// (E2E_STORE=postgres E2E_REPLICAS=2); anywhere else they skip and say why. Which app answered is read from the fake Slack, as the bot and app IDs of the
// messages in a thread: each app has its own.
//
// The cases that change the platform's admission list with Helm are mc_admission_test.go's, and those of a broker with no ChatConnection of its own,
// with the platform's namespace off the list and then on it, are mc_teamonly_test.go's (hack/kind/two-apps.sh runs both). The ones about a token that
// Slack rejects are mc_isolation_test.go's.

// mcUser is the person who talks to the bots in these cases.
const mcUser = "UMC"

// The names of the Secrets of a case in team-b or team-c (the runner's Roles name them: hack/kind/e2e/runner.yaml): the Slack tokens and the agent token.
const (
	mcSlackSecret = "mc-slack"
	mcTokenSecret = "mc-token"
)

// mc is a scenario of the tier with two Slack apps.
type mc struct {
	*scenario
	crs *kube.CRs
	// within is how long a wait for a Binding's status goes on: statusWithin, which is for a status that a change of config calls for (a probe pass follows
	// within 30 seconds), or mcProbeWithin for one that only the next pass brings.
	within time.Duration
}

// mcProbeWithin is how long a Binding that was not ready when its bot was put in its channel may wait for its status: the probes of a Binding run every five
// minutes, with some jitter, and a change of config is what brings a pass sooner.
const mcProbeWithin = 7 * time.Minute

// requireTwoApps skips unless the tier is the fast tier on custom resources and its broker admits team-a's namespace, and returns the environment.
func requireTwoApps(t *testing.T) Env {
	t.Helper()
	env := NewEnv(os.Getenv)
	if err := CheckTier(env.Tier); err != nil {
		t.Fatal(err)
	}
	if env.Tier != "fast" {
		t.Skipf("the scenarios of several Slack apps reach the stub agent and the fake Slack's apps, which the %s tier does not run", env.Tier)
	}
	admitted, err := env.Admits(kube.MultiTeamNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted {
		t.Skipf("the broker does not admit the ChatConnections of %s here (%s); bring the tier up with: E2E_TWO_APPS=1 E2E_CONFIG_SOURCE=crds make e2e-fast-up (add E2E_STORE=postgres E2E_REPLICAS=2 for the failover cases), and run them with: make e2e-two-apps (the Job of make e2e-fast leaves them out)",
			kube.MultiTeamNamespace, env.Admission())
	}
	return env
}

// newMC returns a scenario of the tier with two Slack apps, with the bots in their channels and both connections up (prepare).
func newMC(t *testing.T) *mc {
	t.Helper()
	requireTwoApps(t)
	m := newMCBare(t)
	m.prepare()
	return m
}

// newMCBare is newMC without prepare, for a case that does not expect both connections to be up.
func newMCBare(t *testing.T) *mc {
	t.Helper()
	crs, err := kube.NewCRsInCluster()
	if err != nil {
		t.Fatal(err)
	}
	return &mc{scenario: newScenario(t), crs: crs, within: statusWithin}
}

// newMCFailover is newMC on two replicas, with the signaler that kills the leader (it skips with S16's reason on one).
func newMCFailover(t *testing.T) (*mc, *failover) {
	t.Helper()
	requireTwoApps(t)
	crs, err := kube.NewCRsInCluster()
	if err != nil {
		t.Fatal(err)
	}
	f := newFailover(t)
	m := &mc{scenario: f.scenario, crs: crs, within: statusWithin}
	m.prepare()
	return m, f
}

// prepare puts each app's bot in the channels of its Bindings (the fake delivers a non-default app's events only to the channels its bot is in, and the
// Binding verification asks the connection's own bot) and waits until team-a's app has a Socket Mode client, which is the broker serving its connection,
// and until the leader says both connections are up.
func (m *mc) prepare() {
	m.t.Helper()
	for _, channel := range []string{ChannelMCPlatform, ChannelMCPlatformFinal} {
		if err := m.slack.SetMembers(m.ctx, channel, AppPlatform.BotUser); err != nil {
			m.t.Fatal(err)
		}
	}
	for _, channel := range []string{ChannelMCTeamA, ChannelMCTeamAFinal} {
		if err := m.slack.Join(m.ctx, channel, AppTeamA.AppID); err != nil {
			m.t.Fatal(err)
		}
	}
	m.waitApp(AppTeamA, 3*time.Minute)
	for _, conn := range []string{ConnPlatform, ConnTeamA} {
		m.waitUp(conn)
	}
}

// waitApp waits until the app has a Socket Mode client at the fake Slack.
func (m *mc) waitApp(app FakeApp, within time.Duration) {
	m.t.Helper()
	err := Wait(m.ctx, within, "a Socket Mode client of the app "+app.AppID, func(ctx context.Context) (bool, error) {
		n, err := m.slack.AppConnections(ctx, app.AppID)
		return n >= 1, err
	})
	if err != nil {
		m.t.Fatal(err)
	}
}

// waitUp waits until the leader says the connection is up: agentkourier_chat_connected is 1 for it.
func (m *mc) waitUp(conn string) {
	m.t.Helper()
	err := Wait(m.ctx, 3*time.Minute, "agentkourier_chat_connected{connection="+conn+"} to be 1", func(ctx context.Context) (bool, error) {
		v, err := m.leaderCounter(ctx, "agentkourier_chat_connected", map[string]string{"connection": conn})
		if errors.Is(err, ErrNoSample) {
			return false, nil
		}
		return v == 1, err
	})
	if err != nil {
		m.t.Fatal(err)
	}
}

// say mentions the app's bot in channel as user, and waits for every app that was sent the event to ack it. A connection that is not up refuses the post
// (the fake answers 409 when no client of the apps in the channel is connected): that is the error returned, and the caller tries again.
func (m *mc) say(app FakeApp, channel, user, text string) (fakeslack.UserMessage, error) {
	if err := m.slack.Know(m.ctx, user); err != nil {
		return fakeslack.UserMessage{}, err
	}
	um, err := m.slack.Mention(m.ctx, channel, user, app.Mention()+text)
	if err != nil {
		return um, err
	}
	for _, id := range um.Envelopes {
		if _, err := m.slack.AckWait(m.ctx, 15*time.Second, id); err != nil {
			return um, fmt.Errorf("the event %s of the mention was not acked: %w", id, err)
		}
	}
	return um, nil
}

// answered mentions the app's bot in channel until a Binding answers, and returns the thread with the bot's finished reply: a mention that nothing serves
// yet is ignored, with no reply, so it is made again until one is answered or the time is out.
func (m *mc) answered(app FakeApp, channel string, within time.Duration) []fakeslack.MessageView {
	m.t.Helper()
	deadline := time.Now().Add(within)
	var last error
	for attempt := 1; ; attempt++ {
		um, err := m.say(app, channel, mcUser, fmt.Sprintf("hello %d", attempt))
		if err == nil {
			var th []fakeslack.MessageView
			th, err = m.slack.WaitThread(m.ctx, 12*time.Second, channel, um.TS, func(th []fakeslack.MessageView) bool {
				bots := BotMessages(th)
				return len(bots) >= 1 && finished(bots[len(bots)-1])
			})
			if err == nil {
				return th
			}
			err = fmt.Errorf("%w; the thread: %s", err, describe(th))
		}
		last = err
		if m.ctx.Err() != nil || time.Now().After(deadline) {
			m.t.Fatalf("no Binding answered a mention of %s in %s within %s: %v", app.BotUser, channel, within, last)
		}
		time.Sleep(3 * time.Second)
	}
}

// postedBy checks that the thread holds at least one message of a bot and that every one is the app's: its bot ID and its app ID, which the fake gives each
// app of its own, so a post that went out through another connection's client shows.
func (m *mc) postedBy(th []fakeslack.MessageView, app FakeApp) {
	m.t.Helper()
	bots := BotMessages(th)
	if len(bots) == 0 {
		m.t.Errorf("no message of a bot in the thread; want the app %s's\nthe thread: %s", app.AppID, describe(th))
	}
	for _, b := range bots {
		if b.BotID != app.BotID || b.AppID != app.AppID || b.User != app.BotUser {
			m.t.Errorf("the message %q was posted by bot %s of app %s as %s; want bot %s of app %s as %s", b.Text, b.BotID, b.AppID, b.User, app.BotID, app.AppID, app.BotUser)
		}
	}
}

// channelPostedBy checks that everything a bot posted in channel, in any thread, is the app's.
func (m *mc) channelPostedBy(channel string, app FakeApp) {
	m.t.Helper()
	msgs, err := m.slack.Messages(m.ctx, channel)
	if err != nil {
		m.t.Fatal(err)
	}
	m.postedBy(msgs, app)
}

// condition reads a status condition of the Binding.
func (m *mc) condition(ctx context.Context, namespace, name, typ string) (map[string]any, error) {
	obj, err := m.crs.Get(ctx, kube.BindingGVR, namespace, name)
	if err != nil {
		return nil, err
	}
	cond, ok := condition(obj, typ)
	if !ok {
		return nil, fmt.Errorf("Binding %s/%s has no %s condition yet", namespace, name, typ)
	}
	return cond, nil
}

// waitCondition waits until the Binding's condition has the status and, if reason is not empty, the reason; it returns the condition.
func (m *mc) waitCondition(namespace, name, typ, status, reason string) map[string]any {
	m.t.Helper()
	var found map[string]any
	err := Wait(m.ctx, m.within, fmt.Sprintf("the %s condition of Binding %s/%s to be %s %s", typ, namespace, name, status, reason), func(ctx context.Context) (bool, error) {
		cond, err := m.condition(ctx, namespace, name, typ)
		if err != nil {
			return false, err
		}
		found = cond
		if cond["status"] != status || reason != "" && cond["reason"] != reason {
			return false, fmt.Errorf("it is %v %v: %v", cond["status"], cond["reason"], cond["message"])
		}
		return true, nil
	})
	if err != nil {
		m.t.Fatal(err)
	}
	return found
}

// waitPlatformBindingReady waits until the leader says the platform's Binding is ready: agentkourier_binding_ready is 1 for it.
func (m *mc) waitPlatformBindingReady() {
	m.t.Helper()
	err := Wait(m.ctx, m.within, "agentkourier_binding_ready{binding=agent-kourier/mc-platform} to be 1", func(ctx context.Context) (bool, error) {
		v, err := m.leaderCounter(ctx, "agentkourier_binding_ready", map[string]string{"binding": "agent-kourier/mc-platform"})
		if errors.Is(err, ErrNoSample) {
			return false, nil
		}
		return v == 1, err
	})
	if err != nil {
		m.t.Fatal(err)
	}
}

// restarts says how many times each replica's container has restarted, by pod, as one string that two moments can be compared by.
func (m *mc) restarts() string {
	m.t.Helper()
	all, err := m.reps.All(m.ctx)
	if err != nil {
		m.t.Fatal(err)
	}
	var out []string
	for _, r := range all {
		out = append(out, fmt.Sprintf("%s restarts=%d", r.ID(), r.Restarts))
	}
	return strings.Join(out, ", ")
}

// unsignedJWT is an agent token for the stub agent (-require-auth), which asks of a bearer an email claim and nothing else: a fake constant.
func unsignedJWT(email string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(`{"email":"`+email+`"}`)) + "."
}

// scratch is a team that a case plays in team-b or team-c: its own Slack tokens (those of the app it is given) and an agent token, each granted to the
// broker by a Role that names it, an AgentBackend that is the stub agent, and the ChatConnections and Bindings the case makes. Everything it made is
// removed when the case ends, and what an earlier run left in the namespace before it starts: the namespace is the case's alone.
type scratch struct {
	*mc
	crs *kube.CRs
	ns  string
}

// newScratch plays a team in namespace ns (kube.ScratchNamespace or kube.ScratchNamespace2) whose Slack tokens are those of app.
func (m *mc) newScratch(ns string, app FakeApp) *scratch {
	m.t.Helper()
	crs, err := m.crs.In(ns)
	if err != nil {
		m.t.Fatal(err)
	}
	sc := &scratch{mc: m, crs: crs, ns: ns}
	m.t.Cleanup(sc.remove)
	sc.remove()
	for name, data := range map[string]map[string]string{
		mcSlackSecret: {"botToken": app.BotToken, "appToken": app.AppToken},
		mcTokenSecret: {"token": unsignedJWT("e2e-" + ns + "@example.test")},
	} {
		if err := crs.CreateSecret(m.ctx, name, data); err != nil {
			m.t.Fatal(err)
		}
		if err := crs.GrantSecret(m.ctx, name); err != nil {
			m.t.Fatal(err)
		}
	}
	sc.create(kube.AgentBackendGVR, "AgentBackend", "agents", map[string]any{"dialect": "kagent-v1", "url": m.env.StubURL})
	return sc
}

// remove deletes what the case made, Bindings first so that nothing serves on a connection that is gone, then the connections and backends, then the
// Secrets and their grants. Each is by what the namespace holds, and a missing one is not an error.
func (sc *scratch) remove() {
	sc.t.Helper()
	ctx := context.WithoutCancel(sc.ctx)
	for _, gvr := range []schema.GroupVersionResource{kube.BindingGVR, kube.ChatConnectionGVR, kube.AgentBackendGVR} {
		if err := sc.crs.Purge(ctx, gvr); err != nil {
			sc.t.Errorf("clean up %s in %s: %v", gvr.Resource, sc.ns, err)
		}
	}
	for _, name := range []string{mcSlackSecret, mcTokenSecret} {
		if err := sc.crs.RemoveSecret(ctx, name); err != nil {
			sc.t.Errorf("clean up Secret %s in %s: %v", name, sc.ns, err)
		}
	}
}

// create makes an object of the namespace from its spec.
func (sc *scratch) create(gvr schema.GroupVersionResource, kind, name string, spec map[string]any) {
	sc.t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentkourier.dev/v1alpha1", "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": sc.ns},
		"spec":     spec,
	}}
	if _, err := sc.crs.Create(sc.ctx, gvr, obj); err != nil {
		sc.t.Fatal(err)
	}
}

// connection makes the ChatConnection name, whose credentials are the case's Slack tokens.
func (sc *scratch) connection(name string) {
	sc.t.Helper()
	sc.create(kube.ChatConnectionGVR, "ChatConnection", name, map[string]any{"platform": "slack", "credentialsSecretRef": map[string]any{"name": mcSlackSecret}})
}

// binding makes the Binding name on the connection and channel, answered by the stub's scenario-gated with the case's agent token.
func (sc *scratch) binding(name, connection, channel string) {
	sc.t.Helper()
	sc.create(kube.BindingGVR, "Binding", name, map[string]any{
		"agent":    map[string]any{"backendRef": map[string]any{"name": "agents"}, "namespace": "kagent", "name": "scenario-gated"},
		"identity": map[string]any{"userId": "e2e-" + sc.ns, "tokenSecretRef": map[string]any{"name": mcTokenSecret}},
		"chat":     map[string]any{"connectionRef": map[string]any{"name": connection}, "channel": channel, "output": "live"},
	})
}

// TestMC_BothBindingsAnswerThroughTheirOwnApp: the platform's Binding and team-a's, on two connections of two Slack apps, each answer a mention of their own
// bot, and each answer is posted by that app's bot and by no other: the bot ID and the app ID the fake gives each app are the app's own, in every message of
// the channel. Both connections are up and both Bindings are ready, which for team-a's means the broker asked the channel's members with team-a's bot (the
// platform's bot is not in it): the Binding verification goes through the Binding's own connection.
func TestMC_BothBindingsAnswerThroughTheirOwnApp(t *testing.T) {
	m := newMC(t)
	m.within = mcProbeWithin // the tier puts the bots in their channels when it is brought up; a fake that was restarted since has forgotten, and the next pass finds out
	for _, c := range []struct {
		app     FakeApp
		channel string
	}{{AppPlatform, ChannelMCPlatform}, {AppTeamA, ChannelMCTeamA}} {
		m.postedBy(m.answered(c.app, c.channel, crServeTimeout), c.app)
		m.channelPostedBy(c.channel, c.app)
	}
	for _, conn := range []string{ConnPlatform, ConnTeamA} {
		m.waitUp(conn)
	}
	m.waitPlatformBindingReady()
	m.waitCondition(kube.MultiTeamNamespace, "team-a", "Ready", "True", "")
	m.waitCondition(kube.MultiTeamNamespace, "team-a", "ChannelJoined", "True", "")
}

// TestMC_AConnectionAddedWhileTheBrokerRunsIsServed: a third team, in team-b, which the platform admits, makes its Slack tokens and the Role that grants them,
// a ChatConnection and a Binding, and the broker, not restarted, connects the new app and the Binding answers through it: the new bot's post, not the
// platform's or team-a's. The other two keep answering. When the team removes its connection the app's client goes.
func TestMC_AConnectionAddedWhileTheBrokerRunsIsServed(t *testing.T) {
	m := newMC(t)
	// newScratch removes what an earlier run left in the namespace, a connection of team-b's app included (a Job cut off by its timeout runs no cleanup);
	// the broker stops that app's client then, and the case starts from none.
	sc := m.newScratch(kube.ScratchNamespace, AppTeamB)
	err := Wait(m.ctx, 2*time.Minute, "no client of the app "+AppTeamB.AppID+" before its team connects", func(ctx context.Context) (bool, error) {
		n, err := m.slack.AppConnections(ctx, AppTeamB.AppID)
		return n == 0, err
	})
	if err != nil {
		t.Fatal(err)
	}
	before := m.restarts()
	if err := m.slack.Join(m.ctx, ChannelMCTeamB, AppTeamB.AppID); err != nil {
		t.Fatal(err)
	}
	sc.connection("team-b")
	sc.binding("team-b", "team-b", ChannelMCTeamB)

	m.waitApp(AppTeamB, 3*time.Minute)
	m.postedBy(m.answered(AppTeamB, ChannelMCTeamB, crServeTimeout), AppTeamB)
	m.channelPostedBy(ChannelMCTeamB, AppTeamB)
	m.waitUp("team-b/team-b")
	for _, c := range []struct {
		app     FakeApp
		channel string
	}{{AppPlatform, ChannelMCPlatform}, {AppTeamA, ChannelMCTeamA}} {
		m.postedBy(m.answered(c.app, c.channel, crServeTimeout), c.app)
	}
	if after := m.restarts(); after != before {
		t.Errorf("the replicas were %s before the connection and %s after: the broker restarted for a connection it was to serve while it ran", before, after)
	}

	// The team takes its connection away: the broker stops that app's client, and the others go on.
	if err := sc.crs.Delete(m.ctx, kube.BindingGVR, sc.ns, "team-b"); err != nil {
		t.Fatal(err)
	}
	if err := sc.crs.Delete(m.ctx, kube.ChatConnectionGVR, sc.ns, "team-b"); err != nil {
		t.Fatal(err)
	}
	err = Wait(m.ctx, 2*time.Minute, "the client of the app "+AppTeamB.AppID+" to go with its connection", func(ctx context.Context) (bool, error) {
		n, err := m.slack.AppConnections(ctx, AppTeamB.AppID)
		return n == 0, err
	})
	if err != nil {
		t.Error(err)
	}
	m.postedBy(m.answered(AppTeamA, ChannelMCTeamA, crServeTimeout), AppTeamA)
}

// TestMC_ASecondConnectionOfOneAppIsRefused: a team in team-c makes a ChatConnection with the Slack tokens of team-a's app, which the broker serves already.
// One app's events are spread over its connections, so two intakes of it would each miss some: the older connection, team-a's, is the one served, the app
// has the one client it had, and the new connection's Binding says ConnectionNotServed in its Accepted condition. Team-a keeps answering.
func TestMC_ASecondConnectionOfOneAppIsRefused(t *testing.T) {
	m := newMC(t)
	sc := m.newScratch(kube.ScratchNamespace2, AppTeamA)
	if err := m.slack.Join(m.ctx, ChannelMCDuplicate, AppTeamA.AppID); err != nil {
		t.Fatal(err)
	}
	sc.connection("team-a-again")
	sc.binding("again", "team-a-again", ChannelMCDuplicate)

	cond := m.waitCondition(sc.ns, "again", "Accepted", "False", "ConnectionNotServed")
	if msg := fmt.Sprint(cond["message"]); !strings.Contains(msg, "same Slack app") {
		t.Errorf("Accepted says %q; want it to say another connection of the same Slack app is served", msg)
	}
	m.waitCondition(sc.ns, "again", "Ready", "False", "Accepted:ConnectionNotServed")

	// The older connection is the one served: team-a answers, and the app has the one client, which it keeps.
	m.postedBy(m.answered(AppTeamA, ChannelMCTeamA, crServeTimeout), AppTeamA)
	err := Consistently(m.ctx, 15*time.Second, "the app "+AppTeamA.AppID+" to keep its one client", func(ctx context.Context) (bool, error) {
		n, err := m.slack.AppConnections(ctx, AppTeamA.AppID)
		return n == 1, err
	})
	if err != nil {
		t.Error(err)
	}
	// Nothing answers in the channel of the refused connection's Binding: a mention of team-a's bot there (the bot is a member) has no Binding to serve it.
	um, err := m.say(AppTeamA, ChannelMCDuplicate, mcUser, "anyone there?")
	if err != nil {
		t.Fatal(err)
	}
	err = Consistently(m.ctx, 15*time.Second, "the refused connection's channel to get no reply", func(ctx context.Context) (bool, error) {
		th, err := m.slack.Thread(ctx, ChannelMCDuplicate, um.TS)
		return len(BotMessages(th)) == 0, err
	})
	if err != nil {
		th, _ := m.slack.Thread(context.WithoutCancel(m.ctx), ChannelMCDuplicate, um.TS)
		t.Errorf("%v\nthe thread: %s", err, describe(th))
	}
}

// TestMC_FailoverServesBothConnections: the leader is killed, and the replica that takes over connects both apps: agentkourier_chat_connected is 1 for both
// connections in the new leader, each app has its one client, and both Bindings answer through their own app. Only the leader's process ended.
//
// The fast tier on Postgres with two replicas (E2E_STORE=postgres E2E_REPLICAS=2 E2E_TWO_APPS=1 E2E_CONFIG_SOURCE=crds make e2e-fast-up).
func TestMC_FailoverServesBothConnections(t *testing.T) {
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
}

// acknowledged waits until the app's bot has put the acknowledgment on the message at ts of channel, which it does once the broker has taken the message up
// and while its turn waits or runs: the reaction is the app's own, so it is also the first thing that shows which app the broker answered with.
func (m *mc) acknowledged(app FakeApp, channel, ts string) {
	m.t.Helper()
	err := Wait(m.ctx, stepTimeout, "the acknowledgment of "+app.BotUser+" on the message "+ts, func(ctx context.Context) (bool, error) {
		rs, err := m.slack.Reactions(ctx, channel, ts)
		for _, r := range rs {
			if r.Name == queuedReaction && slices.Contains(r.Users, app.BotUser) {
				return true, err
			}
		}
		return false, err
	})
	if err != nil {
		m.t.Fatal(err)
	}
}

// TestMC_OutboxRowsPostThroughTheirOwnAppAfterAKill: a turn is held on a gate in each connection's channel, whose Binding has the output final, so that the
// reply is one post through the outbox when the turn ends. The fake holds each app's chat.postMessage calls, and the gates open: each turn ends, and its
// reply is a row whose post is certainly in flight, waiting at the fake, when the leader is killed (the case waits until the fake counts a held post of each
// app before it kills). The held posts are abandoned with the dead process and nothing is stored. The replica that takes over finds the rows, and once the
// fake lets the posts go each thread has the one whole reply, posted through the connection of its row, by the bot of that app and no other.
//
// The fast tier on Postgres with two replicas (see TestMC_FailoverServesBothConnections). A reply streamed live is the next case's.
func TestMC_OutboxRowsPostThroughTheirOwnAppAfterAKill(t *testing.T) {
	m, f := newMCFailover(t)
	type turn struct {
		app     FakeApp
		channel string
		gate    string
		root    fakeslack.UserMessage
	}
	turns := []*turn{
		{app: AppPlatform, channel: ChannelMCPlatformFinal},
		{app: AppTeamA, channel: ChannelMCTeamAFinal},
	}
	for _, tn := range turns {
		app := tn.app
		t.Cleanup(func() { // a hold left on would stop every later case that posts as the app
			if err := m.slack.HoldPosts(context.WithoutCancel(m.ctx), app.AppID, false); err != nil {
				t.Errorf("let %s's posts go at the end: %v", app.AppID, err)
			}
		})
	}
	old := f.leader()
	before := f.restarts()
	for i, tn := range turns {
		tn.gate = f.gate(fmt.Sprintf("MC-%d", i))
		if err := m.slack.HoldPosts(m.ctx, tn.app.AppID, true); err != nil {
			t.Fatal(err)
		}
		um, err := m.say(tn.app, tn.channel, "UMCOUTBOX", "answer me gate:"+tn.gate)
		if err != nil {
			t.Fatal(err)
		}
		tn.root = um
		m.acknowledged(tn.app, tn.channel, um.TS)
	}
	// The turns end, and the row of each is on its way to the fake, which holds it: nothing is stored until the posts are let go.
	for _, tn := range turns {
		if err := f.stub.Release(m.ctx, tn.gate); err != nil {
			t.Fatal(err)
		}
	}
	for _, tn := range turns {
		err := Wait(m.ctx, stepTimeout, "a post of "+tn.app.AppID+" to wait on the fake's hold", func(ctx context.Context) (bool, error) {
			n, err := m.slack.HeldPosts(ctx, tn.app.AppID)
			return n >= 1, err
		})
		if err != nil {
			t.Fatal(err)
		}
		if th, err := m.slack.Thread(m.ctx, tn.channel, tn.root.TS); err != nil || len(BotMessages(th)) != 0 {
			t.Fatalf("the thread of %s has bot messages (%v) before its post is let go: %s", tn.app.AppID, err, describe(th))
		}
	}

	if err := f.sig.Kill(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	f.takeover(old)
	for _, tn := range turns {
		if err := m.slack.HoldPosts(m.ctx, tn.app.AppID, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, tn := range turns {
		th := f.thread(tn.channel, tn.root.TS, "the reply of "+tn.app.AppID+" to be posted, once, after the takeover", answered(tn.gate, ""))
		m.postedBy(th, tn.app)
	}
	err := Consistently(f.ctx, 5*time.Second, "each thread keeps its one whole reply, from its own app", func(ctx context.Context) (bool, error) {
		for _, tn := range turns {
			th, err := f.slack.Thread(ctx, tn.channel, tn.root.TS)
			if err != nil || !answered(tn.gate, "")(th) {
				return false, err
			}
			for _, b := range BotMessages(th) {
				if b.BotID != tn.app.BotID {
					return false, fmt.Errorf("the thread of %s holds a message of the bot %s", tn.app.AppID, b.BotID)
				}
			}
		}
		return true, nil
	})
	if err != nil {
		t.Error(err)
	}
	f.settle()
	f.onlyEnded(before, old.Name)
}

// bindingSignals are the leader's counters that a message moves when a Binding takes it in: a trigger matched, what was decided about it (started, queued,
// limited and so on), and a turn that ended. A match that is limited or fails moves no turn outcome, so the first two are held as well as the third.
type bindingSignals struct{ matched, decisions, outcomes float64 }

// signals reads the counters of the Binding from the leader, 0 for a series that is not there yet.
func (m *mc) signals(binding string) bindingSignals {
	m.t.Helper()
	sum := func(name string) float64 {
		labels := map[string]string{"binding": binding}
		many, err := m.replicated(m.ctx)
		var v float64
		if err == nil && many {
			var leader kube.Replica
			if leader, err = m.reps.WaitLeader(m.ctx, 2*time.Minute); err == nil {
				v, err = m.reps.Sum(m.ctx, leader, name, labels)
			}
		} else if err == nil {
			v, err = m.agentKourier.Sum(m.ctx, name, labels)
		}
		if errors.Is(err, ErrNoSample) {
			return 0
		}
		if err != nil {
			m.t.Fatal(err)
		}
		return v
	}
	return bindingSignals{sum("agentkourier_trigger_matched_total"), sum("agentkourier_trigger_decisions_total"), sum("agentkourier_turn_outcomes_total")}
}

// settledSignals is signals once they have stopped moving: the turn that answered a person a moment ago counts its outcome after its reply is posted, and
// that increment must not land inside a window that watches for another.
func (m *mc) settledSignals(binding string) bindingSignals {
	m.t.Helper()
	last, stable := m.signals(binding), 0
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		time.Sleep(time.Second)
		if now := m.signals(binding); now == last {
			if stable++; stable >= 4 {
				return now
			}
		} else {
			last, stable = now, 0
		}
	}
	m.t.Fatalf("the counters of %s are still moving after 30 seconds: %+v", binding, last)
	return last
}

// platformFakeBotToken is the bot token of the fake Slack's default app, the one the platform's Secret slack-fast holds: a token no registered app owns.
const platformFakeBotToken = "xoxb-fake-test-token"

// botClient is the Web API as the app's bot, to post as it does.
func (m *mc) botClient(app FakeApp) *slack.Client {
	token := app.BotToken
	if token == "" {
		token = platformFakeBotToken
	}
	return slack.New(token, slack.OptionAPIURL(m.env.SlackURL+"/"))
}

// botsInLoop are the two apps of ChannelMCLoop with their Bindings there, as the label of agentkourier_turn_outcomes_total names them.
var botsInLoop = []struct {
	app     FakeApp
	binding string
}{
	{AppPlatform, "agent-kourier/mc-platform-loop"},
	{AppTeamA, "team-a/team-a-loop"},
}

// TestMC_ABotsPostStartsNoTurnOfAnotherAppsBinding is the rule against a loop between two apps (D52): the bots of two apps are in one channel, each with a
// Binding of its own connection there, and a message that one bot posts starts no turn of the other's Binding, whatever it says. For each bot as the poster, the
// other bot's Binding has answered a person who mentioned it, and so owns a thread; the poster then posts, as its own bot, a message that mentions the other
// at the top level of the channel and one in the thread the other owns. The other app is sent the events of both and acks them, which is its intake having
// seen them, and for as long as the case watches its Binding starts no turn (none of the Binding's trigger and turn counters moves) and says nothing in either
// thread. The Binding still answers a person afterwards.
func TestMC_ABotsPostStartsNoTurnOfAnotherAppsBinding(t *testing.T) {
	m := newMC(t)
	if err := m.slack.SetMembers(m.ctx, ChannelMCLoop, AppPlatform.BotUser, AppTeamA.BotUser); err != nil {
		t.Fatal(err)
	}
	for _, c := range botsInLoop {
		m.postedBy(m.answered(c.app, ChannelMCLoop, crServeTimeout), c.app)
	}
	botMessagesOf := func(channel, ts string, app FakeApp) int {
		th, err := m.slack.Thread(m.ctx, channel, ts)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, b := range BotMessages(th) {
			if b.BotID == app.BotID {
				n++
			}
		}
		return n
	}
	for _, pair := range [][2]int{{0, 1}, {1, 0}} {
		poster, other := botsInLoop[pair[0]], botsInLoop[pair[1]]
		owned := m.answered(other.app, ChannelMCLoop, crServeTimeout) // a thread the other bot's Binding owns
		thread := owned[0].TS
		baseline, ownedBefore := m.settledSignals(other.binding), botMessagesOf(ChannelMCLoop, thread, other.app)
		if ownedBefore == 0 {
			t.Fatalf("%s has no message in the thread it owns: %s", other.app.AppID, describe(owned))
		}

		bot := m.botClient(poster.app)
		_, top, err := bot.PostMessageContext(m.ctx, ChannelMCLoop, slack.MsgOptionText(other.app.Mention()+"please investigate this", false))
		if err != nil {
			t.Fatal(err)
		}
		_, reply, err := bot.PostMessageContext(m.ctx, ChannelMCLoop, slack.MsgOptionText(other.app.Mention()+"and this", false), slack.MsgOptionTS(thread))
		if err != nil {
			t.Fatal(err)
		}
		// The other app took both events in: it was sent them, and acked them.
		for _, ts := range []string{top, reply} {
			ids, err := m.slack.EventEnvelopes(m.ctx, other.app.AppID, ChannelMCLoop, ts)
			if err != nil || len(ids) == 0 {
				t.Fatalf("%s was sent no event for the post %s of %s (%v); the case proves nothing without one", other.app.AppID, ts, poster.app.AppID, err)
			}
			for _, id := range ids {
				if _, err := m.slack.AckWait(m.ctx, 15*time.Second, id); err != nil {
					t.Fatalf("%s never acked the event %s: %v", other.app.AppID, id, err)
				}
			}
		}
		err = Consistently(m.ctx, 15*time.Second, "the Binding of "+other.app.AppID+" to start no turn for a post of "+poster.app.AppID, func(context.Context) (bool, error) {
			if got := m.signals(other.binding); got != baseline {
				return false, fmt.Errorf("the counters of %s are %+v, from %+v: a bot's post moved them", other.binding, got, baseline)
			}
			if n := botMessagesOf(ChannelMCLoop, top, other.app); n != 0 {
				return false, fmt.Errorf("%s answered the top-level post of %s", other.app.AppID, poster.app.AppID)
			}
			if n := botMessagesOf(ChannelMCLoop, thread, other.app); n != ownedBefore {
				return false, fmt.Errorf("%s has %d messages in its thread, from %d: it answered the post of %s", other.app.AppID, n, ownedBefore, poster.app.AppID)
			}
			return true, nil
		})
		if err != nil {
			t.Error(err)
		}
		// Its Binding is not broken: a person is still answered.
		m.postedBy(m.answered(other.app, ChannelMCLoop, crServeTimeout), other.app)
	}
}

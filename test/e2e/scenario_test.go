//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// botMention is how a person addresses Agent Kourier in a channel: the fake's bot user.
const botMention = "<@UFAKEBOT> "

// queuedReaction is the default acknowledgment emoji on accepted messages while they wait or run.
const queuedReaction = "hourglass_flowing_sand"

// stepTimeout is how long a scenario waits for one thing Agent Kourier does: a stream starting, a reply ending.
const stepTimeout = 60 * time.Second

// scenario is what every Phase 1 scenario drives, all in-cluster. A scenario never opens a Socket Mode
// client of its own: the fake sends an envelope to the newest connection, and Agent Kourier must be the one to
// receive it. Every assertion is about this run's thread, found by the ts of the message the scenario
// posted, and about text the scenario made unique, because the channels, the stub and the fake outlive a
// run.
type scenario struct {
	t            *testing.T
	ctx          context.Context
	env          Env
	slack        *Slack
	stub         *Stub
	agentKourier *AgentKourier
	pods         *kube.Pods
	reps         *Replicas
}

// newScenario returns what a scenario needs, once Agent Kourier is ready and connected to the fake. Both tiers
// have Agent Kourier, and a scenario fails if it is not up.
func newScenario(t *testing.T) *scenario {
	t.Helper()
	env := NewEnv(os.Getenv)
	if err := CheckTier(env.Tier); err != nil {
		t.Fatal(err)
	}
	pods, err := kube.NewInCluster()
	if err != nil {
		t.Fatal(err)
	}
	s := &scenario{t: t, ctx: t.Context(), env: env, slack: NewSlack(env.SlackURL), stub: NewStub(env.StubURL), agentKourier: NewAgentKourier(env.AgentKourierURL), pods: pods, reps: NewReplicas(pods)}
	s.connected()
	return s
}

// a2aScenario is newScenario for the scenarios run through the generic a2a dialect against the stub agent in its plain pod. Their
// Bindings are the fast tier's (hack/kind/fast/agent-kourier/a2a/binding-a2a-*.yaml): the full tier has no stub agent, and the AX tier's
// own Bindings are a2a already, on the channels of the kagent scenarios, through AX's router.
func a2aScenario(t *testing.T) *scenario {
	t.Helper()
	s := newScenario(t)
	if s.env.Tier != "fast" {
		t.Skip("the a2a Bindings of the plain pod are the fast tier's: they reach the stub agent, which the " + s.env.Tier + " tier does not run")
	}
	return s
}

// replicated says whether Agent Kourier runs more than one pod now: with two replicas on Postgres one leads and holds the Socket Mode
// connection, and the standby does nothing, so what only the leader has (its counters, its connection) is looked for there.
func (s *scenario) replicated(ctx context.Context) (bool, error) {
	list, err := s.pods.List(ctx, kube.AgentKourierNamespace, agentKourierPods)
	n := 0
	for _, p := range list {
		if p.DeletionTimestamp == nil {
			n++
		}
	}
	return n > 1, err
}

// connected waits for Agent Kourier to be ready and to hold a Socket Mode connection to the fake, which a mention
// needs (the fake answers 409 with no client). Call it again after anything that restarts Agent Kourier. With two replicas it
// waits for exactly one of them to lead and be connected too: ready is true of a standby, and the connection the fake
// counts may be the one of a leader that is on its way out.
func (s *scenario) connected() {
	s.t.Helper()
	if err := s.agentKourier.WaitReady(s.ctx, 2*time.Minute); err != nil {
		s.t.Fatal(err)
	}
	if many, err := s.replicated(s.ctx); err != nil {
		s.t.Fatal(err)
	} else if many {
		if _, err := s.reps.WaitLeader(s.ctx, 2*time.Minute); err != nil {
			s.t.Fatal(err)
		}
	}
	err := Wait(s.ctx, 2*time.Minute, "Agent Kourier to connect to the fake Slack", func(ctx context.Context) (bool, error) {
		n, err := s.slack.Connections(ctx)
		return n >= 1, err
	})
	if err != nil {
		s.t.Fatal(err)
	}
}

// acked waits for Agent Kourier to acknowledge every event the fake delivered for m, which it does once it has
// taken the message up.
func (s *scenario) acked(m fakeslack.UserMessage) {
	s.t.Helper()
	for _, id := range m.Envelopes {
		if _, err := s.slack.AckWait(s.ctx, 15*time.Second, id); err != nil {
			s.t.Fatalf("Agent Kourier never acked the event %s: %v", id, err)
		}
	}
}

// know tells the fake of user, once, so that Agent Kourier's users.info for them finds one: it asks for the
// person who wrote a message (to say who decided, and in the audit log), and a user the fake does not
// know is a Warn in Agent Kourier's log for no fault of its own.
func (s *scenario) know(user string) {
	s.t.Helper()
	if err := s.slack.Know(s.ctx, user); err != nil {
		s.t.Fatal(err)
	}
}

// mention posts text to Agent Kourier in channel as user, and waits for it to be taken up.
func (s *scenario) mention(channel, user, text string) fakeslack.UserMessage {
	s.t.Helper()
	s.know(user)
	m, err := s.slack.Mention(s.ctx, channel, user, botMention+text)
	if err != nil {
		s.t.Fatal(err)
	}
	s.acked(m)
	return m
}

// reply posts text in the thread at ts as user, and waits for it to be taken up.
func (s *scenario) reply(channel, ts, user, text string) fakeslack.UserMessage {
	s.t.Helper()
	s.know(user)
	m, err := s.slack.Reply(s.ctx, channel, ts, user, text)
	if err != nil {
		s.t.Fatal(err)
	}
	s.acked(m)
	return m
}

// thread waits for the thread at ts in channel to satisfy pred, and returns it. desc says what it waits for.
func (s *scenario) thread(channel, ts, desc string, pred func([]fakeslack.MessageView) bool) []fakeslack.MessageView {
	s.t.Helper()
	thread, err := s.slack.WaitThread(s.ctx, stepTimeout, channel, ts, pred)
	if err != nil {
		s.t.Fatalf("%s: %v\nthe thread: %s", desc, err, describe(thread))
	}
	return thread
}

// describe is a thread for a failure message: who said what, and whether a stream was still open.
func describe(thread []fakeslack.MessageView) string {
	var b strings.Builder
	for _, m := range thread {
		who := m.User
		if m.BotID != "" {
			who = "bot"
		}
		state := ""
		if m.Streaming {
			state = " (streaming)"
		}
		b.WriteString("\n  " + who + state + ": " + m.Text)
	}
	return b.String()
}

// leaderCounter reads one of Agent Kourier's samples from the process that does its work: the Service's pod when there is one
// replica, and the leader's own pod when there are two (the Service would answer from either, and a standby has none of
// the counters of the work).
func (s *scenario) leaderCounter(ctx context.Context, name string, labels map[string]string) (float64, error) {
	many, err := s.replicated(ctx)
	if err != nil {
		return 0, err
	}
	if !many {
		return s.agentKourier.Counter(ctx, name, labels)
	}
	leader, err := s.reps.WaitLeader(ctx, 2*time.Minute)
	if err != nil {
		return 0, err
	}
	return s.reps.Counter(ctx, leader, name, labels)
}

// leaderAuditEntries reads persisted audit rows from the process doing the work. With Postgres, the standby can answer
// the health endpoint too, but the leader is the process whose store receives the decision first.
func (s *scenario) leaderAuditEntries(ctx context.Context) ([]store.AuditEntry, error) {
	many, err := s.replicated(ctx)
	if err != nil {
		return nil, err
	}
	if !many {
		return s.agentKourier.AuditEntries(ctx)
	}
	leader, err := s.reps.WaitLeader(ctx, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	return s.reps.AuditEntries(ctx, leader)
}

// counter reads one of the leader's counters (see leaderCounter), which is 0 until it has been incremented: it is not
// exposed before.
func (s *scenario) counter(name string, labels map[string]string) float64 {
	s.t.Helper()
	v, err := s.leaderCounter(s.ctx, name, labels)
	if errors.Is(err, ErrNoSample) {
		return 0
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return v
}

// finished reports whether m is a bot message whose text has ended: the stream, if there was one, is closed.
func finished(m fakeslack.MessageView) bool { return !m.Streaming }

//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// botMention is how a person addresses courier in a channel: the fake's bot user.
const botMention = "<@UFAKEBOT> "

// queuedReaction is the emoji courier puts on a reply that waits for the active task (spec 8.2).
const queuedReaction = "hourglass_flowing_sand"

// stepTimeout is how long a scenario waits for one thing courier does: a stream starting, a reply ending.
const stepTimeout = 60 * time.Second

// scenario is what every Phase 1 scenario drives, all in-cluster. A scenario never opens a Socket Mode
// client of its own: the fake sends an envelope to the newest connection, and courier must be the one to
// receive it. Every assertion is about this run's thread, found by the ts of the message the scenario
// posted, and about text the scenario made unique, because the channels, the stub and the fake outlive a
// run.
type scenario struct {
	t       *testing.T
	ctx     context.Context
	env     Env
	slack   *Slack
	stub    *Stub
	courier *Courier
	pods    *kube.Pods
}

// newScenario returns what a scenario needs, once courier is ready and connected to the fake. The fast tier
// has courier, and a scenario fails there if it is not up; the full tier gets it with WP-35i, and the
// scenario is skipped until then.
func newScenario(t *testing.T) *scenario {
	t.Helper()
	env := NewEnv(os.Getenv)
	expected, err := CourierExpected(env.Tier)
	if err != nil {
		t.Fatal(err)
	}
	if !expected {
		t.Skip("TODO(WP-35i): the full tier has no courier yet")
	}
	pods, err := kube.NewInCluster()
	if err != nil {
		t.Fatal(err)
	}
	s := &scenario{t: t, ctx: t.Context(), env: env, slack: NewSlack(env.SlackURL), stub: NewStub(env.StubURL), courier: NewCourier(env.CourierURL), pods: pods}
	s.connected()
	return s
}

// connected waits for courier to be ready and to hold a Socket Mode connection to the fake, which a mention
// needs (the fake answers 409 with no client). Call it again after anything that restarts courier.
func (s *scenario) connected() {
	s.t.Helper()
	if err := s.courier.WaitReady(s.ctx, 2*time.Minute); err != nil {
		s.t.Fatal(err)
	}
	err := Wait(s.ctx, 2*time.Minute, "courier to connect to the fake Slack", func(ctx context.Context) (bool, error) {
		n, err := s.slack.Connections(ctx)
		return n >= 1, err
	})
	if err != nil {
		s.t.Fatal(err)
	}
}

// acked waits for courier to acknowledge every event the fake delivered for m, which it does once it has
// taken the message up.
func (s *scenario) acked(m fakeslack.UserMessage) {
	s.t.Helper()
	for _, id := range m.Envelopes {
		if _, err := s.slack.AckWait(s.ctx, 15*time.Second, id); err != nil {
			s.t.Fatalf("courier never acked the event %s: %v", id, err)
		}
	}
}

// know tells the fake of user, once, so that courier's users.info for them finds one: it asks for the
// person who wrote a message (to say who decided, and in the audit log), and a user the fake does not
// know is a Warn in courier's log for no fault of its own.
func (s *scenario) know(user string) {
	s.t.Helper()
	if err := s.slack.Know(s.ctx, user); err != nil {
		s.t.Fatal(err)
	}
}

// mention posts text to courier in channel as user, and waits for it to be taken up.
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

// counter reads one of courier's counters, which is 0 until it has been incremented: it is not exposed before.
func (s *scenario) counter(name string, labels map[string]string) float64 {
	s.t.Helper()
	v, err := s.courier.Counter(s.ctx, name, labels)
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

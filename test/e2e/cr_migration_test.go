//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// migrationWithin is how long a phase waits for a session that has been through a move of the broker: a new leader takes the Lease (nine seconds
// after the last renewal in the fast tier on two replicas), starts, reads its config and recovers the session.
const migrationWithin = 4 * time.Minute

// gateInText finds the gate a message of a migration session names.
var gateInText = regexp.MustCompile(`gate:(\S+)`)

// TestCR_MigrationKeepsASessionAlive is run by hack/kind/crs-migration.sh, which moves a release between the ConfigMap route and the custom resources and
// back with Helm while a session is in flight, in phases (E2E_PHASE): it skips in an ordinary run, since it only means something between two Helm
// upgrades. A session here is a mention that the agent holds on a gate and a reply to it that waits in the queue behind that task, in the channel of a
// Binding that the release's own values render. open-N starts session N and checks that it is held and queued; finish-N finds it again, in whatever
// Agent Kourier is now running, and releases it: the first task ends, with its text once; the queued reply is then answered, once. A thread that lost
// either, or says something else, fails the phase and is printed, so that what a user would have seen is in the log.
//
// It runs in the fast tier, on Postgres with two replicas, where a move is a failover: the new leader recovers what the old one was doing.
func TestCR_MigrationKeepsASessionAlive(t *testing.T) {
	env := NewEnv(os.Getenv)
	if env.Phase == "" {
		t.Skip("a phase of hack/kind/crs-migration.sh (E2E_PHASE=open-1, finish-1, ...): it runs between the Helm upgrades of a migration, and means nothing alone")
	}
	kind, n, ok := strings.Cut(env.Phase, "-")
	if !ok || (kind != "open" && kind != "finish") || n == "" {
		t.Fatalf("E2E_PHASE = %q; want open-N or finish-N", env.Phase)
	}
	if env.Tier != "fast" {
		t.Skipf("the migration runs in the fast tier, which has the stub agent; this is the %s tier", env.Tier)
	}
	s := newScenario(t)
	switch kind {
	case "open":
		migrationOpen(t, s, n)
	case "finish":
		migrationFinish(t, s, n)
	}
}

// migrationOpen starts session n: the first task held on a gate, and a reply queued behind it.
func migrationOpen(t *testing.T, s *scenario, n string) {
	first, second := s.stub.Gate("migration"+n+"a"), s.stub.Gate("migration"+n+"b")
	m := s.mention(ChannelCRMigration, "UMIG"+n, fmt.Sprintf("session %s first gate:%s", n, first))
	s.thread(ChannelCRMigration, m.TS, "the first task, held", held(first, "", true))
	r := s.reply(ChannelCRMigration, m.TS, "UMIG"+n, fmt.Sprintf("session %s second gate:%s", n, second))
	err := Wait(s.ctx, stepTimeout, "the reaction that marks the reply as waiting", func(ctx context.Context) (bool, error) {
		rs, err := s.slack.Reactions(ctx, ChannelCRMigration, r.TS)
		for _, x := range rs {
			if x.Name == queuedReaction {
				return true, err
			}
		}
		return false, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session %s is open: thread %s, the first task held on %s, a reply queued behind it on %s", n, m.TS, first, second)
}

// migrationFinish finds session n again and ends it.
func migrationFinish(t *testing.T, s *scenario, n string) {
	msgs, err := s.slack.Messages(s.ctx, ChannelCRMigration)
	if err != nil {
		t.Fatal(err)
	}
	var root fakeslack.MessageView
	var first, second string
	for _, m := range msgs {
		if m.BotID != "" || m.User != "UMIG"+n || !strings.Contains(m.Text, fmt.Sprintf("session %s ", n)) {
			continue
		}
		gate := gateInText.FindStringSubmatch(m.Text)
		switch {
		case gate != nil && strings.Contains(m.Text, "first"):
			root, first = m, gate[1] // the latest wins: a session of an earlier run has other gates, and is over
		case gate != nil && strings.Contains(m.Text, "second"):
			second = gate[1]
		}
	}
	if first == "" || second == "" {
		t.Fatalf("session %s is not in %s: no mention with its first gate, or no reply with its second", n, ChannelCRMigration)
	}
	t.Cleanup(func() { // a gate left closed holds its task for good
		for _, g := range []string{first, second} {
			if err := s.stub.Release(context.WithoutCancel(s.ctx), g); err != nil {
				t.Errorf("release %s at the end: %v", g, err)
			}
		}
	})
	found, err := s.slack.Thread(s.ctx, ChannelCRMigration, root.TS)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session %s as found, before anything is released:%s", n, describe(found))

	wait := func(desc string, pred func([]fakeslack.MessageView) bool) []fakeslack.MessageView {
		t.Helper()
		th, err := s.slack.WaitThread(s.ctx, migrationWithin, ChannelCRMigration, root.TS, pred)
		if err != nil {
			t.Fatalf("%s: %v\nthe thread:%s", desc, err, describe(th))
		}
		return th
	}
	wait("the first task to be held, as it was", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 1 && strings.Contains(bots[0].Text, "Holding on gate "+first) && !strings.Contains(bots[0].Text, "Released.")
	})
	if err := s.stub.Release(s.ctx, first); err != nil {
		t.Fatal(err)
	}
	wait("the first task to end, with its text once", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 1 && finished(bots[0]) && strings.Count(bots[0].Text, "Holding on gate "+first) == 1 && strings.Count(bots[0].Text, "Released.") == 1
	})
	wait("the queued reply to be answered, after the first task", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 2 && strings.Contains(bots[1].Text, "Holding on gate "+second)
	})
	if err := s.stub.Release(s.ctx, second); err != nil {
		t.Fatal(err)
	}
	end := wait("the second answer to end, with its text once", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 2 && finished(bots[1]) && strings.Count(bots[1].Text, "Released.") == 1
	})
	if bots := BotMessages(end); len(bots) != 2 {
		t.Errorf("the thread has %d bot messages, want the two answers and nothing else:%s", len(bots), describe(end))
	}
	err = Consistently(s.ctx, 5*time.Second, "the thread holds the two answers and nothing more", func(ctx context.Context) (bool, error) {
		th, err := s.slack.Thread(ctx, ChannelCRMigration, root.TS)
		return len(BotMessages(th)) == 2, err
	})
	if err != nil {
		t.Error(err)
	}
	t.Logf("session %s ended:%s", n, describe(end))
}

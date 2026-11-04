//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
// finish-N holds every gate until the move is over, which the old pods survive on the Bindings they still have. The destructive path is the other
// order, which release-N, reply-N and settle-N make: while the way back is held (the old pods running with the custom resources deleted, and having seen
// the Bindings go), release-N ends the first task, so the turn ends in the old pod and the queued reply is next, and reply-N writes a new reply in the
// thread. After the move settle-N wants all three answered, once, and no word that the agent is no longer set up. Before the old pods hold what the
// deleted resources had, this fails, with that word in the thread.
//
// It runs in the fast tier, on Postgres with two replicas, where a move is a failover: the new leader recovers what the old one was doing.
func TestCR_MigrationKeepsASessionAlive(t *testing.T) {
	env := NewEnv(os.Getenv)
	if env.Phase == "" {
		t.Skip("a phase of hack/kind/crs-migration.sh (E2E_PHASE=open-1, finish-1, ...): it runs between the Helm upgrades of a migration, and means nothing alone")
	}
	kind, n, ok := strings.Cut(env.Phase, "-")
	if !ok || !slices.Contains([]string{"open", "finish", "release", "reply", "settle", "probe"}, kind) || n == "" {
		t.Fatalf("E2E_PHASE = %q; want open-N, finish-N, release-N, reply-N, settle-N or probe-SECONDS", env.Phase)
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
	case "release":
		migrationRelease(t, s, n)
	case "reply":
		migrationReply(t, s, n)
	case "settle":
		migrationSettle(t, s, n)
	case "probe":
		seconds, err := strconv.Atoi(n)
		if err != nil || seconds < 10 || seconds > 600 {
			t.Fatalf("E2E_PHASE = %q; want probe-SECONDS with 10 to 600", env.Phase)
		}
		migrationProbe(t, s, time.Duration(seconds)*time.Second)
	}
}

// probeAnswer is what the stub's scenario-gated says to a mention with no gate in it, which is what each probe is: the answer the probe wants.
const probeAnswer = "No gate named, not holding."

// probe is one mention that the probe phase sent, and what became of it.
type probe struct {
	n        int
	sent     time.Time
	ts       string        // the mention's thread, once it was stored
	sendErr  error         // the post to the fake failed; noConn says it was the fake's 409
	noConn   bool          // the fake had no Socket Mode client to deliver the event to (HTTP 409, ErrNoConnection)
	acked    bool          // Agent Kourier took the event
	answered bool          // the thread got a finished reply within the wait
	after    time.Duration // how long the reply took
	text     string        // what the bot said in the thread when it was read again at the end, all of it
	right    bool          // that text holds the answer that was wanted
}

// migrationProbe says what a person who writes in the Binding's channel meets while the broker is moved: for d it mentions Agent Kourier every two
// seconds, from its own goroutine, and each mention gets 20 seconds to be answered (scenario-gated, with no gate, answers at once). When it is over every
// thread is read again, so that an answer that came after the wait is told from one that never came, and the text is checked: a bot message that is not
// the answer (a notice, an error) is not counted as an answer. It is a measurement and does not judge: a mention that is delivered and acked and
// never answered is one that a broker without the Binding ignored without a word, which a person would call a lost message, and the summary says how many
// there were and when. A mention that could not be delivered is counted apart and only when the fake said it had no Socket Mode client (a 409); what real
// Slack does then is unknown, and the fake drops the event. Any other error is printed. hack/kind/crs-migration.sh starts it as the Helm upgrade starts,
// and waits for the line "probing started" in its log.
func migrationProbe(t *testing.T, s *scenario, d time.Duration) {
	const user, every, patience = "UMIGPROBE", 2 * time.Second, 20 * time.Second
	s.know(user)
	var mu sync.Mutex
	var probes []*probe
	var wg sync.WaitGroup
	t.Logf("probing started: a mention of %s every %s for %s", ChannelCRMigration, every, d)
	start := time.Now()
	for i := 1; time.Since(start) < d; i++ {
		p := &probe{n: i, sent: time.Now()}
		mu.Lock()
		probes = append(probes, p)
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(s.ctx, patience+15*time.Second)
			defer cancel()
			m, err := s.slack.Mention(ctx, ChannelCRMigration, user, fmt.Sprintf("%sprobe %d", botMention, p.n))
			if err != nil {
				var se *StatusError
				mu.Lock()
				p.sendErr, p.noConn = err, errors.As(err, &se) && se.Status == http.StatusConflict
				mu.Unlock()
				return
			}
			acked := true
			for _, id := range m.Envelopes {
				if _, err := s.slack.AckWait(ctx, 15*time.Second, id); err != nil {
					acked = false
				}
			}
			_, err = s.slack.WaitThread(ctx, patience, ChannelCRMigration, m.TS, func(th []fakeslack.MessageView) bool {
				bots := BotMessages(th)
				return len(bots) >= 1 && finished(bots[len(bots)-1])
			})
			mu.Lock()
			p.ts, p.acked, p.answered, p.after = m.TS, acked, err == nil, time.Since(p.sent)
			mu.Unlock()
		}()
		time.Sleep(every) // the cadence of the probe: a mention every two seconds, whatever the answers take
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, p := range probes { // late: what each thread holds now
		if p.sendErr != nil || p.ts == "" {
			continue
		}
		th, err := s.slack.Thread(s.ctx, ChannelCRMigration, p.ts)
		if err != nil {
			t.Errorf("read probe %d's thread again: %v", p.n, err)
			continue
		}
		var texts []string
		for _, b := range BotMessages(th) {
			texts = append(texts, b.Text)
			p.right = p.right || strings.Contains(b.Text, probeAnswer)
		}
		p.text = strings.Join(texts, " | ")
	}
	var onTime, late, wrong, never, noConn, other []*probe
	for _, p := range probes {
		switch {
		case p.noConn:
			noConn = append(noConn, p)
		case p.sendErr != nil:
			other = append(other, p)
		case p.answered && p.right:
			onTime = append(onTime, p)
		case p.right:
			late = append(late, p)
		case p.text != "":
			wrong = append(wrong, p)
		default:
			never = append(never, p)
		}
	}
	at := func(ps []*probe) string {
		if len(ps) == 0 {
			return "none"
		}
		return fmt.Sprintf("from +%s to +%s", ps[0].sent.Sub(start).Round(time.Second), ps[len(ps)-1].sent.Sub(start).Round(time.Second))
	}
	t.Logf("probe summary: %d mentions: %d answered, %d answered after the wait, %d got a bot message that is not the answer, %d delivered and never answered (%s), %d not delivered (the fake had no Socket Mode client; unknown on real Slack, the fake drops it) (%s), %d failed otherwise",
		len(probes), len(onTime), len(late), len(wrong), len(never), at(never), len(noConn), at(noConn), len(other))
	for _, p := range probes {
		state := "answered after " + p.after.Round(100*time.Millisecond).String()
		switch {
		case p.noConn:
			state = "NOT DELIVERED (the fake had no Socket Mode client)"
		case p.sendErr != nil:
			state = "FAILED: " + p.sendErr.Error()
		case p.answered && p.right:
		case p.right:
			state = "ANSWERED LATE, after the wait"
		case p.text != "":
			state = fmt.Sprintf("NOT THE ANSWER (acked: %t): %q", p.acked, p.text)
		default:
			state = fmt.Sprintf("NEVER ANSWERED (acked: %t)", p.acked)
		}
		t.Logf("probe %d at +%s: %s", p.n, p.sent.Sub(start).Round(100*time.Millisecond), state)
	}
	if len(probes) == 0 {
		t.Fatal("no probe was sent")
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

// migrationSession finds session n again in the channel, by its two gates, and releases both when the phase ends (a gate left closed holds its task for good).
func migrationSession(t *testing.T, s *scenario, n string) (root fakeslack.MessageView, first, second string) {
	t.Helper()
	msgs, err := s.slack.Messages(s.ctx, ChannelCRMigration)
	if err != nil {
		t.Fatal(err)
	}
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
	t.Cleanup(func() {
		for _, g := range []string{first, second} {
			if err := s.stub.Release(context.WithoutCancel(s.ctx), g); err != nil {
				t.Errorf("release %s at the end: %v", g, err)
			}
		}
	})
	return root, first, second
}

// migrationWait waits for the thread to satisfy pred, and fails with the thread printed.
func migrationWait(t *testing.T, s *scenario, root fakeslack.MessageView, desc string, pred func([]fakeslack.MessageView) bool) []fakeslack.MessageView {
	t.Helper()
	th, err := s.slack.WaitThread(s.ctx, migrationWithin, ChannelCRMigration, root.TS, pred)
	if err != nil {
		t.Fatalf("%s: %v\nthe thread:%s", desc, err, describe(th))
	}
	return th
}

// noticeOfGone is the word of a thread whose Binding the broker no longer has: the notice that replaces a queued reply or a new one.
const noticeOfGone = "no longer set up"

// migrationFinish finds session n again and ends it, once the move is over.
func migrationFinish(t *testing.T, s *scenario, n string) {
	root, first, second := migrationSession(t, s, n)
	found, err := s.slack.Thread(s.ctx, ChannelCRMigration, root.TS)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session %s as found, before anything is released:%s", n, describe(found))

	migrationWait(t, s, root, "the first task to be held, as it was", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 1 && strings.Contains(bots[0].Text, "Holding on gate "+first) && !strings.Contains(bots[0].Text, "Released.")
	})
	if err := s.stub.Release(s.ctx, first); err != nil {
		t.Fatal(err)
	}
	migrationWait(t, s, root, "the first task to end, with its text once", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 1 && finished(bots[0]) && strings.Count(bots[0].Text, "Holding on gate "+first) == 1 && strings.Count(bots[0].Text, "Released.") == 1
	})
	migrationWait(t, s, root, "the queued reply to be answered, after the first task", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 2 && strings.Contains(bots[1].Text, "Holding on gate "+second)
	})
	if err := s.stub.Release(s.ctx, second); err != nil {
		t.Fatal(err)
	}
	end := migrationWait(t, s, root, "the second answer to end, with its text once", func(th []fakeslack.MessageView) bool {
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

// migrationRelease ends the first task of session n now, in whatever Agent Kourier is running now: with the way back held, that is a pod that has seen the
// Bindings of the release go. The turn ends there, and the reply that waited behind it is next. It waits only for the first answer.
func migrationRelease(t *testing.T, s *scenario, n string) {
	root, first, _ := migrationSession(t, s, n)
	if err := s.stub.Release(s.ctx, first); err != nil {
		t.Fatal(err)
	}
	th := migrationWait(t, s, root, "the first task to end", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return len(bots) >= 1 && finished(bots[0]) && strings.Contains(bots[0].Text, "Released.")
	})
	t.Logf("session %s, the first task released inside the hold:%s", n, describe(th))
}

// migrationReply writes a new reply in the thread of session n, which has no gate and is answered at once by a Binding that is there. It is acked and
// not waited for: what becomes of it is for settle-N.
func migrationReply(t *testing.T, s *scenario, n string) {
	root, _, _ := migrationSession(t, s, n)
	r := s.reply(ChannelCRMigration, root.TS, "UMIG"+n, fmt.Sprintf("session %s third", n))
	th, err := s.slack.Thread(s.ctx, ChannelCRMigration, root.TS)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session %s, a new reply (%s) written inside the hold:%s", n, r.TS, describe(th))
}

// migrationSettle ends session n after a move that was held while its first task ended and a new reply was written (release-N, reply-N): the first task
// ended once, the reply that waited behind it is answered, once, the new reply is answered, once, and no message says that the agent is no longer set up.
// It is the case where the broker that ran the turn had lost the Binding: what the thread shows then is the loss.
func migrationSettle(t *testing.T, s *scenario, n string) {
	root, _, second := migrationSession(t, s, n)
	found, err := s.slack.Thread(s.ctx, ChannelCRMigration, root.TS)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session %s as found:%s", n, describe(found))
	gone := func(th []fakeslack.MessageView) bool {
		for _, b := range BotMessages(th) {
			if strings.Contains(b.Text, noticeOfGone) {
				return true
			}
		}
		return false
	}
	// Either the queued reply starts (held on its gate), or the thread says the agent is gone: the second is the loss, and is not waited out.
	th := migrationWait(t, s, root, "the queued reply to be answered, or the thread to say that the agent is no longer set up", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return gone(th) || len(bots) >= 2 && strings.Contains(bots[1].Text, "Holding on gate "+second)
	})
	if gone(th) {
		t.Fatalf("the thread says that the agent is no longer set up, and the queued reply is dropped:%s", describe(th))
	}
	if err := s.stub.Release(s.ctx, second); err != nil {
		t.Fatal(err)
	}
	end := migrationWait(t, s, root, "the queued reply and the new reply to be answered", func(th []fakeslack.MessageView) bool {
		bots := BotMessages(th)
		return gone(th) || len(bots) >= 3 && finished(bots[1]) && finished(bots[2]) && strings.Contains(bots[2].Text, probeAnswer)
	})
	if gone(end) {
		t.Fatalf("the thread says that the agent is no longer set up:%s", describe(end))
	}
	bots := BotMessages(end)
	if len(bots) != 3 || strings.Count(bots[0].Text, "Released.") != 1 || strings.Count(bots[1].Text, "Released.") != 1 || strings.Count(bots[2].Text, probeAnswer) != 1 {
		t.Errorf("the thread has %d bot messages, want the first task's text once, the queued reply's once and the new reply's once:%s", len(bots), describe(end))
	}
	t.Logf("session %s ended:%s", n, describe(end))
}

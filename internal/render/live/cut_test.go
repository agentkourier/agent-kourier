package live_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// These tests are about a turn whose context ends before its final event: the send failed (the agent refused it, or did not
// take an answer, and the session posts a notice of its own), the turn was cut short, or another run of the same turn took it
// over. Whatever the turn showed by then is ended, as a failed task's is: nothing is left looking as if it were under way.

// taskLines is every card of the message as "id title status", as the stream left it.
func taskLines(m fakeslack.Message) []string {
	var out []string
	for _, c := range m.Tasks() {
		out = append(out, c.ID+" "+c.Title+" "+c.Status)
	}
	return out
}

// A stream that a turn opened is stopped when the turn's context ends with no final, with each card ended: an unclaimed
// Working… card and the activity card as Stopped, an error, and a call with no result as one whose outcome is not known. It
// is the case seen on Slack, where an answer the agent did not take had begun a Working… card and left it spinning.
func TestAStreamIsStoppedWhenItsTurnEndsWithNoFinal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     v1alpha1.ProgressMode
		drive    func(t *testing.T, e *env, run *live.Run)
		text     string
		want     []string
		wantCard func(fakeslack.Task) bool
	}{
		{"steps-and-activity, silent past the delay", sa, func(t *testing.T, e *env, _ *live.Run) {
			e.until(t, "the Working… card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
		}, "", []string{"s1 Stopped error"}, nil},
		{"activity, silent past the delay", activity, func(t *testing.T, e *env, _ *live.Run) {
			e.until(t, "the activity card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
		}, "", []string{"s1 Stopped error"}, nil},
		{"steps-and-activity, text and then silence", sa, func(t *testing.T, e *env, run *live.Run) {
			run.Handle(text("a", "Checking the pods."))
			e.writesReach(t, 1)
			e.until(t, "the Working… card", 3*time.Second, func() bool { return len(e.tasks()) == 1 })
		}, "Checking the pods.", []string{"s1 Stopped error"}, nil},
		{"steps, a call under way", v1alpha1.ProgressSteps, func(t *testing.T, e *env, run *live.Run) {
			run.Handle(call("c1", "search", nil))
			eventually(t, "the card of the call", func() bool { return len(e.tasks()) == 1 })
		}, "", []string{"s1 search complete"}, func(c fakeslack.Task) bool { return c.Output != "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			run, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: tc.mode, Recipient: "U1", Footer: footer})
			tc.drive(t, e, run)
			if got := taskLines(e.reply()); len(got) != 1 || !strings.HasSuffix(got[0], " in_progress") {
				t.Fatalf("cards %v before the end; want one card under way", got)
			}
			cancel() // the send returned with no final event
			e.finish(t, run)
			m := e.reply()
			if n := len(e.rec.sent("/chat.stopStream")); n != 1 || m.Streaming {
				t.Fatalf("%d stops, streaming %v; want the stream stopped, once", n, m.Streaming)
			}
			if m.Metadata == nil {
				t.Errorf("the stopped stream has no metadata; want the outbox tag of the stop")
			}
			if got := taskLines(m); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("cards %v; want %v", got, tc.want)
			}
			if tc.wantCard != nil && !tc.wantCard(m.Tasks()[0]) {
				t.Errorf("card %+v; want it to say that its outcome is not known", m.Tasks()[0])
			}
			if m.Text != tc.text {
				t.Errorf("text %q; want %q", m.Text, tc.text)
			}
			if buttons, questions := hasFooter(string(m.Blocks)); buttons || questions > 0 {
				t.Errorf("a turn that ended with no final asks for a rating: %s", m.Blocks)
			}
			if n := len(e.rec.sent("/chat.update")); n != 0 {
				t.Errorf("%d updates; want the stop alone", n)
			}
		})
	}
}

// On the update path, as in an alert thread, a turn whose context ends with no final takes its status line down: a Working…
// line, or the line of a call under way, would otherwise say for good that the agent is working. The text it had stays.
func TestAnAlertThreadLosesItsLineWhenItsTurnEndsWithNoFinal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive func(t *testing.T, e *env, run *live.Run)
		want  string
	}{
		{"silent past the delay", func(t *testing.T, e *env, _ *live.Run) {
			e.until(t, "Working…", 6*time.Second, func() bool { return strings.Contains(e.said(), "Working…") })
		}, "(no output)"},
		{"text and a call under way", func(t *testing.T, e *env, run *live.Run) {
			run.Handle(text("a", "Looking."))
			run.Handle(call("c1", "search", nil))
			e.until(t, "the line of the call", 3*time.Second, func() bool { return strings.Contains(e.said(), "Calling search") })
		}, "Looking."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			run, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Footer: footer})
			tc.drive(t, e, run)
			cancel()
			e.finish(t, run)
			m := e.reply()
			if m.Text != tc.want || strings.Contains(string(m.Blocks), "Working") || strings.Contains(string(m.Blocks), "Calling") {
				t.Errorf("message %q, blocks %s; want %q and no status line", m.Text, m.Blocks, tc.want)
			}
			if buttons, questions := hasFooter(string(m.Blocks)); buttons || questions > 0 {
				t.Errorf("a turn that ended with no final asks for a rating: %s", m.Blocks)
			}
		})
	}
}

// A turn that showed nothing before its context ended still owes nothing: no stream is started and nothing is posted.
func TestATurnThatShowedNothingWritesNothingWhenItsContextEnds(t *testing.T) {
	for _, recipient := range []string{"U1", ""} {
		e := setup(t)
		run, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: recipient})
		e.elapse(time.Second) // short of the delay
		cancel()
		select {
		case <-run.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the run did not end with its context")
		}
		if n := e.writes(); n != 0 {
			t.Errorf("recipient %q: %d writes; want none", recipient, n)
		}
	}
}

// A run of a turn whose earlier run is still going (the session follows the task again under the same turn while the send
// that lost it has not yet returned) takes the message over only once the earlier one has ended: the earlier run leaves what
// it showed to the later one, and makes no write after the later one's, so it cannot put back what it showed over the later
// one's answer. The stream is stopped once, by the later run's takeover.
func TestALaterRunOfATurnWritesOnlyOnceTheEarlierHasEnded(t *testing.T) {
	for _, recipient := range []string{"U1", ""} {
		t.Run(map[string]string{"U1": "stream", "": "alert thread"}[recipient], func(t *testing.T) {
			e := setup(t)
			first, cancelFirst := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: recipient})
			e.until(t, "the earlier run's Working…", 6*time.Second, func() bool { return e.writes() >= 2 || (recipient == "" && e.writes() == 1) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			second := e.live.Start(ctx, live.Turn{Source: "turn-1", Channel: "C1", Thread: e.root, Mode: v1alpha1.OutputLive, Progress: sa, Recipient: recipient, Recovered: true})
			second.Handle(text("b", "Done.")) // at once: the later run's first write is not throttled, the earlier's last one is
			second.Handle(agent.FinalEvent{})
			cancel()
			e.drain(t, e.live) // the earlier run ends what it showed, in its own window, with its context still live
			select {
			case <-first.Done():
			default:
				t.Fatal("the earlier run did not end when a later run of its turn started")
			}
			cancelFirst()
			m := e.reply()
			if m.Streaming || !strings.HasPrefix(m.Text, "Done.") {
				t.Errorf("message %+v; want the later run's answer, and no stream", m)
			}
			if n := len(e.rec.sent("/chat.stopStream")); recipient != "" && n != 1 {
				t.Errorf("%d stops; want the later run's takeover alone", n)
			}
			if strings.Contains(string(m.Blocks), "Working") {
				t.Errorf("blocks %s; want no Working… line", m.Blocks)
			}
		})
	}
}

// startCause starts a turn of the test's thread whose context the test ends with a cause.
func (e *env) startCause(t *testing.T, turn live.Turn) (*live.Run, context.CancelCauseFunc) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	t.Cleanup(func() { cancel(nil) })
	turn.Source, turn.Channel, turn.Thread = "turn-1", "C1", e.root
	return e.live.Start(ctx, turn), cancel
}

// A turn whose task continues under a later turn (a lost stream, followed again, or a shutdown, after which recovery follows
// it) leaves what it showed as it is, for that turn to take over: the stream is not stopped and the line is not taken down.
// The later turn takes the message over whether it has text or nothing to show, so no Working… is left for good.
func TestATurnThatContinuesLeavesItsMessageToTheLaterTurn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recipient string
		later     func(run *live.Run)
		want      string
	}{
		{"stream, the follow has the answer", "U1", func(run *live.Run) { run.Handle(text("a", "Done.")); run.Handle(agent.FinalEvent{}) }, "Done."},
		{"stream, the follow has nothing", "U1", func(run *live.Run) { run.Handle(agent.FinalEvent{}) }, "(no output)"},
		{"alert thread, the follow has the answer", "", func(run *live.Run) { run.Handle(text("a", "Done.")); run.Handle(agent.FinalEvent{}) }, "Done."},
		{"alert thread, the follow has nothing", "", func(run *live.Run) { run.Handle(agent.FinalEvent{}) }, "(no output)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			first, end := e.startCause(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: tc.recipient})
			e.until(t, "the Working…", 6*time.Second, func() bool {
				return strings.Contains(e.said(), "Working…") || len(e.reply().Tasks()) == 1
			})
			writes := e.writes()
			end(session.ErrTurnContinues)
			select {
			case <-first.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the run did not end with its context")
			}
			if n := e.writes(); n != writes {
				t.Fatalf("%d writes after the turn was handed on; want none", n-writes)
			}
			if tc.recipient != "" && !e.reply().Streaming {
				t.Fatal("the stream was stopped; want it left for the follow")
			}
			later, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: tc.recipient, Recovered: true})
			tc.later(later)
			e.finish(t, later)
			th := e.fake.Thread("C1", e.root)
			if m := th[len(th)-1]; len(th) != 2 || m.Streaming || m.Text != tc.want || strings.Contains(string(m.Blocks), "Working") {
				t.Errorf("thread %d long, message %q (streaming %v), blocks %s; want one message saying %q", len(th), m.Text, m.Streaming, m.Blocks, tc.want)
			}
		})
	}
}

// A follow that ends with no final event and nothing of its own (the agent refused it, the task was cancelled as wedged, or
// the session no longer has it, and the session tells the thread so) ends what the turn it followed showed, as that turn
// would have ended it had it not been handed on: the stream is stopped with its text, its Working… card ended as Stopped, and
// the update path loses its status line and keeps its text. Nothing asks for a rating, and the later run posts nothing.
func TestAFollowThatEndsWithNothingEndsWhatItWasHanded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recipient string
	}{{"stream", "U1"}, {"alert thread", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			first, end := e.startCause(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: tc.recipient})
			first.Handle(text("a", "Checking the pods."))
			e.writesReach(t, 1)
			e.until(t, "the Working…", 6*time.Second, func() bool {
				return strings.Contains(e.said(), "Working…") || len(e.reply().Tasks()) == 1
			})
			end(session.ErrTurnContinues) // the stream was lost: the task is followed again
			select {
			case <-first.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the run did not end with its context")
			}
			later, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: tc.recipient, Recovered: true})
			cancel() // the follow ended with no event
			e.finish(t, later)
			th := e.fake.Thread("C1", e.root)
			m := th[len(th)-1]
			if len(th) != 2 || m.Streaming {
				t.Fatalf("thread %d long, streaming %v; want the one message, stopped", len(th), m.Streaming)
			}
			if !strings.HasPrefix(m.Text, "Checking the pods.") || strings.Contains(string(m.Blocks), "Working") {
				t.Errorf("message %q, blocks %s; want the text the turn had and no Working…", m.Text, m.Blocks)
			}
			if got := taskLines(m); tc.recipient != "" && strings.Join(got, ",") != "s1 Stopped error" {
				t.Errorf("cards %v; want the Working… card ended as Stopped", got)
			}
			if buttons, questions := hasFooter(string(m.Blocks)); buttons || questions > 0 {
				t.Errorf("a turn that ended with no final asks for a rating: %s", m.Blocks)
			}
		})
	}
}

// A message that an earlier process left for a turn whose task went on has no run of this Renderer to end it. A recovered run
// of the turn that ends with no final event and nothing of its own still stops that message's stream, which keeps its text
// (the platform shows the card that was under way as ended). A message that is updated, as in an alert thread, is left as it
// is: what it says is not known here, and an update would have to replace it.
func TestARecoveredRunEndsTheStreamAnEarlierProcessLeft(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recipient string
	}{{"stream", "U1"}, {"alert thread", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			first, end := e.startCause(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: tc.recipient})
			first.Handle(text("a", "Checking the pods."))
			e.writesReach(t, 1)
			e.until(t, "the Working…", 6*time.Second, func() bool {
				return strings.Contains(e.said(), "Working…") || len(e.reply().Tasks()) == 1
			})
			end(session.ErrTurnContinues) // the process stops, and leaves the task to recovery
			select {
			case <-first.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the run did not end with its context")
			}
			left := e.reply()
			e.live = newRenderer(t, e.cfg) // the restarted process
			later, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: tc.recipient, Recovered: true})
			cancel() // the attempt ended with no event, and the session tells the thread why
			e.finish(t, later)
			th := e.fake.Thread("C1", e.root)
			m := th[len(th)-1]
			if len(th) != 2 || m.Streaming {
				t.Fatalf("thread %d long, streaming %v; want the one message, not streaming", len(th), m.Streaming)
			}
			if tc.recipient == "" && (m.Text != left.Text || string(m.Blocks) != string(left.Blocks)) {
				t.Errorf("message %q, blocks %s; want it as the earlier process left it", m.Text, m.Blocks)
			}
			if n := len(e.rec.sent("/chat.stopStream")); tc.recipient == "" && n != 0 {
				t.Errorf("%d stops of a message that never streamed; want none", n)
			}
			if !strings.HasPrefix(m.Text, "Checking the pods.") {
				t.Errorf("message %q; want the text the earlier process showed", m.Text)
			}
			if buttons, questions := hasFooter(string(m.Blocks)); buttons || questions > 0 {
				t.Errorf("a turn that ended with no final asks for a rating: %s", m.Blocks)
			}
		})
	}
}

// A later attempt at a turn whose earlier attempt posted a message (a Working… line, ended when that attempt's send failed)
// finds the turn's row made already, and the message the outbox returns for its post is the earlier one, which does not say what
// the later attempt gave it. It writes the message again, recovered or not, so that its answer is what the thread shows.
func TestALaterAttemptRewritesTheMessageAnEarlierOnePosted(t *testing.T) {
	e := setup(t)
	first, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa})
	e.until(t, "the Working…", 6*time.Second, func() bool { return strings.Contains(e.said(), "Working…") })
	cancel() // the send failed, and is tried again
	e.finish(t, first)
	later, _ := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa})
	later.Handle(text("a", "Done."))
	later.Handle(agent.FinalEvent{})
	e.finish(t, later)
	th := e.fake.Thread("C1", e.root)
	if m := th[len(th)-1]; len(th) != 2 || m.Text != "Done." {
		t.Errorf("thread %d long, message %q; want the one message, saying %q", len(th), m.Text, "Done.")
	}
}

// ownStopFails is a Platform whose streams this run starts stop with err.
type ownStopFails struct {
	chat.Platform
	err error
}

func (p ownStopFails) Stream(ctx context.Context, at chat.Target, recipient, markdown string) (chat.Stream, error) {
	s, err := p.Platform.Stream(ctx, at, recipient, markdown)
	if err != nil {
		return nil, err
	}
	return stoppingFails{Stream: s, err: p.err}, nil
}

// A stream that the platform ended before the turn was cut short (Slack ends one left idle) has nothing left to stop: the
// stop is tried once, its refusal is no failure, and nothing replaces the message.
func TestACutTurnWhoseStreamEndedAlreadyLeavesIt(t *testing.T) {
	e := setup(t)
	e.cfg.Platform = ownStopFails{Platform: e.cfg.Platform, err: fmt.Errorf("%w: message_not_in_streaming_state", chat.ErrNotStreaming)}
	e.live = newRenderer(t, e.cfg)
	run, cancel := e.startFor(t, live.Turn{Mode: v1alpha1.OutputLive, Progress: sa, Recipient: "U1"})
	run.Handle(text("a", "Checking."))
	e.writesReach(t, 1)
	cancel()
	e.finish(t, run)
	if strings.Contains(e.logs.String(), "tried again") || strings.Contains(e.logs.String(), "giving up") {
		t.Errorf("the refused stop was retried:\n%s", e.logs)
	}
	if n := len(e.rec.sent("/chat.update")); n != 0 {
		t.Errorf("%d updates; want none", n)
	}
}

// A turn cut short is counted as cancelled when its closing write went through, and by how the write failed when it did not,
// so that the alert on failed finals still sees it.
func TestACutTurnIsCountedByHowItsClosingWriteWent(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		err           error
	}{{"delivered", "cancelled", nil}, {"refused for good", "permanent_failure", chat.ErrInvalid}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			if tc.err != nil {
				e.cfg.Platform = ownStopFails{Platform: e.cfg.Platform, err: tc.err}
			}
			reg := prometheus.NewRegistry()
			obs, err := telemetry.New(reg, nil)
			if err != nil {
				t.Fatal(err)
			}
			renderer := newRenderer(t, e.cfg)
			ctx, cancel := context.WithCancel(telemetry.WithObserver(t.Context(), obs))
			defer cancel()
			run := renderer.Start(ctx, live.Turn{Source: "turn-1", Channel: "C1", Thread: e.root, Mode: v1alpha1.OutputLive, Recipient: "U1"})
			run.Handle(text("a", "Checking."))
			e.writesReach(t, 1)
			cancel()
			e.finish(t, run)
			if got := finalOutcomes(t, reg); strings.Join(got, ",") != tc.outcome {
				t.Errorf("final outcomes %v; want %s", got, tc.outcome)
			}
		})
	}
}

// finalOutcomes is the outcome label of each agentkourier_render_final_total series that has counted something.
func finalOutcomes(t *testing.T, reg *prometheus.Registry) []string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range families {
		if f.GetName() != "agentkourier_render_final_total" {
			continue
		}
		for _, m := range f.Metric {
			if m.Counter.GetValue() > 0 {
				out = append(out, m.Label[0].GetValue())
			}
		}
	}
	return out
}

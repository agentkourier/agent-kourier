package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

// pressed is a presser that answers with the error the test sets, and counts its presses.
type pressed struct {
	err error
	n   *int
}

func (p pressed) Press(context.Context, chat.Interaction) error {
	if p.n != nil {
		*p.n++
	}
	return p.err
}

// The handler of buttons tells the outcomes of a press apart: a button of someone else's is dropped
// without a word, a press refused for the limit is acknowledged and logged, and a failure is returned, so
// that Slack delivers the press again. A button that is no interaction's goes to the feedback buttons' handler, and
// only then is it someone else's; an interaction's never reaches it.
func TestInteractionsHandlerDropsOtherButtonsAcksALimitAndReturnsTheRest(t *testing.T) {
	boom := errors.New("the store is down")
	for _, tc := range []struct {
		name         string
		err, fbErr   error
		want         error
		wantLog      string
		wantFeedback int // presses that reached the feedback handler
	}{
		{"a press recorded", nil, nil, nil, "", 0},
		{"a feedback press recorded", interact.ErrNotInteraction, nil, nil, "", 1},
		{"another handler's button", interact.ErrNotInteraction, feedback.ErrNotFeedback, nil, "", 1},
		{"too many presses waiting", fmt.Errorf("interact: record a press: %w", store.ErrLimit), nil, nil, "too many presses", 0},
		{"too many feedback presses waiting", interact.ErrNotInteraction, fmt.Errorf("feedback: record a press: %w", store.ErrLimit), nil, "too many presses", 1},
		{"any other failure", boom, nil, boom, "", 0},
		{"a feedback press that fails", interact.ErrNotInteraction, boom, boom, "", 1},
	} {
		out := &logBuffer{}
		var fbPresses int
		h := interactionsHandler(pressed{err: tc.err}, pressed{err: tc.fbErr, n: &fbPresses}, slog.New(slog.NewTextHandler(out, nil)))

		err := h(t.Context(), chat.Interaction{At: chat.Ref{Channel: "C1"}, PromptID: "x"})

		if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: handler = %v, want %v", tc.name, err, tc.want)
		}
		if (tc.wantLog == "") != (out.String() == "") || !strings.Contains(out.String(), tc.wantLog) {
			t.Errorf("%s: logged %q, want a line with %q (or nothing)", tc.name, out.String(), tc.wantLog)
		}
		if fbPresses != tc.wantFeedback {
			t.Errorf("%s: %d presses reached the feedback handler, want %d", tc.name, fbPresses, tc.wantFeedback)
		}
	}
}

// The press worker's one processor is the interactions' pipeline or the feedback handler, by whose press it is: a
// feedback press by its lane, which no interaction's has.
func TestPressRouterSendsAPressToItsOwnProcessor(t *testing.T) {
	var interactions, feedbacks []int64
	mk := func(got *[]int64) interact.PressProcessor {
		return fakeProcessor{process: func(p store.Press) { *got = append(*got, p.Seq) }}
	}
	r := pressRouter{interactions: mk(&interactions), feedback: mk(&feedbacks)}
	for _, p := range []store.Press{{Seq: 1, InteractionID: "ix-0123"}, {Seq: 2, InteractionID: "fb1|turn-1"}, {Seq: 3, InteractionID: "ix-4567"}} {
		if _, err := r.Process(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		if err := r.GiveUp(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(interactions, []int64{1, 1, 3, 3}) || !slices.Equal(feedbacks, []int64{2, 2}) {
		t.Errorf("interactions' pipeline got %v and the feedback handler %v; want each press processed and given up by its own", interactions, feedbacks)
	}
}

// fakeProcessor records the presses it is given, to Process and to GiveUp alike.
type fakeProcessor struct{ process func(store.Press) }

func (f fakeProcessor) Process(_ context.Context, p store.Press) (interact.Result, error) {
	f.process(p)
	return interact.Done, nil
}

func (f fakeProcessor) GiveUp(_ context.Context, p store.Press) error {
	f.process(p)
	return nil
}

// Before the manager exists the interactions' session answers that it is not ready, and calls nothing.
func TestLateSessionIsNotReadyUntilItIsBound(t *testing.T) {
	var late lateSession
	if err := late.Answer(t.Context(), store.ConversationKey{}, &a2a.Message{}); !errors.Is(err, errSessionNotReady) {
		t.Errorf("Answer before bind = %v, want errSessionNotReady", err)
	}
	if err := late.Cancel(t.Context(), store.ConversationKey{}, "task-1", "why"); !errors.Is(err, errSessionNotReady) {
		t.Errorf("Cancel before bind = %v, want errSessionNotReady", err)
	}
}

// stepNotes records the steps of startup in the order they were told, with a mutex because the Slack
// run, which is one of them, tells it from its own goroutine.
type stepNotes struct {
	mu    sync.Mutex
	steps []string
}

func (n *stepNotes) note(step string) {
	n.mu.Lock()
	n.steps = append(n.steps, step)
	n.mu.Unlock()
}

func (n *stepNotes) got() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return strings.Join(n.steps, ",")
}

// noting is an idle Slack that says when its connection opens, which is when the intake does.
type noting struct {
	idleSlack
	notes *stepNotes
}

func (s noting) Run(ctx context.Context, _ slack.Sink) error {
	s.notes.note("slack-run")
	<-ctx.Done()
	return ctx.Err()
}

// The order of startup is the contract: the sessions recover, then the presses an earlier run left, and
// only then does the Slack intake open, so that a press listed before it finished is not announced after.
// The sweeper starts after the sessions have recovered, and so does everything in "start-interactions".
func TestRunRecoversSessionsThenPressesAndOpensTheIntakeLast(t *testing.T) {
	notes := &stepNotes{}
	c := startConfig(t)
	c.newSlack = func(slack.Config) slackAPI { return noting{notes: notes} }
	c.startTrace = notes.note
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	for deadline := time.Now().Add(10 * time.Second); !strings.HasSuffix(notes.got(), "slack-run"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the Slack run never began: %s", notes.got())
		}
	}

	if want := "recover-sessions,recover-presses,start-interactions,open-intake,slack-run"; notes.got() != want {
		t.Errorf("startup went %s, want %s", notes.got(), want)
	}
}

// ephemerals is an idle Slack that keeps what it was asked to show a person alone.
type ephemerals struct {
	idleSlack
	mu    sync.Mutex
	texts []string
}

func (s *ephemerals) PostEphemeral(_ context.Context, _, _, _ string, c slack.Content) error {
	s.mu.Lock()
	s.texts = append(s.texts, c.Text)
	s.mu.Unlock()
	return nil
}

func (s *ephemerals) told(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.texts, text)
}

// A press that an earlier run recorded and did not finish is found at startup and worked on: here it is
// for a question that is gone, so its person is told so and the press is deleted. That takes the
// worker, the pipeline and the recovery, all built by Run.
func TestRunProcessesThePressesAnEarlierRunLeft(t *testing.T) {
	c := startConfig(t)
	eph := &ephemerals{}
	c.newSlack = func(slack.Config) slackAPI { return eph }
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	press := store.Press{InteractionID: "ix-gone", User: "U1", Channel: "C0123456789", MessageRef: "1.1", Inputs: []byte("{}"), PressedAt: time.Now()}
	if _, err := seed.RecordPress(t.Context(), press, store.PressLimits{Total: 10, PerUser: 10}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	r := start(t, c)
	r.until("/readyz", http.StatusOK)

	for deadline := time.Now().Add(10 * time.Second); !eph.told("This question is no longer available."); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the press was never worked on: the person was not told the question is gone")
		}
	}
}

// loopsAlive names the loops of the interactions that are running now, from the goroutines' stacks.
func loopsAlive() []string {
	buf := make([]byte, 1<<22)
	stacks := string(buf[:runtime.Stack(buf, true)])
	var alive []string
	for _, loop := range []string{"Worker", "Sweeper", "Replacer", "Retention", "Backstop"} {
		if strings.Contains(stacks, "interact.(*"+loop+").Run") {
			alive = append(alive, loop)
		}
	}
	return alive
}

// What the interactions started stops in its own step, between the intake and the manager: all of the loops
// are running when the shutdown begins, and none is by the time the manager begins to close.
func TestRunStopsTheInteractionsLoopsBetweenTheIntakeAndTheManager(t *testing.T) {
	var atIntake, atManager []string
	trace := &stepTrace{hook: func(step string) {
		switch step {
		case "intake":
			atIntake = loopsAlive()
		case "manager":
			atManager = loopsAlive()
		}
	}}
	c := startConfig(t)
	c.shutdownTrace = trace.record
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	for deadline := time.Now().Add(10 * time.Second); len(loopsAlive()) < 5; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the loops did not all start: %v", loopsAlive())
		}
	}

	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}

	if len(atIntake) != 5 {
		t.Errorf("%v of the 5 loops were running when the shutdown began (the control: the scan finds them)", atIntake)
	}
	if len(atManager) != 0 {
		t.Errorf("%v were still running when the manager began to close, after the interactions' step", atManager)
	}
}

// build refuses a deps with a part left out, and the error names it: the interactions are built from these parts, and one
// that is nil would fail at its first use, or never.
func TestInteractionDepsNameThePartThatIsMissing(t *testing.T) {
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	full := interactionDeps{
		st: st, ob: &outbox.Outbox{}, platform: builtPlatform{Platform: chattest.NewFake()}, auditor: &audit.Writer{},
		bindings: &Bindings{}, metrics: &Metrics{}, log: slog.Default(),
	}
	for _, tc := range []struct {
		part  string
		clear func(*interactionDeps)
	}{
		{"st", func(d *interactionDeps) { d.st = nil }},
		{"ob", func(d *interactionDeps) { d.ob = nil }},
		{"platform", func(d *interactionDeps) { d.platform = builtPlatform{} }},
		{"auditor", func(d *interactionDeps) { d.auditor = nil }},
		{"bindings", func(d *interactionDeps) { d.bindings = nil }},
		{"metrics", func(d *interactionDeps) { d.metrics = nil }},
		{"log", func(d *interactionDeps) { d.log = nil }},
	} {
		t.Run(tc.part, func(t *testing.T) {
			d := full
			tc.clear(&d)
			if _, err := d.build(nil); err == nil || !strings.Contains(err.Error(), tc.part+" is required") {
				t.Fatalf("build = %v; want an error that names %s", err, tc.part)
			}
		})
	}
	if err := full.check(); err != nil {
		t.Errorf("check with every part = %v", err)
	}
}

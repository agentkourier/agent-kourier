package broker

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/feedback"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// sinkTap is a connection's platform that hands the test the sink each Run is given: the intake, as the leader half wires it.
type sinkTap struct {
	chat.Platform
	got chan chat.Sink
}

func (p sinkTap) Run(ctx context.Context, sink chat.Sink) error {
	select {
	case p.got <- sink:
	default: // a later Run, after a retry: the test has its sink
	}
	return p.Platform.Run(ctx, sink)
}

// A press whose platform waits for its decision goes through the intake as the leader half wires it, to the press worker, and comes
// back decided with the words for the presser: a prompt's press to the interactions' pipeline, a vote to the feedback buttons'
// handler. The waits share one cap, pressWaitsMax.
func TestRunDecidesAPressInTheWaitThroughTheIntake(t *testing.T) {
	c := startConfig(t)
	var outcomes atomic.Pointer[interact.Outcomes]
	c.outcomesBuilt = func(o *interact.Outcomes) { outcomes.Store(o) }
	sinks := make(chan chat.Sink, 1)
	c.wrapPlatform = func(p chat.Platform) chat.Platform { return sinkTap{Platform: p, got: sinks} }
	started := leaderUp(t, &c)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	started()

	var sink chat.Sink
	select {
	case sink = <-sinks:
	case <-time.After(10 * time.Second):
		t.Fatal("the connection's run was given no sink")
	}
	d, ok := sink.(chat.InlineDecider)
	if !ok {
		t.Fatalf("the connection's sink, %T, does not decide presses in the wait", sink)
	}
	at := chat.Ref{Platform: "slack", Conn: "agent-kourier-system/slack-main", Channel: "C0123456789", ID: "1700000000.000100"}
	for name, tc := range map[string]struct {
		prompt string
		want   string
	}{
		"a prompt's press": {"ix-never-posted", "This question is no longer available."},
		"a vote": {feedback.Value{Kind: store.FeedbackInvestigation, Verdict: store.VerdictUseful, Thread: "999.9", Turn: "1"}.String(),
			"This message can no longer be rated."},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		got, err := d.DecidePress(ctx, chat.Interaction{User: "UALICE1", At: at, PromptID: tc.prompt, Action: chat.ActionSubmit, Delivery: name})
		cancel()
		if want := (chat.PressOutcome{Decided: true, Text: tc.want}); err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", name, got, err, want)
		}
	}
	o := outcomes.Load()
	if o == nil {
		t.Fatal("the leader half built no Outcomes")
	}
	limitsdoc.Count(t, "pressWaitsMax", o.Limit())
}

// As the chat listener begins its drain at shutdown, a press still waiting for its decision stops waiting and is answered, inside the
// drain, rather than cut with its connection.
func TestRunAnswersAPressStillWaitingAtTheShutdown(t *testing.T) {
	c := startConfig(t)
	var outcomes atomic.Pointer[interact.Outcomes]
	c.outcomesBuilt = func(o *interact.Outcomes) { outcomes.Store(o) }
	waiting := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// The route's context ends after chatHandlerTimeout, later than the drain cuts the request: only the drain's release answers it.
		out := outcomes.Load().Wait(req.Context(), store.Press{Seq: 1}, func(store.Press) { close(waiting) })
		if out.Decided {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	addr, _ := chatListen(t, &c, h)
	started := leaderUp(t, &c)
	r := start(t, c)
	r.until("/readyz", http.StatusOK)
	started()

	answered := make(chan int, 1)
	go func() { answered <- chatPost(t, http.MethodPost, addr, ownRoute) }()
	<-waiting
	r.cancel()
	if code := <-answered; code != http.StatusOK {
		t.Errorf("the press waiting at the shutdown was answered %d; want 200, released with no decision inside the drain", code)
	}
	if err := r.wait(); err != nil {
		t.Fatalf("Run = %v after its context ended, want nil", err)
	}
}

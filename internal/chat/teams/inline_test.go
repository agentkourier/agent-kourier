package teams

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// decidingSink is a pressSink that also decides presses while the invoke waits (chat.InlineDecider): decide says what each comes to,
// and is given the context the invoke waits under.
type decidingSink struct {
	pressSink
	decide func(ctx context.Context) (chat.PressOutcome, error)
}

func (s *decidingSink) DecidePress(ctx context.Context, i chat.Interaction) (chat.PressOutcome, error) {
	s.mu.Lock()
	s.presses = append(s.presses, i)
	s.mu.Unlock()
	return s.decide(ctx)
}

// serveWith starts ct's receiver with sink as its live Serve.
func serveWith(t *testing.T, ct *clientTest, sink chat.Sink) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ct.c.recv.Serve(ctx, sink) }()
	t.Cleanup(func() { cancel(); _ = <-done }) // Serve says only that it was cancelled
	testutil.Eventually(t, "the receiver takes requests", func() bool { return ct.c.recv.live.Load() != nil })
}

// A press whose sink decides it while the invoke waits is answered with what it came to, when that is known and has words; a press not
// decided in the wait, or decided with nothing to tell, is answered as received; and one whose recording failed, as may not be recorded.
func TestAPressDecidedInTheWaitIsAnsweredWithItsOutcome(t *testing.T) {
	button := map[string]any{dataKey: map[string]any{"button": "b", "value": "fb1|v"}}
	for name, tc := range map[string]struct {
		data map[string]any
		out  chat.PressOutcome
		err  error
		want string
	}{
		"a prompt's press decided":          {data: promptData("deny_all"), out: chat.PressOutcome{Decided: true, Text: "Your decision was recorded."}, want: "Your decision was recorded."},
		"a prompt's press refused":          {data: promptData("submit"), out: chat.PressOutcome{Decided: true, Text: "Only members of this channel can answer this question."}, want: "Only members of this channel can answer this question."},
		"a button's press decided":          {data: button, out: chat.PressOutcome{Decided: true, Text: "Your vote is already recorded as useful."}, want: "Your vote is already recorded as useful."},
		"a prompt's press not decided":      {data: promptData("submit"), want: textPromptReceived},
		"a button's press not decided":      {data: button, want: textButtonReceived},
		"a press decided with nothing told": {data: promptData("submit"), out: chat.PressOutcome{Decided: true}, want: textPromptReceived},
		"a press not recorded":              {data: promptData("submit"), err: errors.New("the store is down"), want: textPressNotRecorded},
	} {
		t.Run(name, func(t *testing.T) {
			ct := newClientTest(t)
			sink := &decidingSink{decide: func(context.Context) (chat.PressOutcome, error) { return tc.out, tc.err }}
			serveWith(t, ct, sink)
			if got := told(t, postInvoke(t, ct, "f:1", tc.data, nil)); got != tc.want {
				t.Errorf("told %q; want %q", got, tc.want)
			}
			if n := len(sink.taken()); n != 1 {
				t.Errorf("the sink decided %d presses; want one", n)
			}
		})
	}
}

// A press whose decision takes longer than the wait is answered as received once the wait ends: the sink's context ends then, and the
// press is left to the worker.
func TestAPressNotDecidedWithinTheWaitIsAnsweredAsReceived(t *testing.T) {
	ct := newClientTest(t)
	ct.c.pressWait = 50 * time.Millisecond
	var deadline time.Duration
	sink := &decidingSink{decide: func(ctx context.Context) (chat.PressOutcome, error) {
		d, _ := ctx.Deadline()
		deadline = time.Until(d)
		select { // a wait with no budget fails the test rather than hangs it
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
		return chat.PressOutcome{}, nil
	}}
	serveWith(t, ct, sink)
	start := time.Now()
	if got := told(t, postInvoke(t, ct, "f:1", promptData("submit"), nil)); got != textPromptReceived {
		t.Errorf("told %q; want %q", got, textPromptReceived)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the answer took %v; want it at the end of the 50ms wait", took)
	}
	if deadline > ct.c.pressWait {
		t.Errorf("the sink was given %v to decide; want at most the wait, %v", deadline, ct.c.pressWait)
	}
}

// A sink that does not decide in the wait (the chat.Sink of a platform with no such seam) is given the press as before, and the press is
// answered as received.
func TestAPressToASinkThatDoesNotDecideIsRecordedOnly(t *testing.T) {
	ct := newClientTest(t)
	sink := &pressSink{}
	serveWith(t, ct, sink)
	if got := told(t, postInvoke(t, ct, "f:1", promptData("submit"), nil)); got != textPromptReceived {
		t.Errorf("told %q; want %q", got, textPromptReceived)
	}
	if n := len(sink.taken()); n != 1 {
		t.Errorf("the sink was given %d presses; want one", n)
	}
}

// The receiver asks the chat listener for the invoke's longer deadline, and the wait for a press's decision ends inside it, with time
// left to answer.
func TestTheInvokeDeadlineHoldsTheWait(t *testing.T) {
	ct := newClientTest(t)
	var _ chat.HTTPDeadline = ct.c.recv
	if got := ct.c.recv.HandlerTimeout(); got != invokeTimeout {
		t.Errorf("the receiver asks for %v; want %v", got, invokeTimeout)
	}
	if ct.c.pressWait >= invokeTimeout {
		t.Errorf("the wait for a decision, %v, does not end inside the invoke's deadline, %v", ct.c.pressWait, invokeTimeout)
	}
}

// A request's token waits for the signing keys at most verifyWait, whatever the longer deadline the receiver's requests are given: a
// fetch of the keys that is slow does not hold a place of the chat listener for the whole of it. The request is answered 503.
func TestATokenWaitsForTheKeysNoLongerThanBefore(t *testing.T) {
	key := newSigningKey(t, "k", "msteams")
	f := newFakeBotFramework(t, key)
	release := make(chan struct{})
	defer close(release)
	f.mux.HandleFunc("GET /slow", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	f.mu.Lock()
	f.jwksPath = "/slow"
	f.mu.Unlock()
	r, err := NewReceiver(ReceiverConfig{Connection: testConn, TenantID: testTenant, AppID: testAppID, MetadataURL: f.metadataURL(),
		HTTPClient: f.srv.Client(), Logger: testutil.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	r.verifyWait = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second) // the route's longer deadline
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/chat/teams/"+testConn, bytes.NewReader(editFixture(t, "invoke_action", func(map[string]any) {})))
	req.Header.Set("Authorization", bearer(t, key, claimsAt(time.Now())))
	rec := httptest.NewRecorder()
	start := time.Now()
	r.ServeHTTP(rec, req)
	if took := time.Since(start); rec.Code != http.StatusServiceUnavailable || took > time.Second {
		t.Errorf("answered %d after %v; want 503 once the 50ms wait for the keys ends", rec.Code, took)
	}
}

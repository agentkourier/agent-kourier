package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
)

const (
	wireWorking = `data: {"jsonrpc":"2.0","id":"1","result":{"statusUpdate":{"taskId":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_WORKING"}}}}` + "\n\n"
	wireDone    = `data: {"jsonrpc":"2.0","id":"1","result":{"task":{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"}}}}` + "\n\n"
)

// wireFinal is the final task of a stream that holds the reply three times, as kagent 0.9.x sends it: as the artifact, as the status
// message, and as a message of the history.
func wireFinal(text string) string {
	return fmt.Sprintf(`data: {"jsonrpc":"2.0","id":"1","result":{"task":{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"m-2","role":"ROLE_AGENT","parts":[{"text":%[1]q}]}},`+
		`"artifacts":[{"artifactId":"a-1","parts":[{"text":%[1]q}]}],"history":[{"messageId":"m-1","role":"ROLE_AGENT","parts":[{"text":%[1]q}]}]}}}`+"\n\n", text)
}

// wireText is an event of the stream that adds text to artifact a-1 (append) or replaces it.
func wireText(text string, appendMode bool) string {
	return fmt.Sprintf(`data: {"jsonrpc":"2.0","id":"1","result":{"artifactUpdate":{"taskId":"t-1","contextId":"c-1","append":%v,"artifact":{"artifactId":"a-1","parts":[{"text":%q}]}}}}`+"\n\n", appendMode, text)
}

// endless serves the stream of a tenant's agent that sends text until the client hangs up, counting the bytes it wrote
// and closing gone when it stops.
func endless(sent *atomic.Int64, gone chan<- struct{}, chunk string) func(rpc, http.ResponseWriter) {
	return func(call rpc, w http.ResponseWriter) {
		if call.Method != "SendStreamingMessage" {
			return
		}
		defer close(gone)
		write(w, []byte(wireWorking))
		ev := []byte(wireText(chunk, true))
		for {
			select {
			case <-call.Done:
				return
			default:
			}
			if _, err := w.Write(ev); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			sent.Add(int64(len(ev)))
		}
	}
}

// A tenant's agent that streams text for ever ends the turn with the error once it has sent the budget, and the stream
// is closed: the agent stops being read, and the heap stays a small multiple of the budget. Before the bound the
// heap grew with the stream (384 MiB of it took the heap of the old code past 70 GiB, mostly the strings of the
// concatenations, before the send failed).
func TestEndlessStreamEndsTheTurnWithinTheBudget(t *testing.T) {
	var sent atomic.Int64
	gone := make(chan struct{})
	fa := newFakeAgent(t, endless(&sent, gone, strings.Repeat("a", 64<<10)))
	var peak atomic.Uint64
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.Load() {
					peak.Store(m.HeapAlloc)
				}
			}
		}
	}()
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	h, got := collector()
	err := fa.agentVia(t, generic.New(0)).Send(t.Context(), message("go"), h)
	if !errors.Is(err, dialect.ErrResponseTooLarge) || errors.Is(err, ErrStreamLost) {
		t.Fatalf("err = %v, want ErrResponseTooLarge and no lost stream", err)
	}
	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Error("the stream was not closed: the agent is still being read")
	}
	for _, name := range []string{"GetTask", "SubscribeToTask"} {
		if n := fa.count(name); n != 0 {
			t.Errorf("%s called %d times: the reply was reconnected", name, n)
		}
	}
	var last int
	for _, e := range *got {
		if te, ok := e.(TextEvent); ok {
			last = len(te.Text)
		}
	}
	if last == 0 || last > DefaultMaxReplyBytes {
		t.Errorf("the thread was given %d bytes of text, want some, and at most %d", last, DefaultMaxReplyBytes)
	}
	t.Logf("the server wrote %d MiB, the client kept %d KiB, the heap peaked %d MiB over its start", sent.Load()>>20, last>>10, (peak.Load()-min(peak.Load(), base.HeapAlloc))>>20)
	if grew := peak.Load() - min(peak.Load(), base.HeapAlloc); grew > 128<<20 {
		t.Errorf("the heap grew by %d MiB over a reply bounded at %d MiB", grew>>20, DefaultMaxReplyBytes>>20)
	}
}

// A long reply is not cut: a stream of far more than the budget in all, and more than an answer may be, that replaces
// its text as it goes, ends with the answer.
func TestLongStreamThatReplacesItsTextIsNotCut(t *testing.T) {
	const events, size = 260, 64 << 10 // 16.25 MiB on the wire
	body := strings.Repeat("z", size)
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		if call.Method != "SendStreamingMessage" {
			return
		}
		write(w, []byte(wireWorking))
		for i := range events {
			write(w, []byte(wireText(fmt.Sprint(i%10)+body, false)))
		}
		write(w, []byte(wireDone))
	})
	h, got := collector()
	if err := fa.agentVia(t, generic.New(0)).Send(t.Context(), message("go"), h); err != nil {
		t.Fatal(err)
	}
	all := texts(*got)
	if len(all) != events || len(all[len(all)-1]) != size+1 {
		t.Errorf("got %d texts, the last of %d bytes; want %d, the last of %d", len(all), len(all[len(all)-1]), events, size+1)
	}
	if final, ok := (*got)[len(*got)-1].(FinalEvent); !ok || final.Task == nil {
		t.Errorf("last event = %#v, want the final event", (*got)[len(*got)-1])
	}
}

// A reply of just under the budget in all, in many events, is delivered whole: the largest the thread can have.
func TestReplyJustUnderTheBudgetIsDelivered(t *testing.T) {
	const size = 64 << 10
	chunks := (DefaultMaxReplyBytes - firstChunk) / size
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		if call.Method != "SendStreamingMessage" {
			return
		}
		write(w, []byte(wireWorking))
		for range chunks {
			write(w, []byte(wireText(strings.Repeat("q", size), true)))
		}
		write(w, []byte(wireFinal(strings.Repeat("q", chunks*size))))
	})
	h, got := collector()
	if err := fa.agentVia(t, generic.New(0)).Send(t.Context(), message("go"), h); err != nil {
		t.Fatalf("a reply of %d bytes, %d under the budget: %v", chunks*size, DefaultMaxReplyBytes-chunks*size, err)
	}
	if all := texts(*got); len(all[len(all)-1]) != chunks*size {
		t.Errorf("the last text has %d bytes, want %d", len(all[len(all)-1]), chunks*size)
	}
}

// The answer to GetTask is bounded at dialect.MaxResponseBytes: a task padded to exactly that is read, and one byte more fails
// with the error, with no task.
func TestGetTaskAnswerIsBounded(t *testing.T) {
	const prefix = `{"jsonrpc":"2.0","id":"1","result":{"id":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"}}}`
	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"exactly the bound", dialect.MaxResponseBytes, false},
		{"a byte over", dialect.MaxResponseBytes + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) {
				_, _ = io.WriteString(w, prefix+strings.Repeat(" ", tc.size-len(prefix)))
			})
			task, err := fa.agentVia(t, generic.New(0)).GetTask(t.Context(), "t-1")
			if tc.wantErr {
				if !errors.Is(err, dialect.ErrResponseTooLarge) || task != nil {
					t.Fatalf("task = %v, err = %v, want ErrResponseTooLarge and no task", task, err)
				}
				return
			}
			if err != nil || task == nil || task.ID != "t-1" {
				t.Fatalf("task = %v, err = %v, want the task", task, err)
			}
		})
	}
}

// The probe's card fetch is bounded at dialect.MaxCardBytes, which Check reports with the error: a card padded to exactly
// that is accepted, one byte more is not, and a card of a gibibyte is refused after the bound without being read.
func TestCheckBoundsTheCard(t *testing.T) {
	card := `{"name":"ask","capabilities":{"streaming":true},"supportedInterfaces":[{"url":"http://x","protocolBinding":"JSONRPC","protocolVersion":"1.0"}]}`
	for _, tc := range []struct {
		name    string
		size    int64
		wantErr bool
	}{
		{"exactly the bound", dialect.MaxCardBytes, false},
		{"a byte over", dialect.MaxCardBytes + 1, true},
		{"a gibibyte", 1 << 30, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, card)
				pad := []byte(strings.Repeat(" ", 1<<16))
				for left := tc.size - int64(len(card)); left > 0; {
					n, err := w.Write(pad[:min(int64(len(pad)), left)])
					if err != nil {
						return
					}
					left -= int64(n)
					sent.Add(int64(n))
				}
			}))
			t.Cleanup(srv.Close)
			p := NewPool(generic.New(0), Config{})
			t.Cleanup(p.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second) // the probe's bound
			defer cancel()
			err := p.Check(ctx, Target{
				Binding:     "team/sre",
				Backend:     &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, URL: srv.URL}},
				Agent:       v1alpha1.AgentRef{Name: "ask", Namespace: "kagent"},
				Credentials: token("t"),
			})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Check = %v, want the card accepted", err)
				}
				return
			}
			if !errors.Is(err, dialect.ErrResponseTooLarge) {
				t.Fatalf("Check = %v, want dialect.ErrResponseTooLarge", err)
			}
			// What the server wrote includes what the kernel buffered on both ends of the socket, which is several MiB on
			// a Linux runner and more under load, so this bound is far above the card's and far below the gibibyte.
			if got := sent.Load(); got > 64<<20 && tc.size > 64<<20 {
				t.Errorf("the server got to write %d MiB of %d MiB before the client hung up", got>>20, tc.size>>20)
			}
		})
	}
}

// The budget leaves room for the final task that repeats the reply, and it leaves it for the worst honest text. The final
// task of a stream holds the reply up to three times (the artifact, the status message and a message of the history), and
// JSON may double the size of a text: a newline or a quote is two bytes on the wire (an agent that escapes what is not
// ASCII makes more of it, and this does not cover that). Six times the budget is therefore the most a reply that fits
// the budget takes on the wire, and it must fit one event, or the turn ends with the notice for a reply that was shown
// whole and was under the budget. A budget that is four or five times smaller than an event still holds the repeated
// text of an ordinary reply, but not of one that is all line breaks, which is what this keeps.
func TestBudgetLeavesRoomForAFinalTaskThatRepeatsTheReply(t *testing.T) {
	const copies, escaped = 3, 2
	if copies*escaped*DefaultMaxReplyBytes > dialect.MaxEventBytes {
		t.Fatalf("%d copies of a reply of the budget (%d bytes), each twice its size once escaped, pass the bound of an event (%d): a reply the thread can hold might not fit its final task",
			copies, DefaultMaxReplyBytes, dialect.MaxEventBytes)
	}
	// The answer to GetTask holds the same task, and the history of the turn besides: it has to hold it with room to spare.
	if 2*copies*escaped*DefaultMaxReplyBytes > dialect.MaxResponseBytes {
		t.Fatalf("a final task at the budget, escaped, takes more than half of an answer (%d bytes): the history of a turn would not fit beside it", dialect.MaxResponseBytes)
	}
}

// The largest reply the budget allows, of the characters that JSON doubles, arrives in a final task of three copies that
// is the largest event the bound allows, and the same task is read as the answer to GetTask: both are read whole, by the
// real client, and the turn ends with the final event. This is the margin the test above states, run through the transport.
func TestFinalTaskOfAReplyAtTheBudgetIsReadAsAnEventAndAsAnAnswer(t *testing.T) {
	const size = 1 << 10
	chunks := (DefaultMaxReplyBytes - firstChunk) / size
	doubled := strings.Repeat("\"\n", size/2) // two bytes of text, four on the wire
	reply := strings.Repeat(doubled, chunks)
	final := wireFinal(reply)
	event := len(final) - len("\n") // as the bound counts it: the blank line that ends the event is not part of it
	if event > dialect.MaxEventBytes {
		t.Fatalf("the final task is an event of %d bytes, over the bound of %d", event, dialect.MaxEventBytes)
	}
	// GetTask answers with the task as the result, where an event of a stream has it under "task".
	answer := strings.TrimSuffix(strings.Replace(strings.TrimSuffix(strings.TrimPrefix(final, "data: "), "\n\n"), `"result":{"task":`, `"result":`, 1), "}")
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		switch call.Method {
		case "SendStreamingMessage":
			write(w, []byte(wireWorking))
			for range chunks {
				write(w, []byte(wireText(doubled, true)))
			}
			write(w, []byte(final))
		case "GetTask":
			_, _ = io.WriteString(w, answer)
		}
	})
	a := fa.agentViaWith(t, generic.New(0), quick)
	h, got := collector()
	if err := a.Send(t.Context(), message("go"), h); err != nil {
		t.Fatalf("a reply of %d bytes, whose final task is %d bytes of an event of at most %d: %v", len(reply), event, dialect.MaxEventBytes, err)
	}
	if last, ok := (*got)[len(*got)-1].(FinalEvent); !ok || last.Task == nil {
		t.Fatalf("last event = %#v, want the final event", (*got)[len(*got)-1])
	}
	task, err := a.GetTask(t.Context(), "t-1")
	if err != nil || task == nil || len(task.Artifacts) != 1 {
		t.Fatalf("GetTask = %v, %v; want the task, read whole", task, err)
	}
	t.Logf("a reply of %d KiB; its final task is an event of %d KiB (%d%% of the bound of an event, %d%% of the bound of an answer)",
		len(reply)>>10, event>>10, 100*event/dialect.MaxEventBytes, 100*event/dialect.MaxResponseBytes)
}

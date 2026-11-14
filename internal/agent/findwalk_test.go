package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
)

// A lookup that cannot be completed is incomplete, and each way it can fail to be is one error: the caller must not send the turn to
// find out, and asking again gets the same. A page over the bound at the smallest size is one (findpage_test.go); these are the others.

// A server whose pages never end is cut at maxListPages. The lookup is incomplete, which the caller does not retry: it used to be a plain
// error, which a recovery asked again for ever.
func TestFindTaskWhosePagesNeverEndIsIncomplete(t *testing.T) {
	var calls atomic.Int32
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		if call.Method == "ListTasks" {
			n := calls.Add(1)
			_, _ = w.Write([]byte(listPage(fmt.Sprint("p", n))))
		}
	})
	task, holders, err := fa.agentViaWith(t, generic.New(0), quick).FindTask(t.Context(), "c-1", "m-1")
	if !errors.Is(err, ErrLookupIncomplete) || task != nil || holders != nil {
		t.Fatalf("FindTask = %v, %v, %v; want an incomplete lookup", task, holders, err)
	}
	// The literal, not the constant: the documentation says 200 pages (docs/how-to/troubleshoot.md, docs/explanation/security.md, and the
	// limits page, which TestLimitsPage holds to the constant), and the error is the text that troubleshoot.md tells an operator to look for.
	if n := calls.Load(); n != 200 {
		t.Errorf("%d pages read, want 200 and no more", n)
	}
	if !strings.Contains(err.Error(), "ran past 200 pages") {
		t.Errorf("err = %v; want it to say `ran past 200 pages`", err)
	}
}

// A page token that comes again ends the walk too, as incomplete, and a holder found before it is the answer.
func TestFindTaskThatRepeatsAPageTokenIsIncompleteUnlessAPageHeldTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string
		want  bool // a holder is the answer
	}{
		{"no page held the turn", listPage("p"), false},
		{"the first page held the turn", listPage("p", foundTask), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &listing{fits: 50, pages: map[string]string{"": tc.first, "p": listPage("p")}}
			fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) { l.serveSized(call, w) })
			task, _, err := fa.agentViaWith(t, generic.New(0), quick).FindTask(t.Context(), "c-1", "m-1")
			if tc.want && (err != nil || task == nil || task.ID != "t-found") {
				t.Fatalf("FindTask = %v, %v; want the holder", task, err)
			}
			if !tc.want && !errors.Is(err, ErrLookupIncomplete) {
				t.Fatalf("FindTask = %v, %v; want an incomplete lookup", task, err)
			}
		})
	}
}

// serveSized is serve for a test that wants the server's own page size to fit: a request with no pageSize is answered as 50 would be.
func (l *listing) serveSized(call rpc, w http.ResponseWriter) {
	if call.Method != "ListTasks" {
		return
	}
	var p struct {
		PageToken string `json:"pageToken"`
	}
	_ = json.Unmarshal(call.Params, &p)
	l.mu.Lock()
	page := l.pages[p.PageToken]
	l.mu.Unlock()
	_, _ = w.Write([]byte(page))
}

// The walk as a whole has a deadline, and a walk that passes it is incomplete: each page is a call of its own, and the halvings
// of each, so that without a bound a server that answers slowly holds the conversation's runner for hours. The caller's own
// cancellation is not that: it is the caller's end, and says so.
func TestFindTaskThatTakesLongerThanItsDeadlineIsIncompleteAndTheCallersEndIsNot(t *testing.T) {
	fa := newFakeAgent(t, func(call rpc, _ http.ResponseWriter) {
		if call.Method == "ListTasks" {
			<-call.Done // the server does not answer
		}
	})
	cfg := quick
	cfg.LookupTimeout = 50 * time.Millisecond
	_, _, err := fa.agentViaWith(t, generic.New(0), cfg).FindTask(t.Context(), "c-1", "m-1")
	if !errors.Is(err, ErrLookupIncomplete) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FindTask = %v; want an incomplete lookup, with the deadline as its cause", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	cfg.LookupTimeout = time.Hour
	_, _, err = fa.agentViaWith(t, generic.New(0), cfg).FindTask(ctx, "c-1", "m-1")
	if errors.Is(err, ErrLookupIncomplete) || !errors.Is(err, context.Canceled) {
		t.Errorf("FindTask = %v; want the caller's cancellation, and no claim that the list could not be read", err)
	}
}

// Whether a server continues a page token with another page size is not known. A refusal on the first call at a size the walk lowered itself
// to (a 4xx, or an error the agent answered with) is therefore incomplete. A 5xx is no sign of a token that was not read: it is the error
// it is, which is retried, so that one 503 does not fail a turn for good. Nor is a refusal with no shrink before it.
func TestFindTaskCallThatIsRefusedRightAfterAShrinkIsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstIsBig bool
		status     int // the HTTP status the server answers the later calls with, or 0 for a JSON-RPC error
		wantIncomp bool
	}{
		{"a 400 after a shrink", true, http.StatusBadRequest, true},
		{"a JSON-RPC error after a shrink", true, 0, true},
		{"a 503 after a shrink", true, http.StatusServiceUnavailable, false},
		{"a 400 with no shrink before it", false, http.StatusBadRequest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var sizes []int
			fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
				if call.Method != "ListTasks" {
					return
				}
				var p struct {
					PageSize int `json:"pageSize"`
				}
				_ = json.Unmarshal(call.Params, &p)
				mu.Lock()
				sizes = append(sizes, p.PageSize)
				mu.Unlock()
				switch {
				case tc.firstIsBig && p.PageSize == 0:
					_, _ = w.Write(oversizedAnswer())
				case tc.status == 0:
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","error":{"code":-32602,"message":"invalid params"}}`))
				default:
					http.Error(w, "bad page", tc.status) // as a server might answer a token it does not read at this size
				}
			})
			_, _, err := fa.agentViaWith(t, generic.New(0), quick).FindTask(t.Context(), "c-1", "m-1")
			if err == nil || errors.Is(err, ErrLookupIncomplete) != tc.wantIncomp || (tc.status != 0 && httpStatus(err) != tc.status) {
				t.Fatalf("FindTask = %v; want the failure of the later call, and incomplete = %v", err, tc.wantIncomp)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.firstIsBig && !slices.Equal(sizes, []int{0, 25}) {
				t.Errorf("sizes asked = %v, want [0 25]: the walk stops at the first failure after the shrink", sizes)
			}
		})
	}
}

// An agent card that is too large is no answer that is. It is read when a connection is opened, so a call that had to open one
// meets it, with nothing sent and nothing read of any task: its error says so, and the callers that mean an answer (the lookup of a
// turn, the cancel of a task, the read of one) do not take it for one.
func TestACardThatIsTooLargeIsNotAnAnswerThatIs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"ask","capabilities":{"streaming":true},"x":"` + strings.Repeat("y", dialect.MaxCardBytes) + `"}`))
	}))
	t.Cleanup(srv.Close)
	fa := &fakeAgent{Server: srv}
	for name, call := range map[string]func(a *Agent) error{
		"FindTask":   func(a *Agent) error { _, _, err := a.FindTask(t.Context(), "c-1", "m-1"); return err },
		"CancelTask": func(a *Agent) error { _, err := a.CancelTask(t.Context(), "t-1"); return err },
		"GetTask":    func(a *Agent) error { _, err := a.GetTask(t.Context(), "t-1"); return err },
		"Follow":     func(a *Agent) error { return a.Follow(t.Context(), "t-1", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			err := call(fa.agentViaWith(t, generic.New(0), quick))
			if !errors.Is(err, dialect.ErrCardTooLarge) || !errors.Is(err, dialect.ErrResponseTooLarge) {
				t.Fatalf("err = %v, want the error of a card that is too large", err)
			}
			if dialect.AnswerTooLarge(err) || errors.Is(err, ErrLookupIncomplete) || errors.Is(err, ErrStreamLost) {
				t.Errorf("err = %v is taken for an answer that is too large, an incomplete lookup, or a lost stream", err)
			}
		})
	}
}

// A refusal is taken for a page token that was not continued at another size only on the first call at a size the walk lowered. Once a
// call at that size has worked, the size is the walk's own and a refusal of a later page is the error it is (a 400 is not retried, but it
// is not an incomplete list either): the walk is no longer in the doubt that the shrink brought.
func TestFindTaskRefusalAfterASizeThatWorkedIsNotIncomplete(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	fa := newFakeAgent(t, func(call rpc, w http.ResponseWriter) {
		if call.Method != "ListTasks" {
			return
		}
		var p struct {
			PageSize  int    `json:"pageSize"`
			PageToken string `json:"pageToken"`
		}
		_ = json.Unmarshal(call.Params, &p)
		mu.Lock()
		tokens = append(tokens, p.PageToken)
		mu.Unlock()
		switch {
		case p.PageSize == 0:
			_, _ = w.Write(oversizedAnswer()) // the first call: too large, so the walk lowers the size
		case p.PageToken == "":
			_, _ = w.Write([]byte(listPage("p2"))) // at the lowered size, the first page works
		default:
			http.Error(w, "bad page", http.StatusBadRequest) // the second page is refused at a size that has worked
		}
	})
	_, _, err := fa.agentViaWith(t, generic.New(0), quick).FindTask(t.Context(), "c-1", "m-1")
	if err == nil || errors.Is(err, ErrLookupIncomplete) || httpStatus(err) != http.StatusBadRequest {
		t.Fatalf("FindTask = %v; want the 400 of the second page, and no claim that the list could not be read in full", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"", "", "p2"}; !slices.Equal(tokens, want) {
		t.Errorf("page tokens asked = %q, want %q", tokens, want)
	}
}

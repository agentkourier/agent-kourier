package web_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
)

// receiver is an HTTP endpoint that keeps what the platform sent it, and answers with code (and Retry-After, if set).
type receiver struct {
	srv        *httptest.Server
	mu         sync.Mutex
	got        []*http.Request
	bodies     []string
	code       int
	retryAfter string
	location   string
	hold       chan struct{} // if set, the answer waits for it to close
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{code: http.StatusOK}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, req)
		r.bodies = append(r.bodies, string(body))
		code, ra, loc, hold := r.code, r.retryAfter, r.location, r.hold
		r.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-req.Context().Done():
			}
		}
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		if loc != "" {
			w.Header().Set("Location", loc)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) set(code int, retryAfter string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code, r.retryAfter = code, retryAfter
}

// take returns, and forgets, the requests the receiver got.
func (r *receiver) take() ([]*http.Request, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	got, bodies := r.got, r.bodies
	r.got, r.bodies = nil, nil
	return got, bodies
}

var (
	target = chat.Target{Conn: "ns/web", Channel: "ch-1", Thread: "m-1"}
	hello  = chat.Say(chat.Message{Fallback: "hello", Parts: []chat.Part{chat.Body{Markdown: "**hello**"}}})
)

func TestPostSendsTheEncodedReplyToTheFixedURL(t *testing.T) {
	r := newReceiver(t)
	prof := newTestProfile()
	p := newPlatform(t, prof, r.srv.URL+"/hook", nil)

	ref, err := p.Post(t.Context(), target, hello, "outbox-42")
	if err != nil {
		t.Fatal(err)
	}

	if want := (chat.Ref{Platform: "web", Conn: "ns/web", Channel: "ch-1", Thread: "m-1", ID: "outbox-42"}); ref != want {
		t.Errorf("Ref %+v, want %+v", ref, want)
	}
	reps := prof.taken()
	want := web.Reply{Kind: web.ReplyPost, Connection: "ns/web", Channel: "ch-1", Thread: "m-1", ID: "outbox-42", Key: "outbox-42", Message: hello}
	if len(reps) != 1 || fmt.Sprint(reps[0]) != fmt.Sprint(want) {
		t.Errorf("Encode was given %+v, want [%+v]", reps, want)
	}
	got, bodies := r.take()
	if len(got) != 1 {
		t.Fatalf("the receiver got %d requests, want 1", len(got))
	}
	if got[0].Method != http.MethodPost || got[0].URL.Path != "/hook" {
		t.Errorf("the request is %s %s, want POST /hook", got[0].Method, got[0].URL.Path)
	}
	if got[0].Header.Get("X-Test-Kind") != "post" || got[0].Header.Get("Idempotency-Key") != "outbox-42" {
		t.Errorf("the request's headers are %v, want the encoder's", got[0].Header)
	}
	if !strings.Contains(bodies[0], "**hello**") {
		t.Errorf("the body is %q, want the encoder's", bodies[0])
	}
}

// Each update is a reply of its own, with its own key: an update replaces, so a receiver that applies one twice shows the same, and
// one that drops the second of two updates with the same content would show an older one after an A, B, A.
func TestUpdateSendsAReplyWithANewKeyEachTime(t *testing.T) {
	r := newReceiver(t)
	prof := newTestProfile()
	p := newPlatform(t, prof, r.srv.URL, nil)
	ref := p.RefAt("ch-1", "m-1", "outbox-42")

	for range 2 {
		if err := p.Update(t.Context(), ref, hello); err != nil {
			t.Fatal(err)
		}
	}

	reps := prof.taken()
	if len(reps) != 2 {
		t.Fatalf("Encode was given %d replies, want 2", len(reps))
	}
	for _, rep := range reps {
		if rep.Kind != web.ReplyUpdate || rep.ID != "outbox-42" || rep.Channel != "ch-1" || rep.Thread != "m-1" || rep.Connection != "ns/web" || rep.Key == "" {
			t.Errorf("Encode was given %+v, want an update of outbox-42 with a key", rep)
		}
	}
	if reps[0].Key == reps[1].Key {
		t.Errorf("two updates have the key %q, want one each", reps[0].Key)
	}
	if got, _ := r.take(); len(got) != 2 {
		t.Errorf("the receiver got %d requests, want 2", len(got))
	}
}

func TestPostRefusesWhatItCannotSend(t *testing.T) {
	r := newReceiver(t)
	prof := newTestProfile()
	p := newPlatform(t, prof, r.srv.URL, nil)
	tools := chat.Prompt{ID: "ix", Tools: []chat.Tool{{Name: "rm", Args: "-rf"}}}

	for _, tc := range []struct {
		name string
		out  chat.Outgoing
		tag  string
	}{
		{"a zero Outgoing", chat.Outgoing{}, "t"},
		{"a prompt of tools", chat.Ask(tools), "t"},
		{"the ending of a prompt of tools", chat.End(chat.Ending{Outcome: chat.TimedOut, Prompt: tools}), "t"},
		{"no tag", hello, ""},
	} {
		if _, err := p.Post(t.Context(), target, tc.out, tc.tag); !errors.Is(err, chat.ErrInvalid) {
			t.Errorf("%s: %v, want chat.ErrInvalid", tc.name, err)
		}
	}
	if err := p.Update(t.Context(), p.RefAt("ch-1", "", "m"), chat.Ask(tools)); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("an update to a prompt of tools: %v, want chat.ErrInvalid", err)
	}
	prof.encErr = chat.ErrTooLarge
	if _, err := p.Post(t.Context(), target, hello, "t"); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("a reply the encoder refuses: %v, want the encoder's error", err)
	}
	if got, _ := r.take(); len(got) != 0 {
		t.Errorf("the receiver got %d requests, want none", len(got))
	}
}

// Find says not found without asking: the receiver has no lookup, and a post that the outbox sends again carries the same
// Idempotency-Key, which the receiver drops.
func TestFindAsksNothing(t *testing.T) {
	r := newReceiver(t)
	p := newPlatform(t, newTestProfile(), r.srv.URL, nil)
	if _, found, err := p.Find(t.Context(), target, "outbox-42", epoch); found || err != nil {
		t.Errorf("Find: found %t, %v; want not found", found, err)
	}
	if got, _ := r.take(); len(got) != 0 {
		t.Errorf("the receiver got %d requests, want none", len(got))
	}
}

// A redirect is not followed: the signed body goes to the connection's URL and nowhere else, and the answer is the receiver's error.
func TestARedirectIsNotFollowed(t *testing.T) {
	elsewhere := newReceiver(t)
	r := newReceiver(t)
	r.code, r.location = http.StatusTemporaryRedirect, elsewhere.srv.URL
	p := newPlatform(t, newTestProfile(), r.srv.URL, nil)

	_, err := p.Post(t.Context(), target, hello, "t")

	if v := p.Classify(chat.OpPost, err); v.Class != chat.Permanent || v.Code != "307" {
		t.Errorf("a redirect: %v, classified %+v; want Permanent with the code 307", err, v)
	}
	if got, _ := elsewhere.take(); len(got) != 0 {
		t.Errorf("the redirect was followed: the other server got %d requests", len(got))
	}
}

// A receiver that does not answer within the call's bound fails the post as Transient, so that the outbox tries it again. The bound
// of the platform's own client is ReplyTimeout (TestTheClientIsBounded); the test uses the caller's deadline, which is shorter.
func TestAReplyThatTakesTooLongIsTransient(t *testing.T) {
	r := newReceiver(t)
	r.hold = make(chan struct{})
	t.Cleanup(func() { close(r.hold) })
	p := newPlatform(t, newTestProfile(), r.srv.URL, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err := p.Post(ctx, target, hello, "t")

	if err == nil {
		t.Fatal("no error")
	}
	if v := p.Classify(chat.OpPost, err); v.Class != chat.Transient {
		t.Errorf("Classify is %+v, want Transient", v)
	}
}

func TestTheClientIsBounded(t *testing.T) {
	p := newPlatform(t, newTestProfile(), receiverURL, nil)
	if got := p.ClientTimeout(); got != web.ReplyTimeout {
		t.Errorf("the client's timeout is %s, want ReplyTimeout (%s)", got, web.ReplyTimeout)
	}
}

func TestClassifySortsTheReceiversAnswers(t *testing.T) {
	r := newReceiver(t)
	p := newPlatform(t, newTestProfile(), r.srv.URL, nil)
	inAMinute := epoch.Add(30 * time.Second).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		code       int
		retryAfter string
		want       chat.Verdict
	}{
		{http.StatusTooManyRequests, "7", chat.Verdict{Class: chat.Transient, Code: "429", RetryAfter: 7 * time.Second}},
		{http.StatusTooManyRequests, "", chat.Verdict{Class: chat.Transient, Code: "429", RetryAfter: time.Second}},
		{http.StatusTooManyRequests, "0", chat.Verdict{Class: chat.Transient, Code: "429", RetryAfter: time.Second}},
		{http.StatusTooManyRequests, "junk", chat.Verdict{Class: chat.Transient, Code: "429", RetryAfter: time.Second}},
		{http.StatusTooManyRequests, inAMinute, chat.Verdict{Class: chat.Transient, Code: "429", RetryAfter: 30 * time.Second}},
		{http.StatusTooManyRequests, "86400", chat.Verdict{Class: chat.Transient, Code: "429", RetryAfter: web.MaxRetryAfter}},
		{http.StatusInternalServerError, "", chat.Verdict{Class: chat.Transient, Code: "500"}},
		{http.StatusBadGateway, "", chat.Verdict{Class: chat.Transient, Code: "502"}},
		{http.StatusServiceUnavailable, "5", chat.Verdict{Class: chat.Transient, Code: "503", RetryAfter: 5 * time.Second}},
		{http.StatusServiceUnavailable, "", chat.Verdict{Class: chat.Transient, Code: "503"}},
		{http.StatusRequestTimeout, "", chat.Verdict{Class: chat.Transient, Code: "408"}},
		{http.StatusBadRequest, "", chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonInvalidContent, Code: "400"}},
		{http.StatusUnprocessableEntity, "", chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonInvalidContent, Code: "422"}},
		{http.StatusUnauthorized, "", chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther, Code: "401"}},
		{http.StatusForbidden, "", chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther, Code: "403"}},
		{http.StatusNotFound, "", chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "404"}},
		{http.StatusGone, "", chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "410"}},
		{http.StatusRequestEntityTooLarge, "", chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonTooLong, Code: "413"}},
		{http.StatusConflict, "", chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther, Code: "409"}},
	} {
		r.set(tc.code, tc.retryAfter)
		_, err := p.Post(t.Context(), target, hello, "t")
		if err == nil {
			t.Errorf("%d: no error", tc.code)
			continue
		}
		if v := p.Classify(chat.OpPost, err); v != tc.want {
			t.Errorf("%d (Retry-After %q): %+v, want %+v", tc.code, tc.retryAfter, v, tc.want)
		}
	}
	r.set(http.StatusNoContent, "")
	if _, err := p.Post(t.Context(), target, hello, "t"); err != nil {
		t.Errorf("a 204: %v, want success", err)
	}
	for _, err := range []error{errors.New("boom"), context.Canceled, context.DeadlineExceeded} {
		if v := p.Classify(chat.OpPost, err); v.Class != chat.Transient {
			t.Errorf("%v: %+v, want Transient", err, v)
		}
	}
}

// Fits asks the encoder: a prompt is too large when the encoder says so, and a prompt of tools is invalid, since the platform has no
// presses to approve one with.
func TestFitsAsksTheEncoder(t *testing.T) {
	prof := newTestProfile()
	p := newPlatform(t, prof, receiverURL, nil)
	q := chat.Prompt{ID: "ix", Questions: []chat.Question{{Text: "which"}}}
	if err := p.Fits(q); err != nil {
		t.Errorf("a small prompt: %v", err)
	}
	if err := p.Fits(chat.Prompt{ID: "ix", Tools: []chat.Tool{{Name: "rm"}}}); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("a prompt of tools: %v, want chat.ErrInvalid", err)
	}
	if err := p.Fits(chat.Prompt{}); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("an invalid prompt: %v, want chat.ErrInvalid", err)
	}
	prof.encErr = fmt.Errorf("test: %w", chat.ErrTooLarge)
	if err := p.Fits(q); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("a prompt the encoder refuses as too large: %v, want chat.ErrTooLarge", err)
	}
}

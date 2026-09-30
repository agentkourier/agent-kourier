package agent

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// A reconnect request the backend refuses (4xx) will be refused again, so the send stops at
// once and keeps the status. A 5xx is worth the whole budget.
func TestReconnectStopsOnARefusedRequest(t *testing.T) {
	for _, tc := range []struct {
		code      int
		wantCalls int
	}{{401, 1}, {403, 1}, {404, 1}, {429, 1}, {503, 3}} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				send := make(chan item)
				refused := fmt.Errorf("get task: %w", &dialect.HTTPStatusError{Code: tc.code})
				m := &memConn{sends: []chan item{send}, gets: []getResult{{task: nil, err: refused}}}
				s := sendAsync(t, memAgent(t, m, func(c *Config) { c.MaxReconnects = 3 }))
				s.feed(send, item{ev: working()})
				s.feed(send, item{err: io.ErrUnexpectedEOF})
				err := s.result()
				var lost *StreamLostError
				if !errors.As(err, &lost) || lost.TaskID != "t1" {
					t.Fatalf("err = %v, want a StreamLostError for t1", err)
				}
				if got := errors.Is(err, ErrCredentialsRejected); got != (tc.code == 401 || tc.code == 403) {
					t.Errorf("ErrCredentialsRejected = %v for %d", got, tc.code)
				}
				if n := countOf(m.calls(), "get t1"); n != tc.wantCalls {
					t.Errorf("%d GetTask calls, want %d", n, tc.wantCalls)
				}
			})
		})
	}
}

// A token that expired is refused at the card fetch, which is where the Pool connects on first
// use and after every credential change. Every call must carry the signal to re-mint it.
func TestCardRefusedTypesEveryCall(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			fa := newFakeAgent(t, liveStream(t))
			fa.cardStatus = code
			agent := newPool(t).Agent(targetFor(fa, "team/sre", token("t")))
			calls := map[string]func() error{
				"Send":       func() error { return agent.Send(t.Context(), message("hi"), nil) },
				"Answer":     func() error { return agent.Answer(t.Context(), message("hi"), nil) },
				"GetTask":    func() error { _, err := agent.GetTask(t.Context(), "t1"); return err },
				"CancelTask": func() error { _, err := agent.CancelTask(t.Context(), "t1"); return err },
				"FindTask":   func() error { _, _, err := agent.FindTask(t.Context(), "c1", "m1"); return err },
			}
			for name, call := range calls {
				if err := call(); !errors.Is(err, ErrCredentialsRejected) {
					t.Errorf("%s: err = %v, want ErrCredentialsRejected", name, err)
				}
			}
			if err := calls["Send"](); !errors.Is(err, ErrNotSent) {
				t.Errorf("Send: err = %v, want ErrNotSent too", err)
			}
		})
	}
}

// The front door answers a send with an HTTP status before any event. A 4xx means it
// refused the request, so nothing was sent and retrying is safe; 401 and 403 also say the
// credentials were refused (WP-21e re-mints). A 5xx may have come after the agent took the
// message, so its outcome is unknown.
func TestHTTPStatusBeforeAnyEventTypesTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		code       int
		wantSent   bool // not sent
		wantAuth   bool
		wantUnsure bool
	}{
		{http.StatusUnauthorized, true, true, false},
		{http.StatusForbidden, true, true, false},
		{http.StatusNotFound, true, false, false},
		{http.StatusTooManyRequests, true, false, false},
		{http.StatusInternalServerError, false, false, true},
		{http.StatusBadGateway, false, false, true},
		{http.StatusGatewayTimeout, false, false, true},
	} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) { http.Error(w, "no", tc.code) })
			err := newPool(t).Agent(targetFor(fa, "team/sre", token("t"))).Send(t.Context(), message("hi"), nil)
			if errors.Is(err, ErrNotSent) != tc.wantSent || errors.Is(err, ErrCredentialsRejected) != tc.wantAuth || errors.Is(err, ErrOutcomeUnknown) != tc.wantUnsure {
				t.Errorf("err = %v; want not-sent %v, credentials-rejected %v, unknown %v", err, tc.wantSent, tc.wantAuth, tc.wantUnsure)
			}
		})
	}
}

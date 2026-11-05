package kagentv1

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// A card past its bound is told apart from an answer past its: the card is read when a connection is opened, where no call has
// been made and nothing of a task is known, and a caller that means the answer to a call (dialect.AnswerTooLarge) must not take it
// for one. Both are dialect.ErrResponseTooLarge, as every response past a bound is.
func TestACardPastItsBoundIsNotAnAnswerPastItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     string
		accept     string
		total      int64
		wantCard   bool
		wantAnswer bool
	}{
		{"the card", http.MethodGet, "", dialect.MaxCardBytes + 1, true, false},
		{"the answer to a call", http.MethodPost, "", dialect.MaxResponseBytes + 1, false, true},
		{"an event of a stream", http.MethodPost, "text/event-stream", dialect.MaxEventBytes + 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctype, unit := "application/json", " "
			if tc.accept != "" {
				ctype, unit = "text/event-stream", "x"
			}
			srv := httptest.NewServer(pattern(ctype, unit, tc.total, false))
			t.Cleanup(srv.Close)
			h := http.Header{}
			if tc.accept != "" {
				h.Set("Accept", tc.accept)
			}
			_, err := get(t, srv, tc.method, h, dialect.Credentials{UserID: "u"})
			if !errors.Is(err, dialect.ErrResponseTooLarge) {
				t.Fatalf("err = %v, want dialect.ErrResponseTooLarge", err)
			}
			if got := errors.Is(err, dialect.ErrCardTooLarge); got != tc.wantCard {
				t.Errorf("ErrCardTooLarge = %v, want %v", got, tc.wantCard)
			}
			if got := dialect.AnswerTooLarge(err); got != tc.wantAnswer {
				t.Errorf("AnswerTooLarge = %v, want %v", got, tc.wantAnswer)
			}
		})
	}
}

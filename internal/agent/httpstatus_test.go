package agent

import (
	"errors"
	"net/http"
	"testing"
)

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

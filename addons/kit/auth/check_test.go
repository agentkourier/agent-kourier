package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The reasons are the labels the add-ons count refusals under, so their values are fixed.
func TestReasonsAreTheLabels(t *testing.T) {
	for reason, label := range map[Reason]string{ReasonMissing: "missing", ReasonInvalid: "invalid", ReasonUnavailable: "unavailable"} {
		if string(reason) != label {
			t.Errorf("reason %q, want %q", reason, label)
		}
	}
}

// A refusal's log line names the header the check reads when Header is set, and no header otherwise, with the reason
// and the remote address either way.
func TestRefusalLogNamesTheHeader(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		want         string // the log line's attributes, in order
	}{
		{"header set", "X-Kourier-Token", "reason=missing header=X-Kourier-Token remote=192.0.2.1:1234"},
		{"header unset", "", "reason=missing remote=192.0.2.1:1234"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := newLog()
			check := &Check{Verifier: verifierFunc(func(*http.Request) (string, error) { return "", ErrMissing }), Header: tc.header, Log: log}
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/a2a", nil)
			req.RemoteAddr = "192.0.2.1:1234"
			check.Wrap(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), req)
			if got := buf.String(); !strings.Contains(got, `msg="refused an A2A request" `+tc.want+"\n") {
				t.Fatalf("log = %q, want the refusal with %q", got, tc.want)
			}
		})
	}
}

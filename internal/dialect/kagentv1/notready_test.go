package kagentv1

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/errordetails"
)

// kagent's own answers, as its source builds them (v1.0.0-alpha5, interactions.go).
var (
	errNotAccepted = a2a.NewError(a2a.ErrUnsupportedOperation, "input was not accepted; retry after the session becomes available").
			WithErrorInfoMeta(map[string]string{"reason": "KAGENT_SEND_NOT_ACCEPTED", "retryAfterMs": "100"})
	errLifecycle  = a2a.NewError(a2a.ErrUnsupportedOperation, "Session cannot accept work during a lifecycle operation")
	errNoRevision = a2a.NewError(a2a.ErrUnsupportedOperation, "Agent has no successful revision")
	// errBusy is what a session with an active task answers: measured (WP-02), message and all.
	errBusy = a2a.NewError(a2a.ErrUnsupportedOperation, "resource conflict")
)

func TestNotReadyTellsASessionThatCannotTakeInputFromABusyOne(t *testing.T) {
	d := &Dialect{}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"not accepted, by its reason", errNotAccepted, true},
		{"a lifecycle operation, by its text", errLifecycle, true},
		{"no successful revision, by its text", errNoRevision, true},
		{"wrapped, as the agent package wraps it", fmt.Errorf("send message: %w: %w", errors.New("agent: the session has an active task"), errLifecycle), true},
		{"busy", errBusy, false},
		{"busy, with some other ErrorInfo reason", a2a.NewError(a2a.ErrUnsupportedOperation, "resource conflict").WithErrorInfoMeta(map[string]string{"reason": "SOMETHING_ELSE"}), false},
		{"busy, with an ErrorInfo and no metadata", a2a.NewError(a2a.ErrUnsupportedOperation, "resource conflict").WithTypedDetails(errordetails.NewErrorInfo("UNSUPPORTED_OPERATION", a2a.ProtocolDomain, nil)), false},
		{"the reason on another code", a2a.NewError(a2a.ErrInternalError, "not accepted").WithErrorInfoMeta(map[string]string{"reason": "KAGENT_SEND_NOT_ACCEPTED"}), false},
		{"the text on another code", a2a.NewError(a2a.ErrInternalError, "Session cannot accept work during a lifecycle operation"), false},
		{"no error from the agent", errors.New("connection reset"), false},
		{"no error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.NotReady(tc.err); got != tc.want {
				t.Errorf("NotReady(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// The error that reaches the dialect comes off the wire: the a2a client decodes the JSON-RPC error into an
// a2a.Error, with the ErrorInfo of its data as typed details. A decoder that leaves the metadata as a map of
// values must be read too.
func TestNotReadyReadsTheErrorInfoOffTheWire(t *testing.T) {
	var info errordetails.Typed
	raw := `{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"UNSUPPORTED_OPERATION","domain":"a2a-protocol.org","metadata":{"reason":"KAGENT_SEND_NOT_ACCEPTED","retryAfterMs":"100"}}`
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatal(err)
	}
	err := a2a.NewError(a2a.ErrUnsupportedOperation, "x").WithTypedDetails(&info)
	if !(&Dialect{}).NotReady(err) {
		t.Errorf("NotReady(%v) = false, want true for the reason in decoded metadata", err)
	}
}

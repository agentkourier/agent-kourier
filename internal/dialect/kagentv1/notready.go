package kagentv1

import (
	"errors"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/errordetails"
)

// notAcceptedReason is the metadata reason of the -32004 that kagent answers a send with when the session's
// dispatch could not be reserved within its wait, "input was not accepted; retry after the session becomes
// available" (go/core/internal/service/session/interactions.go, v1.0.0-alpha5).
const notAcceptedReason = "KAGENT_SEND_NOT_ACCEPTED"

// notReadyMessages are the texts of the -32004 answers that say the session or the agent cannot take input at
// all, as opposed to the answer of a busy session, which is "resource conflict" (WP-02, measured). They are
// matched as substrings, so a longer sentence around them does not hide them.
var notReadyMessages = []string{
	"cannot accept work during a lifecycle operation", // the session is not READY, or an operation runs on it
	"has no successful revision",                      // the agent has not been rolled out yet
}

// NotReady reports whether err is a -32004 that says the session cannot take input yet, which kagent tells
// apart from a busy session by the reason in the error's metadata (KAGENT_SEND_NOT_ACCEPTED) or by its
// text. Anything else, including a -32004 it cannot place, is not: a session that is busy and one that is not
// ready both answer -32004, and only these answers are known to mean the second.
//
// UNCONFIRMED against a live kagent: that alpha5 sends these for a session that is still starting. The
// answers were read in its source, and the busy answer ("resource conflict") was measured.
func (*Dialect) NotReady(err error) bool {
	var rpc *a2a.Error
	if !errors.As(err, &rpc) || !errors.Is(rpc, a2a.ErrUnsupportedOperation) {
		return false
	}
	for _, d := range rpc.TypedDetails {
		if d.TypeURL == errordetails.ErrorInfoType && metadata(d.Value["metadata"])["reason"] == notAcceptedReason {
			return true
		}
	}
	for _, m := range notReadyMessages {
		if strings.Contains(rpc.Message, m) {
			return true
		}
	}
	return false
}

// metadata reads the metadata of an ErrorInfo detail, which is a map of strings as the client builds it
// and a map of values as a decoder that did not know the type leaves it.
func metadata(v any) map[string]string {
	switch m := v.(type) {
	case map[string]string:
		return m
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		return out
	}
	return nil
}

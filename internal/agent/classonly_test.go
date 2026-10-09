package agent

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// rpcFailure answers as an agent does with a JSON-RPC error of the given code and message: as an event on a stream, and
// as a plain response to a unary call.
func rpcFailure(code int, message string) func(rpc, http.ResponseWriter) {
	body := fmt.Appendf(nil, `{"jsonrpc":"2.0","id":"1","error":{"code":%d,"message":%q}}`, code, message)
	return func(c rpc, w http.ResponseWriter) {
		if strings.HasPrefix(c.Method, "Send") || strings.HasPrefix(c.Method, "Subscribe") {
			write(w, append(append([]byte("data: "), body...), "\n\n"...))
			return
		}
		_, _ = w.Write(body)
	}
}

// An error the agent answered with says its class and nothing the agent wrote: kagent puts the exception of a failed
// model call or tool in a -32603, and that may quote model output, a tool's input or a credential, and Agent Kourier's
// errors are logged. The error still is the agent's error to errors.Is and errors.As (the typing, isProtocol and the
// dialect's NotReady need that), and an answer's -32603 is still a rejected answer. This is the real client over the
// fake agent, through Send, Answer and the unary calls.
func TestAgentErrorTextIsClassOnly(t *testing.T) {
	const words = "litellm.AuthenticationError: bad key xoxb-1111-2222-FAKEFAKEFAKE password=hunter2-FAKE and the customer's record"
	for _, tc := range []struct {
		name   string
		code   int
		reason string
		is     error
		call   func(t *testing.T, a *Agent) error
	}{
		{"-32603 on a send", -32603, "INTERNAL_ERROR", a2a.ErrInternalError, func(t *testing.T, a *Agent) error { return a.Send(t.Context(), message("hi"), nil) }},
		{"-32603 on an answer", -32603, "INTERNAL_ERROR", ErrAnswerRejected, func(t *testing.T, a *Agent) error { return a.Answer(t.Context(), message("hi"), nil) }},
		{"-32004 on a send", -32004, "UNSUPPORTED_OPERATION", ErrBusy, func(t *testing.T, a *Agent) error { return a.Send(t.Context(), message("hi"), nil) }},
		{"-32603 on a get", -32603, "INTERNAL_ERROR", a2a.ErrInternalError, func(t *testing.T, a *Agent) error { _, err := a.GetTask(t.Context(), "t1"); return err }},
		{"-32603 on a cancel", -32603, "INTERNAL_ERROR", a2a.ErrInternalError, func(t *testing.T, a *Agent) error { _, err := a.CancelTask(t.Context(), "t1"); return err }},
		{"-32603 on a follow", -32603, "INTERNAL_ERROR", a2a.ErrInternalError, func(t *testing.T, a *Agent) error { return a.Follow(t.Context(), "t1", nil) }},
		{"-32603 on a find", -32603, "INTERNAL_ERROR", a2a.ErrInternalError, func(t *testing.T, a *Agent) error { _, _, err := a.FindTask(t.Context(), "c1", "m1"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := newFakeAgent(t, rpcFailure(tc.code, words))
			err := tc.call(t, fa.agent(t))
			if err == nil {
				t.Fatal("the call succeeded; want the agent's error")
			}
			for _, leak := range []string{"hunter2", "xoxb", "customer", "litellm"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("err = %q; holds %q, the agent's own text", err, leak)
				}
			}
			var rpcErr *a2a.Error
			if !errors.Is(err, tc.is) || !errors.As(err, &rpcErr) || !isProtocol(err) || !strings.Contains(err.Error(), "agent error: "+tc.reason) {
				t.Errorf("err = %q; want agent error: %s, and still the agent's error to errors.Is and errors.As (%v)", err, tc.reason, tc.is)
			}
		})
	}
}

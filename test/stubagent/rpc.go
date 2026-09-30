package stubagent

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// obj is a JSON object. Maps marshal with sorted keys, which is how the kagent fixtures read.
type obj = map[string]any

// JSON-RPC error codes the stub answers with.
const (
	codeParse         = -32700
	codeInvalidReq    = -32600
	codeMethod        = -32601
	codeParams        = -32602
	codeInternal      = -32603
	codeTaskNotFound  = -32001
	codeNotCancelable = -32002
	codeBusy          = -32004 // A2A's unsupported operation, which kagent answers a busy session with
	codeDenied        = -31403 // another identity's session or task
	codeUnscripted    = -32099 // a code kagent never sends: the stub has no turn for the message
)

// rpcError is a JSON-RPC error. reason is the google.rpc.ErrorInfo reason kagent attaches.
type rpcError struct {
	code   int
	msg    string
	reason string
}

// body renders the error as kagent does: code, message and a one-element ErrorInfo list.
func (e *rpcError) body(now time.Time) obj {
	return obj{"code": e.code, "message": e.msg, "data": []obj{{
		"@type":    "type.googleapis.com/google.rpc.ErrorInfo",
		"domain":   "a2a-protocol.org",
		"metadata": obj{"timestamp": now.UTC().Format(time.RFC3339)},
		"reason":   e.reason,
	}}}
}

// dig walks nested maps by key; it returns nil when a key is missing.
func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(obj)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// busyError is kagent's answer to a message its session cannot take.
func busyError(msg string) *rpcError { return &rpcError{codeBusy, msg, "UNSUPPORTED_OPERATION"} }

// taskNotFound is -32001 for a task id the agent does not know.
// UNCONFIRMED: no fixture shows kagent's answer; this is A2A's TaskNotFound.
func taskNotFound() *rpcError { return &rpcError{codeTaskNotFound, "task not found", "TASK_NOT_FOUND"} }

// invalidParams is -32602. Its message ends ": invalid params", as a2a-go's does: a2a-go v2.6.0
// internal/jsonrpc ToJSONRPCError sends err.Error() of an error that wraps ErrInvalidParams.
func invalidParams(format string, args ...any) *rpcError {
	return &rpcError{codeParams, fmt.Sprintf(format, args...) + ": invalid params", "INVALID_PARAMS"}
}

// invalidRequest is -32600, for an envelope that is not a JSON-RPC 2.0 request.
func invalidRequest() *rpcError {
	return &rpcError{codeInvalidReq, "invalid request", "INVALID_REQUEST"}
}

// validID reports whether a JSON-RPC id is a string, a number or null, or absent (a2a-go v2.6.0
// internal/jsonrpc IsValidID).
func validID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var id any
	if json.Unmarshal(raw, &id) != nil {
		return false
	}
	switch id.(type) {
	case nil, string, float64:
		return true
	}
	return false
}

func internalError(format string, args ...any) *rpcError {
	return &rpcError{codeInternal, fmt.Sprintf(format, args...), "INTERNAL_ERROR"}
}

// rw writes one call's response. A streaming call answers as SSE, one JSON-RPC envelope per
// event; any other call answers with one JSON body. Every answer is HTTP 200, errors included,
// as kagent's is (WP-02, measured).
type rw struct {
	s      *Server
	w      http.ResponseWriter
	id     json.RawMessage
	stream bool
	ext    bool   // the request names the HITL extension
	owner  string // the identity the request acts as
	held   obj    // a non-streaming call answers with the last result it was given
	failed bool   // an error has been written
}

// namesHITL reports whether a request asks for the HITL extension: the A2A-Extensions header,
// on any of its lines, lists its URI as an entry of a comma-separated list.
func namesHITL(h http.Header) bool {
	for _, line := range h.Values("A2A-Extensions") {
		for entry := range strings.SplitSeq(line, ",") {
			if strings.TrimSpace(entry) == HITLExtension {
				return true
			}
		}
	}
	return false
}

// view is a result as this client sees it. The HITL request in a paused task's status message,
// and the extension URI listing it, are there only for a client that named the extension.
// Measured (WP-02): without the header the pause is INPUT_REQUIRED with the text and no payload,
// and with it the payload is there.
// UNCONFIRMED: what kagent shows a client that asks differently from the one the task paused
// for. The stub goes by the header of each request, and strips the agent's request from the
// history as well as from the status.
func (o *rw) view(res obj) obj {
	if o.ext {
		return res
	}
	if _, ok := res["status"]; ok { // GetTask and CancelTask: the result is the task
		return withoutHITL(res)
	}
	out := maps.Clone(res)
	for kind, v := range res { // task and statusUpdate results
		if e, ok := v.(obj); ok {
			out[kind] = withoutHITL(e)
		}
	}
	return out
}

// withoutHITL returns a task or status update without the HITL parts of the messages in its
// status and its history.
func withoutHITL(e obj) obj {
	out := maps.Clone(e)
	if status, ok := e["status"].(obj); ok {
		if message, ok := status["message"].(obj); ok {
			status = maps.Clone(status)
			status["message"] = withoutHITLParts(message)
			out["status"] = status
		}
	}
	if history, ok := e["history"].([]obj); ok {
		stripped := make([]obj, len(history))
		for i, m := range history {
			stripped[i] = withoutHITLParts(m)
		}
		out["history"] = stripped
	}
	return out
}

// withoutHITLParts returns an agent's message without the HITL extension URI, leaving any
// other extension in the list, and without the HITL payload. A user's message is the client's
// own and stays as it was sent.
func withoutHITLParts(m obj) obj {
	if m["role"] != "ROLE_AGENT" || m["extensions"] == nil {
		return m
	}
	m = maps.Clone(m)
	var kept []string
	switch list := m["extensions"].(type) {
	case []string:
		kept = slices.DeleteFunc(slices.Clone(list), func(e string) bool { return e == HITLExtension })
	case []any:
		for _, e := range list {
			if e != HITLExtension {
				kept = append(kept, fmt.Sprint(e))
			}
		}
	}
	if len(kept) > 0 {
		m["extensions"] = kept
	} else {
		delete(m, "extensions")
	}
	if metadata, ok := m["metadata"].(obj); ok {
		metadata = maps.Clone(metadata)
		delete(metadata, HITLExtension)
		m["metadata"] = metadata
	}
	return m
}

// event delivers a result: at once when streaming, else it replaces the pending answer.
func (o *rw) event(res obj) {
	res = o.view(res)
	if o.stream {
		o.write(obj{"jsonrpc": "2.0", "id": o.id, "result": res})
		return
	}
	o.held = res
}

// fail answers with a JSON-RPC error: a single SSE event when streaming, else a JSON body.
func (o *rw) fail(e *rpcError) {
	o.failed = true
	o.write(obj{"jsonrpc": "2.0", "id": o.id, "error": e.body(o.s.now())})
}

// abort ends a non-streaming call that has no outcome to report, because the server closed
// while the turn ran, with a 503. A stream just stops.
func (o *rw) abort() {
	if !o.stream {
		http.Error(o.w, "stubagent: closed", http.StatusServiceUnavailable)
	}
}

// done writes the pending answer of a non-streaming call.
func (o *rw) done() {
	if !o.stream && !o.failed && o.held != nil {
		o.write(obj{"jsonrpc": "2.0", "id": o.id, "result": o.held})
	}
}

func (o *rw) write(env obj) {
	b, err := json.Marshal(env)
	if err != nil {
		http.Error(o.w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !o.stream {
		o.w.Header().Set("Content-Type", "application/json")
		_, _ = o.w.Write(b)
		return
	}
	o.w.Header().Set("Content-Type", "text/event-stream")
	o.w.Header().Set("Cache-Control", "no-cache")
	_, _ = fmt.Fprintf(o.w, "id: %s\ndata: %s\n\n", o.s.newID("event"), b)
	_ = http.NewResponseController(o.w).Flush()
}

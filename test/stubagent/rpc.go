package stubagent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// obj is a JSON object. Maps marshal with sorted keys, which is how the kagent fixtures read.
type obj = map[string]any

// JSON-RPC error codes the stub answers with.
const (
	codeParse    = -32700
	codeMethod   = -32601
	codeParams   = -32602
	codeInternal = -32603
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

func invalidParams(format string, args ...any) *rpcError {
	return &rpcError{codeParams, fmt.Sprintf(format, args...), "INVALID_PARAMS"}
}

func internalError(format string, args ...any) *rpcError {
	return &rpcError{codeInternal, fmt.Sprintf(format, args...), "INTERNAL_ERROR"}
}

// rw writes one call's response. Every answer is HTTP 200, errors included, as kagent's is
// (WP-02, measured).
type rw struct {
	s      *Server
	w      http.ResponseWriter
	id     json.RawMessage
	held   obj  // the last result the call was given; done answers with it
	failed bool // an error has been written
}

// event delivers a result. A call answers with one JSON body, so this keeps the latest.
func (o *rw) event(res obj) { o.held = res }

// fail answers with a JSON-RPC error.
func (o *rw) fail(e *rpcError) {
	o.failed = true
	o.write(obj{"jsonrpc": "2.0", "id": o.id, "error": e.body(o.s.now())})
}

// done writes the pending answer, if the call did not fail.
func (o *rw) done() {
	if !o.failed && o.held != nil {
		o.write(obj{"jsonrpc": "2.0", "id": o.id, "result": o.held})
	}
}

func (o *rw) write(env obj) {
	b, err := json.Marshal(env)
	if err != nil {
		http.Error(o.w, err.Error(), http.StatusInternalServerError)
		return
	}
	o.w.Header().Set("Content-Type", "application/json")
	_, _ = o.w.Write(b)
}

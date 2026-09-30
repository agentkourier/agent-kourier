package stubagent

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"time"
)

// obj is a JSON object. Maps marshal with sorted keys, which is how the kagent fixtures read.
type obj = map[string]any

// JSON-RPC error codes the stub answers with.
const (
	codeParse         = -32700
	codeMethod        = -32601
	codeParams        = -32602
	codeInternal      = -32603
	codeTaskNotFound  = -32001
	codeNotCancelable = -32002
	codeBusy          = -32004 // A2A's unsupported operation, which kagent answers a busy session with
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

// rw writes one call's response. A streaming call answers as SSE, one JSON-RPC envelope per
// event; any other call answers with one JSON body. Every answer is HTTP 200, errors included,
// as kagent's is (WP-02, measured).
type rw struct {
	s      *Server
	w      http.ResponseWriter
	id     json.RawMessage
	stream bool
	ext    bool // the request names the HITL extension
	held   obj  // a non-streaming call answers with the last result it was given
	failed bool // an error has been written
}

// view is a result as this client sees it. The HITL request in a paused task's status message,
// and the extension URI listing it, are there only for a client that named the extension.
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

// withoutHITL returns a task or status update without the HITL parts of its status message.
func withoutHITL(e obj) obj {
	status, _ := e["status"].(obj)
	message, _ := status["message"].(obj)
	if message["extensions"] == nil {
		return e
	}
	message = maps.Clone(message)
	delete(message, "extensions")
	if metadata, ok := message["metadata"].(obj); ok {
		metadata = maps.Clone(metadata)
		delete(metadata, HITLExtension)
		message["metadata"] = metadata
	}
	status = maps.Clone(status)
	status["message"] = message
	out := maps.Clone(e)
	out["status"] = status
	return out
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

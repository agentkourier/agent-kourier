package teams

import (
	"context"
	"encoding/json"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// Invoke is an invoke activity: a press of an Adaptive Card's Action.Execute (Name adaptiveCard/action), or another invoke Teams
// sends. Teams waits for its answer in the response to the request, about 14 seconds for adaptiveCard/action, and does not send again
// an invoke whose request it held until it gave up (both measured on a test tenant on 2026-11-18); whether it sends again an invoke
// answered 503 at once was not measured.
type Invoke struct {
	// Name is the invoke's name, such as adaptiveCard/action.
	Name string
	// ID is the activity's ID, by which the receiver drops a second delivery: an opaque key of this press.
	ID string
	// At is the message the invoke came from (its ID is the activity's replyToId), with its channel and thread.
	At chat.Ref
	// User is the presser's Microsoft Entra object ID, or "" when the activity has none.
	User string
	// Value is the invoke's value as Teams sent it: untrusted JSON, for the handler to read.
	Value json.RawMessage
}

// InvokeAnswer is the answer to an invoke: the response's status and its body, JSON or nil.
type InvokeAnswer struct {
	Status int
	Body   []byte
}

// InvokeHandler answers invokes. The receiver calls it in the request, after the token, the tenant and the dedup, with the request's
// context, whose deadline is the chat listener's, and the Sink of the live Serve, and writes what it returns. It must answer at once:
// what it hands the sink in the request must be quick (a press is recorded, and processed later), and any other work comes after.
type InvokeHandler interface {
	Invoke(ctx context.Context, sink chat.Sink, inv Invoke) InvokeAnswer
}

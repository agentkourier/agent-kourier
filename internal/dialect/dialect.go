// Package dialect defines how courier talks to one kind of agent backend: where an agent
// lives, how a request proves the Binding's identity, and how the acting chat user is
// attributed. Implementations live in subpackages (kagentv1).
package dialect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// Credentials are what Authenticate puts on a request. The caller resolves them from the
// Binding's identity and the AgentBackend's headers; they hold secret values, so they
// never print, log, or marshal. Never hold Credentials in an unexported field of a type
// that is formatted or logged: fmt cannot call String on it and prints the secrets.
type Credentials struct {
	// UserID is the Binding's X-User-Id, for an insecure-mode backend.
	UserID string `json:"-"`
	// Token is the Binding's OIDC bearer token. When set it is used and UserID is not.
	Token string `json:"-"`
	// Headers are the AgentBackend's static headers with secret values already read.
	Headers http.Header `json:"-"`
}

// String hides the secrets, so %v and %+v are safe.
func (Credentials) String() string { return "dialect.Credentials{redacted}" }

// GoString hides the secrets, so %#v is safe.
func (Credentials) GoString() string { return Credentials{}.String() }

// LogValue hides the secrets from log/slog.
func (Credentials) LogValue() slog.Value { return slog.StringValue(Credentials{}.String()) }

// User is the person who typed or clicked, resolved by the chat platform.
type User struct {
	ID    string // platform user ID, such as a Slack U-ID
	Name  string // display name; empty when unknown
	Email string // empty when unknown
}

// ErrUnusablePause is wrapped by the error ParseInteraction returns for a task that is
// paused on input but cannot be answered. The error text names the reason and never
// repeats text the agent supplied.
var ErrUnusablePause = errors.New("unusable pause")

// HTTPStatusError is what a dialect's transport returns for a response that is not 2xx. It
// lets a caller tell a request the backend refused (4xx) from a connection that dropped or
// a backend that failed (5xx), which the SDK reports as an opaque string. It carries the
// status only, never the response body, which may echo a credential. JSON-RPC errors are
// HTTP 200 and never take this path.
type HTTPStatusError struct{ Code int }

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.Code, http.StatusText(e.Code))
}

// Dialect is one backend protocol variant (spec 6.2).
type Dialect interface {
	// Endpoint is the URL of the agent's A2A service, on the backend's verifying front door.
	Endpoint(backend *v1alpha1.AgentBackend, agent v1alpha1.AgentRef) (string, error)
	// Authenticate sets the Binding's identity and the static headers on req. It returns an
	// error, without changing req, when the credentials cannot be sent safely.
	Authenticate(req *http.Request, creds Credentials) error
	// Attribute records the acting user on msg. The Binding identity stays the principal.
	Attribute(msg *a2a.Message, actor User)
	// Connect returns an A2A client for the agent that sends the Binding's identity to the
	// backend's front door and to nowhere else.
	Connect(ctx context.Context, backend *v1alpha1.AgentBackend, agent v1alpha1.AgentRef, creds Credentials) (*a2aclient.Client, error)
	// ParseInteraction returns the interaction a paused task waits on. It returns nil, nil
	// when the task has no structured pause (free text only, where the dialect allows it),
	// and an error wrapping ErrUnusablePause, naming the reason, when the task is paused
	// but cannot be answered: the caller reports that in the thread and cancels the task.
	ParseInteraction(task a2a.Task) (*Interaction, error)
	// ToolPart classifies an artifact part as a tool call or a tool result. ok is false for
	// any other part, which the caller treats as plain content.
	ToolPart(part *a2a.Part) (tool ToolPart, ok bool)
	// BuildResponse returns the message that answers in. It validates the answer itself
	// (backends accept incomplete ones, or wedge on them) and returns an error, having
	// built nothing, unless every question is answered or every tool decided.
	BuildResponse(in Interaction, answer Answer) (a2a.Message, error)
}

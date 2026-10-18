// Package kagentv09 is the dialect for kagent 0.9.x, which speaks A2A 0.3 at
// /api/a2a/{ns}/{name}/ and has no human-in-the-loop extension. It is the generic a2a dialect
// (the URL used as written, plain-text questions) that also reads kagent's tool parts, so the
// agent's tool calls show as step cards.
package kagentv09

import (
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
)

// Dialect implements dialect.Dialect for kagent 0.9.x. Build it with New to use Connect.
type Dialect struct{ *generic.Dialect }

var _ dialect.Dialect = (*Dialect)(nil)

// New returns a Dialect whose calls must get their response headers within timeout, as
// generic.New does.
func New(timeout time.Duration) *Dialect { return &Dialect{generic.New(timeout)} }

// ToolPart reads kagent's function_call and function_response data parts, which kagent 0.9.x
// marks with kagent_type and sends as status-update messages (test/fixtures/kagent/v09-tools).
func (*Dialect) ToolPart(part *a2a.Part) (dialect.ToolPart, bool) {
	return (*kagentv1.Dialect)(nil).ToolPart(part) // reads no field of its receiver
}

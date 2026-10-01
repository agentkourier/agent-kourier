package e2e

import (
	"context"
	"net/http"
	"net/url"
)

// Stub drives the in-cluster stub agent's control surface, so a scenario can release the task that
// scenario-gated holds. The surface has no authentication, so this client is only for tests inside
// the cluster, and the stub's Service is never published.
type Stub struct{ c *client }

// NewStub returns a client for the stub agent at baseURL.
func NewStub(baseURL string) *Stub { return &Stub{newClient(baseURL)} }

// Release opens gate: the task held on it goes on, and one that reaches it later does not wait.
// Name the gate in the prompt ("gate:<name>"), unique per run.
func (s *Stub) Release(ctx context.Context, gate string) error {
	return s.c.do(ctx, http.MethodPost, "/_control/release/"+url.PathEscape(gate), nil, nil)
}

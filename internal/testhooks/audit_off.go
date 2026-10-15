//go:build !testhooks

package testhooks

import "net/http"

// AuditEndpoint is inert in a production build. The test image is the only
// build that can expose persisted audit rows.
type AuditEndpoint struct{}

// NewAuditEndpoint returns the inert production endpoint.
func NewAuditEndpoint() *AuditEndpoint { return &AuditEndpoint{} }

// Set keeps the production endpoint inert.
func (*AuditEndpoint) Set(any) {}

// RegisterAuditRoute does not install the test-only route in a production build.
func RegisterAuditRoute(*http.ServeMux, *AuditEndpoint) {}

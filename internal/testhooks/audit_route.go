//go:build testhooks

package testhooks

import "net/http"

// RegisterAuditRoute installs the test-only persisted audit endpoint.
func RegisterAuditRoute(mux *http.ServeMux, e *AuditEndpoint) {
	if e == nil {
		return
	}
	mux.Handle("GET /__testhooks/audit", e)
}

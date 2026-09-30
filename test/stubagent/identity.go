package stubagent

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// WithRequiredAuth makes the stub answer HTTP 401 to a request with no bearer token, card
// included, as kagent in OIDC mode does. The default accepts anonymous requests, which all
// share one owner.
// UNCONFIRMED: the card's 401 was seen with curl in WP-02, and there is no fixture.
func WithRequiredAuth() Option { return func(s *Server) { s.requireAuth = true } }

// bearer returns the bearer token of a request.
func bearer(r *http.Request) (string, bool) {
	return strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// ownerOf is the identity a request acts as, which decides whose session it may use. kagent
// with controller.auth.userIdClaim set to email owns a session by the email claim of the
// bearer token, and does not verify the token: an unsigned one with the right claim is the
// owner (WP-02, measured). A token without an email is owned by its sub. That this is why it
// is refused is inferred, not observed. No token, or one that is not a JWT, is the anonymous
// owner, "".
func ownerOf(r *http.Request) string {
	token, _ := bearer(r)
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct{ Email, Sub string }
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.Email != "" {
		return claims.Email
	}
	if claims.Sub != "" {
		return "sub:" + claims.Sub // apart from any email
	}
	return ""
}

// deniedError is kagent's answer to a request that names another identity's session or task.
func deniedError() *rpcError { return &rpcError{codeDenied, "permission denied", "UNAUTHORIZED"} }

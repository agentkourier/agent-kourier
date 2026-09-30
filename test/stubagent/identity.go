package stubagent

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// WithRequiredAuth makes the stub answer HTTP 401 to a request with no bearer token, card
// included, as kagent in OIDC mode does. The default accepts requests with no token, which all
// share one anonymous owner. The fast E2E tier sets it, so that courier is tested sending a
// token as it must against kagent.
// UNCONFIRMED: the card's 401 was seen with curl in WP-02, and there is no fixture. The
// anonymous owner matches no real kagent mode: with OIDC on a request with no token is refused,
// and in insecure mode the user comes from X-User-Id, which the stub does not read.
func WithRequiredAuth() Option { return func(s *Server) { s.requireAuth = true } }

// identify returns the identity a request acts as, which decides whose sessions it may use, and
// whether the request may proceed at all.
//
// kagent with controller.auth.userIdClaim set to email owns a session by the email claim of the
// bearer token, and does not verify the token: an unsigned one with the right claim is the
// owner (WP-02, measured; identity/). UNCONFIRMED: a token without an email is owned by its
// sub, apart from any email; that this is why kagent refused a sub-only token is inferred from
// the refusal, not observed. Only an absent Authorization header is anonymous, the owner "",
// and under WithRequiredAuth not even that. Any other header, even an empty one, must be one
// line of a Bearer credential (the scheme is case-insensitive, RFC 7235) holding a JWT with an
// email or a sub, or the request is refused with HTTP 401.
func identify(r *http.Request, requireAuth bool) (owner string, ok bool) {
	lines := r.Header.Values("Authorization")
	if len(lines) == 0 {
		return "", !requireAuth
	}
	scheme, token, found := strings.Cut(lines[0], " ")
	if len(lines) != 1 || !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct{ Email, Sub string }
	if json.Unmarshal(payload, &claims) != nil {
		return "", false
	}
	switch {
	case claims.Email != "":
		return claims.Email, true
	case claims.Sub != "":
		return "sub:" + claims.Sub, true
	}
	return "", false
}

// deniedError is kagent's answer to a request that names another identity's session or task.
// Measured (WP-02) for a send and GetTask; the stub applies it to every method that names a
// session or task, and checks it before the busy and wedged checks.
// UNCONFIRMED: the refusal for a reply to a HITL pause, a replay, SubscribeToTask and
// CancelTask, and that order of checks.
func deniedError() *rpcError { return &rpcError{codeDenied, "permission denied", "UNAUTHORIZED"} }

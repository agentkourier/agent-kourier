// Package auth is the agent's caller check. A request to the A2A endpoint or the card must pass one of two checks,
// which the configuration picks:
//
//   - The token file: the request carries one of the tokens in a file, which a mounted Secret keeps current. Every
//     accepted token is the same caller, Agent Kourier: tokens exist so that one can be rotated in before the other is
//     removed, not to tell callers apart. The request runs as Caller.
//   - OIDC: the request carries an ID token from the configured issuer, for the configured audience, whose subject is
//     the configured one (Agent Kourier's service identity inside AX). The request runs as that subject.
//
// With the check off (on a loopback address only), every request runs as Caller.
//
// The caller a request runs as owns the tasks it creates and names its ADK sessions' user: a task is visible only to
// the caller that created it.
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Caller is the user name a request runs as when the token file checked it, or the check is off.
const Caller = "agent-kourier"

// The reasons a request is refused, which Check.Refused is told.
const (
	// ReasonMissing is a request with no credential.
	ReasonMissing = "missing"
	// ReasonInvalid is a request whose credential is not accepted.
	ReasonInvalid = "invalid"
	// ReasonUnavailable is a request whose credential could not be checked, because the OIDC issuer or its keys could
	// not be reached.
	ReasonUnavailable = "unavailable"
)

// The errors a Verifier refuses a request with. Any other error is an invalid credential.
var (
	// ErrMissing is a request with no credential.
	ErrMissing = errors.New("no credential")
	// ErrInvalid is a credential that is not accepted.
	ErrInvalid = errors.New("credential not accepted")
	// ErrUnavailable means the identity provider could not be reached. The request fails closed with a retryable 503,
	// not a credential rejection.
	ErrUnavailable = errors.New("authentication unavailable")
)

// Verifier checks a request's credential.
type Verifier interface {
	// Verify returns the caller the request runs as, or why it is refused: ErrMissing, ErrUnavailable, or any other
	// error for an invalid credential. It must never put the credential in its error.
	Verify(r *http.Request) (caller string, err error)
}

// Open is the check turned off: every request runs as Caller.
type Open struct{}

// Verify implements Verifier.
func (Open) Verify(*http.Request) (string, error) { return Caller, nil }

// Check refuses a request that its Verifier does not accept.
type Check struct {
	// Verifier checks each request.
	Verifier Verifier
	// Bearer is set when the credential is an Authorization Bearer token: a 401 then carries WWW-Authenticate: Bearer.
	Bearer bool
	// Log receives each refusal, with its reason and never the presented value.
	Log *slog.Logger
	// Refused, if set, is told the reason of each refusal: ReasonMissing, ReasonInvalid or ReasonUnavailable.
	Refused func(reason string)
}

// callerKey holds, in a request context that passed the check, the caller it runs as.
type callerKey struct{}

// Wrap returns next behind the check. A refused request gets 401, or 503 with Retry-After when the credential could
// not be checked; next never runs for it.
func (c *Check) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := c.Verifier.Verify(r)
		if err == nil && caller == "" {
			err = ErrInvalid
		}
		if err != nil {
			reason := ReasonInvalid
			switch {
			case errors.Is(err, ErrMissing):
				reason = ReasonMissing
			case errors.Is(err, ErrUnavailable):
				reason = ReasonUnavailable
			}
			c.Log.Warn("refused an A2A request", "reason", reason, "remote", r.RemoteAddr)
			if c.Refused != nil {
				c.Refused(reason)
			}
			if reason == ReasonUnavailable {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
				return
			}
			if c.Bearer {
				w.Header().Set("WWW-Authenticate", "Bearer")
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller)))
	})
}

// bearer returns the token of an Authorization value, "Bearer <token>" with the scheme in any case.
func bearer(value string) (string, error) {
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", ErrInvalid
	}
	return token, nil
}

// Interceptor makes a call that passed the check run as its caller, so that the task store records the owner and ADK
// names the session's user. A call that did not pass it is refused: every route to the request handler is behind a
// Check, so one without a caller is a wiring fault, and failing closed is the safe answer to it.
type Interceptor struct {
	a2asrv.PassthroughCallInterceptor
}

// Before implements a2asrv.CallInterceptor.
func (Interceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	caller, _ := ctx.Value(callerKey{}).(string)
	if caller == "" {
		return ctx, nil, a2a.ErrUnauthenticated
	}
	callCtx.User = a2asrv.NewAuthenticatedUser(caller, nil)
	return ctx, nil, nil
}

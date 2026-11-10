package broker

import (
	"errors"
	"fmt"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// connectionStopped is the error of a connection that does not run and will not until something changes, which the broker knows the reason
// of: the platform rejected its credentials, or the Secret that holds them cannot be used. It wraps chat.ErrConnectionDown, so that every
// part that treats a connection that is down (the outbox keeps the row pending, a stream or an edit fails fast, a turn counts the platform
// as down) treats this one alike, and the Bindings of the connection can be told why (connectionDownOutcome). It holds no token: a code
// is the platform's own word, and a cause is the error of reading a Secret, which names the Secret and the key and not their value.
type connectionStopped struct {
	conn string
	// rejected says the platform rejected the credentials, with code. Otherwise the Secret cannot be used, and cause says why.
	rejected bool
	code     string
	cause    error
}

func (e *connectionStopped) Error() string {
	if e.rejected {
		return fmt.Sprintf("%s: %q: the connection stopped, because the platform rejected its credentials (%s)", chat.ErrConnectionDown, e.conn, e.code)
	}
	return fmt.Sprintf("%s: %q: the connection has not started, because its credentials cannot be used: %s", chat.ErrConnectionDown, e.conn, telemetry.LogText(e.cause))
}

// Unwrap makes the error chat.ErrConnectionDown.
func (e *connectionStopped) Unwrap() error { return chat.ErrConnectionDown }

// reason is e as a short word, to tell whether the reason a connection is down changed.
func (e *connectionStopped) reason() string {
	if e.rejected {
		return "rejected:" + e.code
	}
	return "unusable"
}

// secretUnusable says whether err, from reading a credentials Secret, means the Secret, or a key of it, is gone or empty, or the Secret is
// of a type that is never read: what the operator has to mend, as opposed to a read that failed for a moment (a cache that has not synced).
func secretUnusable(err error) bool {
	return errors.Is(err, secret.ErrKeyNotFound) || errors.Is(err, secret.ErrEmptyValue) || errors.Is(err, secret.ErrUnsupportedType) ||
		errors.Is(err, config.ErrSecretNotFound)
}

// credentialsRejectedError is the error of an identity call that the platform refused because the token is not valid. It is
// errCredentialsRejected for errors.Is, and keeps the platform's code.
type credentialsRejectedError struct {
	method, code string
}

func (e *credentialsRejectedError) Error() string {
	return fmt.Sprintf("%s call: Slack answered %q, so %s", e.method, e.code, errCredentialsRejected)
}

// Is makes the error errCredentialsRejected.
func (e *credentialsRejectedError) Is(target error) bool { return target == errCredentialsRejected }

// rejectionCode is the platform's code in err, if err says the platform rejected the credentials: a run or a call that found them rejected
// (ErrSlackAuth), or an identity call that did.
func rejectionCode(err error) (string, bool) {
	var ar *authRejected
	var cr *credentialsRejectedError
	switch {
	case errors.As(err, &ar):
		return ar.code, true
	case errors.As(err, &cr):
		return cr.code, true
	}
	return "", false
}

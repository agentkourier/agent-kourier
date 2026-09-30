// Package secret holds Secret values in a type that no formatting path can print.
//
// A token that reaches a log line, an error message or a JSON body is a leaked token, and
// the usual route there is a careless %v on a struct that holds it. Value makes that route
// print "[redacted]". The only way to get the bytes back is the explicitly named Reveal.
package secret

import (
	"errors"
	"fmt"
	"log/slog"
)

var (
	// ErrKeyNotFound is what a reader wraps when the Secret exists but has no such key.
	ErrKeyNotFound = errors.New("key not found")
	// ErrEmptyValue is what a reader wraps when the key exists but holds no bytes. An empty
	// token is never usable, and would otherwise be sent as an empty credential.
	ErrEmptyValue = errors.New("empty value")
	// ErrUnsupportedType is what a reader wraps when the Secret is of a type it never reads
	// values from, such as a Helm release or a TLS key pair. Only Opaque Secrets are read.
	ErrUnsupportedType = errors.New("unsupported secret type")
)

// redacted is what every formatting path prints in place of the value.
const redacted = "[redacted]"

// Value is a Secret value. Every way of formatting it (fmt with any verb, JSON, text and
// slog) prints "[redacted]"; Reveal returns the value.
//
// The value is held behind a pointer on purpose. fmt cannot call methods on an unexported
// struct field, so it prints the field's representation instead, and a string held inline
// would be printed in full; a pointer prints as an address. Because of that, == compares
// identity, not content: compare the results of Reveal.
//
// Reflection-based dumpers print the value however it is held: go-spew, apimachinery's
// dump.Pretty, and go-cmp with AllowUnexported or an Exporter. Never pass them a struct that
// holds a Value.
//
// The zero Value holds the empty string.
type Value struct {
	v *string
}

// New returns a Value holding s.
func New(s string) Value { return Value{v: &s} }

// Reveal returns the value. It is the only way to read it: call it where the value is used,
// such as when building an Authorization header, and never pass its result to a logger.
func (v Value) Reveal() string {
	if v.v == nil {
		return ""
	}
	return *v.v
}

// IsEmpty reports whether the value is empty, which the zero Value is. It answers without
// handing out the bytes, so a check for a missing token need not Reveal it.
func (v Value) IsEmpty() bool { return v.v == nil || *v.v == "" }

// String returns "[redacted]".
func (Value) String() string { return redacted }

// GoString returns "[redacted]", so %#v is safe.
func (Value) GoString() string { return redacted }

// Format writes "[redacted]" for every verb and flag, so no fmt call can print the value.
func (Value) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// MarshalJSON returns the JSON string "[redacted]".
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText returns "[redacted]".
func (Value) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue returns "[redacted]" to log/slog.
func (Value) LogValue() slog.Value { return slog.StringValue(redacted) }

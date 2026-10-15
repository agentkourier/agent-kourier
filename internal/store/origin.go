package store

import "strings"

// MaxOriginPart is the maximum byte length of either part of an Origin.
// Origin values are identifiers, rather than message text, so each part also
// uses the ASCII name shape.
const MaxOriginPart = 128

// Origin identifies the bounded external source and subject that caused a
// session. The zero value is an ordinary conversation. Source may be present
// without Subject when a source has no subject to retain.
type Origin struct {
	Source  string
	Subject string
}

// Valid reports whether o is an origin that a session can persist.
func (o Origin) Valid() bool {
	if o.Source == "" {
		return o.Subject == ""
	}
	return validOriginPart(o.Source) && validOriginPart(o.Subject)
}

func validOriginPart(s string) bool {
	if len(s) > MaxOriginPart {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && strings.IndexByte("_.-:", c) < 0 {
			return false
		}
	}
	return true
}

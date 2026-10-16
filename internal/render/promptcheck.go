package render

import (
	"fmt"
	"net/url"
)

// The rules a value of a PromptView must pass to reach a prompt's instruction part (spec R2).
// The text of a message is attacker-influenced, and a value extracted from it is part of it, so
// it may be written into the instructions only if it is too narrow to carry any: a name or a
// key, and never a sentence. Link is not from the message: Agent Kourier builds it (see checkLink).
// A value that fails refuses the whole render.
const (
	// maxNameBytes bounds a Fields value, State, Channel and Source.
	maxNameBytes = 128
	// maxKeyBytes bounds Key.
	maxKeyBytes = 128
	// maxLinkBytes bounds Link, a Slack permalink.
	maxLinkBytes = 256
)

// Reason says why a value was refused.
type Reason string

// The reasons a value is refused.
const (
	// ReasonCharacters is a character the value's rule does not allow: white space and control
	// characters included, so one line of an alert can never become several.
	ReasonCharacters Reason = "has characters outside the allowed set"
	// ReasonTooLong is a value longer than its bound.
	ReasonTooLong Reason = "is too long"
	// ReasonNotLink is a Link that is no http or https URL with a host and no credentials.
	ReasonNotLink Reason = "is not an http or https URL"
)

// Code is the reason as a short word from a closed set, for a metric label and an audit entry: "characters",
// "too_long" or "not_link"; "other" for any reason added without one.
func (r Reason) Code() string {
	switch r {
	case ReasonCharacters:
		return "characters"
	case ReasonTooLong:
		return "too_long"
	case ReasonNotLink:
		return "not_link"
	}
	return "other"
}

// RefusedError is the error of a render that refused a value, so that nothing reached the
// agent. The caller audits it. It names the value but never holds it: the text is the
// attacker's, and the error ends up in logs.
type RefusedError struct {
	// Ref is the value as a template names it, without the dot: "State", "Key", "Link",
	// "Channel", "Source" or "Fields.<name>".
	Ref string
	// Reason is the rule the value broke.
	Reason Reason
	// Len is the length of the value in bytes.
	Len int
}

// Error implements error.
func (e *RefusedError) Error() string {
	return fmt.Sprintf("prompt refused: .%s %s (%d bytes)", e.Ref, e.Reason, e.Len)
}

// check returns the refusal of value, or nil if it passes rule.
func check(ref, value string, rule func(string) Reason) error {
	if r := rule(value); r != "" {
		return &RefusedError{Ref: ref, Reason: r, Len: len(value)}
	}
	return nil
}

// checkName is the rule of the alert-name shape, for a Fields value, State, Channel and Source:
// ASCII letters and digits and "_", ".", "-" and ":", at most 128 bytes, possibly none. It is
// ASCII only, so a letter that only looks Latin is refused.
func checkName(s string) Reason { return checkShape(s, maxNameBytes, "_.-:") }

// checkKey is the rule of Key: the alert-name shape and "/", as in "alertname/namespace", at
// most 128 bytes. It allows no space, so a key made of group label values ("KubePodCrashLooping
// payments", as the Alertmanager example extracts) is refused if a template reads .Key: such a
// template should read the entries it needs from .Fields.
func checkKey(s string) Reason { return checkShape(s, maxKeyBytes, "_.-:/") }

// checkShape returns ReasonTooLong if s is longer than limit bytes, and ReasonCharacters if
// it holds a byte that is no ASCII letter or digit and not among extra.
func checkShape(s string, limit int, extra string) Reason {
	if len(s) > limit {
		return ReasonTooLong
	}
	for i := range len(s) {
		if !asciiAlnum(s[i]) && !containsByte(extra, s[i]) {
			return ReasonCharacters
		}
	}
	return ""
}

// linkExtra is what a permalink needs besides letters and digits: the characters of a path, a
// query of name=value pairs and a fragment. It has no "+" or "%", which would let a link carry
// encoded prose ("?Ignore+all+previous+instructions", or a percent-encoded tag), and no "@",
// so a link holds no credentials.
const linkExtra = "-._~:/?#=&"

// checkLink is the rule of Link, which is Agent Kourier's own: the platform's permalink of the triggering
// message, built from the channel and the message's ID. It is never a field of the message (not
// link, which the sender wrote), so the rule is a second line of defence, narrow enough
// for a permalink and for little else. The link is at most 256 bytes of letters, digits and the
// characters of linkExtra, with a scheme of http or https (in any case) and a host. An empty
// link passes: the view may have none.
func checkLink(s string) Reason {
	if s == "" {
		return ""
	}
	if r := checkShape(s, maxLinkBytes, linkExtra); r != "" {
		return r
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ReasonNotLink
	}
	return ""
}

func asciiAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func containsByte(set string, b byte) bool {
	for i := range len(set) {
		if set[i] == b {
			return true
		}
	}
	return false
}

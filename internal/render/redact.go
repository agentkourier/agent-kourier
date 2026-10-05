package render

import (
	"strings"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/redact"
)

const redacted = redact.Redacted

// RedactArgs returns a tool call's arguments with the secrets taken out, in the way Tool does for display (see
// redactArgs), for what is stored instead of shown: a request kept for later holds no more of the arguments
// than the status line would. The result is a new map; args is not changed.
func RedactArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	return redactArgs(args).(map[string]any)
}

// redactArgs returns v with secret values replaced, in nested maps and lists too: the value of a
// key whose name redact.SecretName accepts, the "value" of a map whose "name" or "key" names a secret (which is how
// Kubernetes writes an env var, {"name": "DB_PASSWORD", "value": "..."}), the second of a
// two-element list whose first is a secret name (a header as ["X-Api-Key", "..."]), and what
// redact.Text finds in each string. Keys and strings are cleaned first, so that zero-width and
// control characters cannot split a name, and bounded (see redact.MaxBytes). A string is redacted
// as it is, with its own line breaks, and not as the JSON that shows it: there a newline is a
// backslash and an "n", which would join the next name. Letters that only look like Latin ones
// (Cyrillic "a" in "password") are not handled.
func redactArgs(v any) any {
	switch t := v.(type) {
	case string:
		return redact.Text(chat.Clean(t, false))
	case map[string]any:
		pair := false
		for k, x := range t {
			if s, ok := x.(string); ok && (strings.EqualFold(k, "name") || strings.EqualFold(k, "key")) && redact.SecretName(chat.Clean(s, true)) {
				pair = true
			}
		}
		out := make(map[string]any, len(t))
		for k, x := range t {
			k = redact.Bound(chat.Clean(k, true))
			if redact.SecretName(k) || (pair && strings.EqualFold(k, "value")) {
				out[k] = redacted
			} else {
				out[k] = redactArgs(x)
			}
		}
		return out
	case []any:
		if name, ok := firstString(t); ok && len(t) == 2 && redact.SecretName(chat.Clean(name, true)) {
			return []any{redactArgs(name), redacted}
		}
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = redactArgs(x)
		}
		return out
	}
	return v
}

func firstString(l []any) (string, bool) {
	if len(l) == 0 {
		return "", false
	}
	s, ok := l[0].(string)
	return s, ok
}

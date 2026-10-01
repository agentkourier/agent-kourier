package render

import "github.com/agentkourier/agent-kourier/internal/redact"

// The tests of the redaction in a tool line say these in render's terms.

// maxRedactBytes bounds each key and string that is redacted: see redact.Bound.
const maxRedactBytes = redact.MaxBytes

// isSecretName reports whether name is one whose value is never shown: see redact.SecretName.
func isSecretName(name string) bool { return redact.SecretName(name) }

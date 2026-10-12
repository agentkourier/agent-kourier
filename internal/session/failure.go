package session

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// failureCategory is a closed set: only broker-authored causes and next steps may reach a Notice.
type failureCategory string

const (
	failureUnknown    failureCategory = "unknown"
	failureContext    failureCategory = "context_limit"
	failureRateLimit  failureCategory = "rate_limit"
	failureAuth       failureCategory = "authentication"
	failureTimeout    failureCategory = "timeout"
	failurePermission failureCategory = "permission"
)

const maxFailureText = 4096
const maxFailureParts = 32

// classifyTaskFailure reads bounded status text and selected metadata codes in memory. Text recognition requires
// a specific error signature; broad words such as "timeout" or "unauthorized" are not enough. It never returns
// any backend-provided value; the result is a fixed category suitable for logs and notices.
func classifyTaskFailure(message *a2a.Message) failureCategory {
	if message == nil {
		return failureUnknown
	}
	if len(message.Parts) > maxFailureParts {
		return failureUnknown
	}
	for _, part := range message.Parts {
		if part == nil {
			return failureUnknown
		}
		if _, ok := part.Content.(a2a.Text); !ok {
			return failureUnknown
		}
	}
	parts := make([]string, 0, len(message.Parts))
	var text strings.Builder
	for _, part := range message.Parts {
		remaining := maxFailureText - text.Len()
		separator := text.Len() > 0
		if separator {
			remaining--
		}
		if remaining <= 0 {
			break
		}
		partText := string(part.Content.(a2a.Text))
		if len(partText) > remaining {
			partText = partText[:remaining]
		}
		if separator {
			text.WriteByte(' ')
		}
		text.WriteString(partText)
		parts = append(parts, partText)
	}
	// Parts are separate evidence: classify each one independently. Only the kagent context envelope may
	// span parts, using one inserted space; this supports a split `Error code:` / `400 - ...` message without
	// synthesizing a status signature such as `HTTP 401` from arbitrary fragments.
	joinedCategory := contextCategoryFromFailureText(strings.ToLower(text.String()), message.Metadata)
	textCategory := failureUnknown
	for _, part := range parts {
		category := categoryFromFailureText(strings.ToLower(part), message.Metadata)
		if category == failureUnknown {
			continue
		}
		if textCategory != failureUnknown && textCategory != category {
			return failureUnknown
		}
		textCategory = category
	}
	if joinedCategory != failureUnknown && textCategory != failureUnknown && joinedCategory != textCategory {
		return failureUnknown
	}
	if joinedCategory != failureUnknown {
		textCategory = joinedCategory
	}

	// These are the only metadata fields treated as codes. They must be bounded strings that exactly match
	// categoryForCode; a generic kagent API_ERROR intentionally carries no category by itself. Conflicting codes
	// or a code that conflicts with a strong text signature leave the cause unknown.
	codeCategory := failureUnknown
	for _, key := range []string{"code", "error_code", "kagent_error_code"} {
		if code, ok := message.Metadata[key].(string); ok && len(code) <= 64 {
			if category := categoryForCode(strings.ToLower(code)); category != failureUnknown {
				if codeCategory != failureUnknown && codeCategory != category {
					return failureUnknown
				}
				codeCategory = category
			}
		}
	}
	if codeCategory != failureUnknown && textCategory != failureUnknown && codeCategory != textCategory {
		return failureUnknown
	}
	if codeCategory != failureUnknown {
		return codeCategory
	}
	return textCategory
}

func categoryFromFailureText(text string, metadata map[string]any) failureCategory {
	trimmed := strings.TrimSpace(text)
	found := failureUnknown
	add := func(category failureCategory, match bool) bool {
		if !match {
			return true
		}
		if found != failureUnknown && found != category {
			return false
		}
		found = category
		return true
	}
	contextSignal := contextCategoryFromFailureText(trimmed, metadata) == failureContext
	if !add(failureContext, contextSignal) {
		return failureUnknown
	}
	rateSignal := hasStatusCode(trimmed, "http ", "429") || hasStatusCode(trimmed, "status code: ", "429") ||
		strings.HasPrefix(trimmed, "429: too many requests")
	if !add(failureRateLimit, rateSignal) {
		return failureUnknown
	}
	authSignal := hasStatusCode(trimmed, "http ", "401") || hasStatusCode(trimmed, "status code: ", "401") ||
		strings.HasPrefix(trimmed, "401 unauthorized") || strings.HasPrefix(trimmed, "invalid api key provided:") ||
		strings.HasPrefix(trimmed, "incorrect api key provided:")
	if !add(failureAuth, authSignal) {
		return failureUnknown
	}
	timeoutSignal := strings.HasPrefix(trimmed, "context deadline exceeded") || strings.HasPrefix(trimmed, "deadline exceeded") ||
		strings.HasPrefix(trimmed, "request timed out") || strings.HasPrefix(trimmed, "the request timed out") ||
		strings.HasPrefix(trimmed, "timeouterror:") || strings.HasPrefix(trimmed, "timeout:")
	if !add(failureTimeout, timeoutSignal) {
		return failureUnknown
	}
	permissionSignal := hasStatusCode(trimmed, "http ", "403") || hasStatusCode(trimmed, "status code: ", "403") ||
		strings.HasPrefix(trimmed, "403 forbidden") || strings.HasPrefix(trimmed, "permission denied:") ||
		trimmed == "permission denied" || strings.HasPrefix(trimmed, "forbidden:")
	if !add(failurePermission, permissionSignal) {
		return failureUnknown
	}
	return found
}

func contextCategoryFromFailureText(trimmed string, metadata map[string]any) failureCategory {
	kagentAPIError := false
	if code, ok := metadata["kagent_error_code"].(string); ok && len(code) <= 64 {
		kagentAPIError = strings.EqualFold(code, "API_ERROR")
	}
	contextSignal := kagentAPIError && isKagentContextEnvelope(trimmed) &&
		strings.Contains(trimmed, "this endpoint's maximum context length") &&
		strings.Contains(trimmed, "requested") && strings.Contains(trimmed, "tokens")
	if contextSignal {
		return failureContext
	}
	return failureUnknown
}

// isKagentContextEnvelope accepts only the observed numeric error envelope, including the optional space and dash.
// The boundary rejects codes such as 4000, while evaluation per-part and with one separating space supports an
// envelope split across text parts without joining fragments into HTTP or other status-code signatures.
func isKagentContextEnvelope(text string) bool {
	const prefix = "error code:"
	if !strings.HasPrefix(text, prefix) {
		return false
	}
	rest := strings.TrimLeft(text[len(prefix):], " \t")
	return hasStatusCode(rest, "", "400")
}

// hasStatusCode accepts a status prefix only at a token boundary, so values such as HTTP 4290 do not match 429.
func hasStatusCode(text, prefix, code string) bool {
	if !strings.HasPrefix(text, prefix+code) {
		return false
	}
	end := len(prefix) + len(code)
	return len(text) == end || text[end] == ' ' || text[end] == ':' || text[end] == '\t'
}

func categoryForCode(code string) failureCategory {
	switch code {
	case "context_length_exceeded", "context_limit_exceeded":
		return failureContext
	case "rate_limit_exceeded", "rate_limited":
		return failureRateLimit
	case "unauthenticated", "unauthorized", "authentication_error", "invalid_credentials":
		return failureAuth
	case "permission_denied", "forbidden":
		return failurePermission
	case "deadline_exceeded", "timeout":
		return failureTimeout
	default:
		return failureUnknown
	}
}

// failureReference is a stable, bounded correlation token derived from thread and task identity. It deliberately
// excludes turn and context IDs, which can differ when a task is recovered. The full task ID remains in operator
// logs; no backend-provided identifier is placed in the chat message.
func failureReference(key Key, taskID a2a.TaskID) string {
	h := sha256.New()
	for _, value := range []string{key.Connection, key.Channel, key.Thread, string(taskID)} {
		// Length-prefix values so different tuples cannot share the same byte stream.
		_, _ = h.Write([]byte(strconv.Itoa(len(value)) + ":"))
		_, _ = h.Write([]byte(value))
	}
	return "AK-" + hex.EncodeToString(h.Sum(nil)[:12])
}

func failureDetail(category failureCategory, reference string) string {
	var cause, next string
	switch category {
	case failureContext:
		cause, next = "The request exceeded the model input limit.", "Ask a narrower question in a new thread."
	case failureRateLimit:
		cause, next = "The agent or its provider is rate limited.", "Wait a little, then try again."
	case failureAuth:
		cause, next = "The agent's request could not be authenticated.", "Ask an operator to check the agent's credentials or access."
	case failureTimeout:
		cause, next = "The agent's request timed out.", "Try again; ask an operator if it keeps happening."
	case failurePermission:
		cause, next = "The agent's request was denied by a permission check.", "Ask an operator to check the agent's access."
	default:
		cause, next = "", "An operator can investigate the agent."
	}
	if cause != "" {
		cause += " "
	}
	return cause + next + " Troubleshooting reference: " + reference + "."
}

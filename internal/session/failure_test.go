package session

import (
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func failureMessage(text string, metadata map[string]any) *a2a.Message {
	return &a2a.Message{Parts: a2a.ContentParts{a2a.NewTextPart(text)}, Metadata: metadata}
}

func failureParts(metadata map[string]any, parts ...*a2a.Part) *a2a.Message {
	return &a2a.Message{Parts: a2a.ContentParts(parts), Metadata: metadata}
}

func TestClassifyTaskFailure(t *testing.T) {
	const providerExample = `Error code:400 {'error':{'message':"This endpoint's maximum context length is262144tokens. However,you requested about483214tokens. Please reduce...",'code':400}}`
	for _, tc := range []struct {
		name    string
		message *a2a.Message
		want    failureCategory
	}{
		{"provider context overflow", failureMessage(providerExample, map[string]any{"kagent_error_code": "API_ERROR"}), failureContext},
		{"provider context overflow with spaced error envelope", failureMessage(`Error code: 400 - {"message":"This endpoint's maximum context length; requested 100 tokens"}`, map[string]any{"kagent_error_code": "API_ERROR"}), failureContext},
		{"provider context overflow with no-space error envelope", failureMessage(`Error code:400 - {"message":"This endpoint's maximum context length; requested 100 tokens"}`, map[string]any{"kagent_error_code": "API_ERROR"}), failureContext},
		{"wrong kagent error code", failureMessage(`Error code: 500 - {"message":"This endpoint's maximum context length; requested 100 tokens"}`, map[string]any{"kagent_error_code": "API_ERROR"}), failureUnknown},
		{"kagent error code with numeric suffix", failureMessage(`Error code:4000 - {"message":"This endpoint's maximum context length; requested 100 tokens"}`, map[string]any{"kagent_error_code": "API_ERROR"}), failureUnknown},
		{"split context envelope", failureParts(map[string]any{"kagent_error_code": "API_ERROR"}, a2a.NewTextPart("Error code:"), a2a.NewTextPart(`400 - {"message":"This endpoint's maximum context length; requested 100 tokens"}`)), failureContext},
		{"context structured code", failureMessage("backend detail", map[string]any{"error_code": "CONTEXT_LENGTH_EXCEEDED"}), failureContext},
		{"rate limit wording", failureMessage("429: too many requests", nil), failureRateLimit},
		{"rate limit code", failureMessage("", map[string]any{"code": "RATE_LIMIT_EXCEEDED"}), failureRateLimit},
		{"authentication wording", failureMessage("Incorrect API key provided: sk-proj-********************wxyz", nil), failureAuth},
		{"authentication code", failureMessage("", map[string]any{"kagent_error_code": "UNAUTHENTICATED"}), failureAuth},
		{"timeout wording", failureMessage("context deadline exceeded", nil), failureTimeout},
		{"timeout code", failureMessage("", map[string]any{"code": "DEADLINE_EXCEEDED"}), failureTimeout},
		{"permission wording", failureMessage("permission denied", nil), failurePermission},
		{"permission code", failureMessage("", map[string]any{"error_code": "PERMISSION_DENIED"}), failurePermission},
		{"generic kagent API_ERROR", failureMessage("internal error", map[string]any{"kagent_error_code": "API_ERROR"}), failureUnknown},
		{"unknown text", failureMessage("backend failed for tenant alice", nil), failureUnknown},
		{"negated timeout", failureMessage("not a timeout: worker stopped", nil), failureUnknown},
		{"quoted rate-limit question", failureMessage(`The tool asked "is this a rate limit?"`, nil), failureUnknown},
		{"rate-limit configuration problem", failureMessage("invalid rate limit configuration", nil), failureUnknown},
		{"tool authorization text", failureMessage("tool output: unauthorized", nil), failureUnknown},
		{"quoted permission question", failureMessage(`question: "permission denied?"`, nil), failureUnknown},
		{"conflicting status signatures", failureMessage("maximum context length requested 500 tokens; HTTP 429", nil), failureUnknown},
		{"conflicting metadata codes", failureMessage("", map[string]any{"code": "TIMEOUT", "error_code": "RATE_LIMIT_EXCEEDED"}), failureUnknown},
		{"metadata and text conflict", failureMessage("HTTP 401 unauthorized", map[string]any{"code": "PERMISSION_DENIED"}), failureUnknown},
		{"malformed metadata", failureMessage("backend failed", map[string]any{"kagent_error_code": map[string]any{"code": "CONTEXT_LENGTH_EXCEEDED"}}), failureUnknown},
		{"oversized suffix ignored", failureMessage(strings.Repeat("x", maxFailureText)+" maximum context length 100 tokens", nil), failureUnknown},
		{"oversized status with bounded context signal", failureMessage(`Error code:400 {'error':{'message':"This endpoint's maximum context length; requested 100 tokens"}`+strings.Repeat("x", maxFailureText), map[string]any{"kagent_error_code": "API_ERROR"}), failureContext},
		{"quoted context-limit question", failureMessage(`question: "This endpoint's maximum context length; requested 100 tokens?"`, map[string]any{"kagent_error_code": "API_ERROR"}), failureUnknown},
		{"status code prefix is bounded", failureMessage("HTTP 4290 provider detail", nil), failureUnknown},
		{"split parts cannot synthesize an HTTP signature", failureParts(nil, a2a.NewTextPart("HTTP "), a2a.NewTextPart("401")), failureUnknown},
		{"different HTTP signatures in separate parts conflict", failureParts(nil, a2a.NewTextPart("HTTP 401"), a2a.NewTextPart("HTTP 429")), failureUnknown},
		{"nil part invalidates earlier signal", failureParts(nil, a2a.NewTextPart("HTTP 401"), nil), failureUnknown},
		{"non-text part invalidates earlier signal", failureParts(nil, a2a.NewTextPart("HTTP 401"), a2a.NewRawPart([]byte("private"))), failureUnknown},
		{"nil status", nil, failureUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyTaskFailure(tc.message); got != tc.want {
				t.Errorf("classifyTaskFailure() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFailureDetailAndReferenceAreSafeAndStable(t *testing.T) {
	key := Key{Connection: strings.Repeat("c", 500), Channel: "C1", Thread: "1700.01"}
	const rawTask = "task <@U123> `secret` " + "xoxb-FAKE"
	first := failureReference(key, rawTask)
	if len(first) != len("AK-")+24 || !strings.HasPrefix(first, "AK-") {
		t.Fatalf("reference %q is not bounded to the expected opaque form", first)
	}
	if got := failureReference(key, rawTask); got != first {
		t.Fatalf("reference changed across recovery: %q != %q", got, first)
	}
	if got := failureReference(key, rawTask+"x"); got == first {
		t.Fatal("different task IDs produced the same reference")
	}
	for _, category := range []failureCategory{failureUnknown, failureContext, failureRateLimit, failureAuth, failureTimeout, failurePermission} {
		detail := failureDetail(category, first)
		if strings.ContainsAny(detail, "<>`@") || strings.Contains(detail, "xoxb") || len(detail) > 200 {
			t.Errorf("unsafe or overlong detail for %q: %q", category, detail)
		}
		if !strings.Contains(detail, first) {
			t.Errorf("detail %q omitted its reference", detail)
		}
	}
}

package docsagent_test

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsagent"
)

// fakeKey is not a credential. The tests check that nothing prints it.
const fakeKey = "test-key-not-a-real-credential-0000"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestModelFromEnv(t *testing.T) {
	ok := map[string]string{"MODEL_PROVIDER": "openrouter", "MODEL_NAME": "vendor/some-model", "MODEL_API_KEY": fakeKey}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range ok {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			if kv[i+1] == "" {
				delete(m, kv[i])
			} else {
				m[kv[i]] = kv[i+1]
			}
		}
		return m
	}
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string // empty: must succeed
	}{
		{"openrouter", ok, ""},
		{"openai", with("MODEL_PROVIDER", "openai", "MODEL_NAME", "some-model"), ""},
		{"an OpenAI-compatible endpoint", with("MODEL_PROVIDER", "openai", "MODEL_BASE_URL", "https://llm.example.com/v1"), ""},
		{"no provider", with("MODEL_PROVIDER", ""), "MODEL_PROVIDER"},
		{"bedrock is not supported", with("MODEL_PROVIDER", "bedrock"), "bedrock"},
		{"anthropic is not supported", with("MODEL_PROVIDER", "anthropic"), "anthropic"},
		{"no model name", with("MODEL_NAME", ""), "MODEL_NAME"},
		{"no key", with("MODEL_API_KEY", ""), "MODEL_API_KEY"},
		{"a base URL that is not https", with("MODEL_BASE_URL", "http://llm.example.com/v1"), "MODEL_BASE_URL"},
		{"a base URL with credentials", with("MODEL_BASE_URL", "https://user:pw@llm.example.com/v1"), "MODEL_BASE_URL"},
		{"a base URL with a query", with("MODEL_BASE_URL", "https://llm.example.com/v1?key="+fakeKey), "MODEL_BASE_URL"},
		{"a base URL that is not a URL", with("MODEL_BASE_URL", "::"), "MODEL_BASE_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := docsagent.ModelFromEnv(t.Context(), env(tt.env))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ModelFromEnv: %v", err)
				}
				if m.Name() != tt.env["MODEL_NAME"] {
					t.Errorf("Name() = %q, want %q", m.Name(), tt.env["MODEL_NAME"])
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one naming %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), fakeKey) || strings.Contains(err.Error(), "pw@") {
				t.Errorf("the error prints a credential: %v", err)
			}
		})
	}
}

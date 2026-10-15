package kindmanifests_test

import (
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const fakeOpenRouterKey = "sk-or-v1-FAKE-LEAKCHECK-key-0007"

func openRouterEnv() []string {
	lines := withLine(goodEnv(), "MODEL_PROVIDER", "MODEL_PROVIDER=openrouter")
	lines = withLine(lines, "MODEL_NAME", "MODEL_NAME=anthropic/claude-haiku-4.5")
	return withLine(lines, "MODEL_API_KEY", "MODEL_API_KEY="+fakeOpenRouterKey)
}

func withBaseURL(lines []string, url string) []string {
	return append(append([]string{}, lines...), "MODEL_BASE_URL="+url)
}

func TestEnvCheckAcceptsOpenRouter(t *testing.T) {
	for _, model := range []string{"anthropic/claude-haiku-4.5", "openai/gpt-4.1", "vendor/model:free"} {
		t.Run(model, func(t *testing.T) {
			lines := withLine(openRouterEnv(), "MODEL_NAME", "MODEL_NAME="+model)
			code, stdout, stderr := check(t, envFile(t, 0o600, lines...))
			if code != 0 || strings.Contains(stdout, "MODEL_REGION") {
				t.Errorf("exit %d, stdout %q, stderr %q; want accepted without MODEL_REGION", code, stdout, stderr)
			}
			noLeak(t, "output", stdout+stderr)
		})
	}
}

func TestEnvCheckAcceptsOpenRouterHostnameBoundaries(t *testing.T) {
	maxLabel := strings.Repeat("a", 63)
	maxHost := strings.Join([]string{maxLabel, maxLabel, maxLabel, strings.Repeat("b", 61)}, ".")
	for name, host := range map[string]string{
		"single label":         "a",
		"maximum label":        maxLabel,
		"maximum DNS hostname": maxHost,
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := check(t, envFile(t, 0o600, withBaseURL(openRouterEnv(), "https://"+host+"/v1")...))
			if code != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q; want accepted", code, stdout, stderr)
			}
			noLeak(t, "output", stdout+stderr)
		})
	}
}

func TestEnvCheckRefusesOpenRouterMistakes(t *testing.T) {
	maxLabel := strings.Repeat("a", 63)
	maxHostPlusOne := strings.Join([]string{maxLabel, maxLabel, maxLabel, strings.Repeat("b", 62)}, ".")
	for name, tc := range map[string]struct {
		lines []string
		want  string
	}{
		"endpoint on anthropic":                 {withBaseURL(goodEnv(), "https://router.example.test/v1"), "MODEL_BASE_URL is only for MODEL_PROVIDER=openrouter"},
		"endpoint on openai":                    {withBaseURL(withLine(goodEnv(), "MODEL_PROVIDER", "MODEL_PROVIDER=openai"), "https://router.example.test/v1"), "MODEL_BASE_URL is only for MODEL_PROVIDER=openrouter"},
		"endpoint on bedrock":                   {withBaseURL(bedrockEnv(), "https://router.example.test/v1"), "MODEL_BASE_URL is only for MODEL_PROVIDER=openrouter"},
		"region":                                {append(openRouterEnv(), "MODEL_REGION=us-east-1"), "MODEL_REGION is only for MODEL_PROVIDER=bedrock"},
		"http endpoint":                         {withBaseURL(openRouterEnv(), "http://router.example.test/v1"), "MODEL_BASE_URL is malformed"},
		"endpoint without a host":               {withBaseURL(openRouterEnv(), "https:///v1"), "MODEL_BASE_URL is malformed"},
		"endpoint with credentials":             {withBaseURL(openRouterEnv(), "https://user:pass@router.example.test/v1"), "MODEL_BASE_URL is malformed"},
		"endpoint with a query":                 {withBaseURL(openRouterEnv(), "https://router.example.test/v1?x=1"), "MODEL_BASE_URL is malformed"},
		"endpoint with a fragment":              {withBaseURL(openRouterEnv(), "https://router.example.test/v1#fragment"), "MODEL_BASE_URL is malformed"},
		"endpoint with an invalid port":         {withBaseURL(openRouterEnv(), "https://router.example.test:nope/v1"), "MODEL_BASE_URL is malformed"},
		"endpoint with port zero":               {withBaseURL(openRouterEnv(), "https://router.example.test:0/v1"), "MODEL_BASE_URL is malformed"},
		"endpoint with port too large":          {withBaseURL(openRouterEnv(), "https://router.example.test:65536/v1"), "MODEL_BASE_URL is malformed"},
		"endpoint with an empty hostname label": {withBaseURL(openRouterEnv(), "https://router..example/v1"), "MODEL_BASE_URL is malformed"},
		"endpoint label starts with a hyphen":   {withBaseURL(openRouterEnv(), "https://router.-example/v1"), "MODEL_BASE_URL is malformed"},
		"endpoint label ends with a hyphen":     {withBaseURL(openRouterEnv(), "https://router.example-/v1"), "MODEL_BASE_URL is malformed"},
		"endpoint label too long":               {withBaseURL(openRouterEnv(), "https://"+strings.Repeat("a", 64)+".example/v1"), "MODEL_BASE_URL is malformed"},
		"endpoint hostname too long":            {withBaseURL(openRouterEnv(), "https://"+maxHostPlusOne+"/v1"), "MODEL_BASE_URL is malformed"},
		"endpoint set twice":                    {append(withBaseURL(openRouterEnv(), "https://router.example.test/v1"), "MODEL_BASE_URL=https://router.example.test/v2"), "MODEL_BASE_URL is set twice"},
		"openai key":                            {withLine(openRouterEnv(), "MODEL_API_KEY", "MODEL_API_KEY="+fakeOpenAIKey), "MODEL_API_KEY is not an OpenRouter API key"},
		"anthropic key":                         {withLine(openRouterEnv(), "MODEL_API_KEY", "MODEL_API_KEY="+fakeKey), "MODEL_API_KEY is not an OpenRouter API key"},
		"bedrock key":                           {withLine(openRouterEnv(), "MODEL_API_KEY", "MODEL_API_KEY="+fakeBedrockKey), "looks like a Bedrock API key"},
		"router key sent to openai":             {withLine(withLine(goodEnv(), "MODEL_PROVIDER", "MODEL_PROVIDER=openai"), "MODEL_API_KEY", "MODEL_API_KEY="+fakeOpenRouterKey), "looks like an OpenRouter API key"},
		"router key sent to anthropic":          {withLine(goodEnv(), "MODEL_API_KEY", "MODEL_API_KEY="+fakeOpenRouterKey), "looks like an OpenRouter API key"},
		"router key sent to bedrock":            {withLine(bedrockEnv(), "MODEL_API_KEY", "MODEL_API_KEY="+fakeOpenRouterKey), "looks like an OpenRouter API key"},
		"yaml injection":                        {withLine(openRouterEnv(), "MODEL_NAME", "MODEL_NAME=vendor/model # comment"), "MODEL_NAME is malformed"},
		"url as model":                          {withLine(openRouterEnv(), "MODEL_NAME", "MODEL_NAME=https://example.com/model"), "MODEL_NAME is malformed"},
		"missing vendor":                        {withLine(openRouterEnv(), "MODEL_NAME", "MODEL_NAME=model"), "MODEL_NAME is malformed"},
		"extra slash":                           {withLine(openRouterEnv(), "MODEL_NAME", "MODEL_NAME=vendor/model/extra"), "MODEL_NAME is malformed"},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := check(t, envFile(t, 0o600, tc.lines...))
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q; want refusal saying %q", code, stderr, tc.want)
			}
			noLeak(t, "output", stdout+stderr)
		})
	}
}

func renderOpenRouterModelConfig(t *testing.T) (map[string]any, string) {
	return renderOpenRouterModelConfigWithLines(t, openRouterEnv()...)
}

func TestRenderOpenRouterModelConfig(t *testing.T) {
	_, output := renderOpenRouterModelConfig(t)
	var config struct {
		Spec struct {
			Provider        string `json:"provider"`
			Model           string `json:"model"`
			APIKeySecret    string `json:"apiKeySecret"`
			APIKeySecretKey string `json:"apiKeySecretKey"`
			OpenAI          struct {
				BaseURL string `json:"baseUrl"`
			} `json:"openAI"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(output), &config); err != nil {
		t.Fatal(err)
	}
	spec := config.Spec
	if spec.Provider != "OpenAI" || spec.Model != "anthropic/claude-haiku-4.5" || spec.OpenAI.BaseURL != "https://openrouter.ai/api/v1" || spec.APIKeySecret != "sandbox-model" || spec.APIKeySecretKey != "apiKey" {
		t.Fatalf("unexpected ModelConfig: %+v", spec)
	}
	noLeak(t, "manifest", output)
	if strings.Contains(output, "__MODEL") {
		t.Fatal("unrendered model placeholder")
	}
}

func TestRenderOpenRouterModelAndEndpointAreIndependent(t *testing.T) {
	for name, tc := range map[string]struct {
		model   string
		baseURL string
	}{
		"custom model with default endpoint": {model: "openai/gpt-4.1"},
		"default model with custom endpoint": {model: "anthropic/claude-haiku-4.5", baseURL: "https://router.example.test:8443/openai/v1"},
		"custom model and endpoint":          {model: "vendor/model:free", baseURL: "https://router.example.test/v1"},
	} {
		t.Run(name, func(t *testing.T) {
			lines := withLine(openRouterEnv(), "MODEL_NAME", "MODEL_NAME="+tc.model)
			if tc.baseURL != "" {
				lines = withBaseURL(lines, tc.baseURL)
			}
			document, output := renderOpenRouterModelConfigWithLines(t, lines...)
			spec := document["spec"].(map[string]any)
			openAI := spec["openAI"].(map[string]any)
			wantURL := tc.baseURL
			if wantURL == "" {
				wantURL = "https://openrouter.ai/api/v1"
			}
			if spec["model"] != tc.model || openAI["baseUrl"] != wantURL {
				t.Errorf("spec model=%v baseUrl=%v; want model=%q baseUrl=%q", spec["model"], openAI["baseUrl"], tc.model, wantURL)
			}
			for key := range document {
				if key != "apiVersion" && key != "kind" && key != "metadata" && key != "spec" {
					t.Errorf("model or endpoint text introduced top-level YAML field %q", key)
				}
			}
			for key := range spec {
				if key != "provider" && key != "model" && key != "apiKeySecret" && key != "apiKeySecretKey" && key != "openAI" {
					t.Errorf("model or endpoint text introduced spec YAML field %q", key)
				}
			}
			for key := range openAI {
				if key != "baseUrl" {
					t.Errorf("model or endpoint text introduced openAI YAML field %q", key)
				}
			}
			noLeak(t, "manifest", output)
		})
	}
}

func renderOpenRouterModelConfigWithLines(t *testing.T, lines ...string) (map[string]any, string) {
	t.Helper()
	envPath := envFile(t, 0o600, lines...)
	out, err := exec.CommandContext(t.Context(), "bash", "-c", `set -eu
. "$1/env.sh"
sandbox_env_load "$2"
render "$1/modelconfig-${MODEL_PROVIDER}.yaml"`, "bash", strings.TrimSuffix(sandboxDir, "/"), envPath).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v: %s", err, out)
	}
	var document map[string]any
	if err := yaml.Unmarshal(out, &document); err != nil {
		t.Fatal(err)
	}
	return document, string(out)
}

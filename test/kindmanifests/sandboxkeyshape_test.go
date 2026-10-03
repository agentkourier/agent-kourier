package kindmanifests_test

import (
	"strings"
	"testing"
)

// The API key must be a key for MODEL_PROVIDER: a key sent to another provider's host is sent for nothing, and that host sees it
// (a Bedrock key once went to api.anthropic.com, twice, because the provider said anthropic). The check names the key and the
// provider and never the value. A key that looks like a Bedrock one gets its own message, since this sandbox cannot use Bedrock
// yet. Bedrock's prefixes: bedrock-api-key- for a short-term key is in the AWS token generator's README; ABSK for a long-term key
// is what such a key was seen to start with, and the AWS pages do not say.

// withProviderAndKey is a file for a provider, with the given key, passing every other check.
func withProviderAndKey(provider, key string) []string {
	return withLine(withLine(goodEnv(), "MODEL_PROVIDER", "MODEL_PROVIDER="+provider), "MODEL_API_KEY", "MODEL_API_KEY="+key)
}

func TestEnvCheckAcceptsAKeyOfTheProvidersShape(t *testing.T) {
	for name, tc := range map[string]struct{ provider, key string }{
		"anthropic":                {"anthropic", fakeKey},
		"openai":                   {"openai", fakeOpenAIKey},
		"openai, a legacy sk- key": {"openai", "sk-FAKE-LEAKCHECK-legacy-key-0007"},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := check(t, envFile(t, 0o600, withProviderAndKey(tc.provider, tc.key)...))
			if code != 0 {
				t.Errorf("exit %d, stderr %q; want it accepted", code, stderr)
			}
			noLeak(t, "stdout", stdout)
			noLeak(t, "stderr", stderr)
		})
	}
}

func TestEnvCheckRefusesAKeyOfAnotherProvider(t *testing.T) {
	const (
		anthropic = "MODEL_API_KEY is not an Anthropic API key, which MODEL_PROVIDER=anthropic needs"
		openai    = "MODEL_API_KEY is not an OpenAI API key, which MODEL_PROVIDER=openai needs"
	)
	for name, tc := range map[string]struct{ provider, key, want string }{
		"an OpenAI key for anthropic":     {"anthropic", fakeOpenAIKey, anthropic},
		"a key of no shape for anthropic": {"anthropic", "FAKE-LEAKCHECK-no-prefix-key-0010", anthropic},
		"an Anthropic key for openai":     {"openai", fakeKey, openai},
		"a key of no shape for openai":    {"openai", "FAKE-LEAKCHECK-no-prefix-key-0010", openai},
		"a lower case absk for anthropic": {"anthropic", "absk-FAKE-LEAKCHECK-key-0011", anthropic},
		"an Anthropic prefix, upper case": {"anthropic", "SK-ANT-FAKE-LEAKCHECK-key-0012", anthropic},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := check(t, envFile(t, 0o600, withProviderAndKey(tc.provider, tc.key)...))
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q; want a refusal that says %q", code, stderr, tc.want)
			}
			noLeak(t, "stdout", stdout)
			noLeak(t, "stderr", stderr)
		})
	}
}

// TestEnvCheckSaysABedrockKeyIsABedrockKey: a Bedrock-looking key, under either provider, gets the specific message (this
// sandbox cannot use Bedrock yet), which names the key and no value, and not the generic one.
func TestEnvCheckSaysABedrockKeyIsABedrockKey(t *testing.T) {
	const want = "MODEL_API_KEY looks like a Bedrock API key; this sandbox cannot use Bedrock yet"
	for name, key := range map[string]string{
		"a long-term key":                 fakeBedrockKey,
		"a short-term key":                "bedrock-api-key-FAKE-LEAKCHECK-0008",
		"a base64 payload of a long-term": "ABSKQmVkcm9ja0FQSUtleS1GQUtF+LEAKCHECK/0009==",
	} {
		for _, provider := range []string{"anthropic", "openai"} {
			t.Run(provider+", "+name, func(t *testing.T) {
				code, stdout, stderr := check(t, envFile(t, 0o600, withProviderAndKey(provider, key)...))
				if code == 0 || !strings.Contains(stderr, want) || strings.Contains(stderr, "is not an") {
					t.Errorf("exit %d, stderr %q; want a refusal that says %q and not the generic one", code, stderr, want)
				}
				noLeak(t, "stdout", stdout)
				noLeak(t, "stderr", stderr)
			})
		}
	}
}

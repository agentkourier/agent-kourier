package kindmanifests_test

import (
	"strings"
	"testing"
)

// The API key must be a key for MODEL_PROVIDER: a key sent to another provider's host is sent for nothing, and that host sees it
// (a Bedrock key once went to api.anthropic.com, twice, because the provider said anthropic). The check names the key and the
// provider and never the value. A key that is plainly another provider's says whose it looks like. Bedrock's prefixes:
// bedrock-api-key- for a short-term key is in the AWS token generator's README; ABSK for a long-term key is what such a key was
// seen to start with, and the AWS pages do not say.

// withProviderAndKey is a file for a provider, with the given key, passing every other check.
func withProviderAndKey(provider, key string) []string {
	lines := withLine(withLine(goodEnv(), "MODEL_PROVIDER", "MODEL_PROVIDER="+provider), "MODEL_API_KEY", "MODEL_API_KEY="+key)
	if provider == "bedrock" {
		lines = append(lines, "MODEL_REGION="+fakeRegion)
	}
	return lines
}

func TestEnvCheckAcceptsAKeyOfTheProvidersShape(t *testing.T) {
	for name, tc := range map[string]struct{ provider, key string }{
		"anthropic":                 {"anthropic", fakeKey},
		"openai":                    {"openai", fakeOpenAIKey},
		"openai, a legacy sk- key":  {"openai", "sk-FAKE-LEAKCHECK-legacy-key-0007"},
		"bedrock, long-term":        {"bedrock", fakeBedrockKey},
		"bedrock, short-term":       {"bedrock", "bedrock-api-key-FAKE-LEAKCHECK-0008"},
		"bedrock, a base64 payload": {"bedrock", "ABSKQmVkcm9ja0FQSUtleS1GQUtF+LEAKCHECK/0009=="},
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
		bedrock   = "MODEL_API_KEY is not a Bedrock API key, which MODEL_PROVIDER=bedrock needs"
	)
	for name, tc := range map[string]struct{ provider, key, want string }{
		"an OpenAI key for anthropic":      {"anthropic", fakeOpenAIKey, anthropic},
		"a key of no shape for anthropic":  {"anthropic", "FAKE-LEAKCHECK-no-prefix-key-0010", anthropic},
		"an Anthropic key for openai":      {"openai", fakeKey, openai},
		"a key of no shape for openai":     {"openai", "FAKE-LEAKCHECK-no-prefix-key-0010", openai},
		"a lower case absk for anthropic":  {"anthropic", "absk-FAKE-LEAKCHECK-key-0011", anthropic},
		"an Anthropic prefix, upper case":  {"anthropic", "SK-ANT-FAKE-LEAKCHECK-key-0012", anthropic},
		"a key of no shape for bedrock":    {"bedrock", "FAKE-LEAKCHECK-no-prefix-key-0010", bedrock},
		"a lower case absk for bedrock":    {"bedrock", "absk-FAKE-LEAKCHECK-key-0011", bedrock},
		"bedrock prefix in the middle":     {"bedrock", "xABSK-FAKE-LEAKCHECK-key-0014", bedrock},
		"bedrock short-term prefix, no -":  {"bedrock", "bedrock-api-keyFAKE-LEAKCHECK-0015", bedrock},
		"a short-term prefix, upper case":  {"bedrock", "BEDROCK-API-KEY-FAKE-LEAKCHECK-0016", bedrock},
		"a key that starts with an AWS id": {"bedrock", "AKIAFAKELEAKCHECKKEY0017", bedrock},
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

// TestEnvCheckSaysWhoseKeyItLooksLike: a key that is plainly another provider's gets a message that says so, names the key and the
// provider that cannot use it, and says what to set; it never has the value, and not the generic message.
func TestEnvCheckSaysWhoseKeyItLooksLike(t *testing.T) {
	for name, tc := range map[string]struct{ provider, key, want string }{
		"a long-term Bedrock key for anthropic":  {"anthropic", fakeBedrockKey, "MODEL_API_KEY looks like a Bedrock API key; MODEL_PROVIDER=anthropic cannot use it; set MODEL_PROVIDER=bedrock and MODEL_REGION"},
		"a short-term Bedrock key for anthropic": {"anthropic", "bedrock-api-key-FAKE-LEAKCHECK-0008", "MODEL_API_KEY looks like a Bedrock API key; MODEL_PROVIDER=anthropic cannot use it; set MODEL_PROVIDER=bedrock and MODEL_REGION"},
		"a Bedrock payload for openai":           {"openai", "ABSKQmVkcm9ja0FQSUtleS1GQUtF+LEAKCHECK/0009==", "MODEL_API_KEY looks like a Bedrock API key; MODEL_PROVIDER=openai cannot use it; set MODEL_PROVIDER=bedrock and MODEL_REGION"},
		"a long-term Bedrock key for openai":     {"openai", fakeBedrockKey, "MODEL_API_KEY looks like a Bedrock API key; MODEL_PROVIDER=openai cannot use it; set MODEL_PROVIDER=bedrock and MODEL_REGION"},
		"an Anthropic key for bedrock":           {"bedrock", fakeKey, "MODEL_API_KEY looks like an Anthropic API key; MODEL_PROVIDER=bedrock cannot use it; set MODEL_PROVIDER=anthropic"},
		"an OpenAI key for bedrock":              {"bedrock", fakeOpenAIKey, "MODEL_API_KEY looks like an OpenAI API key; MODEL_PROVIDER=bedrock cannot use it; set MODEL_PROVIDER=openai"},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := check(t, envFile(t, 0o600, withProviderAndKey(tc.provider, tc.key)...))
			if code == 0 || !strings.Contains(stderr, tc.want) || strings.Contains(stderr, "is not a") || strings.Contains(stderr, "is not an") {
				t.Errorf("exit %d, stderr %q; want a refusal that says %q and not the generic one", code, stderr, tc.want)
			}
			noLeak(t, "stdout", stdout)
			noLeak(t, "stderr", stderr)
		})
	}
}

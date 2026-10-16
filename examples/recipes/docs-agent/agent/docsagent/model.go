package docsagent

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/urlcheck"
)

// openRouterBaseURL is OpenRouter's OpenAI-compatible endpoint.
const openRouterBaseURL = "https://openrouter.ai/api/v1"

// ModelFromEnv builds the language model from environment variables, read through getenv:
//
//	MODEL_PROVIDER  openrouter or openai
//	MODEL_NAME      the provider's model name, such as vendor/model on OpenRouter
//	MODEL_API_KEY   the provider's key
//	MODEL_BASE_URL  optional https endpoint; OpenRouter's is the default for openrouter, and openai takes
//	                any OpenAI-compatible endpoint here
//
// Both providers are spoken through the Chat Completions API. The error names a variable, never its value, so a
// key cannot reach a log through it.
func ModelFromEnv(ctx context.Context, getenv func(string) string) (model.LLM, error) {
	provider, name, key, base := getenv("MODEL_PROVIDER"), getenv("MODEL_NAME"), getenv("MODEL_API_KEY"), getenv("MODEL_BASE_URL")
	switch provider {
	case "openrouter":
		if base == "" {
			base = openRouterBaseURL
		}
	case "openai":
	case "":
		return nil, errors.New("MODEL_PROVIDER is required: openrouter or openai")
	default:
		return nil, fmt.Errorf("MODEL_PROVIDER %q is not supported by this agent: use openrouter or openai", provider)
	}
	if name == "" {
		return nil, errors.New("MODEL_NAME is required")
	}
	// An empty key would let the client fall back to OPENAI_API_KEY and send that to whatever MODEL_BASE_URL names.
	if key == "" {
		return nil, errors.New("MODEL_API_KEY is required")
	}
	if base != "" {
		if _, err := urlcheck.HTTPS(base); err != nil {
			return nil, fmt.Errorf("MODEL_BASE_URL %w", err)
		}
	}
	m, err := openaimodel.NewModel(ctx, name, &openaimodel.ClientConfig{APIKey: key, BaseURL: base, API: openaimodel.APIChatCompletions})
	if err != nil {
		return nil, fmt.Errorf("build the model: %w", err)
	}
	return m, nil
}

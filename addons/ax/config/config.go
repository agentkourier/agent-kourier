// Package config reads the agent's settings from its environment and checks them, so that a setting the agent cannot
// use stops it at start with an error that names the variable. The kit's settings package reads the ones every add-on
// shares; this package holds the agent's own. An error never shows the value of a variable that may hold a secret
// (POSTGRES_DSN, AUTH_HEADER, OIDC_ISSUER, RUNNER_URL).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/agentkourier/agent-kourier/addons/kit/settings"
)

// The model providers.
const (
	// ProviderBedrock is Amazon Bedrock's Converse API, in MODEL_REGION.
	ProviderBedrock = "bedrock"
	// ProviderAnthropic is the Anthropic Messages API, whose SDK reads its own credentials (ANTHROPIC_API_KEY) and
	// ANTHROPIC_BASE_URL.
	ProviderAnthropic = "anthropic"
)

// The agent's own variables, in the order they are read.
var (
	modelProvider = settings.Func("MODEL_PROVIDER", ProviderBedrock, func(v string, _ settings.Values) (string, error) {
		switch p := strings.ToLower(v); p {
		case ProviderBedrock, ProviderAnthropic:
			return p, nil
		}
		return "", fmt.Errorf("MODEL_PROVIDER %q is not bedrock or anthropic", v)
	})
	modelRegion = settings.Func("MODEL_REGION", "", func(v string, earlier settings.Values) (string, error) {
		switch modelProvider.From(earlier) {
		case ProviderBedrock:
			if v == "" {
				return "", errors.New("MODEL_REGION is required with MODEL_PROVIDER=bedrock")
			}
		case ProviderAnthropic:
			if v != "" {
				return "", errors.New("MODEL_REGION is set with MODEL_PROVIDER=anthropic, which has no region: unset it")
			}
		}
		return v, nil
	})
	modelName = settings.Required("MODEL_NAME")
	stateDir  = settings.Func("STATE_DIR", "", func(v string, _ settings.Values) (struct{}, error) {
		if v != "" {
			return struct{}{}, errors.New("STATE_DIR is no longer read: set SQLITE_PATH to the database file instead " +
				"(STATE_DIR plus /agent.db, such as /workspace/agent-kourier-state/agent.db) and unset STATE_DIR")
		}
		return struct{}{}, nil
	})
	runnerURL = settings.Func("RUNNER_URL", "", func(v string, _ settings.Values) (string, error) {
		if v != "" {
			if err := originURL(v); err != nil {
				return "", fmt.Errorf("RUNNER_URL %w (its value is not shown)", err)
			}
		}
		return v, nil
	})
)

// Spec is how the agent reads its settings: the shared ones with the token check or OIDC, and its own.
func Spec() settings.Spec {
	return settings.Spec{Name: "ax", Noun: "agent", OIDC: true,
		Vars: []settings.Var{modelProvider, modelRegion, modelName, stateDir, runnerURL}}
}

// Config is the agent's settings.
type Config struct {
	settings.Config
	// ModelProvider is the model's provider: ProviderBedrock or ProviderAnthropic.
	ModelProvider string
	// ModelRegion is the Bedrock region; Bedrock only.
	ModelRegion string
	// ModelName is the model: Bedrock's model or inference profile ID, or the Anthropic API's model ID.
	ModelName string
	// RunnerURL is the AX task runner's metadata server, to which /readyz and /metadata/ are proxied. Empty runs the
	// agent standalone: it answers /readyz itself.
	RunnerURL string
}

// Standalone reports whether the agent runs without the AX task runner beside it, and so answers /readyz itself.
func (c Config) Standalone() bool { return c.RunnerURL == "" }

// Parse reads the settings through getenv (os.Getenv in production), where an empty value is an unset one, and checks
// them.
func Parse(getenv func(string) string) (Config, error) {
	shared, own, err := settings.Parse(getenv, Spec())
	if err != nil {
		return Config{}, err
	}
	return Config{Config: shared, ModelProvider: modelProvider.From(own), ModelRegion: modelRegion.From(own),
		ModelName: modelName.From(own), RunnerURL: runnerURL.From(own)}, nil
}

// originURL reports why v is not an http or https origin: a scheme and a host, with no user info, path, query or
// fragment (a trailing slash is allowed).
func originURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("is not an http or https URL with a host and no user info, path, query or fragment")
	}
	return nil
}

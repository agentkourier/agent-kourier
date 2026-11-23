// Command agent serves an ADK agent on Amazon Bedrock or the Anthropic API as an A2A agent, on the kit's runtime
// (package runtime, which owns the command's flag, exit codes, settings, caller check, store, leader election and stop).
//
// It is configured through its environment (the kit's package settings holds the variables every add-on shares, package
// config the agent's own, and package telemetry the OTEL_* ones). On Bedrock it reads AWS credentials through the AWS
// SDK's default chain (AWS_BEARER_TOKEN_BEDROCK, a role, a profile); on the Anthropic API, the Anthropic SDK reads its
// own (ANTHROPIC_API_KEY first) and ANTHROPIC_BASE_URL. A setting it cannot use stops it at start, with an error that
// names the variable. It takes one flag, --version.
//
// Requests to the A2A endpoint and the card must pass the caller check: a token from AUTH_TOKEN_FILE, or, inside Google
// AX, an OIDC ID token for Agent Kourier's service identity. /healthz, /readyz and /metrics are open. With Postgres its
// replicas elect a leader; a replica that loses the lease exits with cluster.ExitLeadershipLost (5).
package main

import (
	"context"
	"errors"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/addons/ax/agent"
	"github.com/agentkourier/agent-kourier/addons/ax/anthropic"
	"github.com/agentkourier/agent-kourier/addons/ax/bedrock"
	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/ax/store"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// version is set at build time: -ldflags "-X main.version=<value>".
var version = "dev"

func main() { runtime.Main(addon()) }

// addon is the agent at version, its model the provider's.
func addon() runtime.Addon[*store.Store] { return agent.Addon(version, newModel) }

// newModel is the model of cfg's provider, its calls observed by tel.
func newModel(ctx context.Context, cfg config.Config, tel *telemetry.Telemetry) (model.LLM, error) {
	if cfg.ModelProvider == config.ProviderAnthropic {
		return &anthropic.Model{ID: cfg.ModelName, Client: anthropic.Observe(anthropic.NewClient(), tel)}, nil
	}
	aws, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.ModelRegion))
	if err != nil {
		// The SDK's error can quote a shared config file; it is not passed on.
		return nil, errors.New("bedrock: the AWS configuration could not be loaded")
	}
	return &bedrock.Model{ID: cfg.ModelName, Client: bedrock.Observe(bedrockruntime.NewFromConfig(aws), tel)}, nil
}

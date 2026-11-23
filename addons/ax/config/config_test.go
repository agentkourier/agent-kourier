package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/kit/settings"
)

// parse reads the settings as the agent's command does (runtime.Parse of agent.Addon, which this package cannot import):
// the agent's own and the shared ones, with the OIDC check.
func parse(getenv func(string) string) (Config, error) {
	shared, own, err := settings.Parse(getenv, spec())
	if err != nil {
		return Config{}, err
	}
	return From(shared, own), nil
}

// spec is the settings.Spec that the kit's runtime makes of agent.Addon.
func spec() settings.Spec { return settings.Spec{Name: "ax", Noun: "agent", OIDC: true, Vars: Vars()} }

// env is a fake environment: the minimal valid one plus the overrides. An override of "" unsets the variable.
func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		"MODEL_REGION":    "us-east-1",
		"MODEL_NAME":      "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		"AUTH_TOKEN_FILE": "/run/secrets/tokens",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(key string) string { return base[key] }
}

// oidc is a valid OIDC caller check in place of the token file, plus the overrides.
func oidc(overrides map[string]string) map[string]string {
	out := map[string]string{
		"AUTH_TOKEN_FILE": "",
		"OIDC_ISSUER":     "https://issuer.example.test/dex", "OIDC_AUDIENCE": "agent-kourier-binding", "OIDC_SUBJECT": "service",
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// pg is a valid Postgres environment plus the overrides.
func pg(overrides map[string]string) map[string]string {
	out := map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://u:fake-password@db/ax", "ADVERTISE_URL": "http://10.0.0.7:8090"}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func TestDefaults(t *testing.T) {
	c, err := parse(env(nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Config{
		Config: settings.Config{
			ListenAddr: "127.0.0.1:8090", PublicURL: "http://127.0.0.1:8090/a2a",
			AuthTokenFile: "/run/secrets/tokens", AuthHeader: "Authorization",
			Store: settings.StoreSQLite, SQLitePath: "ax-addon.db",
			TaskRetention: 720 * time.Hour, SessionIdleTTL: 2160 * time.Hour,
			Lease:    settings.Lease{Duration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second},
			PodName:  c.PodName,
			LogLevel: slog.LevelInfo,
		},
		ModelProvider: "bedrock", ModelRegion: "us-east-1", ModelName: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
	}
	if c != want {
		t.Fatalf("Parse =\n%+v\nwant\n%+v", c, want)
	}
	if !c.Standalone() || c.OIDC() {
		t.Errorf("Standalone %v OIDC %v, want a standalone agent with the token check", c.Standalone(), c.OIDC())
	}
}

func TestEverySetting(t *testing.T) {
	c, err := parse(env(pg(map[string]string{
		"MODEL_PROVIDER": "Bedrock",
		"LISTEN_ADDR":    ":9000", "PUBLIC_URL": "http://ax.agents.svc:9000/a2a", "RUNNER_URL": "http://127.0.0.1:8081",
		"AUTH_HEADER":    "x-kourier-token",
		"TASK_RETENTION": "0", "SESSION_IDLE_TTL": "0",
		"LEASE_DURATION": "30s", "LEASE_RENEW_DEADLINE": "20s", "LEASE_RETRY_PERIOD": "4s",
		"POD_NAME": "ax-0", "LOG_LEVEL": "DEBUG",
	})))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Config{
		Config: settings.Config{
			ListenAddr: ":9000", PublicURL: "http://ax.agents.svc:9000/a2a",
			AuthTokenFile: "/run/secrets/tokens", AuthHeader: "X-Kourier-Token",
			Store: settings.StorePostgres, SQLitePath: "ax-addon.db", PostgresDSN: "postgres://u:fake-password@db/ax",
			Lease:        settings.Lease{Duration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 4 * time.Second},
			AdvertiseURL: "http://10.0.0.7:8090", PodName: "ax-0", LogLevel: slog.LevelDebug,
		},
		ModelProvider: "bedrock", ModelRegion: "us-east-1", ModelName: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		RunnerURL: "http://127.0.0.1:8081",
	}
	if c != want {
		t.Fatalf("Parse =\n%+v\nwant\n%+v", c, want)
	}
	if c.Standalone() {
		t.Error("Standalone = true with RUNNER_URL set")
	}
}

// The Anthropic API: the model's name alone, with no region. The key is the SDK's to read, not the settings'.
func TestAnthropicProvider(t *testing.T) {
	c, err := parse(env(map[string]string{"MODEL_PROVIDER": "Anthropic", "MODEL_REGION": "", "MODEL_NAME": "claude-haiku-4-5"}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.ModelProvider != ProviderAnthropic || c.ModelName != "claude-haiku-4-5" || c.ModelRegion != "" {
		t.Fatalf("provider %q, name %q, region %q", c.ModelProvider, c.ModelName, c.ModelRegion)
	}
}

// The AX route: OIDC, the runner beside the agent, port 80 and the workspace's database file.
func TestAXRoute(t *testing.T) {
	c, err := parse(env(oidc(map[string]string{
		"LISTEN_ADDR": ":80", "PUBLIC_URL": "https://ax-gateway.example.test/a2a",
		"RUNNER_URL": "http://127.0.0.1:8081", "SQLITE_PATH": "/workspace/agent-kourier-state/agent.db",
	})))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.OIDC() || c.AuthTokenFile != "" || c.AuthDisabled {
		t.Fatalf("caller check = token %q disabled %v OIDC %v, want OIDC alone", c.AuthTokenFile, c.AuthDisabled, c.OIDC())
	}
	if c.OIDCIssuer != "https://issuer.example.test/dex" || c.OIDCAudience != "agent-kourier-binding" || c.OIDCSubject != "service" {
		t.Errorf("OIDC = %q %q %q", c.OIDCIssuer, c.OIDCAudience, c.OIDCSubject)
	}
	if c.Standalone() || c.SQLitePath != "/workspace/agent-kourier-state/agent.db" || c.PublicURL != "https://ax-gateway.example.test/a2a" {
		t.Errorf("runner %q sqlite %q public %q", c.RunnerURL, c.SQLitePath, c.PublicURL)
	}
}

// The agent's own variables. The shared ones are the kit's, and tested there.
func TestRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		names     string // the variable the error must name
	}{
		{"no region", map[string]string{"MODEL_REGION": ""}, "MODEL_REGION"},
		{"no model", map[string]string{"MODEL_NAME": ""}, "MODEL_NAME"},
		{"provider", map[string]string{"MODEL_PROVIDER": "openai"}, "MODEL_PROVIDER"},
		{"provider names the ones there are", map[string]string{"MODEL_PROVIDER": "openai"}, "anthropic"},
		{"anthropic with a region", map[string]string{"MODEL_PROVIDER": "anthropic"}, "MODEL_REGION"},
		{"anthropic without a model", map[string]string{"MODEL_PROVIDER": "anthropic", "MODEL_REGION": "", "MODEL_NAME": ""}, "MODEL_NAME"},
		{"state dir", map[string]string{"STATE_DIR": "/workspace/agent-kourier-state"}, "SQLITE_PATH"},
		{"runner url relative", map[string]string{"RUNNER_URL": "127.0.0.1:8081"}, "RUNNER_URL"},
		{"runner url with a path", map[string]string{"RUNNER_URL": "http://127.0.0.1:8081/readyz"}, "RUNNER_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(env(tc.overrides))
			if err == nil {
				t.Fatal("Parse accepted it")
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("error %q does not name %s", err, tc.names)
			}
		})
	}
}

// How the agent reads the shared variables: the token check or OIDC, its own name for itself in the refusal of
// AUTH_DISABLED, and a URL whose host is a port alone refused.
func TestSharedAsTheAgentReadsThem(t *testing.T) {
	for _, tc := range []struct {
		overrides map[string]string
		want      string
	}{
		{map[string]string{"AUTH_TOKEN_FILE": ""}, "a caller check is required: AUTH_TOKEN_FILE, or OIDC_ISSUER, OIDC_AUDIENCE and OIDC_SUBJECT " +
			"(or AUTH_DISABLED=true on a loopback LISTEN_ADDR)"},
		{map[string]string{"AUTH_TOKEN_FILE": "", "AUTH_DISABLED": "true", "LISTEN_ADDR": "10.0.0.7:8090"},
			`AUTH_DISABLED=true needs a loopback LISTEN_ADDR, and "10.0.0.7:8090" is not one: anyone who reaches the agent could run it`},
		{map[string]string{"LISTEN_ADDR": ":80"}, `PUBLIC_URL is unset and the one made from LISTEN_ADDR, "http://:80/a2a", is not an http or https URL ` +
			"with a host: set PUBLIC_URL to the URL callers reach the A2A endpoint at"},
		{map[string]string{"PUBLIC_URL": "http://:80/a2a"}, `PUBLIC_URL "http://:80/a2a" is not an http or https URL with a host`},
		{oidc(map[string]string{"AUTH_TOKEN_FILE": "/run/secrets/tokens"}), "AUTH_TOKEN_FILE and OIDC_ISSUER, OIDC_AUDIENCE or OIDC_SUBJECT are both set: " +
			"the caller check is the token file or OIDC, not both"},
	} {
		if _, err := parse(env(tc.overrides)); err == nil || err.Error() != tc.want {
			t.Errorf("%v: error %v, want %s", tc.overrides, err, tc.want)
		}
	}
}

// The agent's own errors come before the shared ones, in the order it reads its own.
func TestErrorOrder(t *testing.T) {
	_, err := parse(env(map[string]string{"MODEL_REGION": "", "MODEL_NAME": "", "STATE_DIR": "/x", "RUNNER_URL": "x", "LISTEN_ADDR": "8090"}))
	want := "MODEL_REGION is required with MODEL_PROVIDER=bedrock\nMODEL_NAME is required\n" +
		"STATE_DIR is no longer read: set SQLITE_PATH to the database file instead (STATE_DIR plus /agent.db, such as " +
		"/workspace/agent-kourier-state/agent.db) and unset STATE_DIR\n" +
		"RUNNER_URL is not an http or https URL with a host and no user info, path, query or fragment (its value is not shown)\n" +
		`LISTEN_ADDR "8090" is not host:port`
	if err == nil || err.Error() != want {
		t.Fatalf("error =\n%v\nwant\n%s", err, want)
	}
}

func TestRunnerURLNeverShown(t *testing.T) {
	const secret = "fake-password-do-not-print"
	_, err := parse(env(map[string]string{"RUNNER_URL": "http://u:" + secret + "@127.0.0.1:8081"}))
	if err == nil {
		t.Fatal("Parse accepted a RUNNER_URL with user info")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error shows a secret value: %v", err)
	}
}

// Each of the agent's own variables is in Vars, so the runtime reads it: one left out would read as its zero value.
func TestOwnVarsDeclared(t *testing.T) {
	_, own, err := settings.Parse(env(nil), spec())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, v := range []interface {
		Key() string
		Declared(settings.Values) bool
	}{modelProvider, modelRegion, modelName, stateDir, runnerURL} {
		if !v.Declared(own) {
			t.Errorf("%s is not in Vars", v.Key())
		}
	}
}

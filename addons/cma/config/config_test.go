package config

import (
	"log/slog"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/addons/kit/settings"
)

// env is a fake environment: the minimal valid one plus the overrides. An override of "" unsets the variable.
func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		"CMA_AGENT_ID":       "agent_test",
		"CMA_ENVIRONMENT_ID": "env_test",
		"AUTH_TOKEN_FILE":    "/run/secrets/tokens",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(key string) string { return base[key] }
}

func TestDefaults(t *testing.T) {
	c, err := Parse(env(nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Config{
		Config: settings.Config{
			ListenAddr: "127.0.0.1:8090", PublicURL: "http://127.0.0.1:8090/a2a",
			SessionIdleTTL: 2160 * time.Hour,
			AuthTokenFile:  "/run/secrets/tokens", AuthHeader: "Authorization",
			Store: settings.StoreSQLite, SQLitePath: "cma-addon.db", TaskRetention: 720 * time.Hour,
			Lease:    settings.Lease{Duration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second},
			PodName:  c.PodName,
			LogLevel: slog.LevelInfo,
		},
		AgentID: "agent_test", EnvironmentID: "env_test", ToolResultTimeout: 10 * time.Minute,
	}
	if c != want {
		t.Fatalf("Parse =\n%+v\nwant\n%+v", c, want)
	}
}

func TestEverySetting(t *testing.T) {
	c, err := Parse(env(map[string]string{
		"LISTEN_ADDR": ":9000", "PUBLIC_URL": "http://cma.agents.svc:9000/a2a",
		"SESSION_IDLE_TTL": "0", "TOOL_RESULT_TIMEOUT": "90s",
		"AUTH_HEADER": "x-kourier-token",
		"STORE":       "postgres", "POSTGRES_DSN": "postgres://u:fake-password@db/cma", "TASK_RETENTION": "0",
		"LEASE_DURATION": "30s", "LEASE_RENEW_DEADLINE": "20s", "LEASE_RETRY_PERIOD": "4s",
		"ADVERTISE_URL": "http://10.0.0.7:9000", "POD_NAME": "cma-0", "LOG_LEVEL": "DEBUG",
	}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Config{
		Config: settings.Config{
			ListenAddr: ":9000", PublicURL: "http://cma.agents.svc:9000/a2a",
			AuthTokenFile: "/run/secrets/tokens", AuthHeader: "X-Kourier-Token",
			Store: settings.StorePostgres, SQLitePath: "cma-addon.db", PostgresDSN: "postgres://u:fake-password@db/cma",
			Lease:        settings.Lease{Duration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 4 * time.Second},
			AdvertiseURL: "http://10.0.0.7:9000", PodName: "cma-0", LogLevel: slog.LevelDebug,
		},
		AgentID: "agent_test", EnvironmentID: "env_test", ToolResultTimeout: 90 * time.Second,
	}
	if c != want {
		t.Fatalf("Parse =\n%+v\nwant\n%+v", c, want)
	}
}

// The adapter's own variables. The shared ones are the kit's, and tested there.
func TestRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		want      string // the whole error
	}{
		{"no agent", map[string]string{"CMA_AGENT_ID": ""}, "CMA_AGENT_ID is required"},
		{"no environment", map[string]string{"CMA_ENVIRONMENT_ID": ""}, "CMA_ENVIRONMENT_ID is required"},
		{"tool timeout negative", map[string]string{"TOOL_RESULT_TIMEOUT": "-5s"}, `TOOL_RESULT_TIMEOUT "-5s" is not a duration of zero or more`},
		{"tool timeout not a duration", map[string]string{"TOOL_RESULT_TIMEOUT": "10"}, `TOOL_RESULT_TIMEOUT "10" is not a Go duration such as 90s or 720h`},
		{"own before shared", map[string]string{"CMA_AGENT_ID": "", "LOG_LEVEL": "chatty"},
			"CMA_AGENT_ID is required\n" + `LOG_LEVEL "chatty" is not debug, info, warn or error`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(env(tc.overrides))
			if err == nil {
				t.Fatal("Parse accepted it")
			}
			if err.Error() != tc.want {
				t.Errorf("error =\n%s\nwant\n%s", err, tc.want)
			}
		})
	}
}

// How the adapter reads the shared variables: the token check alone (the OIDC_* variables are not read), and its own
// name for itself in the refusal of AUTH_DISABLED.
func TestSharedAsTheAdapterReadsThem(t *testing.T) {
	c, err := Parse(env(map[string]string{"OIDC_ISSUER": "https://issuer.example.test", "OIDC_AUDIENCE": "a", "OIDC_SUBJECT": "s"}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.OIDC() {
		t.Errorf("OIDC = true, want the OIDC_* variables unread")
	}
	for _, tc := range []struct {
		overrides map[string]string
		want      string
	}{
		{map[string]string{"AUTH_TOKEN_FILE": ""}, "AUTH_TOKEN_FILE is required (or AUTH_DISABLED=true on a loopback LISTEN_ADDR)"},
		{map[string]string{"AUTH_TOKEN_FILE": "", "AUTH_DISABLED": "true", "LISTEN_ADDR": ":8090", "PUBLIC_URL": "http://cma.agents.svc:8090/a2a"},
			`AUTH_DISABLED=true needs a loopback LISTEN_ADDR, and ":8090" is not one: anyone who reaches the adapter could run the agent`},
	} {
		if _, err := Parse(env(tc.overrides)); err == nil || err.Error() != tc.want {
			t.Errorf("%v: error %v, want %s", tc.overrides, err, tc.want)
		}
	}
}

// A URL whose host is a port alone names no host a caller could reach: refused for PUBLIC_URL, set or made from
// LISTEN_ADDR, and for ADVERTISE_URL. The adapter accepted one before it read its settings through the kit.
func TestHostlessURLsRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{"public url", map[string]string{"LISTEN_ADDR": ":8090", "PUBLIC_URL": "http://:8090"},
			`PUBLIC_URL "http://:8090" is not an http or https URL with a host`},
		{"public url made from LISTEN_ADDR", map[string]string{"LISTEN_ADDR": ":8090"},
			`PUBLIC_URL is unset and the one made from LISTEN_ADDR, "http://:8090/a2a", is not an http or https URL with a host: ` +
				"set PUBLIC_URL to the URL callers reach the A2A endpoint at"},
		{"advertise url", map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://db/cma", "ADVERTISE_URL": "http://:8090"},
			`ADVERTISE_URL "http://:8090" is not an http or https URL with a host`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(env(tc.overrides)); err == nil || err.Error() != tc.want {
				t.Errorf("error %v, want %s", err, tc.want)
			}
		})
	}
}

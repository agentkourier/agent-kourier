package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
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
		AgentID: "agent_test", EnvironmentID: "env_test",
		ListenAddr: "127.0.0.1:8090", PublicURL: "http://127.0.0.1:8090/a2a",
		SessionIdleTTL: 2160 * time.Hour, ToolResultTimeout: 10 * time.Minute,
		AuthTokenFile: "/run/secrets/tokens", AuthHeader: "Authorization",
		Store: StoreSQLite, SQLitePath: "cma-addon.db", TaskRetention: 720 * time.Hour,
		Lease:    Lease{Duration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second},
		PodName:  c.PodName,
		LogLevel: slog.LevelInfo,
	}
	if c != want {
		t.Fatalf("Parse =\n%+v\nwant\n%+v", c, want)
	}
	if c.PodName == "" {
		t.Error("POD_NAME unset should default to the hostname")
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
		AgentID: "agent_test", EnvironmentID: "env_test",
		ListenAddr: ":9000", PublicURL: "http://cma.agents.svc:9000/a2a",
		ToolResultTimeout: 90 * time.Second,
		AuthTokenFile:     "/run/secrets/tokens", AuthHeader: "X-Kourier-Token",
		Store: StorePostgres, SQLitePath: "cma-addon.db", PostgresDSN: "postgres://u:fake-password@db/cma",
		Lease:        Lease{Duration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 4 * time.Second},
		AdvertiseURL: "http://10.0.0.7:9000", PodName: "cma-0", LogLevel: slog.LevelDebug,
	}
	if c != want {
		t.Fatalf("Parse =\n%+v\nwant\n%+v", c, want)
	}
}

func TestAuthDisabledOnLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8090", "127.9.9.9:1", "[::1]:8090", "localhost:8090"} {
		t.Run(addr, func(t *testing.T) {
			c, err := Parse(env(map[string]string{"AUTH_DISABLED": "true", "AUTH_TOKEN_FILE": "", "LISTEN_ADDR": addr}))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !c.AuthDisabled {
				t.Fatal("AuthDisabled = false")
			}
		})
	}
}

func TestRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		names     string // the variable the error must name
	}{
		{"no agent", map[string]string{"CMA_AGENT_ID": ""}, "CMA_AGENT_ID"},
		{"no environment", map[string]string{"CMA_ENVIRONMENT_ID": ""}, "CMA_ENVIRONMENT_ID"},
		{"listen not host:port", map[string]string{"LISTEN_ADDR": "8090"}, "LISTEN_ADDR"},
		{"public url relative", map[string]string{"PUBLIC_URL": "/a2a"}, "PUBLIC_URL"},
		{"public url scheme", map[string]string{"PUBLIC_URL": "ftp://x/a2a"}, "PUBLIC_URL"},
		{"idle ttl negative", map[string]string{"SESSION_IDLE_TTL": "-1h"}, "SESSION_IDLE_TTL"},
		{"idle ttl not a duration", map[string]string{"SESSION_IDLE_TTL": "90 days"}, "SESSION_IDLE_TTL"},
		{"tool timeout negative", map[string]string{"TOOL_RESULT_TIMEOUT": "-5s"}, "TOOL_RESULT_TIMEOUT"},
		{"retention negative", map[string]string{"TASK_RETENTION": "-1s"}, "TASK_RETENTION"},
		{"retention not a duration", map[string]string{"TASK_RETENTION": "30d"}, "TASK_RETENTION"},
		{"no token file", map[string]string{"AUTH_TOKEN_FILE": ""}, "AUTH_TOKEN_FILE"},
		{"auth disabled not a bool", map[string]string{"AUTH_DISABLED": "nope"}, "AUTH_DISABLED"},
		{"auth disabled on every interface", map[string]string{"AUTH_DISABLED": "true", "AUTH_TOKEN_FILE": "", "LISTEN_ADDR": ":8090"}, "AUTH_DISABLED"},
		{"auth disabled on a pod address", map[string]string{"AUTH_DISABLED": "true", "AUTH_TOKEN_FILE": "", "LISTEN_ADDR": "10.0.0.7:8090"}, "AUTH_DISABLED"},
		{"auth disabled with a token file", map[string]string{"AUTH_DISABLED": "true"}, "AUTH_TOKEN_FILE"},
		{"auth header not a token", map[string]string{"AUTH_HEADER": "X Token"}, "AUTH_HEADER"},
		{"unknown store", map[string]string{"STORE": "mysql"}, "STORE"},
		{"empty sqlite path", map[string]string{"SQLITE_PATH": "   "}, "SQLITE_PATH"},
		{"postgres without dsn", map[string]string{"STORE": "postgres", "ADVERTISE_URL": "http://10.0.0.7:8090"}, "POSTGRES_DSN"},
		{"postgres without advertise url", map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://db/cma"}, "ADVERTISE_URL"},
		{"advertise url relative", map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://db/cma", "ADVERTISE_URL": "10.0.0.7:8090"}, "ADVERTISE_URL"},
		{"dsn with sqlite", map[string]string{"POSTGRES_DSN": "postgres://db/cma"}, "POSTGRES_DSN"},
		{"lease with sqlite", map[string]string{"LEASE_DURATION": "15s"}, "LEASE_DURATION"},
		{"renew deadline with sqlite", map[string]string{"LEASE_RENEW_DEADLINE": "10s"}, "LEASE_RENEW_DEADLINE"},
		{"retry period with sqlite", map[string]string{"LEASE_RETRY_PERIOD": "2s"}, "LEASE_RETRY_PERIOD"},
		{"lease not above renew", pg(map[string]string{"LEASE_DURATION": "10s"}), "LEASE_DURATION"},
		{"renew not above 1.2 retry", pg(map[string]string{"LEASE_RETRY_PERIOD": "9s"}), "LEASE_RENEW_DEADLINE"},
		{"retry zero", pg(map[string]string{"LEASE_RETRY_PERIOD": "0"}), "LEASE_RETRY_PERIOD"},
		{"lease negative", pg(map[string]string{"LEASE_DURATION": "-15s"}), "LEASE_DURATION"},
		{"log level", map[string]string{"LOG_LEVEL": "chatty"}, "LOG_LEVEL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(env(tc.overrides))
			if err == nil {
				t.Fatal("Parse accepted it")
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("error %q does not name %s", err, tc.names)
			}
		})
	}
}

// pg is a valid Postgres environment plus the overrides.
func pg(overrides map[string]string) map[string]string {
	out := map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://u:fake-password@db/cma", "ADVERTISE_URL": "http://10.0.0.7:8090"}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func TestErrorsNeverShowTheDSN(t *testing.T) {
	const secret = "fake-password-do-not-print"
	for name, overrides := range map[string]map[string]string{
		"with sqlite":     {"POSTGRES_DSN": "postgres://u:" + secret + "@db/cma"},
		"bad lease":       pg(map[string]string{"POSTGRES_DSN": "postgres://u:" + secret + "@db/cma", "LEASE_DURATION": "1s"}),
		"no advertise":    {"STORE": "postgres", "POSTGRES_DSN": "postgres://u:" + secret + "@db/cma"},
		"bad auth header": {"AUTH_HEADER": secret + " x"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(env(overrides))
			if err == nil {
				t.Fatal("Parse accepted it")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error shows a secret value: %v", err)
			}
		})
	}
}

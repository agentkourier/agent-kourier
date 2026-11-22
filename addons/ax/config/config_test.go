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
	c, err := Parse(env(nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Config{
		ModelProvider: "bedrock", ModelRegion: "us-east-1", ModelName: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		ListenAddr: "127.0.0.1:8090", PublicURL: "http://127.0.0.1:8090/a2a",
		AuthTokenFile: "/run/secrets/tokens", AuthHeader: "Authorization",
		Store: StoreSQLite, SQLitePath: "ax-addon.db",
		TaskRetention: 720 * time.Hour, SessionIdleTTL: 2160 * time.Hour,
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
	if !c.Standalone() || c.OIDC() {
		t.Errorf("Standalone %v OIDC %v, want a standalone agent with the token check", c.Standalone(), c.OIDC())
	}
}

func TestEverySetting(t *testing.T) {
	c, err := Parse(env(pg(map[string]string{
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
		ModelProvider: "bedrock", ModelRegion: "us-east-1", ModelName: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		ListenAddr: ":9000", PublicURL: "http://ax.agents.svc:9000/a2a", RunnerURL: "http://127.0.0.1:8081",
		AuthTokenFile: "/run/secrets/tokens", AuthHeader: "X-Kourier-Token",
		Store: StorePostgres, SQLitePath: "ax-addon.db", PostgresDSN: "postgres://u:fake-password@db/ax",
		Lease:        Lease{Duration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 4 * time.Second},
		AdvertiseURL: "http://10.0.0.7:8090", PodName: "ax-0", LogLevel: slog.LevelDebug,
	}
	if c != want {
		t.Fatalf("Parse =\n%+v\nwant\n%+v", c, want)
	}
	if c.Standalone() {
		t.Error("Standalone = true with RUNNER_URL set")
	}
}

// The AX route: OIDC, the runner beside the agent, port 80 and the workspace's database file.
func TestAXRoute(t *testing.T) {
	c, err := Parse(env(oidc(map[string]string{
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

// Authorization, the default, may be named, in any case, and with OIDC too.
func TestAuthHeaderAuthorization(t *testing.T) {
	c, err := Parse(env(map[string]string{"AUTH_HEADER": "authorization"}))
	if err != nil || c.AuthHeader != "Authorization" {
		t.Fatalf("Parse = %q, %v; want Authorization", c.AuthHeader, err)
	}
	if _, err := Parse(env(oidc(map[string]string{"AUTH_HEADER": "Authorization"}))); err != nil {
		t.Fatalf("OIDC with AUTH_HEADER=Authorization: %v", err)
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
		{"no region", map[string]string{"MODEL_REGION": ""}, "MODEL_REGION"},
		{"no model", map[string]string{"MODEL_NAME": ""}, "MODEL_NAME"},
		{"provider", map[string]string{"MODEL_PROVIDER": "openai"}, "MODEL_PROVIDER"},
		{"state dir", map[string]string{"STATE_DIR": "/workspace/agent-kourier-state"}, "SQLITE_PATH"},
		{"listen not host:port", map[string]string{"LISTEN_ADDR": "8090"}, "LISTEN_ADDR"},
		{"public url relative", map[string]string{"PUBLIC_URL": "/a2a"}, "PUBLIC_URL"},
		{"public url scheme", map[string]string{"PUBLIC_URL": "ftp://x/a2a"}, "PUBLIC_URL"},
		{"public url no host", map[string]string{"PUBLIC_URL": "http://:80/a2a"}, "PUBLIC_URL"},
		{"public url derived from a listen address with no host", map[string]string{"LISTEN_ADDR": ":80"}, "PUBLIC_URL"},
		{"runner url relative", map[string]string{"RUNNER_URL": "127.0.0.1:8081"}, "RUNNER_URL"},
		{"runner url with a path", map[string]string{"RUNNER_URL": "http://127.0.0.1:8081/readyz"}, "RUNNER_URL"},
		{"idle ttl negative", map[string]string{"SESSION_IDLE_TTL": "-1h"}, "SESSION_IDLE_TTL"},
		{"idle ttl not a duration", map[string]string{"SESSION_IDLE_TTL": "90 days"}, "SESSION_IDLE_TTL"},
		{"retention negative", map[string]string{"TASK_RETENTION": "-1s"}, "TASK_RETENTION"},
		{"retention not a duration", map[string]string{"TASK_RETENTION": "30d"}, "TASK_RETENTION"},
		{"no caller check", map[string]string{"AUTH_TOKEN_FILE": ""}, "AUTH_TOKEN_FILE"},
		{"no caller check names oidc", map[string]string{"AUTH_TOKEN_FILE": ""}, "OIDC_ISSUER"},
		{"token file and oidc", oidc(map[string]string{"AUTH_TOKEN_FILE": "/run/secrets/tokens"}), "OIDC_ISSUER"},
		{"oidc without issuer", oidc(map[string]string{"OIDC_ISSUER": ""}), "OIDC_ISSUER"},
		{"oidc without audience", oidc(map[string]string{"OIDC_AUDIENCE": ""}), "OIDC_AUDIENCE"},
		{"oidc without subject", oidc(map[string]string{"OIDC_SUBJECT": ""}), "OIDC_SUBJECT"},
		{"oidc subject alone", map[string]string{"OIDC_SUBJECT": "service"}, "OIDC_ISSUER"},
		{"oidc issuer relative", oidc(map[string]string{"OIDC_ISSUER": "issuer.example.test"}), "OIDC_ISSUER"},
		{"oidc issuer with a query", oidc(map[string]string{"OIDC_ISSUER": "https://issuer.example.test/?x=1"}), "OIDC_ISSUER"},
		{"oidc issuer with userinfo", oidc(map[string]string{"OIDC_ISSUER": "https://u:p@issuer.example.test"}), "OIDC_ISSUER"},
		{"oidc with another header", oidc(map[string]string{"AUTH_HEADER": "X-Kourier-Token"}), "AUTH_HEADER"},
		{"auth disabled not a bool", map[string]string{"AUTH_DISABLED": "nope"}, "AUTH_DISABLED"},
		{"auth disabled on every interface", map[string]string{"AUTH_DISABLED": "true", "AUTH_TOKEN_FILE": "", "LISTEN_ADDR": ":8090"}, "AUTH_DISABLED"},
		{"auth disabled on a pod address", map[string]string{"AUTH_DISABLED": "true", "AUTH_TOKEN_FILE": "", "LISTEN_ADDR": "10.0.0.7:8090"}, "AUTH_DISABLED"},
		{"auth disabled with a token file", map[string]string{"AUTH_DISABLED": "true"}, "AUTH_TOKEN_FILE"},
		{"auth disabled with oidc", oidc(map[string]string{"AUTH_DISABLED": "true"}), "OIDC_ISSUER"},
		{"auth header not a token", map[string]string{"AUTH_HEADER": "X Token"}, "AUTH_HEADER"},
		{"auth header proxy-authorization", map[string]string{"AUTH_HEADER": "Proxy-Authorization"}, "AUTH_HEADER"},
		{"auth header cookie", map[string]string{"AUTH_HEADER": "cookie"}, "AUTH_HEADER"},
		{"auth header x-user-id", map[string]string{"AUTH_HEADER": "x-user-id"}, "AUTH_HEADER"},
		{"auth header x-agent-name", map[string]string{"AUTH_HEADER": "X-AGENT-NAME"}, "AUTH_HEADER"},
		{"auth header x-share-token", map[string]string{"AUTH_HEADER": "X-Share-Token"}, "AUTH_HEADER"},
		{"auth header kagent runtime identity", map[string]string{"AUTH_HEADER": "x-kagent-insecure-runtime-identity"}, "AUTH_HEADER"},
		{"unknown store", map[string]string{"STORE": "mysql"}, "STORE"},
		{"empty sqlite path", map[string]string{"SQLITE_PATH": "   "}, "SQLITE_PATH"},
		{"postgres without dsn", map[string]string{"STORE": "postgres", "ADVERTISE_URL": "http://10.0.0.7:8090"}, "POSTGRES_DSN"},
		{"postgres without advertise url", map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://db/ax"}, "ADVERTISE_URL"},
		{"advertise url relative", pg(map[string]string{"ADVERTISE_URL": "10.0.0.7:8090"}), "ADVERTISE_URL"},
		{"dsn with sqlite", map[string]string{"POSTGRES_DSN": "postgres://db/ax"}, "POSTGRES_DSN"},
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

func TestErrorsNeverShowASecret(t *testing.T) {
	const secret = "fake-password-do-not-print"
	for name, overrides := range map[string]map[string]string{
		"dsn with sqlite":   {"POSTGRES_DSN": "postgres://u:" + secret + "@db/ax"},
		"bad lease":         pg(map[string]string{"POSTGRES_DSN": "postgres://u:" + secret + "@db/ax", "LEASE_DURATION": "1s"}),
		"no advertise":      {"STORE": "postgres", "POSTGRES_DSN": "postgres://u:" + secret + "@db/ax"},
		"bad auth header":   {"AUTH_HEADER": secret + " x"},
		"issuer userinfo":   oidc(map[string]string{"OIDC_ISSUER": "https://u:" + secret + "@issuer.example.test"}),
		"oidc with a token": oidc(map[string]string{"AUTH_HEADER": secret}),
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

// The chart sets ADVERTISE_URL to http://$(POD_IP):8090, which leaves an IPv6 address unbracketed: Parse brackets it.
func TestAdvertiseURLBracketsIPv6(t *testing.T) {
	for in, want := range map[string]string{
		"http://fd00::7:8090":      "http://[fd00::7]:8090",
		"http://::1:8090/":         "http://[::1]:8090/",
		"http://fd00::7":           "http://[fd00::7]",
		"http://[fd00::7]:8090":    "http://[fd00::7]:8090",
		"http://10.0.0.7:8090":     "http://10.0.0.7:8090",
		"https://ax-0.ax:8090":     "https://ax-0.ax:8090",
		"http://2001:db8::1:65535": "http://[2001:db8::1]:65535",
	} {
		c, err := Parse(env(map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://db/ax", "ADVERTISE_URL": in}))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if c.AdvertiseURL != want {
			t.Errorf("ADVERTISE_URL %s = %s, want %s", in, c.AdvertiseURL, want)
		}
	}
}

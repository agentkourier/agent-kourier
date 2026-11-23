package settings

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// The two ways an add-on reads the shared variables today: the token check alone, with a URL's host allowed to be a
// port alone, and the token check or OIDC.
var (
	tokenSpec = Spec{Name: "token", Noun: "adapter", PortOnlyHosts: true}
	oidcSpec  = Spec{Name: "oidc", Noun: "agent", OIDC: true}
)

// specs are both, by name, for the cases that hold for each.
func specs() map[string]Spec { return map[string]Spec{"token": tokenSpec, "oidc": oidcSpec} }

// env is a fake environment: the minimal valid one plus the overrides. An override of "" unsets the variable.
func env(overrides map[string]string) func(string) string {
	base := map[string]string{"AUTH_TOKEN_FILE": "/run/secrets/tokens"}
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
	out := map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://u:fake-password@db/addon", "ADVERTISE_URL": "http://10.0.0.7:8090"}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// parse is Parse of the shared variables alone.
func parse(getenv func(string) string, spec Spec) (Config, error) {
	c, _, err := Parse(getenv, spec)
	return c, err
}

func TestDefaults(t *testing.T) {
	for name, spec := range specs() {
		t.Run(name, func(t *testing.T) {
			c, err := parse(env(nil), spec)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			want := Config{
				ListenAddr: "127.0.0.1:8090", PublicURL: "http://127.0.0.1:8090/a2a",
				AuthTokenFile: "/run/secrets/tokens", AuthHeader: "Authorization",
				Store: StoreSQLite, SQLitePath: name + "-addon.db",
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
			if c.OIDC() {
				t.Error("OIDC = true with the token file")
			}
		})
	}
}

func TestEverySetting(t *testing.T) {
	for name, spec := range specs() {
		t.Run(name, func(t *testing.T) {
			c, err := parse(env(pg(map[string]string{
				"LISTEN_ADDR": ":9000", "PUBLIC_URL": "http://addon.agents.svc:9000/a2a",
				"AUTH_HEADER":    "x-kourier-token",
				"TASK_RETENTION": "0", "SESSION_IDLE_TTL": "0", "SQLITE_PATH": "/data/addon.db",
				"LEASE_DURATION": "30s", "LEASE_RENEW_DEADLINE": "20s", "LEASE_RETRY_PERIOD": "4s",
				"POD_NAME": "addon-0", "LOG_LEVEL": "DEBUG",
			})), spec)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			want := Config{
				ListenAddr: ":9000", PublicURL: "http://addon.agents.svc:9000/a2a",
				AuthTokenFile: "/run/secrets/tokens", AuthHeader: "X-Kourier-Token",
				Store: StorePostgres, SQLitePath: "/data/addon.db", PostgresDSN: "postgres://u:fake-password@db/addon",
				Lease:        Lease{Duration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 4 * time.Second},
				AdvertiseURL: "http://10.0.0.7:8090", PodName: "addon-0", LogLevel: slog.LevelDebug,
			}
			if c != want {
				t.Fatalf("Parse =\n%+v\nwant\n%+v", c, want)
			}
		})
	}
}

// The OIDC route: OIDC, port 80 and a database file of its own.
func TestOIDCRoute(t *testing.T) {
	c, err := parse(env(oidc(map[string]string{
		"LISTEN_ADDR": ":80", "PUBLIC_URL": "https://ax-gateway.example.test/a2a", "SQLITE_PATH": "/workspace/agent-kourier-state/agent.db",
	})), oidcSpec)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.OIDC() || c.AuthTokenFile != "" || c.AuthDisabled {
		t.Fatalf("caller check = token %q disabled %v OIDC %v, want OIDC alone", c.AuthTokenFile, c.AuthDisabled, c.OIDC())
	}
	if c.OIDCIssuer != "https://issuer.example.test/dex" || c.OIDCAudience != "agent-kourier-binding" || c.OIDCSubject != "service" {
		t.Errorf("OIDC = %q %q %q", c.OIDCIssuer, c.OIDCAudience, c.OIDCSubject)
	}
	if c.SQLitePath != "/workspace/agent-kourier-state/agent.db" || c.PublicURL != "https://ax-gateway.example.test/a2a" {
		t.Errorf("sqlite %q public %q", c.SQLitePath, c.PublicURL)
	}
}

// Without Spec.OIDC the OIDC_* variables are not read: they neither make a caller check nor clash with the token file.
func TestOIDCVariablesUnreadWithoutOIDC(t *testing.T) {
	c, err := parse(env(oidc(map[string]string{"AUTH_TOKEN_FILE": "/run/secrets/tokens", "AUTH_HEADER": "X-Kourier-Token"})), tokenSpec)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.OIDC() || c.OIDCIssuer != "" || c.OIDCAudience != "" || c.OIDCSubject != "" {
		t.Fatalf("OIDC = %q %q %q, want none read", c.OIDCIssuer, c.OIDCAudience, c.OIDCSubject)
	}
	if _, err := parse(env(oidc(nil)), tokenSpec); err == nil || !strings.Contains(err.Error(), "AUTH_TOKEN_FILE is required") {
		t.Fatalf("OIDC alone without Spec.OIDC: %v, want AUTH_TOKEN_FILE required", err)
	}
}

// Authorization, the default, may be named, in any case, and with OIDC too.
func TestAuthHeaderAuthorization(t *testing.T) {
	for name, spec := range specs() {
		t.Run(name, func(t *testing.T) {
			c, err := parse(env(map[string]string{"AUTH_HEADER": "authorization"}), spec)
			if err != nil || c.AuthHeader != "Authorization" {
				t.Fatalf("Parse = %q, %v; want Authorization", c.AuthHeader, err)
			}
		})
	}
	if _, err := parse(env(oidc(map[string]string{"AUTH_HEADER": "Authorization"})), oidcSpec); err != nil {
		t.Fatalf("OIDC with AUTH_HEADER=Authorization: %v", err)
	}
}

func TestAuthDisabledOnLoopback(t *testing.T) {
	for name, spec := range specs() {
		for _, addr := range []string{"127.0.0.1:8090", "127.9.9.9:1", "[::1]:8090", "localhost:8090"} {
			t.Run(name+"/"+addr, func(t *testing.T) {
				c, err := parse(env(map[string]string{"AUTH_DISABLED": "true", "AUTH_TOKEN_FILE": "", "LISTEN_ADDR": addr}), spec)
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if !c.AuthDisabled {
					t.Fatal("AuthDisabled = false")
				}
			})
		}
	}
}

// refusedCases are refused by every Spec.
func refusedCases() []struct {
	name      string
	overrides map[string]string
	names     string // the variable the error must name
} {
	return []struct {
		name      string
		overrides map[string]string
		names     string
	}{
		{"listen not host:port", map[string]string{"LISTEN_ADDR": "8090"}, "LISTEN_ADDR"},
		{"public url relative", map[string]string{"PUBLIC_URL": "/a2a"}, "PUBLIC_URL"},
		{"public url scheme", map[string]string{"PUBLIC_URL": "ftp://x/a2a"}, "PUBLIC_URL"},
		{"public url made from a listen address that is no URL", map[string]string{"LISTEN_ADDR": "a b:80"}, "PUBLIC_URL"},
		{"idle ttl negative", map[string]string{"SESSION_IDLE_TTL": "-1h"}, "SESSION_IDLE_TTL"},
		{"idle ttl not a duration", map[string]string{"SESSION_IDLE_TTL": "90 days"}, "SESSION_IDLE_TTL"},
		{"retention negative", map[string]string{"TASK_RETENTION": "-1s"}, "TASK_RETENTION"},
		{"retention not a duration", map[string]string{"TASK_RETENTION": "30d"}, "TASK_RETENTION"},
		{"no caller check", map[string]string{"AUTH_TOKEN_FILE": ""}, "AUTH_TOKEN_FILE"},
		{"auth disabled not a bool", map[string]string{"AUTH_DISABLED": "nope"}, "AUTH_DISABLED"},
		{"auth disabled on every interface", map[string]string{"AUTH_DISABLED": "true", "AUTH_TOKEN_FILE": "", "LISTEN_ADDR": ":8090"}, "AUTH_DISABLED"},
		{"auth disabled on a pod address", map[string]string{"AUTH_DISABLED": "true", "AUTH_TOKEN_FILE": "", "LISTEN_ADDR": "10.0.0.7:8090"}, "AUTH_DISABLED"},
		{"auth disabled with a token file", map[string]string{"AUTH_DISABLED": "true"}, "AUTH_TOKEN_FILE"},
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
		{"postgres without advertise url", map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://db/addon"}, "ADVERTISE_URL"},
		{"advertise url relative", pg(map[string]string{"ADVERTISE_URL": "10.0.0.7:8090"}), "ADVERTISE_URL"},
		{"dsn with sqlite", map[string]string{"POSTGRES_DSN": "postgres://db/addon"}, "POSTGRES_DSN"},
		{"lease with sqlite", map[string]string{"LEASE_DURATION": "15s"}, "LEASE_DURATION"},
		{"renew deadline with sqlite", map[string]string{"LEASE_RENEW_DEADLINE": "10s"}, "LEASE_RENEW_DEADLINE"},
		{"retry period with sqlite", map[string]string{"LEASE_RETRY_PERIOD": "2s"}, "LEASE_RETRY_PERIOD"},
		{"lease not above renew", pg(map[string]string{"LEASE_DURATION": "10s"}), "LEASE_DURATION"},
		{"renew not above 1.2 retry", pg(map[string]string{"LEASE_RETRY_PERIOD": "9s"}), "LEASE_RENEW_DEADLINE"},
		{"retry zero", pg(map[string]string{"LEASE_RETRY_PERIOD": "0"}), "LEASE_RETRY_PERIOD"},
		{"lease negative", pg(map[string]string{"LEASE_DURATION": "-15s"}), "LEASE_DURATION"},
		{"lease not a duration", pg(map[string]string{"LEASE_DURATION": "15"}), "LEASE_DURATION"},
		{"log level", map[string]string{"LOG_LEVEL": "chatty"}, "LOG_LEVEL"},
	}
}

func TestRefused(t *testing.T) {
	for name, spec := range specs() {
		for _, tc := range refusedCases() {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				_, err := parse(env(tc.overrides), spec)
				if err == nil {
					t.Fatal("Parse accepted it")
				}
				if !strings.Contains(err.Error(), tc.names) {
					t.Errorf("error %q does not name %s", err, tc.names)
				}
			})
		}
	}
}

func TestOIDCRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		names     string // the variable the error must name
	}{
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
		{"auth disabled with oidc", oidc(map[string]string{"AUTH_DISABLED": "true"}), "OIDC_ISSUER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(env(tc.overrides), oidcSpec)
			if err == nil {
				t.Fatal("Parse accepted it")
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("error %q does not name %s", err, tc.names)
			}
		})
	}
}

// A URL whose host is a port alone is refused without Spec.PortOnlyHosts, and passes with it.
func TestPortOnlyHosts(t *testing.T) {
	for _, overrides := range []map[string]string{
		{"PUBLIC_URL": "http://:80/a2a"},
		{"LISTEN_ADDR": ":80"},
		pg(map[string]string{"ADVERTISE_URL": "http://:8090"}),
	} {
		if _, err := parse(env(overrides), oidcSpec); err == nil {
			t.Errorf("%v: accepted without PortOnlyHosts", overrides)
		}
		if _, err := parse(env(overrides), tokenSpec); err != nil {
			t.Errorf("%v: refused with PortOnlyHosts: %v", overrides, err)
		}
	}
}

// The errors whose text depends on the Spec, and a few more, word for word.
func TestErrorText(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spec      Spec
		overrides map[string]string
		want      string
	}{
		{"auth disabled, adapter", tokenSpec, map[string]string{"AUTH_DISABLED": "true", "AUTH_TOKEN_FILE": "", "LISTEN_ADDR": ":8090"},
			`AUTH_DISABLED=true needs a loopback LISTEN_ADDR, and ":8090" is not one: anyone who reaches the adapter could run the agent`},
		{"auth disabled, agent", oidcSpec, map[string]string{"AUTH_DISABLED": "true", "AUTH_TOKEN_FILE": "", "LISTEN_ADDR": ":8090"},
			`PUBLIC_URL is unset and the one made from LISTEN_ADDR, "http://:8090/a2a", is not an http or https URL with a host: set PUBLIC_URL to the URL callers reach the A2A endpoint at` + "\n" +
				`AUTH_DISABLED=true needs a loopback LISTEN_ADDR, and ":8090" is not one: anyone who reaches the agent could run it`},
		{"no caller check, token", tokenSpec, map[string]string{"AUTH_TOKEN_FILE": ""},
			`AUTH_TOKEN_FILE is required (or AUTH_DISABLED=true on a loopback LISTEN_ADDR)`},
		{"no caller check, oidc", oidcSpec, map[string]string{"AUTH_TOKEN_FILE": ""},
			`a caller check is required: AUTH_TOKEN_FILE, or OIDC_ISSUER, OIDC_AUDIENCE and OIDC_SUBJECT (or AUTH_DISABLED=true on a loopback LISTEN_ADDR)`},
		{"public url made from listen, port-only allowed", tokenSpec, map[string]string{"LISTEN_ADDR": "a b:80"},
			`PUBLIC_URL "http://a b:80/a2a" is not an http or https URL with a host`},
		{"public url made from listen", oidcSpec, map[string]string{"LISTEN_ADDR": "a b:80"},
			`PUBLIC_URL is unset and the one made from LISTEN_ADDR, "http://a b:80/a2a", is not an http or https URL with a host: set PUBLIC_URL to the URL callers reach the A2A endpoint at`},
		{"public url set", oidcSpec, map[string]string{"PUBLIC_URL": "http://:80/a2a"},
			`PUBLIC_URL "http://:80/a2a" is not an http or https URL with a host`},
		{"advertise url", oidcSpec, pg(map[string]string{"ADVERTISE_URL": "http://:8090"}),
			`ADVERTISE_URL "http://:8090" is not an http or https URL with a host`},
		{"lease order", tokenSpec, pg(map[string]string{"LEASE_RETRY_PERIOD": "9s"}),
			`LEASE_RENEW_DEADLINE (10s) must be greater than LEASE_RETRY_PERIOD (9s) times 1.2`},
		{"several", tokenSpec, map[string]string{"TASK_RETENTION": "y", "SESSION_IDLE_TTL": "-1h", "STORE": "mysql", "LOG_LEVEL": "loud"},
			`TASK_RETENTION "y" is not a Go duration such as 90s or 720h` + "\n" +
				`SESSION_IDLE_TTL "-1h" is not a duration of zero or more` + "\n" +
				`STORE "mysql" is not sqlite or postgres` + "\n" +
				`LOG_LEVEL "loud" is not debug, info, warn or error`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(env(tc.overrides), tc.spec)
			if err == nil {
				t.Fatal("Parse accepted it")
			}
			if err.Error() != tc.want {
				t.Errorf("error =\n%s\nwant\n%s", err, tc.want)
			}
		})
	}
}

func TestErrorsNeverShowASecret(t *testing.T) {
	const secret = "fake-password-do-not-print"
	for name, overrides := range map[string]map[string]string{
		"dsn with sqlite":   {"POSTGRES_DSN": "postgres://u:" + secret + "@db/addon"},
		"bad lease":         pg(map[string]string{"POSTGRES_DSN": "postgres://u:" + secret + "@db/addon", "LEASE_DURATION": "1s"}),
		"no advertise":      {"STORE": "postgres", "POSTGRES_DSN": "postgres://u:" + secret + "@db/addon"},
		"bad auth header":   {"AUTH_HEADER": secret + " x"},
		"issuer userinfo":   oidc(map[string]string{"OIDC_ISSUER": "https://u:" + secret + "@issuer.example.test"}),
		"oidc with a token": oidc(map[string]string{"AUTH_HEADER": secret}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parse(env(overrides), oidcSpec)
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
		"http://fd00::7:8090":        "http://[fd00::7]:8090",
		"http://::1:8090/":           "http://[::1]:8090/",
		"http://fd00::7":             "http://[fd00::7]",
		"http://[fd00::7]:8090":      "http://[fd00::7]:8090",
		"http://10.0.0.7:8090":       "http://10.0.0.7:8090",
		"https://addon-0.addon:8090": "https://addon-0.addon:8090",
		"http://2001:db8::1:65535":   "http://[2001:db8::1]:65535",
	} {
		for name, spec := range specs() {
			c, err := parse(env(map[string]string{"STORE": "postgres", "POSTGRES_DSN": "postgres://db/addon", "ADVERTISE_URL": in}), spec)
			if err != nil {
				t.Errorf("%s %s: %v", name, in, err)
				continue
			}
			if c.AdvertiseURL != want {
				t.Errorf("%s: ADVERTISE_URL %s = %s, want %s", name, in, c.AdvertiseURL, want)
			}
		}
	}
}

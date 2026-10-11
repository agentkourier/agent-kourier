package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/broker"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
)

// env is a fake environment.
type env map[string]string

func (e env) get(k string) string { return e[k] }

func TestRunVersionAndUsage(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr bool
	}{
		{name: "version flag prints version", args: []string{"--version"}, wantCode: 0, wantStdout: "v1.2.3\n"},
		{name: "unknown flag fails", args: []string{"--nope"}, wantCode: exitUsage, wantStderr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, "v1.2.3", &stdout, &stderr, env{}.get)
			if code != tt.wantCode || stdout.String() != tt.wantStdout || (stderr.Len() > 0) != tt.wantStderr {
				t.Errorf("run = %d, stdout %q, stderr %q; want %d, %q, stderr %t", code, stdout.String(), stderr.String(), tt.wantCode, tt.wantStdout, tt.wantStderr)
			}
		})
	}
}

// Every outcome of the broker has its own exit code, so a supervisor, or a person reading `kubectl
// describe pod`, can tell a bad token from missing RBAC from a bad config without reading the log.
func TestExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"a clean stop", nil, exitOK},
		{"Slack rejected the tokens", fmt.Errorf("run: %w", broker.ErrSlackAuth), exitSlackAuth},
		{"the config or the wiring is wrong", fmt.Errorf("run: %w", broker.ErrWiring), exitUsage},
		{"RBAC denied access to Secrets", fmt.Errorf("bootstrap: %w", k8ssecrets.ErrAccessDenied), exitRBAC},
		{"any other failure", errors.New("store: disk full"), exitFailure},
	} {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("%s: exitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
	codes := map[int]bool{exitOK: true, exitSlackAuth: true, exitUsage: true, exitRBAC: true, exitFailure: true}
	if len(codes) != 5 || exitOK != 0 || exitSlackAuth != 1 || exitUsage != 2 || exitRBAC != 3 {
		t.Errorf("the exit codes are %v, want 0 clean, 1 Slack auth, 2 wiring or usage, 3 RBAC, and one more for the rest", codes)
	}
}

func TestEnvConfigDefaultsAndOverrides(t *testing.T) {
	d, err := loadEnv(env{}.get)
	if err != nil {
		t.Fatal(err)
	}
	if d.configDir != "/etc/agent-kourier" || d.dbPath != "/data/agent-kourier.db" || d.listen != ":8080" || d.syncTimeout != 30*time.Second || d.klogV != 0 || d.logLevel != slog.LevelInfo {
		t.Errorf("defaults = %+v", d)
	}
	o, err := loadEnv(env{
		"AGENTKOURIER_CONFIG_DIR": "/c", "AGENTKOURIER_DB_PATH": "/d/x.db", "AGENTKOURIER_LISTEN": "127.0.0.1:9", "AGENTKOURIER_SYNC_TIMEOUT": "5s", "AGENTKOURIER_KLOG_V": "3", "AGENTKOURIER_LOG_LEVEL": "debug",
	}.get)
	if err != nil {
		t.Fatal(err)
	}
	if o.configDir != "/c" || o.dbPath != "/d/x.db" || o.listen != "127.0.0.1:9" || o.syncTimeout != 5*time.Second || o.klogV != 3 || o.logLevel != slog.LevelDebug {
		t.Errorf("overrides = %+v", o)
	}
}

// AGENTKOURIER_AUDIT_RETENTION is "keep" until an operator sets a duration: unset or "keep" mean the audit log is never
// purged, and a duration is read as written, not less than a day. "0" is no way to say keep, and is refused with the
// rest: it would read as either.
func TestEnvConfigAuditRetention(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 0, "keep": 0, "KEEP": 0, "2160h": 2160 * time.Hour, "24h": 24 * time.Hour, "8760h30m": 8760*time.Hour + 30*time.Minute} {
		c, err := loadEnv(env{"AGENTKOURIER_AUDIT_RETENTION": value}.get)
		if err != nil || c.auditRetention != want {
			t.Errorf("AGENTKOURIER_AUDIT_RETENTION=%q: retention %v, %v; want %v", value, c.auditRetention, err, want)
		}
	}
	for _, value := range []string{"0", "soon", "90d", "-24h", "23h59m", "1h"} {
		_, err := loadEnv(env{"AGENTKOURIER_AUDIT_RETENTION": value}.get)
		if !errors.Is(err, broker.ErrWiring) || !strings.Contains(err.Error(), "AGENTKOURIER_AUDIT_RETENTION") {
			t.Errorf("AGENTKOURIER_AUDIT_RETENTION=%q: loadEnv = %v, want an ErrWiring naming it", value, err)
		}
	}
}

// A value that cannot be read is a usage error that names the variable and, for a number or a
// duration, says what it expects: nothing is guessed.
func TestEnvConfigRefusesWhatItCannotRead(t *testing.T) {
	for name, tc := range map[string]struct{ key, value string }{
		"sync timeout": {"AGENTKOURIER_SYNC_TIMEOUT", "soon"}, "negative timeout": {"AGENTKOURIER_SYNC_TIMEOUT", "-5s"},
		"klog v": {"AGENTKOURIER_KLOG_V", "loud"}, "log level": {"AGENTKOURIER_LOG_LEVEL", "chatty"},
	} {
		_, err := loadEnv(env{tc.key: tc.value}.get)
		if !errors.Is(err, broker.ErrWiring) || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s: loadEnv = %v, want an ErrWiring naming %s", name, err, tc.key)
		}
	}
}

// Started with a good environment but no cluster, Agent Kourier exits 2 and says that it runs only in one:
// there is no kubeconfig to fall back on, and in either build.
func TestStartSaysItRunsOnlyInACluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Cleanup(restoreDefaultLogger())
	var stdout, stderr bytes.Buffer

	code := run(nil, "dev", &stdout, &stderr, env{"AGENTKOURIER_LISTEN": "127.0.0.1:0"}.get)

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (stderr %q)", code, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "only in a cluster") {
		t.Errorf("stderr %q should say Agent Kourier runs only in a cluster", stderr.String())
	}
}

// Flag parsing stops at the first argument that is no flag, so a mistyped subcommand must be refused and not start the
// broker, which in a pod would be a second one. The argument is not echoed: it may be a token typed in its place.
func TestRunRefusesAnUnknownCommand(t *testing.T) {
	for _, args := range [][]string{{"dryrun"}, {"dry_run", "--config", "/c"}, {"dry-run-all"}, {"--version", "extra"}, {"xoxb-CANARY-0123456789"}} {
		var stdout, stderr bytes.Buffer
		code := run(args, "dev", &stdout, &stderr, env{"AGENTKOURIER_LISTEN": "127.0.0.1:0"}.get)
		if code != exitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "unknown command") || !strings.Contains(stderr.String(), "dry-run") {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q; want %d and an unknown command that names dry-run", args, code, stdout.String(), stderr.String(), exitUsage)
		}
		if strings.Contains(stderr.String(), "CANARY") || strings.Contains(stderr.String(), args[0]+"\n") {
			t.Errorf("run(%q) echoed the argument: %q", args, stderr.String())
		}
	}
}

// The store is SQLite unless the environment says Postgres, and Postgres takes the reference to the Secret that holds its DSN.
func TestEnvConfigChoosesTheStore(t *testing.T) {
	ref := broker.SecretKey{Namespace: "agent-kourier", Name: "agent-kourier-pg", Key: "dsn"}
	lease := env{"AGENTKOURIER_LEASE_NAMESPACE": "agent-kourier", "AGENTKOURIER_LEASE_NAME": "agent-kourier-leader", "AGENTKOURIER_POD_NAME": "agent-kourier-0"}
	for name, tc := range map[string]struct {
		env  env
		want broker.StoreConfig
	}{
		"default is sqlite":           {env{}, broker.StoreConfig{Kind: broker.StoreSQLite}},
		"sqlite, named":               {env{"AGENTKOURIER_STORE": "sqlite"}, broker.StoreConfig{Kind: broker.StoreSQLite}},
		"the case is ignored":         {env{"AGENTKOURIER_STORE": "SQLite"}, broker.StoreConfig{Kind: broker.StoreSQLite}},
		"postgres":                    {env{"AGENTKOURIER_STORE": "postgres", "AGENTKOURIER_POSTGRES_DSN_SECRET": "agent-kourier/agent-kourier-pg/dsn"}, broker.StoreConfig{Kind: broker.StorePostgres, PostgresDSN: ref}},
		"postgres, the Agent Kourier": {env{"AGENTKOURIER_STORE": "postgres", "AGENTKOURIER_POSTGRES_DSN_SECRET": "agent-kourier/agent-kourier-pg/dsn", "AGENTKOURIER_POSTGRES_TIMEOUTS": "agent-kourier"}, broker.StoreConfig{Kind: broker.StorePostgres, PostgresDSN: ref}},
		"postgres, the role":          {env{"AGENTKOURIER_STORE": "postgres", "AGENTKOURIER_POSTGRES_DSN_SECRET": "agent-kourier/agent-kourier-pg/dsn", "AGENTKOURIER_POSTGRES_TIMEOUTS": "role"}, broker.StoreConfig{Kind: broker.StorePostgres, PostgresDSN: ref, PostgresRoleTimeouts: true}},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.want.Kind == broker.StorePostgres {
				tc.env = maps.Clone(tc.env)
				maps.Copy(tc.env, lease)
			}
			got, err := loadEnv(tc.env.get)
			if err != nil || got.store != tc.want {
				t.Errorf("store = %+v, %v; want %+v", got.store, err, tc.want)
			}
		})
	}
}

// With Postgres the election is on: the Lease and the pod are required, the timings are optional, and the pod's name is
// the identity (run makes it unique to the process). With SQLite there is no election and no variable asks for one.
func TestEnvConfigReadsTheElection(t *testing.T) {
	pg := env{"AGENTKOURIER_STORE": "postgres", "AGENTKOURIER_POSTGRES_DSN_SECRET": "agent-kourier/agent-kourier-pg/dsn", "AGENTKOURIER_POD_NAME": "agent-kourier-7d9f", "AGENTKOURIER_LEASE_NAMESPACE": "agent-kourier", "AGENTKOURIER_LEASE_NAME": "agent-kourier-leader"}
	with := func(kv ...string) env {
		e := maps.Clone(pg)
		for i := 0; i < len(kv); i += 2 {
			e[kv[i]] = kv[i+1]
		}
		return e
	}
	got, err := loadEnv(pg.get)
	want := broker.ElectionConfig{Namespace: "agent-kourier", Name: "agent-kourier-leader", Identity: "agent-kourier-7d9f"}
	if err != nil || got.election != want {
		t.Errorf("election = %+v, %v; want %+v with the timings left to the default", got.election, err, want)
	}
	got, err = loadEnv(with("AGENTKOURIER_LEASE_DURATION", "20s", "AGENTKOURIER_LEASE_RENEW_DEADLINE", "12s", "AGENTKOURIER_LEASE_RETRY_PERIOD", "3s").get)
	if err != nil || got.election.LeaseDuration != 20*time.Second || got.election.RenewDeadline != 12*time.Second || got.election.RetryPeriod != 3*time.Second {
		t.Errorf("timings = %+v, %v; want 20s, 12s and 3s", got.election, err)
	}
	if got, err = loadEnv(env{}.get); err != nil || got.election != (broker.ElectionConfig{}) {
		t.Errorf("SQLite's election = %+v, %v; want none", got.election, err)
	}

	for name, tc := range map[string]struct {
		env env
		key string
	}{
		"no Lease namespace":       {with("AGENTKOURIER_LEASE_NAMESPACE", ""), "AGENTKOURIER_LEASE_NAMESPACE"},
		"no Lease name":            {with("AGENTKOURIER_LEASE_NAME", ""), "AGENTKOURIER_LEASE_NAME"},
		"no pod name":              {with("AGENTKOURIER_POD_NAME", ""), "AGENTKOURIER_POD_NAME"},
		"a timing that is no time": {with("AGENTKOURIER_LEASE_DURATION", "soon"), "AGENTKOURIER_LEASE_DURATION"},
		"a timing of zero":         {with("AGENTKOURIER_LEASE_RENEW_DEADLINE", "0s"), "AGENTKOURIER_LEASE_RENEW_DEADLINE"},
		"a negative timing":        {with("AGENTKOURIER_LEASE_RETRY_PERIOD", "-1s"), "AGENTKOURIER_LEASE_RETRY_PERIOD"},
		"a lease with sqlite":      {env{"AGENTKOURIER_LEASE_NAME": "x"}, "AGENTKOURIER_LEASE_NAME"},
		"a timing with sqlite":     {env{"AGENTKOURIER_LEASE_DURATION": "15s"}, "AGENTKOURIER_LEASE_DURATION"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadEnv(tc.env.get); !errors.Is(err, broker.ErrWiring) || !strings.Contains(err.Error(), tc.key) {
				t.Errorf("loadEnv = %v; want an ErrWiring naming %s", err, tc.key)
			}
		})
	}
}

func TestUniqueIdentityIsThePodNameAndSomethingOfTheProcess(t *testing.T) {
	a, errA := uniqueIdentity("agent-kourier-7d9f")
	b, errB := uniqueIdentity("agent-kourier-7d9f")
	if errA != nil || errB != nil || a == b || !strings.HasPrefix(a, "agent-kourier-7d9f_") || len(a) != len("agent-kourier-7d9f_")+8 {
		t.Errorf("identities %q and %q (%v, %v); want the pod's name and 8 hex digits that differ between processes", a, b, errA, errB)
	}
}

// What cannot be read, or would be ignored, is a usage error that names the variable. None echoes the value: a person who
// puts the DSN where its reference belongs must not see it printed.
func TestEnvConfigRefusesAStoreItCannotUse(t *testing.T) {
	const dsn = "postgres://agentkourier:fake-pg-password-7Qz@db.example.test:5432/agentkourier"
	ref := "agent-kourier/agent-kourier-pg/dsn"
	for name, tc := range map[string]struct {
		env env
		key string
	}{
		"unknown store":               {env{"AGENTKOURIER_STORE": "mysql"}, "AGENTKOURIER_STORE"},
		"postgres without a Secret":   {env{"AGENTKOURIER_STORE": "postgres"}, "AGENTKOURIER_POSTGRES_DSN_SECRET"},
		"the DSN in place of a ref":   {env{"AGENTKOURIER_STORE": "postgres", "AGENTKOURIER_POSTGRES_DSN_SECRET": dsn}, "AGENTKOURIER_POSTGRES_DSN_SECRET"},
		"no key":                      {env{"AGENTKOURIER_STORE": "postgres", "AGENTKOURIER_POSTGRES_DSN_SECRET": "agent-kourier/agent-kourier-pg"}, "AGENTKOURIER_POSTGRES_DSN_SECRET"},
		"an empty part":               {env{"AGENTKOURIER_STORE": "postgres", "AGENTKOURIER_POSTGRES_DSN_SECRET": "agent-kourier//dsn"}, "AGENTKOURIER_POSTGRES_DSN_SECRET"},
		"too many parts":              {env{"AGENTKOURIER_STORE": "postgres", "AGENTKOURIER_POSTGRES_DSN_SECRET": "a/b/c/d"}, "AGENTKOURIER_POSTGRES_DSN_SECRET"},
		"a Secret with sqlite":        {env{"AGENTKOURIER_POSTGRES_DSN_SECRET": ref}, "AGENTKOURIER_POSTGRES_DSN_SECRET"},
		"a Secret with sqlite, named": {env{"AGENTKOURIER_STORE": "sqlite", "AGENTKOURIER_POSTGRES_DSN_SECRET": ref}, "AGENTKOURIER_POSTGRES_DSN_SECRET"},
		"unknown timeouts":            {env{"AGENTKOURIER_STORE": "postgres", "AGENTKOURIER_POSTGRES_DSN_SECRET": ref, "AGENTKOURIER_POSTGRES_TIMEOUTS": "never"}, "AGENTKOURIER_POSTGRES_TIMEOUTS"},
		"timeouts with sqlite":        {env{"AGENTKOURIER_POSTGRES_TIMEOUTS": "role"}, "AGENTKOURIER_POSTGRES_TIMEOUTS"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadEnv(tc.env.get)
			if !errors.Is(err, broker.ErrWiring) || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("loadEnv = %v, want an ErrWiring naming %s", err, tc.key)
			}
			for _, v := range tc.env {
				if strings.Contains(v, "fake-pg-password") && strings.Contains(err.Error(), "fake-pg-password") {
					t.Errorf("the error echoes the value: %v", err)
				}
			}
		})
	}
}

// restoreDefaultLogger returns what puts slog's default logger back as it is now: run sets it, for the life of the process.
func restoreDefaultLogger() func() {
	prev := slog.Default()
	return func() { slog.SetDefault(prev) }
}

// A component that falls back to slog.Default() logs through Agent Kourier's redacting handler once run has built it, and
// the error run exits with is redacted and capped as a log line's is.
func TestRunMakesTheRedactingLoggerTheDefault(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Cleanup(restoreDefaultLogger())
	var stdout, stderr bytes.Buffer
	run(nil, "dev", &stdout, &stderr, env{"AGENTKOURIER_LISTEN": "127.0.0.1:0"}.get)
	stderr.Reset()

	slog.Default().Warn("a fallback logger", "error", errors.New("dial: Authorization: Bearer FAKE.bearer.credential password=hunter2-FAKE"))

	if out := stderr.String(); strings.Contains(out, "hunter2") || strings.Contains(out, "FAKE.bearer") || !strings.Contains(out, `"error":"dial: Authorization: [redacted]`) {
		t.Errorf("the default logger wrote %q; want a JSON line with the error redacted", out)
	}
}

func TestFailPrintsARedactedCappedError(t *testing.T) {
	var stderr bytes.Buffer
	err := fmt.Errorf("%w: connect: %s password=hunter2-FAKE xoxb-1111-2222-FAKEFAKEFAKE TAIL-CANARY", broker.ErrWiring, strings.Repeat("a long agent message. ", 40))
	if code := fail(&stderr, err); code != exitUsage {
		t.Errorf("exit code = %d; want %d: the code follows the error, not its text", code, exitUsage)
	}
	out := stderr.String()
	if strings.Contains(out, "hunter2") || strings.Contains(out, "xoxb-") || strings.Contains(out, "TAIL-CANARY") || !strings.HasPrefix(out, "agent-kourier: ") || len(out) > 400 {
		t.Errorf("stderr = %q; want one line, redacted and capped", out)
	}
}

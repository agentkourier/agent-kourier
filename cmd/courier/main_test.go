package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
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
	if d.configDir != "/etc/courier" || d.dbPath != "/data/courier.db" || d.listen != ":8080" || d.syncTimeout != 30*time.Second || d.klogV != 0 || d.logLevel != slog.LevelInfo {
		t.Errorf("defaults = %+v", d)
	}
	o, err := loadEnv(env{
		"COURIER_CONFIG_DIR": "/c", "COURIER_DB_PATH": "/d/x.db", "COURIER_LISTEN": "127.0.0.1:9", "COURIER_SYNC_TIMEOUT": "5s", "COURIER_KLOG_V": "3", "COURIER_LOG_LEVEL": "debug",
	}.get)
	if err != nil {
		t.Fatal(err)
	}
	if o.configDir != "/c" || o.dbPath != "/d/x.db" || o.listen != "127.0.0.1:9" || o.syncTimeout != 5*time.Second || o.klogV != 3 || o.logLevel != slog.LevelDebug {
		t.Errorf("overrides = %+v", o)
	}
}

// A value that cannot be read is a usage error that names the variable and, for a number or a
// duration, says what it expects: nothing is guessed.
func TestEnvConfigRefusesWhatItCannotRead(t *testing.T) {
	for name, tc := range map[string]struct{ key, value string }{
		"sync timeout": {"COURIER_SYNC_TIMEOUT", "soon"}, "negative timeout": {"COURIER_SYNC_TIMEOUT", "-5s"},
		"klog v": {"COURIER_KLOG_V", "loud"}, "log level": {"COURIER_LOG_LEVEL", "chatty"},
	} {
		_, err := loadEnv(env{tc.key: tc.value}.get)
		if !errors.Is(err, broker.ErrWiring) || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s: loadEnv = %v, want an ErrWiring naming %s", name, err, tc.key)
		}
	}
}

// Started with a good environment but no cluster, courier exits 2 and says that it runs only in one:
// there is no kubeconfig to fall back on, and in either build.
func TestStartSaysItRunsOnlyInACluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	var stdout, stderr bytes.Buffer

	code := run(nil, "dev", &stdout, &stderr, env{"COURIER_LISTEN": "127.0.0.1:0"}.get)

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (stderr %q)", code, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "only in a cluster") {
		t.Errorf("stderr %q should say courier runs only in a cluster", stderr.String())
	}
}

// Flag parsing stops at the first argument that is no flag, so a mistyped subcommand must be refused and not start the
// broker, which in a pod would be a second one. The argument is not echoed: it may be a token typed in its place.
func TestRunRefusesAnUnknownCommand(t *testing.T) {
	for _, args := range [][]string{{"dryrun"}, {"dry_run", "--config", "/c"}, {"dry-run-all"}, {"--version", "extra"}, {"xoxb-CANARY-0123456789"}} {
		var stdout, stderr bytes.Buffer
		code := run(args, "dev", &stdout, &stderr, env{"COURIER_LISTEN": "127.0.0.1:0"}.get)
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
	ref := broker.SecretKey{Namespace: "courier", Name: "courier-pg", Key: "dsn"}
	for name, tc := range map[string]struct {
		env  env
		want broker.StoreConfig
	}{
		"default is sqlite":     {env{}, broker.StoreConfig{Kind: broker.StoreSQLite}},
		"sqlite, named":         {env{"COURIER_STORE": "sqlite"}, broker.StoreConfig{Kind: broker.StoreSQLite}},
		"the case is ignored":   {env{"COURIER_STORE": "SQLite"}, broker.StoreConfig{Kind: broker.StoreSQLite}},
		"postgres":              {env{"COURIER_STORE": "postgres", "COURIER_POSTGRES_DSN_SECRET": "courier/courier-pg/dsn"}, broker.StoreConfig{Kind: broker.StorePostgres, PostgresDSN: ref}},
		"postgres, the courier": {env{"COURIER_STORE": "postgres", "COURIER_POSTGRES_DSN_SECRET": "courier/courier-pg/dsn", "COURIER_POSTGRES_TIMEOUTS": "courier"}, broker.StoreConfig{Kind: broker.StorePostgres, PostgresDSN: ref}},
		"postgres, the role":    {env{"COURIER_STORE": "postgres", "COURIER_POSTGRES_DSN_SECRET": "courier/courier-pg/dsn", "COURIER_POSTGRES_TIMEOUTS": "role"}, broker.StoreConfig{Kind: broker.StorePostgres, PostgresDSN: ref, PostgresRoleTimeouts: true}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := loadEnv(tc.env.get)
			if err != nil || got.store != tc.want {
				t.Errorf("store = %+v, %v; want %+v", got.store, err, tc.want)
			}
		})
	}
}

// What cannot be read, or would be ignored, is a usage error that names the variable. None echoes the value: a person who
// puts the DSN where its reference belongs must not see it printed.
func TestEnvConfigRefusesAStoreItCannotUse(t *testing.T) {
	const dsn = "postgres://courier:fake-pg-password-7Qz@db.example.test:5432/courier"
	ref := "courier/courier-pg/dsn"
	for name, tc := range map[string]struct {
		env env
		key string
	}{
		"unknown store":               {env{"COURIER_STORE": "mysql"}, "COURIER_STORE"},
		"postgres without a Secret":   {env{"COURIER_STORE": "postgres"}, "COURIER_POSTGRES_DSN_SECRET"},
		"the DSN in place of a ref":   {env{"COURIER_STORE": "postgres", "COURIER_POSTGRES_DSN_SECRET": dsn}, "COURIER_POSTGRES_DSN_SECRET"},
		"no key":                      {env{"COURIER_STORE": "postgres", "COURIER_POSTGRES_DSN_SECRET": "courier/courier-pg"}, "COURIER_POSTGRES_DSN_SECRET"},
		"an empty part":               {env{"COURIER_STORE": "postgres", "COURIER_POSTGRES_DSN_SECRET": "courier//dsn"}, "COURIER_POSTGRES_DSN_SECRET"},
		"too many parts":              {env{"COURIER_STORE": "postgres", "COURIER_POSTGRES_DSN_SECRET": "a/b/c/d"}, "COURIER_POSTGRES_DSN_SECRET"},
		"a Secret with sqlite":        {env{"COURIER_POSTGRES_DSN_SECRET": ref}, "COURIER_POSTGRES_DSN_SECRET"},
		"a Secret with sqlite, named": {env{"COURIER_STORE": "sqlite", "COURIER_POSTGRES_DSN_SECRET": ref}, "COURIER_POSTGRES_DSN_SECRET"},
		"unknown timeouts":            {env{"COURIER_STORE": "postgres", "COURIER_POSTGRES_DSN_SECRET": ref, "COURIER_POSTGRES_TIMEOUTS": "never"}, "COURIER_POSTGRES_TIMEOUTS"},
		"timeouts with sqlite":        {env{"COURIER_POSTGRES_TIMEOUTS": "role"}, "COURIER_POSTGRES_TIMEOUTS"},
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

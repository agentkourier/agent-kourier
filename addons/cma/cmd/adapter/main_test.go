package main

import (
	"bytes"
	"database/sql"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/addons/cma/store"
)

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version"}, &out, &errOut, func(string) string { return "" }); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != version || got != "dev" {
		t.Fatalf("--version printed %q, want the version set at build time (dev in a test)", got)
	}
}

func TestUsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"an argument":      {"fake-token-typed-by-mistake"},
		"an unknown flag":  {"--nope"},
		"no configuration": nil,
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(args, &out, &errOut, func(string) string { return "" }); code != exitUsage {
				t.Fatalf("exit %d, want %d", code, exitUsage)
			}
			if strings.Contains(errOut.String(), "fake-token") {
				t.Fatal("the argument was echoed")
			}
		})
	}
}

// A bad OTEL setting stops the adapter at start, like a bad setting of its own, and its value is not shown.
func TestBadTraceSettingIsAUsageError(t *testing.T) {
	env := map[string]string{"CMA_AGENT_ID": "agent_test", "CMA_ENVIRONMENT_ID": "env_test", "AUTH_DISABLED": "true",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:canary@collector"}
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut, func(k string) string { return env[k] }); code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
	if strings.Contains(errOut.String(), "canary") {
		t.Fatalf("stderr shows the value: %s", errOut.String())
	}
}

// A store an earlier build of the adapter wrote stops the adapter at start: it logs why and exits 1, and the store
// keeps the layout it had.
func TestAnEarlierStoreStopsTheStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cma.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(t.Context(), `CREATE TABLE context_sessions (context_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, last_active INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("ANTHROPIC_API_KEY", "fake-api-key-for-tests")
	env := map[string]string{"CMA_AGENT_ID": "agent_test", "CMA_ENVIRONMENT_ID": "env_test", "AUTH_DISABLED": "true",
		"LISTEN_ADDR": freeAddr(t), "SQLITE_PATH": path}
	var errOut bytes.Buffer
	if code := run(nil, io.Discard, &errOut, func(k string) string { return env[k] }); code != exitFailure {
		t.Fatalf("exit %d, want %d; stderr %s", code, exitFailure, errOut.String())
	}
	// The log line cuts a long error (telemetry.MaxLogErrorBytes), and a test's directory is long: its reason leads.
	if reason, _, _ := strings.Cut(store.ErrEarlierLayout.Error(), ":"); !strings.Contains(errOut.String(), reason) {
		t.Fatalf("stderr does not say why: %s", errOut.String())
	}
	var tables int
	if err := raw.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("tables = %d, %v; want the earlier layout's one alone", tables, err)
	}
}

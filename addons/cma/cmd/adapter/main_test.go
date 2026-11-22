package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
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

func TestFailStopExitsOnceWithItsCode(t *testing.T) {
	var codes []int
	var log bytes.Buffer
	f := &failStop{log: newLogger(&log), exit: func(code int) { codes = append(codes, code) }}
	f.now("deposed")
	f.now("again")
	if len(codes) != 1 || codes[0] != 5 {
		t.Fatalf("exit codes %v, want one 5", codes)
	}
}

func newLogger(w *bytes.Buffer) *slog.Logger { return slog.New(slog.NewJSONHandler(w, nil)) }

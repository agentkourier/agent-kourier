package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionFlagPrintsTheBuildVersion(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "1.2.3"
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr, func(string) string { return "" }); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "1.2.3" {
		t.Fatalf("--version printed %q, want 1.2.3", got)
	}
}

func TestVersionDefaultsToDev(t *testing.T) {
	if version != "dev" {
		t.Fatalf("version = %q, want dev unless -X main.version sets it", version)
	}
}

func TestUsageErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		env  map[string]string
		says string
	}{
		"an argument":       {[]string{"fake-token-typed-by-mistake"}, nil, "takes no arguments"},
		"an unknown flag":   {[]string{"--nope"}, nil, "nope"},
		"a missing setting": {nil, map[string]string{"AUTH_TOKEN_FILE": "/t"}, "MODEL_REGION"},
		"STATE_DIR":         {nil, map[string]string{"MODEL_REGION": "r", "MODEL_NAME": "m", "AUTH_TOKEN_FILE": "/t", "STATE_DIR": "/s"}, "SQLITE_PATH"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr, func(k string) string { return tc.env[k] })
			if code != exitUsage {
				t.Fatalf("exit %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), tc.says) {
				t.Errorf("stderr %q does not say %q", stderr.String(), tc.says)
			}
			if strings.Contains(stderr.String(), "fake-token-typed-by-mistake") {
				t.Error("stderr echoes the argument")
			}
		})
	}
}

// A bad OTEL setting stops the agent at start, like a bad setting of its own, and its value is not shown.
func TestBadTraceSettingIsAUsageError(t *testing.T) {
	env := map[string]string{"MODEL_REGION": "r", "MODEL_NAME": "m", "AUTH_TOKEN_FILE": "/t",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://user:canary@collector"}
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr, func(k string) string { return env[k] }); code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
	if strings.Contains(stderr.String(), "canary") || !strings.Contains(stderr.String(), "OTEL") {
		t.Fatalf("stderr %q: want an OTEL error that does not show the value", stderr.String())
	}
}

// The configuration error is a line of the agent's own JSON log.
func TestUsageErrorIsAJSONLogLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run(nil, &stdout, &stderr, func(string) string { return "" })
	var line map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &line); err != nil || line["level"] != "ERROR" || line["msg"] != "configuration" {
		t.Fatalf("stderr %q is not one JSON error line: %v", stderr.String(), err)
	}
}

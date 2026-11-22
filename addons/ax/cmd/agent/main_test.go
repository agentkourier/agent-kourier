package main

import (
	"bytes"
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

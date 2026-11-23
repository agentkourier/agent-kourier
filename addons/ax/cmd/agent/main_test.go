package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/addons/ax/config"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
)

func TestVersionFlagPrintsTheBuildVersion(t *testing.T) {
	defer func(v string) { version = v }(version)
	version = "1.2.3"
	var stdout, stderr bytes.Buffer
	if code := runtime.Command(addon(), []string{"--version"}, &stdout, &stderr, func(string) string { return "" }); code != runtime.ExitOK {
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
			code := runtime.Command(addon(), tc.args, &stdout, &stderr, func(k string) string { return tc.env[k] })
			if code != runtime.ExitUsage {
				t.Fatalf("exit %d, want %d", code, runtime.ExitUsage)
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
	if code := runtime.Command(addon(), nil, &stdout, &stderr, func(k string) string { return env[k] }); code != runtime.ExitUsage {
		t.Fatalf("exit %d, want %d", code, runtime.ExitUsage)
	}
	if strings.Contains(stderr.String(), "canary") || !strings.Contains(stderr.String(), "OTEL") {
		t.Fatalf("stderr %q: want an OTEL error that does not show the value", stderr.String())
	}
}

// The configuration error is a line of the agent's own JSON log.
func TestUsageErrorIsAJSONLogLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runtime.Command(addon(), nil, &stdout, &stderr, func(string) string { return "" })
	var line map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &line); err != nil || line["level"] != "ERROR" || line["msg"] != "configuration" {
		t.Fatalf("stderr %q is not one JSON error line: %v", stderr.String(), err)
	}
}

// The model is the provider's: Bedrock's Converse in its region, or the Anthropic Messages API, each with its calls observed.
func TestTheModelIsTheProviders(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, tc := range []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{ModelProvider: config.ProviderBedrock, ModelRegion: "us-east-1", ModelName: "us.anthropic.claude-haiku-4-5-20251001-v1:0"}, "*bedrock.Model"},
		{config.Config{ModelProvider: config.ProviderAnthropic, ModelName: "claude-haiku-4-5"}, "*anthropic.Model"},
	} {
		t.Run(tc.cfg.ModelProvider, func(t *testing.T) {
			m, err := newModel(t.Context(), tc.cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%T", m); got != tc.want || m.Name() != tc.cfg.ModelName {
				t.Errorf("model %s named %q; want %s named %q", got, m.Name(), tc.want, tc.cfg.ModelName)
			}
		})
	}
}

package broker

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// A ChatConnection that is not a namespace and a name Kubernetes could hold is refused before anything is logged, and the refusal never shows
// the value: a token pasted in its place would otherwise reach the log with the route line.
func TestValidateRefusesAChatConnectionKubernetesCannotHold(t *testing.T) {
	const canary = "xoxb-FAKE-LEAK-CANARY"
	for name, conn := range map[string]v1alpha1.ObjectRef{
		"a token as the namespace":  {Namespace: canary, Name: "slack-main"},
		"a token as the name":       {Namespace: "agent-kourier", Name: canary},
		"capitals in the namespace": {Namespace: "Agent-Kourier", Name: "slack-main"},
		"a dot in the namespace":    {Namespace: "agent.kourier", Name: "slack-main"},
		"an underscore in the name": {Namespace: "agent-kourier", Name: "slack_main"},
		"a name of 254 characters":  {Namespace: "agent-kourier", Name: strings.Repeat("a", 254)},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid(t)
			c.ConfigSource, c.ConfigDir, c.ChatConnection = SourceCRDs, "", conn
			err := c.validate()
			if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "DNS-1123") {
				t.Fatalf("validate = %v; want an ErrWiring that says the name is not a DNS-1123 one", err)
			}
			if strings.Contains(err.Error(), conn.Name) || strings.Contains(err.Error(), conn.Namespace) {
				t.Errorf("validate = %q; want a refusal that does not show the value", err)
			}
		})
	}
	// A subdomain is a valid name, and a label is a valid namespace.
	c := valid(t)
	c.ConfigSource, c.ConfigDir, c.ChatConnection = SourceCRDs, "", v1alpha1.ObjectRef{Namespace: "a-1", Name: "slack.main-1"}
	if err := c.validate(); err != nil {
		t.Errorf("validate = %v; want nil for a namespace that is a label and a name that is a subdomain", err)
	}
}

// With tracing on, the exporter and the SDK can log warnings of their own as Run starts (here, a timeout it cannot parse); the route line still
// comes before all of them.
func TestRunNamesTheConfigRouteBeforeTheExporterSpeaks(t *testing.T) {
	quietOTel(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "NOT-A-DURATION")
	out := &logBuffer{}
	c := startConfig(t)
	c.Logger = slog.New(slog.NewJSONHandler(out, nil))
	c.Telemetry = telemetry.Config{Endpoint: "https://collector.example/v1/traces", ServiceName: "test", SampleRatio: 0}
	r := start(t, c)
	r.until("/readyz", http.StatusOK)

	recs := logRecords(t, out)
	otelAt := -1
	for i, rec := range recs {
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, "otel: ") {
			otelAt = i
			break
		}
	}
	if otelAt < 0 {
		t.Fatalf("no otel: line was logged, so this does not test the order:\n%s", out.String())
	}
	if recs[0]["msg"] != "config route" || otelAt == 0 {
		t.Errorf("the first line is %q and the first otel: line is number %d; want the route first", recs[0]["msg"], otelAt)
	}
}

// logRecords parses what a JSON slog handler wrote into one map per line.
func logRecords(t *testing.T, out *logBuffer) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// The broker names the route it reads its resources from, once, before anything else is logged, so that a log tells the two routes apart: on
// the custom resources it also names the ChatConnection it serves, which the files route learns from its directory and logs later.
func TestRunNamesTheConfigRouteAtStart(t *testing.T) {
	named := func(t *testing.T) RunConfig {
		c := startConfig(t)
		c.ConfigSource = SourceFiles
		return c
	}
	for name, tc := range map[string]struct {
		config         func(*testing.T) RunConfig
		wantRoute      string
		wantConnection string // "" is no connection field
	}{
		"files":                        {startConfig, "files", ""},
		"files, named":                 {named, "files", ""},
		"custom resources, connection": {func(t *testing.T) RunConfig { return crStartConfig(t, nil) }, "crds", "agent-kourier-system/slack-main"},
	} {
		t.Run(name, func(t *testing.T) {
			out := &logBuffer{}
			c := tc.config(t)
			c.Logger = slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))
			r := start(t, c)
			r.until("/readyz", http.StatusOK)

			recs := logRecords(t, out)
			var routes []map[string]any
			for _, rec := range recs {
				if rec["msg"] == "config route" {
					routes = append(routes, rec)
				}
			}
			if len(routes) != 1 {
				t.Fatalf("%d lines say \"config route\"; want 1, in:\n%s", len(routes), out.String())
			}
			if routes[0]["config_source"] != tc.wantRoute {
				t.Errorf("config_source = %v, want %q", routes[0]["config_source"], tc.wantRoute)
			}
			got, has := routes[0]["connection"]
			if tc.wantConnection == "" && has {
				t.Errorf("connection = %v on the files route; want none", got)
			}
			if tc.wantConnection != "" && got != tc.wantConnection {
				t.Errorf("connection = %v, want %q", got, tc.wantConnection)
			}
			if recs[0]["msg"] != "config route" {
				t.Errorf("the first line is %q; want the route, so that a log opens with it", recs[0]["msg"])
			}
		})
	}
}

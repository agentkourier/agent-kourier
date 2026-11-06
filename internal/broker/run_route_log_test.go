package broker

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

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

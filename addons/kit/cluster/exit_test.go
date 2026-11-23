package cluster

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// FailStop ends the process once, with ExitLeadershipLost (5, the broker's code for the same end), after one line that
// says why; a second cause, which comes while the first exit runs, adds nothing.
func TestFailStopExitsOnceWithItsCode(t *testing.T) {
	for name, tc := range map[string]struct {
		stop   func(*FailStop)
		reason string
	}{
		"deposed":    {stop: (*FailStop).Deposed, reason: "a write found that another replica has taken the lease"},
		"lease lost": {stop: (*FailStop).LeaseLost, reason: "the lease was lost or could not be renewed"},
	} {
		t.Run(name, func(t *testing.T) {
			var codes []int
			var log bytes.Buffer
			f := &FailStop{Log: slog.New(slog.NewJSONHandler(&log, nil)), Exit: func(code int) { codes = append(codes, code) }}
			tc.stop(f)
			f.Deposed()
			f.LeaseLost()
			if len(codes) != 1 || codes[0] != 5 || ExitLeadershipLost != 5 {
				t.Fatalf("exit codes %v, want one 5", codes)
			}
			lines := strings.Split(strings.TrimSpace(log.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("log lines %q, want one", lines)
			}
			var line struct{ Level, Msg, Reason string }
			if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
				t.Fatal(err)
			}
			if line.Level != "ERROR" || line.Msg != "leadership lost: stopping at once, without a drain" || line.Reason != tc.reason {
				t.Fatalf("log line %s", lines[0])
			}
		})
	}
}

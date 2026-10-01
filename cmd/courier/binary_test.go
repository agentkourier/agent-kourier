package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hookTraces are what only a testhooks build of courier may contain besides the crash function (which
// is checked as a symbol): the names of the environment variables that arm it and move the Slack URL,
// and the warnings that say the binary is a test one and what it runs without. A default build that
// holds one can be told to crash itself, or to send tokens elsewhere, in production.
var hookTraces = []string{
	"COURIER_FAULT",
	"COURIER_SLACK_API_URL",
	"testhooks build: this binary",
	"testhooks build: running with no renderer",
	"testhooks build: running with no interactions",
}

// buildCourier compiles ./cmd/courier with GOFLAGS cleared, as the Dockerfile does, so that a
// developer's -tags in the environment cannot change what is built, and returns the binary's path.
func buildCourier(t *testing.T, tagged bool) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "courier")
	args := []string{"build", "-o", bin}
	if tagged {
		args = append(args, "-tags", "testhooks")
	}
	cmd := exec.CommandContext(t.Context(), "go", append(args, ".")...)
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, out)
	}
	return bin
}

// TestDefaultBinaryLacksTheHooks builds the real command both ways and looks for each trace. The
// tagged build is the control: it must hold every trace, or finding none in the default build proves
// nothing (the strings could have been renamed, or the tag could have stopped selecting anything).
func TestDefaultBinaryLacksTheHooks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tagged bool
	}{{"default", false}, {"testhooks", true}} {
		t.Run(tc.name, func(t *testing.T) {
			bin := buildCourier(t, tc.tagged)
			raw, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			for _, trace := range hookTraces {
				if got := bytes.Contains(raw, []byte(trace)); got != tc.tagged {
					t.Errorf("%s build: %q present = %v, want %v", tc.name, trace, got, tc.tagged)
				}
			}
			nm, err := exec.CommandContext(t.Context(), "go", "tool", "nm", bin).Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(nm), "testhooks.crashSelf"); got != tc.tagged {
				t.Errorf("%s build: symbol testhooks.crashSelf present = %v, want %v", tc.name, got, tc.tagged)
			}
		})
	}
}

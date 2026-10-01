package testhooks_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const probe = "./testdata/probe"

// build compiles the probe, with the testhooks tag if tagged, and returns the binary's path.
func build(t *testing.T, tagged bool) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "probe")
	args := []string{"build", "-o", bin}
	if tagged {
		args = append(args, "-tags", "testhooks")
	}
	if out, err := exec.CommandContext(t.Context(), "go", append(args, probe)...).CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, out)
	}
	return bin
}

// run runs the probe with both hooks armed in its environment.
func run(t *testing.T, bin string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin)
	cmd.Env = append(os.Environ(), "COURIER_FAULT=crash-after-post", "COURIER_SLACK_API_URL=http://fake.test")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestDefaultBuildHasNeitherHook builds the probe both ways and inspects the binaries. The tagged
// build is the control: it must show what the default build must not, so the checks are not vacuous.
func TestDefaultBuildHasNeitherHook(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tagged bool
	}{{"default", false}, {"testhooks", true}} {
		t.Run(tc.name, func(t *testing.T) {
			bin := build(t, tc.tagged)
			nm, err := exec.CommandContext(t.Context(), "go", "tool", "nm", bin).Output()
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			checks := map[string]bool{
				"the crash function (symbol)":    strings.Contains(string(nm), "testhooks.crashSelf"),
				"the COURIER_FAULT name":         bytes.Contains(raw, []byte("COURIER_FAULT")),
				"the COURIER_SLACK_API_URL name": bytes.Contains(raw, []byte("COURIER_SLACK_API_URL")),
			}
			for what, present := range checks {
				if present != tc.tagged {
					t.Errorf("%s build: %s present = %v, want %v", tc.name, what, present, tc.tagged)
				}
			}

			out, err := run(t, bin)
			var exit *exec.ExitError
			killed := errors.As(err, &exit) && exit.Sys().(syscall.WaitStatus).Signal() == syscall.SIGKILL
			switch {
			case tc.tagged && (!killed || !strings.Contains(out, `slack="http://fake.test"`)):
				t.Errorf("tagged probe with both hooks armed: err %v, output %q; want the override applied, then a SIGKILL", err, out)
			case !tc.tagged && (err != nil || !strings.Contains(out, `slack=""`) || !strings.Contains(out, "after-post-returned")):
				t.Errorf("default probe with both hooks armed: err %v, output %q; want no override and no crash", err, out)
			}
		})
	}
}

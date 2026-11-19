package testhooks_test

import (
	"bytes"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// run runs the probe with every hook armed in its environment.
func run(t *testing.T, bin string) (string, error) {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler()) // only its certificate is used: the fake Teams service's CA
	srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	cmd := exec.CommandContext(t.Context(), bin)
	cmd.Env = append(os.Environ(), "AGENTKOURIER_FAULT=crash-after-post", "AGENTKOURIER_SLACK_API_URL=http://fake.test",
		"AGENTKOURIER_TEAMS_URL=https://faketeams.test", "AGENTKOURIER_TEAMS_CA="+string(ca))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// traces reports which traces of the hooks the binary at bin holds: the crash and send-gate functions' symbols, the transport that keeps
// a Teams connection on the fake, and the names of the environment variables, which only the testhooks files refer to.
func traces(t *testing.T, bin string) map[string]bool {
	t.Helper()
	nm, err := exec.CommandContext(t.Context(), "go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]bool{
		"the send-gate function (symbol)":     strings.Contains(string(nm), "testhooks.holdBeforeSend"),
		"the crash function (symbol)":         strings.Contains(string(nm), "testhooks.crashSelf"),
		"the AGENTKOURIER_FAULT name":         bytes.Contains(raw, []byte("AGENTKOURIER_FAULT")),
		"the AGENTKOURIER_SLACK_API_URL name": bytes.Contains(raw, []byte("AGENTKOURIER_SLACK_API_URL")),
		"the fake Teams transport (symbol)":   strings.Contains(string(nm), "testhooks.onlyHost"),
		"the AGENTKOURIER_TEAMS_URL name":     bytes.Contains(raw, []byte("AGENTKOURIER_TEAMS_URL")),
		"the AGENTKOURIER_TEAMS_CA name":      bytes.Contains(raw, []byte("AGENTKOURIER_TEAMS_CA")),
	}
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
			checks := traces(t, bin)
			for what, present := range checks {
				if present != tc.tagged {
					t.Errorf("%s build: %s present = %v, want %v", tc.name, what, present, tc.tagged)
				}
			}

			out, err := run(t, bin)
			var exit *exec.ExitError
			crashed := errors.As(err, &exit) && exit.ExitCode() == 137
			switch {
			case tc.tagged && (!crashed || !strings.Contains(out, `slack="http://fake.test"`) ||
				!strings.Contains(out, `teams="https://faketeams.test/v1.0" err=<nil>`)):
				t.Errorf("tagged probe with every hook armed: err %v, output %q; want the overrides applied, then exit 137", err, out)
			case !tc.tagged && (err != nil || !strings.Contains(out, `slack=""`) || !strings.Contains(out, `teams="" err=<nil>`) ||
				!strings.Contains(out, "after-post-returned")):
				t.Errorf("default probe with every hook armed: err %v, output %q; want no override and no crash", err, out)
			}
		})
	}
}

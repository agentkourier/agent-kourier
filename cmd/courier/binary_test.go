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
// and the warning that says the binary is a test one. A default build that holds one can be told to
// crash itself, or to send tokens elsewhere, in production.
var hookTraces = []string{
	"COURIER_FAULT",
	"COURIER_SLACK_API_URL",
	"testhooks build: this binary",
}

// buildCourier compiles ./cmd/courier with GOFLAGS cleared and GOENV off, as the Dockerfile does, so that
// neither a developer's -tags in the environment nor one in their `go env -w` file can change what is
// built. An empty GOFLAGS= is not enough: cmd/go ignores it and falls back to that file, so GOENV=off
// is what shuts the file out. It returns the binary's path.
func buildCourier(t *testing.T, tagged bool) string {
	t.Helper()
	return goBuild(t, tagged, "GOFLAGS=", "GOENV=off")
}

// goBuild compiles ./cmd/courier, with the testhooks tag if tagged, under env added to the test's own.
func goBuild(t *testing.T, tagged bool, env ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "courier")
	args := []string{"build", "-o", bin}
	if tagged {
		args = append(args, "-tags", "testhooks")
	}
	cmd := exec.CommandContext(t.Context(), "go", append(args, ".")...)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, out)
	}
	return bin
}

// hooks reports which hook traces the binary at bin holds: each of hookTraces, and the crash function's
// symbol, by name.
func hooks(t *testing.T, bin string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, trace := range hookTraces {
		found[trace] = bytes.Contains(raw, []byte(trace))
	}
	nm, err := exec.CommandContext(t.Context(), "go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatal(err)
	}
	found["symbol testhooks.crashSelf"] = strings.Contains(string(nm), "testhooks.crashSelf")
	return found
}

// expectHooks fails the test for each trace of bin whose presence is not want.
func expectHooks(t *testing.T, name, bin string, want bool) {
	t.Helper()
	for trace, got := range hooks(t, bin) {
		if got != want {
			t.Errorf("%s build: %q present = %v, want %v", name, trace, got, want)
		}
	}
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
			expectHooks(t, tc.name, buildCourier(t, tc.tagged), tc.tagged)
		})
	}
}

// A `go env -w` file that sets GOFLAGS=-tags=testhooks must not reach the production build. The first
// half is the control: an empty GOFLAGS= alone does let that file add the tag, so the second half, which
// builds as buildCourier does, would prove nothing without it.
func TestDefaultBinaryIgnoresAGoEnvFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(file, []byte("GOFLAGS=-tags=testhooks\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", file)

	expectHooks(t, "empty GOFLAGS only, with the file", goBuild(t, false, "GOFLAGS="), true)
	expectHooks(t, "default, with the file", buildCourier(t, false), false)
}

//go:build image

package testhooks_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// imageBinary copies the Agent Kourier binary out of a Docker image, which is built, not run: the image has no shell.
func imageBinary(t *testing.T, image string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "docker", "create", image).Output()
	if err != nil {
		t.Fatalf("docker create %s: %v (build it first: make image)", image, err)
	}
	id := strings.TrimSpace(string(out))
	// t.Context has ended by the time cleanup runs.
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "rm", id).Run() })
	bin := filepath.Join(t.TempDir(), "agent-kourier")
	if out, err := exec.CommandContext(t.Context(), "docker", "cp", id+":/agent-kourier", bin).CombinedOutput(); err != nil {
		t.Fatalf("docker cp: %v\n%s", err, out)
	}
	return bin
}

// TestImageHasNeitherHook proves, on the binary the production Dockerfile builds into agent-kourier:dev (or
// $AGENTKOURIER_IMAGE), that the default build carries neither test hook: not the crash function, and not
// the names of the environment variables that arm it or move the Slack API URL. The probe test in
// buildcheck_test.go proves the same of a probe; this proves it of the shipped command line, with
// GOFLAGS cleared as the Dockerfile does. Run it with `make image-check`.
func TestImageHasNeitherHook(t *testing.T) {
	image := os.Getenv("AGENTKOURIER_IMAGE")
	if image == "" {
		image = "agent-kourier:dev"
	}
	user, err := exec.CommandContext(t.Context(), "docker", "image", "inspect", "--format", "{{.Config.User}}", image).Output()
	if err != nil || strings.TrimSpace(string(user)) != "65532:65532" {
		t.Errorf("%s runs as %q, %v; want the non-root 65532:65532", image, user, err)
	}
	assertTraces(t, image, imageBinary(t, image), false)
	// The control: the testhooks image (make image-testhooks) must show every trace, or the checks
	// above prove nothing, as they would not if cmd/agent-kourier stopped linking the hooks at all.
	const testImage = "agent-kourier-e2e/agent-kourier-testhooks:dev"
	assertTraces(t, testImage, imageBinary(t, testImage), true)
}

// imageTraces is traces plus the warning a testhooks build logs at start.
func imageTraces(t *testing.T, bin string) map[string]bool {
	t.Helper()
	found := traces(t, bin)
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	const warning = "testhooks build: this binary"
	found["the warning "+warning] = bytes.Contains(raw, []byte(warning))
	return found
}

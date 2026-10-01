//go:build image

package testhooks_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// imageBinary copies the courier binary out of a Docker image, which is built, not run: the image has no shell.
func imageBinary(t *testing.T, image string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "docker", "create", image).Output()
	if err != nil {
		t.Fatalf("docker create %s: %v (build it first: make image)", image, err)
	}
	id := strings.TrimSpace(string(out))
	// t.Context has ended by the time cleanup runs.
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "rm", id).Run() })
	bin := filepath.Join(t.TempDir(), "courier")
	if out, err := exec.CommandContext(t.Context(), "docker", "cp", id+":/courier", bin).CombinedOutput(); err != nil {
		t.Fatalf("docker cp: %v\n%s", err, out)
	}
	return bin
}

// TestImageHasNeitherHook proves, on the binary the production Dockerfile builds into courier:dev (or
// $COURIER_IMAGE), that the default build carries neither test hook: not the crash function, and not
// the names of the environment variables that arm it or move the Slack API URL. The probe test in
// buildcheck_test.go proves the same of a probe; this proves it of the shipped command line, with
// GOFLAGS cleared as the Dockerfile does. Run it with `make image-check`.
func TestImageHasNeitherHook(t *testing.T) {
	image := os.Getenv("COURIER_IMAGE")
	if image == "" {
		image = "courier:dev"
	}
	user, err := exec.CommandContext(t.Context(), "docker", "image", "inspect", "--format", "{{.Config.User}}", image).Output()
	if err != nil || strings.TrimSpace(string(user)) != "65532:65532" {
		t.Errorf("%s runs as %q, %v; want the non-root 65532:65532", image, user, err)
	}
	for what, present := range traces(t, imageBinary(t, image)) {
		if present {
			t.Errorf("%s: %s is in the production image", image, what)
		}
	}
	// TODO(WP-35f2d): cmd/courier does not link the outbox or the broker yet, so this holds for any
	// binary. Once it does, add the control: the testhooks image (courier-e2e/courier-testhooks:dev)
	// must show all three traces, or the checks above prove nothing.
}

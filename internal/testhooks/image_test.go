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
	prod := imageBinary(t, image)
	for what, present := range imageTraces(t, prod) {
		if present {
			t.Errorf("%s: %s is in the production image", image, what)
		}
	}
	// The control: the testhooks image (make image-testhooks) must show every trace, or the checks
	// above prove nothing, as they would not if cmd/courier stopped linking the hooks at all.
	const testImage = "courier-e2e/courier-testhooks:dev"
	for what, present := range imageTraces(t, imageBinary(t, testImage)) {
		if !present {
			t.Errorf("%s: %s is missing, so the production image's lack of it proves nothing (make image-testhooks)", testImage, what)
		}
	}
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

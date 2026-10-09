package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestDockerfileEntrypointIsWhereTheBinaryIsCopied: the runtime stage's ENTRYPOINT names the path the binary
// is copied to. The image checks run the binary outside the image, so an entrypoint that points nowhere
// builds, passes every check, and fails only when a pod starts it (a rename once did exactly that).
func TestDockerfileEntrypointIsWhereTheBinaryIsCopied(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	last := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "FROM ") {
			last = i
		}
	}
	if last < 0 {
		t.Fatal("the Dockerfile has no FROM")
	}
	var dest string
	var entrypoint []string
	for _, l := range lines[last:] {
		switch {
		case strings.HasPrefix(l, "COPY --from="):
			f := strings.Fields(l)
			dest = f[len(f)-1]
		case strings.HasPrefix(l, "ENTRYPOINT "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "ENTRYPOINT ")), &entrypoint); err != nil {
				t.Fatalf("the ENTRYPOINT is not an exec-form JSON array: %v", err)
			}
		}
	}
	if dest == "" || len(entrypoint) == 0 {
		t.Fatalf("the runtime stage must COPY the binary and set an ENTRYPOINT; got COPY destination %q and ENTRYPOINT %v", dest, entrypoint)
	}
	if entrypoint[0] != dest {
		t.Errorf("ENTRYPOINT %q is not where the binary is copied, %q: no pod could start this image", entrypoint[0], dest)
	}
}

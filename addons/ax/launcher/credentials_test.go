package launcher

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/addons/kit/auth"
)

func TestCallbackRotationKeepsTheLastValidSet(t *testing.T) {
	dir := t.TempDir()
	a2aFile := filepath.Join(dir, "a2a-token")
	if err := os.WriteFile(a2aFile, []byte("fake-a2a"), 0o600); err != nil {
		t.Fatal(err)
	}
	a2a, err := auth.LoadTokens(a2aFile, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tokens := &callbackTokens{path: filepath.Join(dir, "callback-token"), a2a: a2a}
	reload := func(value string, valid bool) {
		t.Helper()
		if err := os.WriteFile(tokens.path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := tokens.reload(); (err == nil) != valid {
			t.Fatalf("reload error = %v, want valid=%v", err, valid)
		}
	}
	reload("fake-old", true)
	reload("fake-old\nfake-new\n", true)
	for _, invalid := range []string{"", strings.Repeat("a", maxCallbackTokenSize+1), "x y", "x,y",
		"ax-placeholder", "x\nx", "x\ny\nz", "fake-old\nfake-a2a"} {
		reload(invalid, false)
		if !tokens.accepts("fake-old") || !tokens.accepts("fake-new") || tokens.accepts("fake-a2a") {
			t.Fatal("invalid update replaced the last valid set")
		}
	}
	reload("fake-new", true)
	if tokens.accepts("fake-old") || !tokens.accepts("fake-new") {
		t.Fatal("retired callback credential remains active")
	}
}

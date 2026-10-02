package chattest

import (
	"os/exec"
	"strings"
	"testing"
)

// TestGoldmarkIsOnlyForTests: the CommonMark reader of this package uses goldmark, which is a dependency of tests
// alone. Nothing courier runs may hold it, and that nothing imports this package outside tests is what keeps it out:
// go list -deps of the command lists every package it is built from.
func TestGoldmarkIsOnlyForTests(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "./cmd/courier")
	cmd.Dir = "../../.." // the module root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/courier: %v", err)
	}
	deps := string(out)
	// The anchors: the courier is built from the chat model and the Slack adapter that this package tests, so a list
	// without them is not the list that proves anything.
	for _, anchor := range []string{"internal/broker", "internal/chat", "internal/chat/slack"} {
		if !strings.Contains(deps, "github.com/agentkourier/agent-kourier/"+anchor+"\n") {
			t.Fatalf("go list -deps ./cmd/courier lists no %s, so it says nothing about goldmark", anchor)
		}
	}
	for _, banned := range []string{"github.com/yuin/goldmark", "github.com/agentkourier/agent-kourier/internal/chat/chattest"} {
		if strings.Contains(deps, banned) {
			t.Errorf("courier is built from %s: a test dependency has reached the binary", banned)
		}
	}
}

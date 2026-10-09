package redacttest

import (
	"os/exec"
	"strings"
	"testing"
)

// TestRedacttestIsOnlyForTests: this package imports the testing package, and nothing Agent Kourier runs may hold it.
// That nothing imports it outside tests is what keeps it out of the binary: go list -deps of the command lists
// every package it is built from.
func TestRedacttestIsOnlyForTests(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "./cmd/agent-kourier")
	cmd.Dir = "../../.." // the module root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/agent-kourier: %v", err)
	}
	deps := "\n" + string(out)
	const module = "github.com/agentkourier/agent-kourier/"
	// The anchors: the Agent Kourier is built from the redaction that this package tests, so a list without them is
	// not the list that proves anything.
	for _, anchor := range []string{"internal/broker", "internal/redact"} {
		if !strings.Contains(deps, "\n"+module+anchor+"\n") {
			t.Fatalf("go list -deps ./cmd/agent-kourier lists no %s, so it says nothing about redacttest", anchor)
		}
	}
	for _, banned := range []string{"testing", module + "internal/redact/redacttest"} {
		if strings.Contains(deps, "\n"+banned+"\n") { // a whole line: one import path
			t.Errorf("Agent Kourier is built from %s: a test dependency has reached the binary", banned)
		}
	}
}

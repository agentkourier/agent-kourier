package storetest

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStoretestIsOnlyForTests: this package imports the testing package and the telemetry test harness, and
// nothing courier runs may hold them. That nothing imports it outside tests is what keeps it out of the
// binary: go list -deps of the command lists every package it is built from.
func TestStoretestIsOnlyForTests(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "./cmd/courier")
	cmd.Dir = "../../.." // the module root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/courier: %v", err)
	}
	deps := "\n" + string(out)
	const module = "github.com/agentkourier/agent-kourier/"
	// The anchors: the courier is built from the store that this package tests, so a list without them is not
	// the list that proves anything.
	for _, anchor := range []string{"internal/broker", "internal/store", "internal/store/sqlite"} {
		if !strings.Contains(deps, module+anchor+"\n") {
			t.Fatalf("go list -deps ./cmd/courier lists no %s, so it says nothing about storetest", anchor)
		}
	}
	for _, banned := range []string{
		"testing", module + "internal/store/storetest", module + "internal/telemetry/telemetrytest",
		"go.opentelemetry.io/otel/sdk/trace/tracetest",
	} {
		if strings.Contains(deps, "\n"+banned+"\n") { // a whole line: one import path
			t.Errorf("courier is built from %s: a test dependency has reached the binary", banned)
		}
	}
}

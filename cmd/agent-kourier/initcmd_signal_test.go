package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// initPromptHelper, set in the environment, makes the test binary run `agent-kourier init` on its own stdin and exit:
// the process that TestInitStopsOnInterruptAtAPrompt interrupts.
const initPromptHelper = "AGENTKOURIER_TEST_INIT_PROMPT_HELPER"

// Ctrl-C at a question stops init. A question waits on stdin, which no context ends, so a signal caught there would
// be lost and the command would wait on.
func TestInitStopsOnInterruptAtAPrompt(t *testing.T) {
	if out := os.Getenv(initPromptHelper); out != "" {
		os.Exit(initCommand([]string{"--out", out}, "dev", os.Stdin, os.Stdout, os.Stderr, func(string) string { return "" }))
	}
	out := filepath.Join(t.TempDir(), "my-values.yaml")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestInitStopsOnInterruptAtAPrompt$")
	cmd.Env = append(os.Environ(), initPromptHelper+"="+out, botTokenKey+"=")
	stdin, err := cmd.StdinPipe() // held open: the question waits for an answer that never comes
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	asked := make(chan struct{})
	go func() {
		r := bufio.NewReader(stderr)
		var seen strings.Builder
		for {
			b, err := r.ReadByte()
			if err != nil {
				return
			}
			seen.WriteByte(b)
			if strings.Contains(seen.String(), "Agent dialect") {
				close(asked)
				_, _ = r.WriteTo(&seen) // drain, so that the process never blocks on a full pipe
				return
			}
		}
	}()
	select {
	case <-asked:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("init asked no question")
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("init exited 0 after SIGINT; want an interrupted exit")
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("init was still waiting at the question 10 seconds after SIGINT")
	}
	if _, err := os.Lstat(out); err == nil {
		t.Errorf("init wrote %s although it was interrupted", out)
	}
}

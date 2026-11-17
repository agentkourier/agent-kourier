package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// lineWriter sends each line written to it on a channel: run writes exactly one, the listening line.
type lineWriter chan string

func (l lineWriter) Write(p []byte) (int, error) {
	l <- strings.TrimSpace(string(p))
	return len(p), nil
}

func TestRunServesTheRepliesAndStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "127.0.0.1:0", "-chat-url", "http://chat.test:8081", "-kourier-v1-secret", "fake-a", "-github-secret", "fake-b"}, out)
	}()
	addr, ok := strings.CutPrefix(<-out, "fakeweb: listening on ")
	if !ok {
		t.Fatal("run did not announce its address")
	}
	resp, err := http.Get("http://" + addr + "/_control/replies") //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("GET /_control/replies = %d %q; want 200 and an empty list", resp.StatusCode, body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("run = %v after the cancel; want nil", err)
	}
}

// TestRunRefusesFlagsWithoutRepeatingThem: a flag it does not know, and a config New refuses, are errors that name no value given.
func TestRunRefusesFlagsWithoutRepeatingThem(t *testing.T) {
	for _, args := range [][]string{
		{"-signing", "fake-secret-value"},
		{"-chat-url", "http://chat.test/path", "-kourier-v1-secret", "fake-secret-value", "-github-secret", "fake-secret-value"},
		{"-chat-url", "http://chat.test", "-kourier-v1-secret", "fake-secret-value"},
	} {
		err := run(t.Context(), args, io.Discard)
		if err == nil {
			t.Errorf("run(%q) succeeded", args)
		} else if strings.Contains(err.Error(), "fake-secret-value") {
			t.Errorf("run(%q) = %q, which repeats a value", args, err)
		}
	}
}

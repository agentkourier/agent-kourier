package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// lineWriter sends each line written to it on a channel. The tests make it with a buffer of one because run writes
// exactly one line (the listening line) and must not block on it.
type lineWriter chan string

func (l lineWriter) Write(p []byte) (int, error) {
	l <- strings.TrimSpace(string(p))
	return len(p), nil
}

func TestRunServesAndStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-addr", "127.0.0.1:0"}, out) }()
	addr, ok := strings.CutPrefix(<-out, "signaler: listening on ")
	if !ok {
		t.Fatal("run did not announce its address")
	}

	// A container that is not one is refused before any signal could go anywhere.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/stop?container=nope", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /stop with no container ID = %d, want 400", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after cancel = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
}

func TestRunRejectsUnknownFlags(t *testing.T) {
	if err := run(t.Context(), []string{"-nope"}, io.Discard); err == nil {
		t.Fatal("run with an unknown flag = nil, want an error")
	}
}

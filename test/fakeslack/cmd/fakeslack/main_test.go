package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// lineWriter sends each line written to it on a channel.
type lineWriter chan string

func (l lineWriter) Write(p []byte) (int, error) {
	l <- strings.TrimSpace(string(p))
	return len(p), nil
}

func call(t *testing.T, method, url string, body io.Reader, header http.Header) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: body is not JSON: %v", method, url, err)
	}
	return out
}

func TestRunServesSlackAndTheControlSurfaceUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-addr", "127.0.0.1:0"}, out) }()
	addr, ok := strings.CutPrefix(<-out, "fakeslack: listening on ")
	if !ok {
		t.Fatal("run did not announce its address")
	}

	auth := call(t, http.MethodPost, "http://"+addr+"/auth.test", nil, http.Header{"Authorization": {"Bearer xoxb-fake-token"}})
	if auth["ok"] != true || auth["user_id"] == "" {
		t.Errorf("auth.test = %v, want ok with a user_id", auth)
	}
	conns := call(t, http.MethodGet, "http://"+addr+"/_control/connections", nil, nil)
	if conns["connections"] != float64(0) {
		t.Errorf("/_control/connections = %v, want zero connections", conns)
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

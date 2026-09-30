package main

import (
	"context"
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

func do(t *testing.T, method, url, body string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

func TestRunRecordsAWebhookAndStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-addr", "127.0.0.1:0"}, out) }()
	addr, ok := strings.CutPrefix(<-out, "sink: listening on ")
	if !ok {
		t.Fatal("run did not announce its address")
	}

	do(t, http.MethodPost, "http://"+addr+"/alertmanager", `{"status":"firing"}`)
	got := do(t, http.MethodGet, "http://"+addr+"/requests", "")
	if !strings.Contains(got, `"body":"{\"status\":\"firing\"}"`) {
		t.Errorf("GET /requests = %s, want the posted body", got)
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

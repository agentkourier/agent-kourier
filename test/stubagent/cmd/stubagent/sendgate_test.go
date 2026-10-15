package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSendGateHoldsOnlyTheFirstSender(t *testing.T) {
	srv := serve(t, 0, false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if got := statusOf(ctx, t, http.MethodPost, srv.URL+"/_control/send-gate/arm"); got != http.StatusNoContent {
		t.Fatalf("arm status %d", got)
	}
	first := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/_control/before-send", strings.NewReader(`{"message_id":"m1","sender":"old"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			first <- 0
			return
		}
		_ = resp.Body.Close()
		first <- resp.StatusCode
	}()
	for {
		stateReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/_control/send-gate", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(stateReq)
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			MessageID string `json:"message_id"`
			Sender    string `json:"sender"`
		}
		err = json.NewDecoder(resp.Body).Decode(&state)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if state.MessageID == "m1" {
			if state.Sender != "old" {
				t.Fatalf("sender %q", state.Sender)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("first sender never reached gate")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case got := <-first:
		t.Fatalf("first sender escaped with %d", got)
	default:
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/_control/before-send", strings.NewReader(`{"message_id":"m1","sender":"new"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second sender status %d", resp.StatusCode)
	}
	if got := statusOf(ctx, t, http.MethodPost, srv.URL+"/_control/send-gate/release"); got != http.StatusNoContent {
		t.Fatalf("release %d", got)
	}
	select {
	case got := <-first:
		if got != http.StatusNoContent {
			t.Fatalf("first status %d", got)
		}
	case <-ctx.Done():
		t.Fatal("released sender stayed held")
	}
}

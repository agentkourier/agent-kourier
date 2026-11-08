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

// lineWriter sends each line written to it on a channel. The tests make it with a buffer of
// one because run writes exactly one line (the listening line) and must not block on it.
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

func TestRunHostsTheAppsItIsGiven(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-addr", "127.0.0.1:0", "-strict-membership", "-socket-spread",
			"-app", "app_id=ATEAM,bot_user=UTEAM,bot_id=BTEAM,team_id=TTEAM,team_name=team,bot_token=xoxb-team-token,app_token=xapp-team-token",
		}, out)
	}()
	addr, ok := strings.CutPrefix(<-out, "fakeslack: listening on ")
	if !ok {
		t.Fatal("run did not announce its address")
	}

	for token, want := range map[string]string{"xoxb-team-token": "UTEAM", "xoxb-someone-else": "UFAKEBOT"} {
		auth := call(t, http.MethodPost, "http://"+addr+"/auth.test", nil, http.Header{"Authorization": {"Bearer " + token}})
		if auth["ok"] != true || auth["user_id"] != want {
			t.Errorf("auth.test with %s = %v, want user_id %s", token, auth, want)
		}
	}
	apps := func() []any {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/_control/apps", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out []any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}()
	if len(apps) != 2 {
		t.Errorf("/_control/apps = %v, want the default app and the one given", apps)
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

func TestRunRejectsAnAppItCannotUseWithoutRepeatingIt(t *testing.T) {
	for name, app := range map[string]string{
		"no key=value":    "xoxb-secret-token",
		"unknown key":     "app_id=A1,bot_user=U1,bot_id=B1,colour=xoxb-secret-token",
		"missing ids":     "bot_token=xoxb-secret-token",
		"wrong token tag": "app_id=A1,bot_user=U1,bot_id=B1,bot_token=secret-token",
	} {
		t.Run(name, func(t *testing.T) {
			err := run(t.Context(), []string{"-addr", "127.0.0.1:0", "-app", app}, io.Discard)
			if err == nil {
				t.Fatal("run = nil, want an error")
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Errorf("error %q repeats the token", err)
			}
		})
	}
}
